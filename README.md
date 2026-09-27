# go-usage-metering

带**修正记录**与**结算截点**的用量计量 Go 服务：原始用量事件幂等上报、
增量修正、结算周期关闭生成不可变快照、迟到数据进入下一周期调整项。

开发环境：Go 1.23.0，零第三方依赖。

## 功能与规则

### 1. 事件上报（幂等 + 冲突检测）

每条原始用量事件包含：

- 外部事件号 `ExternalID`（幂等键）
- 租户 `TenantID`、计量项 `MeterID`
- 发生时间 `OccurredAt`
- 精确数量 `Quantity`（定点十进制，见下）

同一事件号重复上报：

- 内容（租户 / 计量项 / 发生时间 / 数量）完全一致 → 返回首次提交的原结果，
  `RecordResult.Replayed = true`，状态不变；`100.00` 与 `100` 视为同一数量。
- 任一内容不同 → 返回 `KindConflict` 错误，原数据不被覆盖。

### 2. 增量修正（不覆盖历史）

修正（`ReportCorrection`）必须引用一条已存在的原事件号，且只记录
**增量 `Delta`**（可为负），修正后的累计值 = 原数量 + 全部修正增量。

- 同一原事件可以有任意多次独立修正，每次修正有自己的修正号
  `CorrectionID`，修正号同样幂等（同号同内容重放、同号异内容冲突）。
- 每次修正都会校验：原事件数量与全部修正增量之和不得低于业务允许下限
  （`FloorPolicy`，默认下限为 `0`）。低于下限时返回 `KindBelowFloor`，
  该修正不会落库。
- 历史记录永不被改写，修正只是追加一条新记录。

### 3. 结算截点与周期归属

`Schedule` 定义结算周期，内置月结方案 `MonthlySchedule`：每月指定日/时刻
为截点，周期为左闭右开区间 `[Start, End)`，周期 ID 形如 `2026-09`。
截点日超过当月天数时自动钳制到月末（如 31 日遇到 2 月）。

原始事件按 **发生时间** 决定业务归属周期。周期是否仍打开取决于
**提交时刻该周期是否已关闭**（见下）。

### 4. 周期关闭与不可变快照

`ClosePeriod("2026-09")` 生成该周期的不可变快照：

- 快照按 `(租户, 计量项)` 聚合，给出 `Quantity` 与记录条数 `EventCount`。
- 周期未到结束时间不能关闭；ID 非法返回校验错误。
- 重复关闭返回同一份原快照（`Replayed = true`）。
- 快照一经生成永不变化；查询接口返回的是深拷贝，调用方无法污染存储。

### 5. 迟到事件与迟到修正 → 下一周期调整项

周期关闭之后才提交的记录**不会改写旧快照**：

- 迟到的原始事件：按发生时间找到的周期已关闭 → 进入提交时刻的当前
  打开周期，`SourcePeriodID` 记录其本应归属的周期。
- 迟到的修正：原事件所在周期已关闭 → 同样进入当前打开周期，并保留
  原事件号与来源周期关联。

这些记录在归属周期的快照中：

- 数量照常计入对应 `SnapshotLine.Quantity`；
- 同时单列在 `Snapshot.Adjustments`（可用 `ListAdjustments` 查询），
  类型为 `adjustment_event` / `adjustment_correction`，携带来源周期、
  来源事件、发生时间、提交时间与增量，保留完整来源关联。

### 6. 并发与提交边界

所有写操作在 `Storage` 的单个原子事务（读-改-写串行化）内完成。
上报与关闭并发时，以**事务提交顺序**决定唯一归属：

| 提交顺序 | 结果 |
|---|---|
| 事件先提交、关闭后提交 | 事件进入本周期快照 |
| 关闭先提交、事件后提交 | 事件作为调整项进入下一周期 |

不变量：事件不会遗漏，也不会同时进入两个周期；并发关闭只有一个事务
真正生成快照，其余全部返回该快照。

### 7. 精确数量

