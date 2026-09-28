# go-usage-metering

带**修正记录**、**结算截点**、**费率版本**与**账单草稿**的用量计量 Go 服务。支持幂等事件上报、
增量修正、结算周期关闭与不可变快照，并在上报/关闭并发下保证每条数据有且仅有一个周期归属；
周期关闭后到达的迟到数据进入下一周期调整项，旧快照永不被改写。在此之上，费率按租户/计量项/
生效时间作为不可变版本发布；账单草稿在生成时冻结快照范围、适用费率副本与逐条计价明细，
支持作废重算、版本保留与精确金额汇总。

- 语言版本：Go 1.23.0
- 存储：SQLite（纯 Go 驱动 [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)），自动建表迁移
- 精确数量/金额：[shopspring/decimal](https://pkg.go.dev/github.com/shopspring/decimal)，规范文本落库，无浮点误差

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

### 7. 费率版本（不可变）

费率按 `(租户, 计量项, 生效时间)` 发布，一份版本包含：

| 字段 | 说明 |
| --- | --- |
| `Version` | `(租户, 计量项)` 内按发布顺序单调分配，从 1 开始 |
| `EffectiveFrom` | 生效起点（含）。适用费率 = `effective_from <= 用量发生时间` 的最新版本 |
| `Tiers` | 分段价格 `[]PriceTier{UpTo, UnitPrice}`，按上限升序，最后一段无上限（`UpTo=0`） |
| `Currency` | 币种；同一草稿内全部明细必须币种一致 |
| `QuantityScale` / `AmountScale` | 数量/金额计价精度（保留小数位，半数远离零取整） |

- **发布后不可修改**：没有修订/删除接口；要改价就发布新生效时间的新版本。
- **同一时刻只有一份有效费率**：同一 `(租户, 计量项, 生效时间)` 只允许一份版本。
  同内容重复发布幂等返回原版本；内容不同返回 `rate_overlap`。
- 允许**补发生效时间更早**的版本（用于修复历史计价配置），但不得与既有生效时间重合。
- 分段计价口径：数量先按 `QuantityScale` 取整，再按“落在每段内的数量 × 该段单价”累加，
  明细金额按 `AmountScale` 取整。修正增量为负时，按绝对值分段计价后恢复符号。

### 8. 账单草稿（冻结、版本化、可作废重算）

账单草稿只能针对**已关闭周期**生成（`GenerateDraft`），生成时一次性冻结：

1. **周期快照范围**：周期边界与关账提交边界 `BoundarySeq`，明细只覆盖快照内数据
   （`commit_seq <= 边界`）——迟到用量不会被塞进旧周期草稿；
2. **适用费率**：每条用量按其计价时间解析费率版本，并把该版本的完整内容
   （tiers/scale/currency）作为 JSON 副本**冻结进明细行**；
3. **计价明细**：每条事件/修正一行 `DraftLine`，含计价数量、费率版本号、金额；
4. **总额**：`TotalAmount` 为全部明细金额的**精确求和**，恒等于
   `NormalAmount + AdjustmentAmount`。

规则与不变量：

- **幂等**：重复生成返回同一草稿（同版本号、同明细）。
- **冻结**：草稿生成后再发布/补发费率，不会改写既有草稿。
- **费率选择**：事件按其发生时间取费率；**修正一律按原事件发生时间取费率**
  （即使是下一周期才到达的迟到修正，也用原事件发生时的费率版本计价）。
- **迟到调整单列**：顺延到本周期的迟到事件/修正，明细 `Adjustment=true`，
  并携带原事件号 `EventID` 与原周期起点 `OriginPeriodStart`，金额计入
  `AdjustmentAmount`，与正常项分列（可用 `ListDraftLines(..., true)` 只看调整项）。
- **作废与重算**：`VoidDraft` 作废当前版本后，再次 `GenerateDraft` 产生**新版本号**；
  旧版本及其明细永久保留（`GetDraftVersion` / `ListDraftVersions` 可查，含作废版本）。
- **当前版本唯一**：同一周期至多一个 `status=current` 的版本，由数据库部分唯一索引
  与进程内互斥共同保证；并发重算只有一个版本成为当前草稿。`VoidDraft` 支持
  `expectedVersion` 乐观并发控制，版本不符返回 `version_conflict`。
- 某计量项在用量发生时刻没有任何已生效费率时返回 `no_applicable_rate`，
  整笔生成回滚，不留下半成品草稿；币种混用返回 `conflict`。

### 9. 计价明细查询

- `GetDraft`：取当前草稿（含全部明细）；无当前版本（未生成或已全部作废）返回 `not_found`。
- `GetDraftVersion`：取指定版本（含明细），**包括已作废历史版本**。
- `ListDraftVersions`：列出周期全部版本头（版本号、状态、边界、总额、创建/作废时间）。
- `ListDraftLines(tenant, periodStart, onlyAdjustment)`：取当前草稿明细，
  `onlyAdjustment=true` 时只返回迟到调整项。

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

	// 发布不可变费率版本：分段价格 + 计价精度。
	// 0-100 单价 0.10，100 以上单价 0.08；数量保留 3 位、金额保留 2 位。
	if _, err := svc.PublishRate(ctx, usagemetering.PublishRateInput{
		Tenant:        "tenant-a",
		Meter:         "storage_gb",
		EffectiveFrom: monthStart,
		Tiers: []usagemetering.PriceTier{
			{UpTo: decimal.RequireFromString("100"), UnitPrice: decimal.RequireFromString("0.10")},
			{UpTo: decimal.Zero, UnitPrice: decimal.RequireFromString("0.08")},
		},
		Currency:      "USD",
		QuantityScale: 3,
		AmountScale:   2,
	}); err != nil {
		panic(err)
	}

	// 为已关闭周期生成账单草稿（冻结快照范围、费率副本与逐条明细）。
	draft, err := svc.GenerateDraft(ctx, "tenant-a", monthStart)
	if err != nil {
		panic(err)
	}
	// draft.TotalAmount 恒等于全部 draft.Lines 金额之和。

	// 作废后可重新生成新版本；旧版本及明细保留。
	if _, err := svc.VoidDraft(ctx, "tenant-a", monthStart, draft.Version); err != nil {
		panic(err)
	}
	newDraft, err := svc.GenerateDraft(ctx, "tenant-a", monthStart)
	if err != nil {
		panic(err)
	}
	_ = newDraft

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
| `PublishRate(ctx, PublishRateInput)` | 发布不可变费率版本；同刻重叠报 `rate_overlap`，同内容幂等 |
| `GetRateVersion(ctx, tenant, meter, v)` | 按版本号读取费率版本 |
| `GetEffectiveRate(ctx, tenant, meter, at)` | 取某时刻适用费率（`effective_from <= at` 最新版） |
| `ListRateVersions(ctx, tenant, meter)` | 列出租户/计量项的全部费率版本（按生效时间） |
| `GenerateDraft(ctx, tenant, periodStart)` | 为已关闭周期生成/返回当前草稿；重复生成返回同一草稿 |
| `VoidDraft(ctx, tenant, periodStart, expectedVersion)` | 作废当前草稿（可校验期望版本号） |
| `GetDraft(ctx, tenant, periodStart)` | 读取当前草稿（含明细） |
| `GetDraftVersion(ctx, tenant, periodStart, v)` | 读取指定版本草稿（含已作废版本） |
| `ListDraftVersions(ctx, tenant, periodStart)` | 列出周期全部草稿版本头 |
| `ListDraftLines(ctx, tenant, periodStart, onlyAdjustment)` | 查询当前草稿计价明细（可仅看调整项） |
| `GetPeriod` / `GetEvent` / `GetCorrection` | 单对象查询 |

## 错误分类

所有错误均为 `*usagemetering.Error`，用 `usagemetering.IsCode(err, code)` 或
`errors.As` 判断；`Cause` 保留底层错误。

| Code | 触发场景 |
| --- | --- |
| `invalid_argument` | 缺字段、负数量/负下限/负精度、终点不晚于起点、周期不连续、分段配置非法等 |
| `not_found` | 周期/事件/原事件/快照/费率版本/草稿版本不存在；或已无当前草稿 |
| `conflict` | 事件号或修正号已存在但内容不同；重复关闭终点不一致；已存在开放周期；草稿内币种不一致 |
| `below_floor` | 修正后累计数量低于业务下限（错误信息含原值、历史增量、本次增量与累计值） |
| `period_closed` | 预留给直接改写已关闭周期的场景（当前写路径自动顺延，不会产生） |
| `period_open` | 对开放周期生成/读取草稿——草稿只能在周期关闭后生成 |
| `rate_overlap` | 同一租户、同一计量项在同一生效时刻已有版本且内容不同；任一时刻有效费率唯一 |
| `no_applicable_rate` | 草稿计价时某计量项在用量发生时刻没有已生效费率；整笔回滚 |
| `version_conflict` | 作废时期望版本号与当前版本不一致（乐观并发控制） |
| `internal` | 存储层等内部故障 |

## 数据模型（SQLite）

- `meta`：全局 `commit_seq` 计数器。
- `periods`：周期（起止、关闭标志、关账边界、关闭时间）。
- `events`：事件（幂等键、发生时间、规范数量文本、提交序号、承载周期、调整标记与来源周期）。
- `corrections`：修正（幂等键、原事件外键、增量、下限、承载周期、调整标记与来源周期）。
- `rate_versions`：不可变费率版本（租户/计量项/版本号主键、生效时刻唯一索引、
  分段价格 JSON、币种、数量与金额精度、发布时间）。
- `drafts`：账单草稿版本（周期+版本号主键、状态、冻结边界、总额、创建/作废时间；
  `status='current'` 的部分唯一索引保证每周期至多一个当前版本）。
- `draft_lines`：草稿计价明细（版本外键、行内唯一 `(kind, ref_id)`、调整标记与
  原周期起点、计价数量/金额、费率版本号及其生效时间、冻结费率 JSON 副本）。

时间一律以 UTC 纳秒时间戳存储；decimal 以 `String()` 规范文本存储；
费率分段与冻结费率副本以 JSON 文本存储。

## 测试

```bash
go test ./...            # 全量测试
go test -race ./...      # 带竞态检测（CI 建议）
```

测试覆盖：事件幂等/冲突与校验、修正增量/幂等/冲突/自定义下限/跨周期下限、
快照精确合计、重复与并发关闭只产生一个结果、迟到事件与迟到修正进入下一周期调整项、
提前上报数据按发生时间移交、上报与关闭高并发下的归属不重不漏（总量配平 +
逐行归属计数）、租户隔离、文件库关闭重开后的精度与状态保持；

费率版本：版本号按发布顺序分配、同刻幂等/`rate_overlap`、入参校验（空分段/上限不递增/
非末段开放/负精度等）、按发生时间选择适用版本（含补发更早版本与边界含）、
分段计价与数量/金额精度取整（含半数远离零与负增量）；

账单草稿：分段计价精确合计且总额恒等于明细之和（落库重读仍成立）、重复生成幂等且
费率后发布不改写冻结草稿、开放周期报 `period_open`、无适用费率整笔回滚不留半成品、
币种混用报 `conflict`、空快照周期出草稿；并发重复生成只产生一个当前版本；
作废重算保留全部历史版本、当前版本唯一、期望版本号乐观并发控制；
迟到调整在下一周期草稿单列、引用原事件与原周期、按原事件发生时间费率计价且不渗入旧周期；
修正提交与关账并发下每笔调整按边界唯一归属、并发作废/重算下版本内不重复且总额配平；
费率/草稿版本/明细在文件库关闭重开后完整保留。
