package usagemetering

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// GenerateBillInput 是生成（或重算）账单草稿的入参。
type GenerateBillInput struct {
	Tenant      string
	PeriodStart time.Time
	// IdempotencyKey 调用方提供的幂等键：同一周期内，同键的重复生成返回同一版本；
	// 已存在当前版本但使用不同键再次请求时返回 CodeBillConflict，避免不同调用方
	// 在不知情下触发重算。作废后可用原键生成新版本。
	IdempotencyKey string
}

// GenerateBill 为指定周期生成账单草稿。
//
// 语义：
//   - 周期允许尚未关闭：此时按“当前时刻已提交”为边界冻结，PeriodClosed=false；
//   - 生成时冻结周期快照（边界、状态、用量数量）、适用费率版本与逐条计价明细；
//   - 重复生成（同一幂等键）返回同一草稿；之后发布或修订费率不会改写已冻结草稿，
//     迟到用量也不会直接塞进旧周期（它们进入下一周期调整行）；
//   - 已存在当前版本而幂等键不同：返回 CodeBillConflict，需先 VoidBill 再重算；
//   - 并发重算（作废与生成竞争）由数据库部分唯一索引保证同一周期只有一个当前版本。
func (s *Service) GenerateBill(ctx context.Context, in GenerateBillInput) (*BillVersion, error) {
	if in.Tenant == "" {
		return nil, newError(CodeInvalidArgument, "tenant is required")
	}
	if in.PeriodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "period start is required")
	}
	if in.IdempotencyKey == "" {
		return nil, newError(CodeInvalidArgument, "idempotency key is required")
	}
	startNS := in.PeriodStart.UTC().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	p, err := getPeriod(ctx, tx, in.Tenant, in.PeriodStart.UTC())
	if err != nil {
		return nil, err
	}

	// 已存在当前版本：按幂等键决定返回原草稿还是报冲突。
	if cur, err := currentBillVersion(ctx, tx, in.Tenant, startNS); err == nil {
		if cur.IdempotencyKey != in.IdempotencyKey {
			return nil, newError(CodeBillConflict,
				"a current bill draft v%d already exists for tenant %q period %s with a different "+
					"idempotency key; void it before regenerating",
				cur.Version, in.Tenant, p.Start.Format(time.RFC3339Nano))
		}
		return loadBillVersion(ctx, tx, in.Tenant, startNS, cur.Version)
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	// 确定本次冻结的提交边界与周期状态。周期已关闭则用关账边界，否则用当前最大提交序号。
	boundary := p.CloseBoundarySeq
	if !p.Closed {
		var cur int64
		if err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = 'commit_seq'`).Scan(&cur); err != nil {
			return nil, wrapError(CodeInternal, err, "read commit boundary")
		}
		boundary = cur
	}

	// 版本号 = 该周期历史版本数 + 1（含已作废版本，版本号单调不复用）。
	var nextVersion int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(version), 0) + 1 FROM bill_versions
WHERE tenant = ? AND period_start_unix = ?`, in.Tenant, startNS).Scan(&nextVersion); err != nil {
		return nil, wrapError(CodeInternal, err, "allocate bill version")
	}

	// 收集承载于本周期、且 commit_seq <= 边界的全部事件与修正（计算视图）。
	rowsData, err := loadBillableRows(ctx, tx, in.Tenant, startNS, boundary)
	if err != nil {
		return nil, err
	}

	// 加载该用量集合涉及计量项的费率目录，并校验每条数据都有适用费率。
	meters := map[string]struct{}{}
	for _, r := range rowsData {
		meters[r.meter] = struct{}{}
	}
	catalog, err := loadRateCatalog(ctx, tx, in.Tenant, meters)
	if err != nil {
		return nil, err
	}
	for _, r := range rowsData {
		if _, ok := catalog.lookup(r.meter, r.rateAtNS); !ok {
			return nil, newError(CodeNoRate,
				"no applicable rate for tenant %q meter %q at %s (source %s %q); "+
					"publish a rate version effective no later than the usage time first",
				in.Tenant, r.meter, nsTime(r.rateAtNS).Format(time.RFC3339Nano), r.kind, r.refID)
		}
	}

	// 逐行计价并冻结费率副本。
	now := time.Now().UTC()
	built := make([]*BillLine, 0, len(rowsData))
	snapshotQty := map[string]decimal.Decimal{}
	total := decimal.Zero
	for i, r := range rowsData {
		rate, _ := catalog.lookup(r.meter, r.rateAtNS)
		segs, amount := priceQuantity(r.quantity, rate)
		origin := time.Time{}
		if r.isAdjust && r.originNS != 0 {
			origin = nsTime(r.originNS)
		}
		line := &BillLine{
			LineSeq:           i + 1,
			Kind:              r.kind,
			RefID:             r.refID,
			EventID:           r.eventID,
			Meter:             r.meter,
			Adjustment:        r.isAdjust,
			OriginPeriodStart: origin,
			OccurredAt:        nsTime(r.occurredNS),
			Quantity:          r.quantity,
			RateVersionID:     rate.versionID,
			RateEffectiveAt:   nsTime(rate.effectiveNS),
			Tiers:             cloneTiers(rate.tiers),
			AmountScale:       rate.amountScale,
			Segments:          segs,
			Amount:            amount,
			CommitSeq:         r.commitSeq,
		}
		built = append(built, line)
		snapshotQty[r.meter] = snapshotQty[r.meter].Add(r.quantity)
		total = total.Add(amount)
	}

	periodEnd := time.Time{}
	if p.Closed {
		periodEnd = p.End
	}

	if err := insertBillVersion(ctx, tx, billInsert{
		tenant:         in.Tenant,
		startNS:        startNS,
		version:        nextVersion,
		status:         StatusCurrent,
		periodEndNS:    periodEnd.UnixNano(),
		periodClosed:   p.Closed,
		boundary:       boundary,
		idempotencyKey: in.IdempotencyKey,
		total:          total,
		createdNS:      now.UnixNano(),
		lines:          built,
		snapshotQty:    snapshotQty,
	}); err != nil {
		if isUniqueErr(err) && isPartialCurrentIndexErr(err) {
			// 与并发重算竞争：另一个版本已成为当前草稿。
			return nil, newError(CodeBillConflict,
				"concurrent bill regeneration for tenant %q period %s: another version became current first; "+
					"reload the current draft",
				in.Tenant, p.Start.Format(time.RFC3339Nano))
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit bill")
	}
	return assembleBill(in.Tenant, startNS, nextVersion, periodEnd, p.Closed, boundary,
		snapshotQty, total, built, now, time.Time{}), nil
}

// GetCurrentBill 返回某周期的当前草稿版本；无当前版本（从未生成或已全部作废）
// 返回 CodeNotFound。
func (s *Service) GetCurrentBill(ctx context.Context, tenant string, periodStart time.Time) (*BillVersion, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	startNS := periodStart.UTC().UnixNano()
	cur, err := currentBillVersion(ctx, s.db, tenant, startNS)
	if err != nil {
		return nil, err
	}
	return loadBillVersion(ctx, s.db, tenant, startNS, cur.Version)
}

// GetBillVersion 返回指定版本号的草稿（含已作废版本，其明细一并保留）。
func (s *Service) GetBillVersion(ctx context.Context, tenant string, periodStart time.Time, version int) (*BillVersion, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	if version <= 0 {
		return nil, newError(CodeInvalidArgument, "version must be positive, got %d", version)
	}
	return loadBillVersion(ctx, s.db, tenant, periodStart.UTC().UnixNano(), version)
}

// ListBillVersions 列出某周期的全部草稿版本（含已作废），按版本号升序。
func (s *Service) ListBillVersions(ctx context.Context, tenant string, periodStart time.Time) ([]BillVersion, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	startNS := periodStart.UTC().UnixNano()
	if _, err := getPeriod(ctx, s.db, tenant, periodStart.UTC()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT version FROM bill_versions
WHERE tenant = ? AND period_start_unix = ? ORDER BY version`, tenant, startNS)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list bill versions")
	}
	defer func() { _ = rows.Close() }()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, wrapError(CodeInternal, err, "scan bill version")
		}
		versions = append(versions, v)
	}
	_ = rows.Close()
	out := make([]BillVersion, 0, len(versions))
	for _, v := range versions {
		bv, err := loadBillVersion(ctx, s.db, tenant, startNS, v)
		if err != nil {
			return nil, err
		}
		out = append(out, *bv)
	}
	return out, nil
}

// VoidBill 作废当前草稿版本。旧版本及其明细继续保留（状态置为 voided），
// 随后可用（原或新的）幂等键调用 GenerateBill 生成新版本。
//
// 对已作废版本或没有当前版本时重复作废：返回 CodeBillConflict。
func (s *Service) VoidBill(ctx context.Context, tenant string, periodStart time.Time) (*BillVersion, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	startNS := periodStart.UTC().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := currentBillVersion(ctx, tx, tenant, startNS)
	if err != nil {
		if IsCode(err, CodeNotFound) {
			return nil, newError(CodeBillConflict,
				"no current bill draft to void for tenant %q period %s",
				tenant, periodStart.UTC().Format(time.RFC3339Nano))
		}
		return nil, err
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
UPDATE bill_versions SET status = ?, voided_at_unix = ?
WHERE tenant = ? AND period_start_unix = ? AND version = ? AND status = ?`,
		StatusVoided, now.UnixNano(), tenant, startNS, cur.Version, StatusCurrent)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "void bill")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, newError(CodeBillConflict,
			"bill v%d for tenant %q period %s is no longer current", cur.Version, tenant,
			periodStart.UTC().Format(time.RFC3339Nano))
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit void")
	}
	return loadBillVersion(ctx, s.db, tenant, startNS, cur.Version)
}

