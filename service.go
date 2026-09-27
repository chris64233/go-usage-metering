package usagemetering

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// EventInput 是上报一条用量事件的入参。
type EventInput struct {
	// EventID 外部事件号，租户内幂等。
	EventID string
	Tenant  string
	Meter   string
	// OccurredAt 用量实际发生时间，决定业务归属周期。
	OccurredAt time.Time
	// Quantity 精确数量，必须非负。
	Quantity decimal.Decimal
}

// CorrectionInput 是提交一次增量修正的入参。
type CorrectionInput struct {
	// CorrectionID 外部修正号，租户内幂等。
	CorrectionID string
	Tenant       string
	// EventID 被修正的原事件号，必须已经上报。
	EventID string
	// OccurredAt 修正的业务发生时间，用于周期归属。
	OccurredAt time.Time
	// Delta 有符号增量。
	Delta decimal.Decimal
	// Floor 业务允许的下限；为 nil 时按 0 处理。
	// 校验口径：原事件数量 + 该原事件全部修正增量（含本次）不得低于 Floor。
	Floor *decimal.Decimal
}

// Service 是用量计量服务。所有写操作在进程内互斥串行，并在单个数据库事务中
// 完成“提交序号分配 → 周期归属判定 → 落库”，因此上报与关账并发时，
// 提交顺序与关账边界构成明确的线性序列，事件归属唯一且不重不漏。
type Service struct {
	db *sql.DB
	mu sync.Mutex
}

// Open 打开（必要时创建并迁移）一个计量服务。
// dsn 示例："file:meter.db"、"/var/data/meter.db"（持久化文件）或 ":memory:"（测试用内存库）。
func Open(dsn string) (*Service, error) {
	if dsn == "" {
		return nil, newError(CodeInvalidArgument, "empty dsn")
	}
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	return &Service{db: db}, nil
}

// Close 关闭底层数据库。
func (s *Service) Close() error {
	if err := s.db.Close(); err != nil {
		return wrapError(CodeInternal, err, "close database")
	}
	return nil
}

// EnsurePeriod 确保租户存在一个从 start 开始的开放结算周期，幂等返回。
//
// 周期为半开区间 [Start, End)，租户内必须首尾相接：同一时刻至多一个开放周期，
// 新建开放周期的起点必须等于上一周期的关闭终点（首个周期除外）。
func (s *Service) EnsurePeriod(ctx context.Context, tenant string, start time.Time) (*Period, error) {
	if tenant == "" {
		return nil, newError(CodeInvalidArgument, "tenant is required")
	}
	if start.IsZero() {
		return nil, newError(CodeInvalidArgument, "period start is required")
	}
	start = start.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	if p, err := getPeriod(ctx, tx, tenant, start); err == nil {
		return p, nil // 幂等：周期已存在
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT start_unix, end_unix, closed FROM periods WHERE tenant = ? ORDER BY start_unix`, tenant)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list periods")
	}
	type p struct {
		startNS, endNS int64
		closed         bool
	}
	var existing []p
	for rows.Next() {
		var st, en int64
		var closed int
		if err := rows.Scan(&st, &en, &closed); err != nil {
			_ = rows.Close()
			return nil, wrapError(CodeInternal, err, "scan period")
		}
		existing = append(existing, p{st, en, closed == 1})
	}
	_ = rows.Close()

	for _, e := range existing {
		if !e.closed {
			return nil, newError(CodeConflict,
				"tenant %q already has an open period starting at %s; close it before creating another",
				tenant, nsTime(e.startNS).Format(time.RFC3339Nano))
		}
	}
	if len(existing) > 0 {
		last := existing[len(existing)-1]
		if start.UnixNano() != last.endNS {
			return nil, newError(CodeInvalidArgument,
				"new period must start at %s (the previous period's end) to keep periods contiguous, got %s",
				nsTime(last.endNS).Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO periods(tenant, start_unix, end_unix, closed) VALUES(?, ?, 0, 0)`,
		tenant, start.UnixNano()); err != nil {
		return nil, wrapError(CodeInternal, err, "insert period")
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit period")
	}
	return &Period{Tenant: tenant, Start: start}, nil
}

// GetPeriod 查询周期；不存在返回 CodeNotFound。
func (s *Service) GetPeriod(ctx context.Context, tenant string, start time.Time) (*Period, error) {
	if tenant == "" || start.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	return getPeriod(ctx, s.db, tenant, start.UTC())
}