`Amount` 内部为 `*big.Int` 定点十进制（`val × 10^-scale`），加减比较
全程不经过浮点；JSON 序列化为字符串（也兼容裸数字反序列化），
尾零自动规范化（`1.20 == 1.2`）。

## 错误分类

错误为 `*usagemetering.Error`，用 `errors.As` 或 `usagemetering.AsError`
取出 `Kind`：

| Kind | 含义 |
|---|---|
| `validation` | 入参非法（空字段、数量格式错、周期未结束不能关闭等） |
| `not_found` | 原事件 / 修正号 / 快照不存在 |
| `conflict` | 幂等号已存在但内容不一致 |
| `below_floor` | 修正使累计数量低于业务下限 |
| `storage` | 持久化层错误（IO、数据损坏、状态校验失败） |

## 持久化

内置 `FileStorage`：状态以 JSON 文件落盘，进程内 `sync.RWMutex` 串行化
事务，写入采用「临时文件 + fsync + 原子 rename + 目录 fsync」，崩溃后
只会保留旧版或新版完整状态；只读查询走 `View`，不触发写盘。
多实例部署时可自行实现基于数据库事务的 `Storage` 接口：

```go
type Storage interface {
    Update(ctx context.Context, fn func(State) (State, any, error)) (any, error)
    View(ctx context.Context, fn func(State) (any, error)) (any, error)
}
```

## 快速开始

```go
package main

import (
    "context"
    "fmt"
    "time"

    um "github.com/chris64233/go-usage-metering"
)

func main() {
    ctx := context.Background()
    store := um.NewFileStorage("data/state.json")
    sched, _ := um.NewMonthlySchedule(1, 0, 0, 0, time.UTC) // 每月 1 日 00:00 截点
    svc := um.NewService(store, sched)

    // 上报原始事件
    svc.ReportEvent(ctx, um.EventInput{
        ExternalID: "evt-1001",
        TenantID:   "tenant-a",
        MeterID:    "api-calls",
        OccurredAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
        Quantity:   um.MustParseAmount("100.25"),
    })

    // 增量修正：-20.25，修正后累计 80
    svc.ReportCorrection(ctx, um.CorrectionInput{
        CorrectionID: "corr-1",
        OriginID:     "evt-1001",
        OccurredAt:   time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC),
        Delta:        um.MustParseAmount("-20.25"),
    })

    // 关闭周期（需在周期结束之后）
    res, _ := svc.ClosePeriod(ctx, "2026-09")
    fmt.Println(res.Snapshot.Lines[0].Quantity) // 80

    // 查询快照与调整项
    snap, _ := svc.GetSnapshot(ctx, "2026-09")
    adjs, _ := svc.ListAdjustments(ctx, "2026-09")
    _, _, _ = snap, adjs, ctx
}
```

自定义业务下限：

```go
svc := um.NewService(store, sched, um.WithFloorPolicy(func(tenant, meter string) (um.Amount, bool) {
    if meter == "prepaid-balance" {
        return um.MustParseAmount("-1000"), true // 允许透支到 -1000
    }
    return um.MustParseAmount("0"), true
}))
```

## API 一览

| 方法 | 说明 |
|---|---|
| `ReportEvent` | 原始事件上报（幂等/冲突） |
| `ReportCorrection` | 增量修正（幂等/冲突/下限校验） |
| `ClosePeriod` | 关闭周期，生成不可变快照（重复/并发关闭返回同一快照） |
| `GetSnapshot` | 查询周期快照 |
| `ListAdjustments` | 查询周期快照中的跨期调整项 |
| `GetEvent` / `GetCorrection` | 按外部事件号 / 修正号查询记录 |

## 测试

```bash
go test -race ./...      # 全部测试（含竞态检测）
go test -cover ./...     # 覆盖率
```

测试覆盖：精确十进制运算与 JSON、月结截点边界（含月末钳制）、幂等重放、
内容冲突、多次修正与下限拒绝、迟到事件/修正调整项、旧快照不可变、
快照返回值防污染、上报与关闭并发的不遗漏/不重复、并发关闭只生成一个
快照、进程重启持久化恢复、文件存储原子写/只读不写/损坏检测等。