// ListBillLines 查询某草稿版本的计价明细（含分段计价过程），按行号排序。
// 版本不存在返回 CodeNotFound。
func (s *Service) ListBillLines(ctx context.Context, tenant string, periodStart time.Time, version int) ([]BillLine, error) {
	bv, err := s.GetBillVersion(ctx, tenant, periodStart, version)
	if err != nil {
		return nil, err
	}
	return bv.Lines, nil
}

// ---------------------------------------------------------------------------
// 计算视图与持久化
// ---------------------------------------------------------------------------

// billableRow 是账单计算的输入行：承载于本周期、提交序号不晚于边界的数据。
type billableRow struct {
	kind       string
	refID      string
	eventID    string
	meter      string
	isAdjust   bool
	originNS   int64
	occurredNS int64
	// rateAtNS 计价费率的查询时刻：事件行取事件发生时间；正常修正取修正发生时间；
	// 迟到修正调整行取“原事件发生时间”，金额按原事件当时的费率计算。
	rateAtNS  int64
	quantity  decimal.Decimal
	commitSeq int64
}

func loadBillableRows(ctx context.Context, q rowQuerier, tenant string, startNS, boundary int64) ([]billableRow, error) {
	rows, err := q.QueryContext(ctx, `
SELECT kind, ref_id, event_id, meter, is_adjustment, origin_period_start_unix,
       occurred_at_unix, rate_at_unix, quantity, commit_seq
FROM (
	SELECT 'event' AS kind, event_id AS ref_id, event_id, meter, is_adjustment,
	       origin_period_start_unix, occurred_at_unix,
	       occurred_at_unix AS rate_at_unix, quantity, commit_seq
	FROM events
	WHERE tenant = ? AND period_start_unix = ? AND commit_seq <= ?
	UNION ALL
	SELECT 'correction', c.correction_id, c.event_id, c.meter, c.is_adjustment,
	       c.origin_period_start_unix, c.occurred_at_unix,
	       -- 迟到修正的金额按原事件发生时的费率计算；周期内正常修正按自身发生时间。
	       CASE WHEN c.is_adjustment = 1 THEN e.occurred_at_unix ELSE c.occurred_at_unix END,
	       c.delta, c.commit_seq
	FROM corrections c
	JOIN events e ON e.tenant = c.tenant AND e.event_id = c.event_id
	WHERE c.tenant = ? AND c.period_start_unix = ? AND c.commit_seq <= ?
) ORDER BY commit_seq, kind, ref_id`,
		tenant, startNS, boundary, tenant, startNS, boundary)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "query billable rows")
	}
	defer func() { _ = rows.Close() }()

	var out []billableRow
	for rows.Next() {
		var r billableRow
		var isAdj int64
		var qty string
		if err := rows.Scan(&r.kind, &r.refID, &r.eventID, &r.meter, &isAdj,
			&r.originNS, &r.occurredNS, &r.rateAtNS, &qty, &r.commitSeq); err != nil {
			return nil, wrapError(CodeInternal, err, "scan billable row")
		}
		q, err := decimal.NewFromString(qty)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse billable quantity")
		}
		r.quantity = q
		r.isAdjust = isAdj == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// billMeta 是 bill_versions 行的精简投影。
