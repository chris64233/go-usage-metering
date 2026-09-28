package usagemetering

import (
	"time"

	"github.com/shopspring/decimal"
)

// Event 是一条用量事件的持久化记录。
type Event struct {
	// EventID 外部事件号，租户内唯一、幂等。
	EventID string
	Tenant  string
	// Meter 计量项标识。
	Meter string
	// OccurredAt 用量实际发生时间，决定业务归属周期。
	OccurredAt time.Time
	// Quantity 精确数量，必须非负。
	Quantity decimal.Decimal

	// CommitSeq 提交序号，由存储层在写入成功时单调分配。
	// 它与关账边界（CloseBoundarySeq）共同决定并发上报时事件的唯一周期归属：
	// 提交序号 <= 边界的事件进入被关闭周期，其余事件即使发生时间落在该周期内，
	// 也作为迟到数据进入下一周期。
	CommitSeq int64
	// CommittedAt 提交落库时间（UTC）。
	CommittedAt time.Time
}

// Correction 是针对一条原事件的增量修正，永不覆盖历史。
type Correction struct {
	// CorrectionID 外部修正号，租户内唯一、幂等。
	CorrectionID string
	Tenant       string
	// EventID 被修正的原事件号，必须存在。
	EventID string
	Meter   string
	// OccurredAt 修正业务发生时间，用于周期归属与查询展示。
	OccurredAt time.Time
	// Delta 有符号增量。修正后累计 = 原事件数量 + 该原事件全部修正增量之和。
	Delta decimal.Decimal
	// Floor 业务允许的下限，累计结果不得低于它（通常为 0）。
	Floor decimal.Decimal

	CommitSeq   int64
	CommittedAt time.Time
}

// Period 是一个租户的结算周期，区间为半开 [Start, End)，End 为零值表示开放周期。
type Period struct {
	Tenant string
	Start  time.Time
	// End 为零值表示尚未关闭。
	End time.Time
	// Closed 该周期是否已关闭（是否已生成不可变快照）。
	Closed bool
	// CloseBoundarySeq 关账提交边界：关闭时刻已提交（commit_seq <= 该值）
	// 且发生时间落在周期内的事件/修正才属于本周期快照。
	CloseBoundarySeq int64
	ClosedAt         time.Time
}

// SnapshotLine 是快照中的一行：某计量项在某来源维度上的数量合计。
type SnapshotLine struct {
	Meter string
	// Kind 取值 "event"（原始事件）或 "correction"（修正增量）。
	Kind string
	// Adjustment 为 true 时表示该行来自顺延到本周期的迟到数据（调整项）。
	Adjustment bool
	Quantity   decimal.Decimal
}

// Adjustment 是关闭周期之后才到达、被顺延到下一个开放周期的迟到数据。
// 它保留与原始事件号/修正号以及本应归属周期的来源关联。
type Adjustment struct {
	Tenant string
	// PeriodStart 实际承载该调整项的开放周期起点。
	PeriodStart time.Time
	// Kind 取值 "event" 或 "correction"。
	Kind string
	// RefID 事件号（Kind=event）或修正号（Kind=correction）。
	RefID string
	// EventID 对应的原始事件号（修正调整项即其引用的原事件）。
	EventID string
	Meter   string
	// OccurredAt 业务发生时间（可能落在已关闭周期内）。
	OccurredAt time.Time
	Quantity   decimal.Decimal
	// OriginPeriodStart 该数据按发生时间本应归属的周期起点；
	// 若发生时间早于系统内最早周期，则为零值。
	OriginPeriodStart time.Time
	CommitSeq         int64
	CommittedAt       time.Time
}

// Snapshot 是一个已关闭周期的不可变用量快照。
type Snapshot struct {
	Tenant           string
	PeriodStart      time.Time
	PeriodEnd        time.Time
	ClosedAt         time.Time
	CloseBoundarySeq int64
	// Lines 按计量项、来源类型与是否调整项汇总的数量，稳定排序后返回。
	Lines []SnapshotLine
	// Events 正常原始事件数量合计（按计量项），不含调整项。
	Events map[string]decimal.Decimal
	// Corrections 正常修正增量合计（按计量项），不含调整项。
	Corrections map[string]decimal.Decimal
	// AdjustmentEvents 顺延到本周期的迟到事件合计（按计量项）。
	AdjustmentEvents map[string]decimal.Decimal
	// AdjustmentCorrections 顺延到本周期的迟到修正增量合计（按计量项）。
	AdjustmentCorrections map[string]decimal.Decimal
	// Totals 本周期最终用量合计（按计量项）=
	// Events + Corrections + AdjustmentEvents + AdjustmentCorrections。
	Totals map[string]decimal.Decimal
}

// 来源类型常量。
const (
	KindEvent      = "event"
	KindCorrection = "correction"
)