// ReportEvent 上报一条用量事件。
//
// 同一 (租户, 事件号) 重复上报：
//   - 内容（计量项、发生时间、数量）完全一致：返回首次的结果，不产生任何副作用；
//   - 内容不一致：返回 CodeConflict，首次上报内容不会被改写。
func (s *Service) ReportEvent(ctx context.Context, in EventInput) (*Event, error) {
	if err := validateEvent(in); err != nil {
		return nil, err
	}
	in.OccurredAt = in.OccurredAt.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := scanEvent(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE tenant = ? AND event_id = ?`, in.Tenant, in.EventID)); err == nil {
		if !sameEventContent(existing, in) {
			return nil, conflictEvent(existing, in)
		}
		return existing, nil // 幂等重放
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	assign, err := assignOccurrence(ctx, tx, in.Tenant, in.OccurredAt)
	if err != nil {
		return nil, err
	}
	seq, err := nextCommitSeq(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO events(event_id, tenant, meter, occurred_at_unix, quantity,
                   commit_seq, committed_at_unix, period_start_unix, is_adjustment, origin_period_start_unix)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.EventID, in.Tenant, in.Meter, in.OccurredAt.UnixNano(), in.Quantity.String(),
		seq, now.UnixNano(),
		assign.periodStartNS, b2i(assign.isAdjustment), assign.originStartNS); err != nil {
		if isUniqueErr(err) {
			// 理论上不会发生（互斥 + 预查），防御性地转为重放/冲突判断。
			if existing, gerr := scanEvent(tx.QueryRowContext(ctx,
				`SELECT `+eventCols+` FROM events WHERE tenant = ? AND event_id = ?`, in.Tenant, in.EventID)); gerr == nil {
				if !sameEventContent(existing, in) {
					return nil, conflictEvent(existing, in)
				}
				return existing, nil
			}
		}
		return nil, wrapError(CodeInternal, err, "insert event %q", in.EventID)
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit event")
	}
	return &Event{
		EventID:     in.EventID,
		Tenant:      in.Tenant,
		Meter:       in.Meter,
		OccurredAt:  in.OccurredAt,
		Quantity:    in.Quantity,
		CommitSeq:   seq,
		CommittedAt: now,
	}, nil
}

// ApplyCorrection 提交一次针对原事件的增量修正。
//
// 修正永不覆盖历史：每次调用都会新增一条修正记录。同一原事件允许多次独立修正，
// 但 (租户, 修正号) 幂等——同号同内容返回原结果，同号不同内容返回 CodeConflict。
// 累计结果（原数量 + 该原事件全部修正增量之和，含本次）低于 Floor 时返回 CodeBelowFloor，
// 本次修正不会落库。
func (s *Service) ApplyCorrection(ctx context.Context, in CorrectionInput) (*Correction, error) {
	if err := validateCorrection(in); err != nil {
		return nil, err
	}
	in.OccurredAt = in.OccurredAt.UTC()
	floor := decimal.Zero
	if in.Floor != nil {
		floor = *in.Floor
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := scanCorrection(tx.QueryRowContext(ctx,
		`SELECT `+correctionCols+` FROM corrections WHERE tenant = ? AND correction_id = ?`,
		in.Tenant, in.CorrectionID)); err == nil {
		if !sameCorrectionContent(existing, in) {
			return nil, conflictCorrection(existing, in)
		}
		return existing, nil // 幂等重放
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	// 原事件必须存在，计量项继承原事件。
	var meter, qtyText string
	err = tx.QueryRowContext(ctx,
		`SELECT meter, quantity FROM events WHERE tenant = ? AND event_id = ?`,
		in.Tenant, in.EventID).Scan(&meter, &qtyText)
	if isNoRows(err) {
		return nil, newError(CodeNotFound, "original event %q not found for tenant %q", in.EventID, in.Tenant)
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load original event")
	}
	origQty, err := decimal.NewFromString(qtyText)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse stored quantity")
	}

	// 该原事件此前全部修正增量之和。
	sumRows, err := tx.QueryContext(ctx,
		`SELECT delta FROM corrections WHERE tenant = ? AND event_id = ?`, in.Tenant, in.EventID)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "sum corrections")
	}
	sumDelta := decimal.Zero
	for sumRows.Next() {
		var d string
		if err := sumRows.Scan(&d); err != nil {
			_ = sumRows.Close()
			return nil, wrapError(CodeInternal, err, "scan delta")
		}
		parsed, err := decimal.NewFromString(d)
		if err != nil {
			_ = sumRows.Close()
			return nil, wrapError(CodeInternal, err, "parse stored delta")
		}
		sumDelta = sumDelta.Add(parsed)
	}
	_ = sumRows.Close()

	projected := origQty.Add(sumDelta).Add(in.Delta)
	if projected.Cmp(floor) < 0 {
		return nil, &Error{
			Code:    CodeBelowFloor,
			Message: formatBelowFloor(in.EventID, origQty, sumDelta, in.Delta, floor, projected),
		}
	}

	assign, err := assignOccurrence(ctx, tx, in.Tenant, in.OccurredAt)
	if err != nil {
		return nil, err
	}
	seq, err := nextCommitSeq(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO corrections(correction_id, tenant, event_id, meter, occurred_at_unix, delta, floor,
                        commit_seq, committed_at_unix, period_start_unix, is_adjustment, origin_period_start_unix)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.CorrectionID, in.Tenant, in.EventID, meter, in.OccurredAt.UnixNano(),
		in.Delta.String(), floor.String(),
		seq, now.UnixNano(),
		assign.periodStartNS, b2i(assign.isAdjustment), assign.originStartNS); err != nil {
		if isUniqueErr(err) {
			if existing, gerr := scanCorrection(tx.QueryRowContext(ctx,
				`SELECT `+correctionCols+` FROM corrections WHERE tenant = ? AND correction_id = ?`,
				in.Tenant, in.CorrectionID)); gerr == nil {
				if !sameCorrectionContent(existing, in) {
					return nil, conflictCorrection(existing, in)
				}
				return existing, nil
			}
		}
		if isFKErr(err) {
			return nil, newError(CodeNotFound, "original event %q not found for tenant %q", in.EventID, in.Tenant)
		}
		return nil, wrapError(CodeInternal, err, "insert correction %q", in.CorrectionID)
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit correction")
	}
	return &Correction{
		CorrectionID: in.CorrectionID,
		Tenant:       in.Tenant,
		EventID:      in.EventID,
		Meter:        meter,
		OccurredAt:   in.OccurredAt,
		Delta:        in.Delta,
		Floor:        floor,
		CommitSeq:    seq,
		CommittedAt:  now,
	}, nil
}

// ClosePeriod 关闭指定周期并生成不可变快照。
//
// end 为周期业务终点（半开区间的右端），必须晚于起点；若已存在下一个周期，
// end 必须等于下一周期起点。关账时确定提交边界 CloseBoundarySeq：
// commit_seq <= 边界且发生时间在周期内的事件/修正属于本快照，之后提交的
// 同时间数据作为迟到调整项进入下一开放周期。
//
// 重复关闭返回同一份快照；并发关闭在互斥与状态检查下也只会产生一个结果。
func (s *Service) ClosePeriod(ctx context.Context, tenant string, start, end time.Time) (*Snapshot, error) {
	if tenant == "" {
		return nil, newError(CodeInvalidArgument, "tenant is required")
	}
	if start.IsZero() || end.IsZero() {
		return nil, newError(CodeInvalidArgument, "period start and end are required")
	}
	start, end = start.UTC(), end.UTC()
	if !end.After(start) {
		return nil, newError(CodeInvalidArgument, "period end %s must be after start %s",
			end.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	p, err := getPeriod(ctx, tx, tenant, start)
	if err != nil {
		return nil, err
	}
	if p.Closed {
		if !p.End.Equal(end) {
			return nil, newError(CodeConflict,
				"period starting at %s is already closed with end %s, cannot close it again with end %s",
				start.Format(time.RFC3339Nano), p.End.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
		}
		// 重复关闭：重建并返回既有不可变快照。
		return buildSnapshot(ctx, tx, tenant, start.UnixNano())
	}

	// 若下一个周期已存在（通常由迟到数据触发自动创建），终点必须与其起点一致。
	var nextStart sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(start_unix) FROM periods WHERE tenant = ? AND start_unix > ?`,
		tenant, start.UnixNano()).Scan(&nextStart); err != nil {
		return nil, wrapError(CodeInternal, err, "lookup next period")
	}
	if nextStart.Valid && nextStart.Int64 != end.UnixNano() {
		return nil, newError(CodeInvalidArgument,
			"period end must equal the next period start %s, got %s",
			nsTime(nextStart.Int64).Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	}

	var boundary int64
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'commit_seq'`).Scan(&boundary); err != nil {
		return nil, wrapError(CodeInternal, err, "read commit boundary")
	}
	now := time.Now().UTC()

	// 关账同时确定下一周期的起点：若下一开放周期尚不存在则自动建立，
	// 让顺延的迟到数据与提前上报的“未来”数据都有明确归属。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO periods(tenant, start_unix, end_unix, closed)
		 VALUES(?, ?, 0, 0) ON CONFLICT DO NOTHING`,
		tenant, end.UnixNano()); err != nil {
		return nil, wrapError(CodeInternal, err, "ensure next period")
	}

	// 提交时当前周期仍开放（右端无界），因此可能已接收发生时间 >= end 的数据。
	// 关账按发生时间定稿：把这些数据移交下一周期正常承载，使其不进入本快照、
	// 也不会丢失或重复归属（调整项 is_adjustment=1 的行保留在本周期，不受影响）。
	if _, err := tx.ExecContext(ctx, `
UPDATE events SET period_start_unix = ?
WHERE tenant = ? AND period_start_unix = ? AND is_adjustment = 0 AND occurred_at_unix >= ?`,
		end.UnixNano(), tenant, start.UnixNano(), end.UnixNano()); err != nil {
		return nil, wrapError(CodeInternal, err, "reassign future events")
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE corrections SET period_start_unix = ?
WHERE tenant = ? AND period_start_unix = ? AND is_adjustment = 0 AND occurred_at_unix >= ?`,
		end.UnixNano(), tenant, start.UnixNano(), end.UnixNano()); err != nil {
		return nil, wrapError(CodeInternal, err, "reassign future corrections")
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE periods SET end_unix = ?, closed = 1, close_boundary_seq = ?, closed_at_unix = ?
WHERE tenant = ? AND start_unix = ?`,
		end.UnixNano(), boundary, now.UnixNano(), tenant, start.UnixNano()); err != nil {
		return nil, wrapError(CodeInternal, err, "close period")
	}

	snap, err := buildSnapshot(ctx, tx, tenant, start.UnixNano())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit close")
	}
	return snap, nil
}

