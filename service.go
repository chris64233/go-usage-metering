package usagemetering

import (
	"context"
	"sort"
	"time"
)

// FloorPolicy 返回某 (租户, 计量项) 维度允许的累计数量下限。
// ok=false 表示不设下限；默认策略对所有维度返回 0（累计不允许为负）。
// 策略必须是确定性的纯函数（同一输入恒定输出），因为它会在存储事务内调用。
type FloorPolicy func(tenantID, meterID string) (floor Amount, ok bool)

// Service 是用量计量服务，负责事件上报、修正、结算周期关闭与查询。
// 所有写操作都在底层 Storage 的单个原子事务内完成，因此上报与关闭、
// 关闭与关闭之间天然串行化，提交顺序即线性化顺序：
//
//	先提交的上报 -> 一定计入被关闭的快照；
//	先提交的关闭 -> 后到的记录只能作为调整项进入后续周期。
type Service struct {
	store Storage
	sched Schedule
	now   func() time.Time
	floor FloorPolicy
}

// Option 配置 Service。
type Option func(*Service)

// WithClock 注入时钟，默认使用 time.Now（测试可替换为固定时钟）。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithFloorPolicy 注入累计下限策略。
func WithFloorPolicy(p FloorPolicy) Option {
	return func(s *Service) { s.floor = p }
}