// PriceTier 是费率中的一个分段：落在 (上一段上限, UpperQuantity] 区间内的数量
// 按 UnitPrice 单价累进计价；UpperQuantity 为 nil 表示开口分段（+∞）。
type PriceTier struct {
	// UpperQuantity 本分段的数量上限（含）；nil 表示开口分段，必须位于末段。
	UpperQuantity *decimal.Decimal
	// UnitPrice 本分段单价，不得为负。
	UnitPrice decimal.Decimal
}

// RateVersion 是按租户、计量项与生效时间发布的费率版本，发布后不可变。
//
// 同一 (租户, 计量项) 下，生效时间最大且不晚于用量发生时间的版本即为该时刻的
// 适用费率；同一生效时间只允许存在一个版本（否则同一时刻会有两份有效费率）。
type RateVersion struct {
	// VersionID 外部费率版本号，租户内唯一、幂等。
	VersionID string
	Tenant    string
	Meter     string
	// EffectiveAt 生效时间（含），UTC。
	EffectiveAt time.Time
	// Tiers 分段价格，按上限严格递增排列，末段必须开口。
	Tiers []PriceTier
	// AmountScale 计价精度：金额保留的小数位数（四舍五入，半数远离零）。
	AmountScale int32
	// PublishedAt 发布落库时间（UTC）。
	PublishedAt time.Time
}

// 草稿版本状态。
const (
	// StatusCurrent 当前草稿版本：同一周期同一时刻至多一个。
	StatusCurrent = "current"
	// StatusVoided 已作废版本：只保留历史，不再是当前草稿。
	StatusVoided = "voided"
)

// PricedSegment 是一条明细在单个费率分段内的计价过程（未舍入的精确值）。
type PricedSegment struct {
	TierIndex int
	// Quantity 落入该分段的数量（非负）。
	Quantity decimal.Decimal
	// UnitPrice 该分段单价（冻结自适用费率版本）。
	UnitPrice decimal.Decimal
	// RawAmount 该分段金额 = Quantity * UnitPrice，未舍入。
	RawAmount decimal.Decimal
}

// BillLine 是账单草稿中的一条计价明细，生成时整体冻结，之后永不改变。
type BillLine struct {
	// LineSeq 版本内行号，按提交序号稳定排序。
	LineSeq int
	// Kind 取值 "event" 或 "correction"。
	Kind string
	// RefID 事件号（Kind=event）或修正号（Kind=correction）。
	RefID string
	// EventID 对应的原始事件号（事件行即其自身）。
	EventID string
	Meter   string
	// Adjustment 为 true 表示迟到调整行（发生时间属于更早周期、顺延到本周期承载）。
	Adjustment bool
	// OriginPeriodStart 调整行按发生时间本应归属的周期；非调整行为零值。
	OriginPeriodStart time.Time
	// OccurredAt 该行用量自身的业务发生时间。
	OccurredAt time.Time
	// Quantity 计价数量（修正可为负；按其绝对值分段计价后再恢复符号）。
	Quantity decimal.Decimal

	// RateVersionID 本行适用的费率版本号。
	RateVersionID string
	// RateEffectiveAt 适用费率的生效时间。
	RateEffectiveAt time.Time
	// Tiers 冻结自适用费率版本的分段价格副本。
	Tiers []PriceTier
	// AmountScale 冻结自适用费率版本的计价精度。
	AmountScale int32
	// Segments 分段计价过程（精确值）。
	Segments []PricedSegment
	// Amount 本行金额：分段精确金额合计后按 AmountScale 四舍五入。
	Amount decimal.Decimal

	// CommitSeq 该行来源数据的提交序号。
	CommitSeq int64
}

// BillVersion 是某周期账单草稿的一个版本。
type BillVersion struct {
	Tenant      string
	PeriodStart time.Time
	// PeriodEnd 生成时冻结的周期终点；生成时周期仍开放则为零值。
	PeriodEnd time.Time
	// Version 版本号，同一周期内从 1 单调递增。
	Version int
	// Status 取值 StatusCurrent 或 StatusVoided。
	Status string
	// PeriodClosed 生成该版本时周期是否已关闭（冻结的周期状态）。
	PeriodClosed bool
	// BoundarySeq 冻结的提交边界：仅 commit_seq <= 该值且由本周期承载的行计入。
	BoundarySeq int64
	// SnapshotQuantities 冻结的周期用量数量快照（按计量项合计，含全部来源与调整行）。
	SnapshotQuantities map[string]decimal.Decimal
	// TotalAmount 金额合计 = Lines 各行 Amount 精确求和（等于全部明细之和）。
	TotalAmount decimal.Decimal
	// Lines 冻结的计价明细，按 LineSeq 排序。
	Lines []BillLine

	CreatedAt time.Time
	VoidedAt  time.Time
}