// GetSnapshot 返回已关闭周期的不可变快照；周期不存在或尚未关闭返回 CodeNotFound。
func (s *Service) GetSnapshot(ctx context.Context, tenant string, start time.Time) (*Snapshot, error) {
	if tenant == "" || start.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	p, err := getPeriod(ctx, s.db, tenant, start.UTC())
	if err != nil {
		return nil, err
	}
	if !p.Closed {
		return nil, newError(CodeNotFound, "snapshot for period starting at %s does not exist yet (period is open)",
			start.UTC().Format(time.RFC3339Nano))
	}
	return buildSnapshot(ctx, s.db, tenant, p.Start.UnixNano())
}

// ListAdjustments 查询某周期承载的全部调整项（按提交序号排序）。
// 调整项都是在其发生时间所属周期关闭之后才提交、因而顺延到该周期的数据，
// 保留了事件号/修正号与原应归属周期（OriginPeriodStart）的来源关联。
func (s *Service) ListAdjustments(ctx context.Context, tenant string, periodStart time.Time) ([]Adjustment, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	if _, err := getPeriod(ctx, s.db, tenant, periodStart.UTC()); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT kind, ref_id, event_id, meter, occurred_at_unix, quantity, origin_period_start_unix,
       commit_seq, committed_at_unix
FROM (
	SELECT 'event' AS kind, event_id AS ref_id, event_id, meter, occurred_at_unix, quantity,
	       origin_period_start_unix, commit_seq, committed_at_unix
	FROM events
	WHERE tenant = ? AND period_start_unix = ? AND is_adjustment = 1
	UNION ALL
	SELECT 'correction', correction_id, event_id, meter, occurred_at_unix, delta,
	       origin_period_start_unix, commit_seq, committed_at_unix
	FROM corrections
	WHERE tenant = ? AND period_start_unix = ? AND is_adjustment = 1
) ORDER BY commit_seq`,
		tenant, periodStart.UnixNano(), tenant, periodStart.UnixNano())
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list adjustments")
	}
	defer func() { _ = rows.Close() }()

	var out []Adjustment
	for rows.Next() {
		var a Adjustment
		var kind, refID, eventID, meter, qty string
		var occNS, originNS, seq, committedNS int64
		if err := rows.Scan(&kind, &refID, &eventID, &meter, &occNS, &qty, &originNS, &seq, &committedNS); err != nil {
			return nil, wrapError(CodeInternal, err, "scan adjustment")
		}
		q, err := decimal.NewFromString(qty)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse adjustment quantity")
		}
		a = Adjustment{
			Tenant:            tenant,
			PeriodStart:       periodStart.UTC(),
			Kind:              kind,
			RefID:             refID,
			EventID:           eventID,
			Meter:             meter,
			OccurredAt:        nsTime(occNS),
			Quantity:          q,
			OriginPeriodStart: nsTime(originNS),
			CommitSeq:         seq,
			CommittedAt:       nsTime(committedNS),
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetEvent 按事件号查询已上报事件；不存在返回 CodeNotFound。
func (s *Service) GetEvent(ctx context.Context, tenant, eventID string) (*Event, error) {
	if tenant == "" || eventID == "" {
		return nil, newError(CodeInvalidArgument, "tenant and event id are required")
	}
	return scanEvent(s.db.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE tenant = ? AND event_id = ?`, tenant, eventID))
}

