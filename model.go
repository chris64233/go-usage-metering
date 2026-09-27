package usagemetering

import "time"

// EventKind 区分原始用量事件与修正记录。
type EventKind string

const (
	// KindEvent 原始用量事件。
	KindEvent EventKind = "event"
	// KindCorrection 修正记录，以增量形式引用某条原始事件。
	KindCorrection EventKind = "correction"
)

// Event 是一条用量记录。原始事件（Type=event）时，
// OriginEventID 为空、Delta 即上报数量；
// 修正记录（Type=correction）时，OriginEventID 指向被修正的原始事件，
// Delta 为相对原始数量的增量（可为负），而不是修正后的绝对值。
//
// 同一条业务记录由外部幂等号唯一标识：原始事件用 ExternalID，
// 修正用 CorrectionID。
type Event struct {
	// Type 记录类型：原始事件 / 修正。
	Type EventKind
	// ExternalID 外部事件号（原始事件的幂等键）。
	ExternalID string
	// CorrectionID 修正号（修正记录的幂等键）。仅修正记录非空。
	CorrectionID string
	// OriginEventID 被修正原始事件的外部事件号。仅修正记录非空。
	OriginEventID string

	TenantID    string
	MeterID     string
	OccurredAt  time.Time
	Delta       Amount
	SubmittedAt time.Time

	// PeriodID 归属周期。原始事件按发生时间归属；迟到的修正/事件记录
	// 按提交时“当前打开周期”归属，并在快照中作为调整项关联到来源。
	PeriodID string

	// SourcePeriodID 来源关联：修正记录所引用原始事件的归属周期；
	// 迟到原始事件按发生时间本应归属的周期。与 PeriodID 不同即表示跨期调整。
	SourcePeriodID string
}

// IsLate 报告该记录是否以调整项形式进入了来源周期之后的周期。
func (e Event) IsLate() bool {
	return e.SourcePeriodID != "" && e.SourcePeriodID != e.PeriodID
}

// SnapshotLine 是快照中单个 (租户, 计量项) 维度的聚合结果。
type SnapshotLine struct {
	TenantID string
	MeterID  string
	// Quantity 归属本周期的全部原始事件与修正增量之和。
	Quantity Amount
	// EventCount 计入数量的记录条数（原始事件与修正各算一条）。
	EventCount int64
}

// Adjustment 是快照中单独列示的跨期调整项（迟到事件或迟到修正）。
// 它保留与来源周期/来源事件的关联，且其数量已包含在本周期 SnapshotLine
// 的 Quantity 中，不会被重复计算。
type Adjustment struct {
	// Kind adjustment_event（迟到原始事件）或 adjustment_correction（迟到修正）。
	Kind EventKind
	// RecordID 外部事件号或修正号。
	RecordID string
	// OriginEventID 修正对应的原始事件号；迟到原始事件时即其自身事件号。
	OriginEventID string
	TenantID      string
	MeterID       string
	OccurredAt    time.Time
	SubmittedAt   time.Time
	Delta         Amount
	// SourcePeriodID 该业务本应归属、但已关闭的周期。
	SourcePeriodID string
}

// Snapshot 是结算周期关闭时生成的不可变用量快照。
type Snapshot struct {
	PeriodID    string
	ClosedAt    time.Time
	Lines       []SnapshotLine
	Adjustments []Adjustment
}
