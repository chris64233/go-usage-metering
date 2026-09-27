# go-usage-metering

带**修正记录**与**结算截点**的用量计量 Go 服务。支持幂等事件上报、增量修正、
结算周期关闭与不可变快照，并在上报/关闭并发下保证每条数据有且仅有一个周期归属；
周期关闭后到达的迟到数据进入下一周期调整项，旧快照永不被改写。

- 语言版本：Go 1.23.0
- 存储：SQLite（纯 Go 驱动 [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)），自动建表迁移
- 精确数量：[shopspring/decimal](https://pkg.go.dev/github.com/shopspring/decimal)，规范文本落库，无浮点误差

## 能力与规则

### 1. 用量事件

每条事件携带外部事件号，并包含：

| 字段 | 说明 |
| --- | --- |
| `EventID` | 外部事件号，**租户内唯一、幂等** |
| `Tenant` | 租户 |
| `Meter` | 计量项 |
| `OccurredAt` | 业务发生时间，决定归属周期 |
| `Quantity` | 精确数量，非负 |

- 同一事件号以**完全相同内容**重复上报：返回首次结果（同一 `CommitSeq`），无副作用。
- 同一事件号内容变化（计量项 / 发生时间 / 数量不同）：返回 `conflict`，首次内容不变。
- 数量按精确数值比较（`1` 与 `1.0` 视为相同内容）。

### 2. 修正（增量，不覆盖历史）

- 修正**必须引用一条已存在的原事件**，计量项继承原事件。
- 每次修正以有符号增量 `Delta` 记账，历史记录只增不改。
- 同一原事件可有多次独立修正，每次使用独立的修正号。
- 修正号本身**租户内幂等**：同号同内容返回原结果；同号内容不同返回 `conflict`。
- **下限校验**：`原事件数量 + 该原事件全部修正增量（含本次） < Floor` 时返回
  `below_floor`，本次修正不落库。`Floor` 缺省为 `0`，可按业务自定义。
  校验口径是该原事件的全局累计，即使修正发生在后续周期也统一生效。

### 3. 结算周期与提交截点

- 周期为租户内首尾相接的半开区间 `[Start, End)`，同一租户至多一个开放周期。
- 每条事件/修正落库时获得全局单调的 `CommitSeq`（明确的提交边界）。
- 关闭周期 `[Start, End)` 时：
  1. 读取当前最大提交序号作为关账边界 `CloseBoundarySeq`；
  2. `commit_seq <= 边界` 且发生时间在周期内的数据进入本周期快照；
  3. 边界之后才提交、但发生时间落在已关闭周期的数据成为**迟到数据**，
     顺延到下一开放周期并标记为调整项；
  4. 提交时周期仍开放（右端未定）、但发生时间 `>= End` 的“提前上报”数据，
     在关账时按发生时间移交下一周期正常承载。
- 关账自动建立从 `End` 开始的下一开放周期（若不存在）。

### 4. 不可变快照

- 周期关闭即生成快照；之后不再被任何上报或修正改写。
- 快照按计量项分别汇总：
  - `Events` / `Corrections`：周期内正常事件与修正；
  - `AdjustmentEvents` / `AdjustmentCorrections`：顺延到本周期的迟到数据；
  - `Totals = Events + Corrections + AdjustmentEvents + AdjustmentCorrections`。
- 重复关闭返回同一份快照；终点不一致的重复关闭返回 `conflict`。
- 并发关闭经进程内互斥与“先查状态再关闭”保证只产生一个结果。

### 5. 调整项（迟到数据）

周期关闭后到达的迟到事件或修正：

- **不改写旧快照**；
- 进入下一开放周期，作为调整项可通过 `ListAdjustments` 查询，并在下一周期关闭时计入其快照；
- 保留来源关联：`RefID`（事件号/修正号）、`EventID`（原事件）、
  `OriginPeriodStart`（按发生时间本应归属的周期；若早于系统内最早周期则为零值）。

### 6. 并发正确性

所有写操作在进程内互斥，并在单个数据库事务中完成
“分配提交序号 → 周期归属判定 → 落库”。上报与关闭因此构成明确的线性序列：

- 先于关账事务提交者进入旧周期；
- 后提交者进入下一周期；
- 不会遗漏，也不会让一条数据同时进入两个周期。

SQLite 连接池被限制为单连接并开启 WAL 与外键约束；数据持久化到文件，重开不丢。

## 快速开始

```go
package main

import (
	"context"
	"time"

	"github.com/chris64233/go-usage-metering"
	"github.com/shopspring/decimal"
)

func main() {
	ctx := context.Background()

	// 文件持久化；首次打开自动建表。测试可用 ":memory:"。
	svc, err := usagemetering.Open("file:meter.db")
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	monthStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)

	// 也可不显式建周期：首次上报会以发生时间自动建立首个开放周期。
	if _, err := svc.EnsurePeriod(ctx, "tenant-a", monthStart); err != nil {
		panic(err)
	}

	// 上报事件（精确数量）。
	if _, err := svc.ReportEvent(ctx, usagemetering.EventInput{
		EventID:    "evt-20260101-001",
		Tenant:     "tenant-a",
		Meter:      "storage_gb",
		OccurredAt: monthStart.Add(2 * time.Hour),
		Quantity:   decimal.RequireFromString("12.345"),
	}); err != nil {
		panic(err)
	}

	// 增量修正（不覆盖历史），默认下限 0。
	if _, err := svc.ApplyCorrection(ctx, usagemetering.CorrectionInput{
		CorrectionID: "corr-001",
		Tenant:       "tenant-a",
		EventID:      "evt-20260101-001",
		OccurredAt:   monthStart.Add(3 * time.Hour),
		Delta:        decimal.RequireFromString("-0.345"),
	}); err != nil {
		panic(err)
	}

	// 关闭周期，得到不可变快照。
	snap, err := svc.ClosePeriod(ctx, "tenant-a", monthStart, monthEnd)
	if err != nil {
		panic(err)
	}
	// snap.Totals["storage_gb"] == 12

	// 关闭后到达的迟到事件自动进入下一周期调整项，可查询来源关联。
	adj, err := svc.ListAdjustments(ctx, "tenant-a", monthEnd)
	_ = adj
}
```

## API 概览

| 方法 | 说明 |
| --- | --- |
| `Open(dsn)` / `Close()` | 打开/关闭存储，自动迁移 |
| `EnsurePeriod(ctx, tenant, start)` | 确保开放周期存在（幂等）；周期须首尾相接 |
| `ReportEvent(ctx, EventInput)` | 幂等上报事件；内容变化报 `conflict` |
| `ApplyCorrection(ctx, CorrectionInput)` | 增量修正；幂等、引用原事件、校验下限 |
| `ClosePeriod(ctx, tenant, start, end)` | 关闭周期并生成快照；重复关闭返回原快照 |
| `GetSnapshot(ctx, tenant, start)` | 查询已关闭周期的不可变快照 |
| `ListAdjustments(ctx, tenant, periodStart)` | 查询某周期承载的迟到调整项（按提交序排序） |
| `GetPeriod` / `GetEvent` / `GetCorrection` | 单对象查询 |

## 错误分类

所有错误均为 `*usagemetering.Error`，用 `usagemetering.IsCode(err, code)` 或
`errors.As` 判断；`Cause` 保留底层错误。

| Code | 触发场景 |
| --- | --- |
| `invalid_argument` | 缺字段、负数量/负下限、终点不晚于起点、周期不连续等 |
| `not_found` | 周期/事件/原事件/快照不存在 |
| `conflict` | 事件号或修正号已存在但内容不同；重复关闭终点不一致；已存在开放周期 |
| `below_floor` | 修正后累计数量低于业务下限（错误信息含原值、历史增量、本次增量与累计值） |
| `period_closed` | 预留给直接改写已关闭周期的场景（当前写路径自动顺延，不会产生） |
| `internal` | 存储层等内部故障 |

## 数据模型（SQLite）

- `meta`：全局 `commit_seq` 计数器。
- `periods`：周期（起止、关闭标志、关账边界、关闭时间）。
- `events`：事件（幂等键、发生时间、规范数量文本、提交序号、承载周期、调整标记与来源周期）。
- `corrections`：修正（幂等键、原事件外键、增量、下限、承载周期、调整标记与来源周期）。

时间一律以 UTC 纳秒时间戳存储；decimal 以 `String()` 规范文本存储。

## 测试

```bash
go test ./...            # 全量测试
go test -race ./...      # 带竞态检测（CI 建议）
```

测试覆盖：事件幂等/冲突与校验、修正增量/幂等/冲突/自定义下限/跨周期下限、
快照精确合计、重复与并发关闭只产生一个结果、迟到事件与迟到修正进入下一周期调整项、
提前上报数据按发生时间移交、上报与关闭高并发下的归属不重不漏（总量配平 +
逐行归属计数）、租户隔离、文件库关闭重开后的精度与状态保持。