// GetCorrection 按修正号查询修正记录；不存在返回 CodeNotFound。
func (s *Service) GetCorrection(ctx context.Context, tenant, correctionID string) (*Correction, error) {
	if tenant == "" || correctionID == "" {
		return nil, newError(CodeInvalidArgument, "tenant and correction id are required")
	}
	return scanCorrection(s.db.QueryRowContext(ctx,
		`SELECT `+correctionCols+` FROM corrections WHERE tenant = ? AND correction_id = ?`, tenant, correctionID))
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

const eventCols = `event_id, tenant, meter, occurred_at_unix, quantity, commit_seq, committed_at_unix`
const correctionCols = `correction_id, tenant, event_id, meter, occurred_at_unix, delta, floor, commit_seq, committed_at_unix`

// rowQuerier 被 *sql.DB 与 *sql.Tx 同时满足，便于在事务内外复用读模型构建。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type assignment struct {
	periodStartNS int64
	originStartNS int64 // 0 表示无更早的已登记周期（发生时间早于系统内最早周期）
	isAdjustment  bool
}

// periodInfo 是归属判定用的周期行投影。
type periodInfo struct {
	startNS, endNS int64
	closed         bool
}

// assignOccurrence 根据业务发生时间与提交时刻的周期开关状态，决定唯一周期归属。
// 必须在持有写事务（且已持有 s.mu）时调用。
func assignOccurrence(ctx context.Context, tx *sql.Tx, tenant string, at time.Time) (assignment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT start_unix, end_unix, closed FROM periods WHERE tenant = ? ORDER BY start_unix`, tenant)
	if err != nil {
		return assignment{}, wrapError(CodeInternal, err, "list periods")
	}
	var periods []periodInfo
	for rows.Next() {
		var st, en, closed int64
		if err := rows.Scan(&st, &en, &closed); err != nil {
			_ = rows.Close()
			return assignment{}, wrapError(CodeInternal, err, "scan periods")
		}
		periods = append(periods, periodInfo{st, en, closed == 1})
	}
	_ = rows.Close()

	atNS := at.UnixNano()

	// 尚无任何周期：以发生时间为起点自动创建首个开放周期，数据正常归属。
	if len(periods) == 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO periods(tenant, start_unix, end_unix, closed) VALUES(?, ?, 0, 0)`,
			tenant, atNS); err != nil {
			return assignment{}, wrapError(CodeInternal, err, "auto create first period")
		}
		return assignment{periodStartNS: atNS}, nil
	}

	// 唯一开放周期（不变量：租户内至多一个，且位于链尾）。
	var open *periodInfo
	for i := range periods {
		if !periods[i].closed {
			open = &periods[i]
		}
	}
	if open != nil && atNS >= open.startNS {
		// 发生时间落在开放周期内（开放周期右端无界）：正常归属。
		return assignment{periodStartNS: open.startNS}, nil
	}

	// 发生时间落入某个已关闭周期：迟到数据，顺延到下一个开放周期。
	for _, p := range periods {
		if p.closed && atNS >= p.startNS && atNS < p.endNS {
			target, err := ensureTailOpen(ctx, tx, tenant, periods)
			if err != nil {
				return assignment{}, err
			}
			return assignment{periodStartNS: target, originStartNS: p.startNS, isAdjustment: true}, nil
		}
	}

	// 发生时间早于系统内最早周期：作为调整项进入当前开放周期（无开放周期则在链尾补建）。
	target, err := ensureTailOpen(ctx, tx, tenant, periods)
	if err != nil {
		return assignment{}, err
	}
	var origin int64 // 无对应历史周期，保留零值
	return assignment{periodStartNS: target, originStartNS: origin, isAdjustment: true}, nil
}

