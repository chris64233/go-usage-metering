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

// DraftLine 是账单草稿中的一条计价明细，对应快照范围内的一条事件或修正。
type DraftLine struct {
	// Kind 取值 "event"（原始事件）或 "correction"（修正增量）。
	Kind string
	// RefID 事件号（Kind=event）或修正号（Kind=correction）。
	RefID string
	// EventID 对应的原始事件号（事件明细即其自身）。
	EventID string
	Meter   string
	// Adjustment 为 true 表示该明细来自顺延到本周期的迟到数据，在草稿中单列。
	Adjustment bool
	// OriginPeriodStart 调整项按发生时间本应归属的原周期起点；非调整项为零值，
	// 早于系统内最早周期的调整项也为零值。
	OriginPeriodStart time.Time
	// OccurredAt 计价依据时间：事件为其发生时间；修正一律按“原事件发生时间”取费率
	// （迟到修正也使用原事件发生时的费率版本，而非修正提交或发生时间的费率）。
	OccurredAt time.Time
	// Quantity 计价数量；修正为有符号增量，金额随之可负。
	Quantity decimal.Decimal
	// RateVersion 该明细解析并冻结的费率版本号。
	RateVersion int32
	// RateEffectiveFrom 适用费率版本的生效时间。
	RateEffectiveFrom time.Time
	// UnitAmount 分段计价摊回的单价（Amount/Quantity），仅供展示。
	UnitAmount decimal.Decimal
	// Amount 该行计价金额（精确计算、按金额精度取整后的不可变结果）。
	Amount decimal.Decimal
}

// Draft 是一个已关闭周期的账单草稿版本。
//
// 一个周期可以有多个版本：作废后重新生成会产生新版本号，旧版本及其明细继续保留；
// 同一周期至多一个 status=current 的版本。草稿内容（快照边界、适用费率副本、
// 逐条计价明细、总额）在生成时冻结，之后即使发布/补发新费率也不会被改写。
type Draft struct {
	Tenant      string
	PeriodStart time.Time
	PeriodEnd   time.Time
	// Version 周期内单调递增的草稿版本号，从 1 开始。
	Version int32
	// Status 取值 DraftStatusCurrent / DraftStatusVoided。
	Status string
	// BoundarySeq 冻结的关账提交边界；明细只覆盖 commit_seq <= 该值的快照数据。
	BoundarySeq int64
	Currency    string
	// TotalAmount 全部明细 Amount 精确求和（normal + adjustment）。
	TotalAmount decimal.Decimal
	// NormalAmount 非调整项明细金额之和。
	NormalAmount decimal.Decimal
	// AdjustmentAmount 迟到调整项明细金额之和（草稿中单列）。
	AdjustmentAmount decimal.Decimal
	// Lines 稳定排序的全部计价明细（正常项在前、调整项在后）。
	Lines []DraftLine
	// CreatedAt 版本生成时间。
	CreatedAt time.Time
	// VoidedAt 作废时间；当前版本为零值。
	VoidedAt time.Time
}