// NewService 创建计量服务。
func NewService(store Storage, sched Schedule, opts ...Option) *Service {
	s := &Service{
		store: store,
		sched: sched,
		now:   time.Now,
		floor: func(_, _ string) (Amount, bool) { return MustParseAmount("0"), true },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// EventInput 是原始用量事件上报参数。
type EventInput struct {
	ExternalID string    // 外部事件号（幂等键）
	TenantID   string    // 租户
	MeterID    string    // 计量项
	OccurredAt time.Time // 发生时间（决定按时间的周期归属）
	Quantity   Amount    // 精确数量
}

// RecordResult 是上报/修正的返回结果。
type RecordResult struct {
	// Record 已落库的记录（原始事件或修正）。
	Record Event
	// Replayed 为 true 表示这是幂等重放：幂等号已存在且内容一致，
	// 返回的是首次提交时的原结果，状态未发生任何变化。
	Replayed bool
}

// ReportEvent 上报一条原始用量事件。
//
// 同一 ExternalID 以相同内容重复上报时返回首次结果（Replayed=true）；
// 若租户、计量项、发生时间或数量任一不同，返回 KindConflict。
//
// 归属规则：按 OccurredAt 找到对应周期；该周期仍打开则计入该周期，
// 该周期已关闭则作为调整项进入提交时刻的当前打开周期，并保留来源周期关联。
func (s *Service) ReportEvent(ctx context.Context, in EventInput) (RecordResult, error) {
	if err := validateEventInput(in); err != nil {
		return RecordResult{}, err
	}
	now := s.now()

	out, err := s.store.Update(ctx, func(st State) (State, any, error) {
		if existing, ok := findEvent(st, in.ExternalID); ok {
			if sameEventContent(existing, in) {
				return st, RecordResult{Record: existing, Replayed: true}, nil
			}
			return st, nil, newError(KindConflict,
				"external event id %q already used with different content", in.ExternalID)
		}

		occurredPeriod := s.sched.PeriodAt(in.OccurredAt)
		targetPeriodID := occurredPeriod.ID
		sourcePeriodID := ""
		if _, closed := st.Snapshots[occurredPeriod.ID]; closed {
			target := s.currentOpenPeriod(st, now)
			targetPeriodID = target.ID
			sourcePeriodID = occurredPeriod.ID
		}

		e := Event{
			Type:           KindEvent,
			ExternalID:     in.ExternalID,
			TenantID:       in.TenantID,
			MeterID:        in.MeterID,
			OccurredAt:     in.OccurredAt.UTC(),
			Delta:          in.Quantity,
			SubmittedAt:    now.UTC(),
			PeriodID:       targetPeriodID,
			SourcePeriodID: sourcePeriodID,
		}
		st.Events = append(st.Events, e)
		return st, RecordResult{Record: e}, nil
	})
	if err != nil {
		return RecordResult{}, err
	}
	return out.(RecordResult), nil
}

// CorrectionInput 是修正参数。Delta 为相对原始数量的增量（可为负），
// 不是修正后的绝对值；修正不覆盖历史，只追加一条增量记录。
type CorrectionInput struct {
	CorrectionID string    // 修正号（幂等键）
	OriginID     string    // 被修正原始事件的外部事件号
	OccurredAt   time.Time // 修正发生/生效时间（仅作记录，归属由提交边界决定）
	Delta        Amount    // 增量
}

// ReportCorrection 提交一条修正。
//
// 修正必须引用一条已存在的原始事件（KindNotFound）。同一 CorrectionID
// 重放且内容一致时返回原结果，内容变化返回 KindConflict。
// 同一原事件允许多次独立修正（各自有不同修正号）；应用本次增量后，
// 原事件数量与其全部修正增量之和不得低于该维度业务下限，否则
// 返回 KindBelowFloor，本次修正不会落库。
//
// 归属规则：原事件所在周期仍打开时，修正计入该周期；该周期已关闭时，
// 修正作为调整项进入提交时刻的当前打开周期，并保留与原事件/来源周期的关联。
func (s *Service) ReportCorrection(ctx context.Context, in CorrectionInput) (RecordResult, error) {
	if err := validateCorrectionInput(in); err != nil {
		return RecordResult{}, err
	}
	now := s.now()

	out, err := s.store.Update(ctx, func(st State) (State, any, error) {
		if existing, ok := findCorrection(st, in.CorrectionID); ok {
			if sameCorrectionContent(existing, in) {
				return st, RecordResult{Record: existing, Replayed: true}, nil
			}
			return st, nil, newError(KindConflict,
				"correction id %q already used with different content", in.CorrectionID)
		}

		origin, ok := findEvent(st, in.OriginID)
		if !ok {
			return st, nil, newError(KindNotFound,
				"origin event %q not found for correction %q", in.OriginID, in.CorrectionID)
		}

		chainTotal := origin.Delta
		for _, e := range st.Events {
			if e.Type == KindCorrection && e.OriginEventID == origin.ExternalID {
				chainTotal = chainTotal.Add(e.Delta)
			}
		}
		chainTotal = chainTotal.Add(in.Delta)
		if floor, ok := s.lookupFloorOk(origin.TenantID, origin.MeterID); ok && chainTotal.Cmp(floor) < 0 {
			return st, nil, newError(KindBelowFloor,
				"correction %q would make corrected quantity of event %q = %s, below floor %s",
				in.CorrectionID, origin.ExternalID, chainTotal.String(), floor.String())
		}

		targetPeriodID := origin.PeriodID
		sourcePeriodID := ""
		if _, closed := st.Snapshots[origin.PeriodID]; closed {
			target := s.currentOpenPeriod(st, now)
			targetPeriodID = target.ID
			sourcePeriodID = origin.PeriodID
		}

		c := Event{
			Type:           KindCorrection,
			CorrectionID:   in.CorrectionID,
			OriginEventID:  origin.ExternalID,
			TenantID:       origin.TenantID,
			MeterID:        origin.MeterID,
			OccurredAt:     in.OccurredAt.UTC(),
			Delta:          in.Delta,
			SubmittedAt:    now.UTC(),
			PeriodID:       targetPeriodID,
			SourcePeriodID: sourcePeriodID,
		}
		st.Events = append(st.Events, c)
		return st, RecordResult{Record: c}, nil
	})
	if err != nil {
		return RecordResult{}, err
	}
	return out.(RecordResult), nil
}

// CloseResult 是周期关闭结果。
type CloseResult struct {
	Snapshot Snapshot
	// Replayed 为 true 表示周期此前已关闭，返回的是原快照。
	Replayed bool
}

// ClosePeriod 关闭指定周期并生成不可变用量快照。
//
// 快照只包含在本事务之前已提交、且归属该周期的记录；因此上报与关闭
// 并发时，按事务提交顺序决定唯一归属，既不遗漏也不会重复计入。
// 重复关闭（含并发关闭）返回同一份快照：事务串行化保证只有第一个
// 关闭事务真正生成快照，后续调用全部 Replayed=true。
func (s *Service) ClosePeriod(ctx context.Context, periodID string) (CloseResult, error) {
	p, err := s.sched.PeriodByID(periodID)
	if err != nil {
		return CloseResult{}, err
	}
	now := s.now()
	if p.End.After(now) {
		return CloseResult{}, newError(KindValidation,
			"period %s ends at %s and cannot be closed yet", periodID, p.End.Format(time.RFC3339Nano))
	}

	out, err := s.store.Update(ctx, func(st State) (State, any, error) {
		if existing, ok := st.Snapshots[periodID]; ok {
			return st, CloseResult{Snapshot: cloneSnapshot(existing), Replayed: true}, nil
		}
		snap := buildSnapshot(periodID, now.UTC(), st.Events)
		st.Snapshots[periodID] = snap
		return st, CloseResult{Snapshot: cloneSnapshot(snap)}, nil
	})
	if err != nil {
		return CloseResult{}, err
	}
	return out.(CloseResult), nil
}

// GetSnapshot 查询已关闭周期的不可变快照。周期未关闭返回 KindNotFound。
func (s *Service) GetSnapshot(ctx context.Context, periodID string) (Snapshot, error) {
	out, err := s.store.View(ctx, func(st State) (any, error) {
		snap, ok := st.Snapshots[periodID]
		if !ok {
			return nil, newError(KindNotFound, "snapshot for period %q not found", periodID)
		}
		return cloneSnapshot(snap), nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return out.(Snapshot), nil
}

// ListAdjustments 查询某周期快照中的跨期调整项（迟到事件与迟到修正）。
// 周期未关闭返回 KindNotFound。返回切片按提交时间排序。
func (s *Service) ListAdjustments(ctx context.Context, periodID string) ([]Adjustment, error) {
	snap, err := s.GetSnapshot(ctx, periodID)
	if err != nil {
		return nil, err
	}
	return append([]Adjustment(nil), snap.Adjustments...), nil
}

// GetEvent 按外部事件号查询原始事件（含其归属与来源周期信息）。
func (s *Service) GetEvent(ctx context.Context, externalID string) (Event, error) {
	out, err := s.store.View(ctx, func(st State) (any, error) {
		e, ok := findEvent(st, externalID)
		if !ok {
			return nil, newError(KindNotFound, "event %q not found", externalID)
		}
		return e, nil
	})
	if err != nil {
		return Event{}, err
	}
	return out.(Event), nil
}

// GetCorrection 按修正号查询修正记录。
func (s *Service) GetCorrection(ctx context.Context, correctionID string) (Event, error) {
	out, err := s.store.View(ctx, func(st State) (any, error) {
		c, ok := findCorrection(st, correctionID)
		if !ok {
			return nil, newError(KindNotFound, "correction %q not found", correctionID)
		}
		return c, nil
	})
	if err != nil {
		return Event{}, err
	}
	return out.(Event), nil
}

// currentOpenPeriod 返回提交时刻记录应进入的打开周期：从“现在”所在周期
// 开始，若它已被关闭（例如在测试或边界场景中提前关闭），则顺次向后找。
func (s *Service) currentOpenPeriod(st State, now time.Time) Period {
	p := s.sched.PeriodAt(now)
	for {
		if _, closed := st.Snapshots[p.ID]; !closed {
			return p
		}
		p = s.sched.Next(p)
	}
}

func (s *Service) lookupFloorOk(tenantID, meterID string) (Amount, bool) {
	if s.floor == nil {
		return Amount{}, false
	}
	return s.floor(tenantID, meterID)
}

func validateEventInput(in EventInput) error {
	if in.ExternalID == "" {
		return newError(KindValidation, "external event id is required")
	}
	if in.TenantID == "" {
		return newError(KindValidation, "tenant id is required for event %q", in.ExternalID)
	}
	if in.MeterID == "" {
		return newError(KindValidation, "meter id is required for event %q", in.ExternalID)
	}
	if in.OccurredAt.IsZero() {
		return newError(KindValidation, "occurred_at is required for event %q", in.ExternalID)
	}
	if in.Quantity.val == nil {
		return newError(KindValidation, "quantity is required for event %q", in.ExternalID)
	}
	return nil
}

func validateCorrectionInput(in CorrectionInput) error {
	if in.CorrectionID == "" {
		return newError(KindValidation, "correction id is required")
	}
	if in.OriginID == "" {
		return newError(KindValidation, "origin event id is required for correction %q", in.CorrectionID)
	}
	if in.OccurredAt.IsZero() {
		return newError(KindValidation, "occurred_at is required for correction %q", in.CorrectionID)
	}
	if in.Delta.val == nil {
		return newError(KindValidation, "delta is required for correction %q", in.CorrectionID)
	}
	return nil
}

func findEvent(st State, externalID string) (Event, bool) {
	for _, e := range st.Events {
		if e.Type == KindEvent && e.ExternalID == externalID {
			return e, true
		}
	}
	return Event{}, false
}

func findCorrection(st State, correctionID string) (Event, bool) {
	for _, e := range st.Events {
		if e.Type == KindCorrection && e.CorrectionID == correctionID {
			return e, true
		}
	}
	return Event{}, false
}

func sameEventContent(e Event, in EventInput) bool {
	return e.TenantID == in.TenantID &&
		e.MeterID == in.MeterID &&
		e.OccurredAt.Equal(in.OccurredAt) &&
		e.Delta.Equal(in.Quantity)
}

func sameCorrectionContent(c Event, in CorrectionInput) bool {
	return c.OriginEventID == in.OriginID &&
		c.OccurredAt.Equal(in.OccurredAt) &&
		c.Delta.Equal(in.Delta)
}

type lineKey struct {
	tenantID string
	meterID  string
}

// buildSnapshot 依据全部已提交记录计算周期快照。
// 调整项的数量同样包含在对应 SnapshotLine.Quantity 中，不重复累计。
func buildSnapshot(periodID string, closedAt time.Time, events []Event) Snapshot {
	snap := Snapshot{PeriodID: periodID, ClosedAt: closedAt}

	totals := map[lineKey]Amount{}
	counts := map[lineKey]int64{}
	var keys []lineKey

	for _, e := range events {
		if e.PeriodID != periodID {
			continue
		}
		k := lineKey{e.TenantID, e.MeterID}
		if _, seen := totals[k]; !seen {
			totals[k] = MustParseAmount("0")
			keys = append(keys, k)
		}
		totals[k] = totals[k].Add(e.Delta)
		counts[k]++

		if e.IsLate() {
			a := Adjustment{
				RecordID:       e.ExternalID,
				OriginEventID:  e.ExternalID,
				TenantID:       e.TenantID,
				MeterID:        e.MeterID,
				OccurredAt:     e.OccurredAt,
				SubmittedAt:    e.SubmittedAt,
				Delta:          e.Delta,
				SourcePeriodID: e.SourcePeriodID,
			}
			if e.Type == KindCorrection {
				a.Kind = "adjustment_correction"
				a.RecordID = e.CorrectionID
				a.OriginEventID = e.OriginEventID
			} else {
				a.Kind = "adjustment_event"
			}
			snap.Adjustments = append(snap.Adjustments, a)
		}
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tenantID != keys[j].tenantID {
			return keys[i].tenantID < keys[j].tenantID
		}
		return keys[i].meterID < keys[j].meterID
	})
	for _, k := range keys {
		snap.Lines = append(snap.Lines, SnapshotLine{
			TenantID:   k.tenantID,
			MeterID:    k.meterID,
			Quantity:   totals[k],
			EventCount: counts[k],
		})
	}

	sort.Slice(snap.Adjustments, func(i, j int) bool {
		if !snap.Adjustments[i].SubmittedAt.Equal(snap.Adjustments[j].SubmittedAt) {
			return snap.Adjustments[i].SubmittedAt.Before(snap.Adjustments[j].SubmittedAt)
		}
		return snap.Adjustments[i].RecordID < snap.Adjustments[j].RecordID
	})
	return snap
}

func cloneSnapshot(s Snapshot) Snapshot {
	out := s
	out.Lines = append([]SnapshotLine(nil), s.Lines...)
	out.Adjustments = append([]Adjustment(nil), s.Adjustments...)
	return out
}