// ensureTailOpen 返回链尾开放周期起点；不存在则以上一周期终点自动创建一个。
func ensureTailOpen(ctx context.Context, tx *sql.Tx, tenant string, periods []periodInfo) (int64, error) {
	for i := range periods {
		if !periods[i].closed {
			return periods[i].startNS, nil
		}
	}
	last := periods[len(periods)-1]
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO periods(tenant, start_unix, end_unix, closed) VALUES(?, ?, 0, 0)`,
		tenant, last.endNS); err != nil {
		return 0, wrapError(CodeInternal, err, "auto create next open period")
	}
	return last.endNS, nil
}

func nextCommitSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE meta SET value = value + 1 WHERE key = 'commit_seq' RETURNING value`).Scan(&seq); err != nil {
		return 0, wrapError(CodeInternal, err, "allocate commit seq")
	}
	return seq, nil
}

func buildSnapshot(ctx context.Context, q rowQuerier, tenant string, startNS int64) (*Snapshot, error) {
	p, err := getPeriod(ctx, q, tenant, nsTime(startNS))
	if err != nil {
		return nil, err
	}
	if !p.Closed {
		return nil, newError(CodeInternal, "snapshot build on open period (start=%s)", p.Start.Format(time.RFC3339Nano))
	}

	snap := &Snapshot{
		Tenant:                tenant,
		PeriodStart:           p.Start,
		PeriodEnd:             p.End,
		ClosedAt:              p.ClosedAt,
		CloseBoundarySeq:      p.CloseBoundarySeq,
		Events:                map[string]decimal.Decimal{},
		Corrections:           map[string]decimal.Decimal{},
		AdjustmentEvents:      map[string]decimal.Decimal{},
		AdjustmentCorrections: map[string]decimal.Decimal{},
		Totals:                map[string]decimal.Decimal{},
	}

	add := func(target map[string]decimal.Decimal, meter, qtyText string) error {
		q, err := decimal.NewFromString(qtyText)
		if err != nil {
			return wrapError(CodeInternal, err, "parse snapshot quantity")
		}
		target[meter] = target[meter].Add(q)
		return nil
	}

	// 正常数据与调整项分别聚合（两者都计入本周期快照，且调整项可清晰区分）。
	evRows, err := q.QueryContext(ctx, `
SELECT meter, quantity, is_adjustment FROM events
WHERE tenant = ? AND period_start_unix = ? AND commit_seq <= ?`,
		tenant, startNS, p.CloseBoundarySeq)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "query snapshot events")
	}
	for evRows.Next() {
		var meter, qty string
		var isAdj int64
		if err := evRows.Scan(&meter, &qty, &isAdj); err != nil {
			_ = evRows.Close()
			return nil, wrapError(CodeInternal, err, "scan snapshot event")
		}
		bucket := snap.Events
		if isAdj == 1 {
			bucket = snap.AdjustmentEvents
		}
		if err := add(bucket, meter, qty); err != nil {
			_ = evRows.Close()
			return nil, err
		}
	}
	_ = evRows.Close()

	corrRows, err := q.QueryContext(ctx, `
SELECT meter, delta, is_adjustment FROM corrections
WHERE tenant = ? AND period_start_unix = ? AND commit_seq <= ?`,
		tenant, startNS, p.CloseBoundarySeq)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "query snapshot corrections")
	}
	for corrRows.Next() {
		var meter, qty string
		var isAdj int64
		if err := corrRows.Scan(&meter, &qty, &isAdj); err != nil {
			_ = corrRows.Close()
			return nil, wrapError(CodeInternal, err, "scan snapshot correction")
		}
		bucket := snap.Corrections
		if isAdj == 1 {
			bucket = snap.AdjustmentCorrections
		}
		if err := add(bucket, meter, qty); err != nil {
			_ = corrRows.Close()
			return nil, err
		}
	}
	_ = corrRows.Close()

	meters := make(map[string]struct{},
		len(snap.Events)+len(snap.Corrections)+len(snap.AdjustmentEvents)+len(snap.AdjustmentCorrections))
	collect := func(m map[string]decimal.Decimal) {
		for meter := range m {
			meters[meter] = struct{}{}
		}
	}
	collect(snap.Events)
	collect(snap.Corrections)
	collect(snap.AdjustmentEvents)
	collect(snap.AdjustmentCorrections)
	for m := range meters {
		snap.Totals[m] = snap.Events[m].
			Add(snap.Corrections[m]).
			Add(snap.AdjustmentEvents[m]).
			Add(snap.AdjustmentCorrections[m])
	}

	// Lines 按 (meter, kind, adjustment) 排序，保证快照内容稳定可比较。
	snap.Lines = make([]SnapshotLine, 0, len(meters)*4)
	addLines := func(bucket map[string]decimal.Decimal, kind string, isAdj bool) {
		for meter, qty := range bucket {
			if qty.IsZero() {
				continue
			}
			snap.Lines = append(snap.Lines, SnapshotLine{
				Meter: meter, Kind: kind, Adjustment: isAdj, Quantity: qty,
			})
		}
	}
	addLines(snap.Events, KindEvent, false)
	addLines(snap.Corrections, KindCorrection, false)
	addLines(snap.AdjustmentEvents, KindEvent, true)
	addLines(snap.AdjustmentCorrections, KindCorrection, true)
	sort.Slice(snap.Lines, func(i, j int) bool {
		a, b := snap.Lines[i], snap.Lines[j]
		if a.Meter != b.Meter {
			return a.Meter < b.Meter
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Adjustment != b.Adjustment {
			return !a.Adjustment // 正常数据排在调整项之前
		}
		return false
	})
	return snap, nil
}