type billMeta struct {
	Version        int
	Status         string
	IdempotencyKey string
}

func currentBillVersion(ctx context.Context, q rowQuerier, tenant string, startNS int64) (billMeta, error) {
	var m billMeta
	err := q.QueryRowContext(ctx, `
SELECT version, status, idempotency_key FROM bill_versions
WHERE tenant = ? AND period_start_unix = ? AND status = ?`,
		tenant, startNS, StatusCurrent).Scan(&m.Version, &m.Status, &m.IdempotencyKey)
	if isNoRows(err) {
		return billMeta{}, newError(CodeNotFound,
			"no current bill draft for tenant %q period %s", tenant, nsTime(startNS).Format(time.RFC3339Nano))
	}
	if err != nil {
		return billMeta{}, wrapError(CodeInternal, err, "load current bill")
	}
	return m, nil
}

type billInsert struct {
	tenant         string
	startNS        int64
	version        int
	status         string
	periodEndNS    int64
	periodClosed   bool
	boundary       int64
	idempotencyKey string
	total          decimal.Decimal
	createdNS      int64
	lines          []*BillLine
	snapshotQty    map[string]decimal.Decimal
}

func insertBillVersion(ctx context.Context, tx *sql.Tx, in billInsert) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO bill_versions(tenant, period_start_unix, version, status, period_end_unix,
                          period_closed, boundary_seq, total_amount, idempotency_key, created_at_unix)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.tenant, in.startNS, in.version, in.status, in.periodEndNS, b2i(in.periodClosed),
		in.boundary, in.total.String(), in.idempotencyKey, in.createdNS); err != nil {
		return wrapError(CodeInternal, err, "insert bill version")
	}

	// 快照数量按计量项稳定写入。
	qtyMeters := make([]string, 0, len(in.snapshotQty))
	for m := range in.snapshotQty {
		qtyMeters = append(qtyMeters, m)
	}
	sort.Strings(qtyMeters)
	for _, m := range qtyMeters {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO bill_snapshot_lines(tenant, period_start_unix, version, meter, quantity)
VALUES(?, ?, ?, ?, ?)`,
			in.tenant, in.startNS, in.version, m, in.snapshotQty[m].String()); err != nil {
			return wrapError(CodeInternal, err, "insert bill snapshot line")
		}
	}

	for _, l := range in.lines {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO bill_lines(tenant, period_start_unix, version, line_seq, kind, ref_id, event_id,
                       meter, is_adjustment, origin_period_start_unix, occurred_at_unix, quantity,
                       rate_version_id, rate_effective_at_unix, amount_scale, amount, commit_seq)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.tenant, in.startNS, in.version, l.LineSeq, l.Kind, l.RefID, l.EventID,
			l.Meter, b2i(l.Adjustment), l.OriginPeriodStart.UnixNano(), l.OccurredAt.UnixNano(),
			l.Quantity.String(), l.RateVersionID, l.RateEffectiveAt.UnixNano(), l.AmountScale,
			l.Amount.String(), l.CommitSeq); err != nil {
			return wrapError(CodeInternal, err, "insert bill line %q", l.RefID)
		}
		for _, sg := range l.Segments {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO bill_line_segments(tenant, period_start_unix, version, line_seq, tier_index,
                               quantity, unit_price, raw_amount)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				in.tenant, in.startNS, in.version, l.LineSeq, sg.TierIndex,
				sg.Quantity.String(), sg.UnitPrice.String(), sg.RawAmount.String()); err != nil {
				return wrapError(CodeInternal, err, "insert bill line segment")
			}
		}
	}
	return nil
}

func loadBillVersion(ctx context.Context, q rowQuerier, tenant string, startNS int64, version int) (*BillVersion, error) {
	var (
		status, idemKey, totalText       string
		periodEndNS, createdNS, voidedNS int64
		periodClosed, boundary           int64
	)
	err := q.QueryRowContext(ctx, `
SELECT status, period_end_unix, period_closed, boundary_seq, total_amount,
       idempotency_key, created_at_unix, voided_at_unix
FROM bill_versions WHERE tenant = ? AND period_start_unix = ? AND version = ?`,
		tenant, startNS, version).Scan(&status, &periodEndNS, &periodClosed, &boundary,
		&totalText, &idemKey, &createdNS, &voidedNS)
	if isNoRows(err) {
		return nil, newError(CodeNotFound, "bill v%d not found for tenant %q period %s",
			version, tenant, nsTime(startNS).Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load bill version")
	}
	total, err := decimal.NewFromString(totalText)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse bill total")
	}

	snapRows, err := q.QueryContext(ctx, `
SELECT meter, quantity FROM bill_snapshot_lines
WHERE tenant = ? AND period_start_unix = ? AND version = ? ORDER BY meter`,
		tenant, startNS, version)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load bill snapshot")
	}
	snapshotQty := map[string]decimal.Decimal{}
	for snapRows.Next() {
		var m, qt string
		if err := snapRows.Scan(&m, &qt); err != nil {
			_ = snapRows.Close()
			return nil, wrapError(CodeInternal, err, "scan bill snapshot")
		}
		d, err := decimal.NewFromString(qt)
		if err != nil {
			_ = snapRows.Close()
			return nil, wrapError(CodeInternal, err, "parse bill snapshot quantity")
		}
		snapshotQty[m] = d
	}
	_ = snapRows.Close()

	lines, err := loadBillLines(ctx, q, tenant, startNS, version)
	if err != nil {
		return nil, err
	}

	var periodEnd time.Time
	if periodEndNS != 0 {
		periodEnd = nsTime(periodEndNS)
	}
	var voidedAt time.Time
	if voidedNS != 0 {
		voidedAt = nsTime(voidedNS)
	}
	bv := &BillVersion{
		Tenant:             tenant,
		PeriodStart:        nsTime(startNS),
		PeriodEnd:          periodEnd,
		Version:            version,
		Status:             status,
		PeriodClosed:       periodClosed == 1,
		BoundarySeq:        boundary,
		SnapshotQuantities: snapshotQty,
		TotalAmount:        total,
		Lines:              lines,
		CreatedAt:          nsTime(createdNS),
		VoidedAt:           voidedAt,
	}
	return bv, nil
}

func loadBillLines(ctx context.Context, qr rowQuerier, tenant string, startNS int64, version int) ([]BillLine, error) {
	rows, err := qr.QueryContext(ctx, `
SELECT line_seq, kind, ref_id, event_id, meter, is_adjustment, origin_period_start_unix,
       occurred_at_unix, quantity, rate_version_id, rate_effective_at_unix, amount_scale,
       amount, commit_seq
FROM bill_lines
WHERE tenant = ? AND period_start_unix = ? AND version = ? ORDER BY line_seq`,
		tenant, startNS, version)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load bill lines")
	}
	defer func() { _ = rows.Close() }()

	var lines []BillLine
	for rows.Next() {
		var l BillLine
		var isAdj, originNS, occNS, rateEffNS int64
		var qty, amount string
		if err := rows.Scan(&l.LineSeq, &l.Kind, &l.RefID, &l.EventID, &l.Meter, &isAdj,
			&originNS, &occNS, &qty, &l.RateVersionID, &rateEffNS, &l.AmountScale,
			&amount, &l.CommitSeq); err != nil {
			return nil, wrapError(CodeInternal, err, "scan bill line")
		}
		qtyDec, err := decimal.NewFromString(qty)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse bill line quantity")
		}
		amtDec, err := decimal.NewFromString(amount)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse bill line amount")
		}
		l.Quantity = qtyDec
		l.Amount = amtDec
		l.Adjustment = isAdj == 1
		if originNS != 0 {
			l.OriginPeriodStart = nsTime(originNS)
		}
		l.OccurredAt = nsTime(occNS)
		l.RateEffectiveAt = nsTime(rateEffNS)
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInternal, err, "iterate bill lines")
	}
	_ = rows.Close()

	// 单连接池下嵌套查询必须在外层游标关闭后进行；分段过程与费率副本逐行补齐。
	loadedTiers := map[string][]PriceTier{}
	for i := range lines {
		l := &lines[i]
		segs, err := loadBillSegments(ctx, qr, tenant, startNS, version, l.LineSeq)
		if err != nil {
			return nil, err
		}
		l.Segments = segs
		// 费率分段副本按明细计价费率版本加载；该版本不可变，内容与生成时冻结的一致。
		tiers, ok := loadedTiers[l.RateVersionID]
		if !ok {
			tiers, err = loadTiers(ctx, qr, tenant, l.RateVersionID)
			if err != nil {
				return nil, err
			}
			loadedTiers[l.RateVersionID] = tiers
		}
		l.Tiers = tiers
	}
	return lines, nil
}

func loadBillSegments(ctx context.Context, q rowQuerier, tenant string, startNS int64, version, lineSeq int) ([]PricedSegment, error) {
	rows, err := q.QueryContext(ctx, `
SELECT tier_index, quantity, unit_price, raw_amount FROM bill_line_segments
WHERE tenant = ? AND period_start_unix = ? AND version = ? AND line_seq = ? ORDER BY tier_index`,
		tenant, startNS, version, lineSeq)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load bill segments")
	}
	defer func() { _ = rows.Close() }()
	var segs []PricedSegment
	for rows.Next() {
		var sg PricedSegment
		var qty, price, raw string
		if err := rows.Scan(&sg.TierIndex, &qty, &price, &raw); err != nil {
			return nil, wrapError(CodeInternal, err, "scan bill segment")
		}
		var err error
		if sg.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, wrapError(CodeInternal, err, "parse segment quantity")
		}
		if sg.UnitPrice, err = decimal.NewFromString(price); err != nil {
			return nil, wrapError(CodeInternal, err, "parse segment price")
		}
		if sg.RawAmount, err = decimal.NewFromString(raw); err != nil {
			return nil, wrapError(CodeInternal, err, "parse segment amount")
		}
		segs = append(segs, sg)
	}
	return segs, rows.Err()
}

func assembleBill(tenant string, startNS int64, version int, periodEnd time.Time, periodClosed bool,
	boundary int64, snapshotQty map[string]decimal.Decimal, total decimal.Decimal,
	lines []*BillLine, createdAt, voidedAt time.Time) *BillVersion {
	plain := make([]BillLine, 0, len(lines))
	for _, l := range lines {
		plain = append(plain, *l)
	}
	return &BillVersion{
		Tenant:             tenant,
		PeriodStart:        nsTime(startNS),
		PeriodEnd:          periodEnd,
		Version:            version,
		Status:             StatusCurrent,
		PeriodClosed:       periodClosed,
		BoundarySeq:        boundary,
		SnapshotQuantities: snapshotQty,
		TotalAmount:        total,
		Lines:              plain,
		CreatedAt:          createdAt,
		VoidedAt:           voidedAt,
	}
}

func cloneTiers(tiers []PriceTier) []PriceTier {
	out := make([]PriceTier, len(tiers))
	copy(out, tiers)
	return out
}

// isPartialCurrentIndexErr 判断唯一冲突是否来自“每周期仅一个当前版本”的部分索引。
func isPartialCurrentIndexErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, "bill_versions")
}