func getPeriod(ctx context.Context, q rowQuerier, tenant string, start time.Time) (*Period, error) {
	var st, en, boundary, closedAtNS int64
	var closed int
	err := q.QueryRowContext(ctx, `
SELECT start_unix, end_unix, closed, close_boundary_seq, closed_at_unix
FROM periods WHERE tenant = ? AND start_unix = ?`,
		tenant, start.UnixNano()).Scan(&st, &en, &closed, &boundary, &closedAtNS)
	if isNoRows(err) {
		return nil, newError(CodeNotFound, "period starting at %s not found for tenant %q",
			start.Format(time.RFC3339Nano), tenant)
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load period")
	}
	p := &Period{
		Tenant:           tenant,
		Start:            nsTime(st),
		Closed:           closed == 1,
		CloseBoundarySeq: boundary,
		ClosedAt:         nsTime(closedAtNS),
	}
	if en != 0 {
		p.End = nsTime(en)
	}
	return p, nil
}

func scanEvent(row *sql.Row) (*Event, error) {
	var e Event
	var occNS, seq, committedNS int64
	var qty string
	if err := row.Scan(&e.EventID, &e.Tenant, &e.Meter, &occNS, &qty, &seq, &committedNS); err != nil {
		if isNoRows(err) {
			return nil, newError(CodeNotFound, "event not found")
		}
		return nil, wrapError(CodeInternal, err, "scan event")
	}
	q, err := decimal.NewFromString(qty)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse stored quantity")
	}
	e.Quantity = q
	e.OccurredAt = nsTime(occNS)
	e.CommitSeq = seq
	e.CommittedAt = nsTime(committedNS)
	return &e, nil
}

func scanCorrection(row *sql.Row) (*Correction, error) {
	var c Correction
	var occNS, seq, committedNS int64
	var delta, floor string
	if err := row.Scan(&c.CorrectionID, &c.Tenant, &c.EventID, &c.Meter, &occNS, &delta, &floor, &seq, &committedNS); err != nil {
		if isNoRows(err) {
			return nil, newError(CodeNotFound, "correction not found")
		}
		return nil, wrapError(CodeInternal, err, "scan correction")
	}
	d, err := decimal.NewFromString(delta)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse stored delta")
	}
	f, err := decimal.NewFromString(floor)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse stored floor")
	}
	c.Delta = d
	c.Floor = f
	c.OccurredAt = nsTime(occNS)
	c.CommitSeq = seq
	c.CommittedAt = nsTime(committedNS)
	return &c, nil
}

// ---------------------------------------------------------------------------
// 校验与对比
// ---------------------------------------------------------------------------

func validateEvent(in EventInput) error {
	if in.Tenant == "" {
		return newError(CodeInvalidArgument, "tenant is required")
	}
	if in.EventID == "" {
		return newError(CodeInvalidArgument, "event id is required")
	}
	if in.Meter == "" {
		return newError(CodeInvalidArgument, "meter is required")
	}
	if in.OccurredAt.IsZero() {
		return newError(CodeInvalidArgument, "occurred_at is required")
	}
	if in.Quantity.IsNegative() {
		return newError(CodeInvalidArgument, "quantity must not be negative, got %s", in.Quantity.String())
	}
	return nil
}

func validateCorrection(in CorrectionInput) error {
	if in.Tenant == "" {
		return newError(CodeInvalidArgument, "tenant is required")
	}
	if in.CorrectionID == "" {
		return newError(CodeInvalidArgument, "correction id is required")
	}
	if in.EventID == "" {
		return newError(CodeInvalidArgument, "original event id is required")
	}
	if in.OccurredAt.IsZero() {
		return newError(CodeInvalidArgument, "occurred_at is required")
	}
	if in.Floor != nil && in.Floor.IsNegative() {
		return newError(CodeInvalidArgument, "floor must not be negative, got %s", in.Floor.String())
	}
	return nil
}

func sameEventContent(stored *Event, in EventInput) bool {
	return stored.Meter == in.Meter &&
		stored.OccurredAt.Equal(in.OccurredAt.UTC()) &&
		stored.Quantity.Equal(in.Quantity)
}

func sameCorrectionContent(stored *Correction, in CorrectionInput) bool {
	floor := decimal.Zero
	if in.Floor != nil {
		floor = *in.Floor
	}
	return stored.EventID == in.EventID &&
		stored.OccurredAt.Equal(in.OccurredAt.UTC()) &&
		stored.Delta.Equal(in.Delta) &&
		stored.Floor.Equal(floor)
}

func conflictEvent(stored *Event, in EventInput) *Error {
	return newError(CodeConflict,
		"event %q already reported for tenant %q with different content: "+
			"stored meter=%q occurred_at=%s quantity=%s; submitted meter=%q occurred_at=%s quantity=%s",
		in.EventID, in.Tenant,
		stored.Meter, stored.OccurredAt.Format(time.RFC3339Nano), stored.Quantity.String(),
		in.Meter, in.OccurredAt.UTC().Format(time.RFC3339Nano), in.Quantity.String())
}

func conflictCorrection(stored *Correction, in CorrectionInput) *Error {
	floor := decimal.Zero
	if in.Floor != nil {
		floor = *in.Floor
	}
	return newError(CodeConflict,
		"correction %q already submitted for tenant %q with different content: "+
			"stored event_id=%q occurred_at=%s delta=%s floor=%s; submitted event_id=%q occurred_at=%s delta=%s floor=%s",
		in.CorrectionID, in.Tenant,
		stored.EventID, stored.OccurredAt.Format(time.RFC3339Nano), stored.Delta.String(), stored.Floor.String(),
		in.EventID, in.OccurredAt.UTC().Format(time.RFC3339Nano), in.Delta.String(), floor.String())
}

func formatBelowFloor(eventID string, orig, prevSum, delta, floor, projected decimal.Decimal) string {
	return "correction rejected: cumulative quantity for event " + eventID +
		" would be " + projected.String() +
		" (original " + orig.String() +
		" + previous corrections " + prevSum.String() +
		" + this delta " + delta.String() +
		"), below the allowed floor " + floor.String()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func isNoRows(err error) bool { return err == sql.ErrNoRows }

func nsTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
