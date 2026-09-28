package usagemetering

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// ratedRow 是快照范围内一条待计价的用量行。
type ratedRow struct {
	kind         string
	refID        string
	eventID      string
	meter        string
	isAdjustment bool
	originNS     int64 // 调整项本应归属的原周期起点；0 表示非调整项或无更早周期
	occurredNS   int64 // 业务发生时间（展示用）
	pricingAtNS  int64 // 取费率依据时间；修正为原事件发生时间
	quantity     decimal.Decimal
}

// GenerateDraft 为已关闭周期生成（或返回已有的）当前账单草稿。
//
// 草稿在生成时一次性冻结：
//   - 周期快照范围（period 与 CloseBoundarySeq，明细只覆盖快照内数据）；
//   - 每条用量解析到的费率版本及其完整内容（tiers/scale/currency 的 JSON 副本）；
//   - 逐条计价明细与精确总额（总额等于全部明细金额之和）。
//
// 重复生成返回同一草稿（同版本号、同明细）；费率后来发布或修订不会改写已冻结草稿。
// 周期尚未关闭返回 CodePeriodOpen；某计量项在用量发生时刻没有适用费率返回
// CodeNoApplicableRate（整笔生成回滚，不产生半成品草稿）。
func (s *Service) GenerateDraft(ctx context.Context, tenant string, periodStart time.Time) (*Draft, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	periodStart = periodStart.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	p, err := getPeriod(ctx, tx, tenant, periodStart)
	if err != nil {
		return nil, err
	}
	if !p.Closed {
		return nil, newError(CodePeriodOpen,
			"period starting at %s for tenant %q is still open; close it before generating a draft",
			periodStart.Format(time.RFC3339Nano), tenant)
	}

	// 幂等：当前草稿已存在则原样返回（冻结内容不会因重发生成而变化）。
	if v, err := currentDraftVersion(ctx, tx, tenant, p.Start.UnixNano()); err == nil {
		return loadDraft(ctx, tx, tenant, p.Start.UnixNano(), v)
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	var next int32
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM drafts WHERE tenant = ? AND period_start_unix = ?`,
		tenant, p.Start.UnixNano()).Scan(&next); err != nil {
		return nil, wrapError(CodeInternal, err, "allocate draft version")
	}

	rows, err := collectSnapshotRows(ctx, tx, tenant, p)
	if err != nil {
		return nil, err
	}

	type frozenLine struct {
		line        DraftLine
		pricingJSON string
		rateEffNS   int64
	}
	frozenLines := make([]frozenLine, 0, len(rows))
	totalAmount := decimal.Zero
	normalAmount := decimal.Zero
	adjustmentAmount := decimal.Zero
	currency := ""

	for _, r := range rows {
		_, fr, err := effectiveRate(ctx, tx, tenant, r.meter, r.pricingAtNS)
		if err != nil {
			return nil, err
		}
		if currency == "" {
			currency = fr.Currency
		} else if currency != fr.Currency {
			return nil, newError(CodeConflict,
				"cannot rate period starting at %s into one draft: meter %q uses currency %q but the draft already contains %q",
				periodStart.Format(time.RFC3339Nano), r.meter, fr.Currency, currency)
		}

		amount := priceQuantity(r.quantity, fr)
		pricedQty := roundAwayFromZero(r.quantity, fr.QuantityScale)
		unitAmount := decimal.Zero
		if !pricedQty.IsZero() {
			unitAmount = amount.DivRound(pricedQty, fr.AmountScale+4)
		}
		pricingJSON, err := json.Marshal(fr)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "freeze rate copy")
		}

		line := DraftLine{
			Kind:              r.kind,
			RefID:             r.refID,
			EventID:           r.eventID,
			Meter:             r.meter,
			Adjustment:        r.isAdjustment,
			OriginPeriodStart: nsTime(r.originNS),
			OccurredAt:        nsTime(r.occurredNS),
			Quantity:          pricedQty,
			RateVersion:       fr.Version,
			RateEffectiveFrom: nsTime(fr.EffectiveNS),
			UnitAmount:        unitAmount,
			Amount:            amount,
		}
		frozenLines = append(frozenLines, frozenLine{
			line:        line,
			pricingJSON: string(pricingJSON),
			rateEffNS:   fr.EffectiveNS,
		})

		totalAmount = totalAmount.Add(amount)
		if r.isAdjustment {
			adjustmentAmount = adjustmentAmount.Add(amount)
		} else {
			normalAmount = normalAmount.Add(amount)
		}
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO drafts(tenant, period_start_unix, version, status, boundary_seq,
                   total_amount, currency, created_at_unix)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		tenant, p.Start.UnixNano(), next, DraftStatusCurrent, p.CloseBoundarySeq,
		totalAmount.String(), currency, now.UnixNano()); err != nil {
		if isUniqueErr(err) {
			// 部分唯一索引兜底：并发下已有其它版本成为当前草稿，返回它。
			if v, gerr := currentDraftVersion(ctx, tx, tenant, p.Start.UnixNano()); gerr == nil {
				return loadDraft(ctx, tx, tenant, p.Start.UnixNano(), v)
			}
		}
		return nil, wrapError(CodeInternal, err, "insert draft version %d", next)
	}

	for seq, fl := range frozenLines {
		l := fl.line
		originNS := int64(0)
		if !l.OriginPeriodStart.IsZero() {
			originNS = l.OriginPeriodStart.UnixNano()
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO draft_lines(tenant, period_start_unix, draft_version, line_seq, kind, ref_id,
                        event_id, meter, is_adjustment, origin_period_start_unix, occurred_at_unix, quantity,
                        rate_version, rate_effective_at_unix, pricing_json, unit_amount, amount)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tenant, p.Start.UnixNano(), next, seq, l.Kind, l.RefID,
			l.EventID, l.Meter, b2i(l.Adjustment), originNS, l.OccurredAt.UnixNano(),
			l.Quantity.String(), l.RateVersion, fl.rateEffNS, fl.pricingJSON,
			l.UnitAmount.String(), l.Amount.String()); err != nil {
			return nil, wrapError(CodeInternal, err, "insert draft line %d (%s %s)",
				seq, l.Kind, l.RefID)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit draft")
	}
	return &Draft{
		Tenant:           tenant,
		PeriodStart:      p.Start,
		PeriodEnd:        p.End,
		Version:          next,
		Status:           DraftStatusCurrent,
		BoundarySeq:      p.CloseBoundarySeq,
		Currency:         currency,
		TotalAmount:      totalAmount,
		NormalAmount:     normalAmount,
		AdjustmentAmount: adjustmentAmount,
		Lines: func() []DraftLine {
			out := make([]DraftLine, len(frozenLines))
			for i := range frozenLines {
				out[i] = frozenLines[i].line
			}
			return out
		}(),
		CreatedAt: now,
	}, nil
}

// VoidDraft 作废周期的当前草稿版本。旧版本及其明细继续保留，可通过
// GetDraftVersion / ListDraftVersions 查询；作废后再次调用 GenerateDraft
// 会生成新的版本号。没有当前草稿时返回 CodeNotFound。
//
// expectedVersion > 0 时要求当前版本号与其一致，否则返回 CodeVersionConflict，
// 供调用方做乐观并发控制（“并发重算只允许一个版本成为当前草稿”）。
func (s *Service) VoidDraft(ctx context.Context, tenant string, periodStart time.Time, expectedVersion int32) (*Draft, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	periodStart = periodStart.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	p, err := getPeriod(ctx, tx, tenant, periodStart)
	if err != nil {
		return nil, err
	}
	version, err := currentDraftVersion(ctx, tx, tenant, p.Start.UnixNano())
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && version != expectedVersion {
		return nil, newError(CodeVersionConflict,
			"current draft version for period starting at %s is %d, cannot void expected version %d",
			periodStart.Format(time.RFC3339Nano), version, expectedVersion)
	}

	draft, err := loadDraft(ctx, tx, tenant, p.Start.UnixNano(), version)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE drafts SET status = ?, voided_at_unix = ?
WHERE tenant = ? AND period_start_unix = ? AND version = ? AND status = ?`,
		DraftStatusVoided, now.UnixNano(), tenant, p.Start.UnixNano(), version, DraftStatusCurrent); err != nil {
		return nil, wrapError(CodeInternal, err, "void draft version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit void")
	}
	draft.Status = DraftStatusVoided
	draft.VoidedAt = now
	return draft, nil
}

// GetDraft 返回周期的当前草稿（含全部计价明细）。
// 周期不存在返回 CodeNotFound；周期未关闭返回 CodePeriodOpen；
// 尚无草稿（或当前版本已作废且未重新生成）返回 CodeNotFound。
func (s *Service) GetDraft(ctx context.Context, tenant string, periodStart time.Time) (*Draft, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	periodStart = periodStart.UTC()
	p, err := getPeriod(ctx, s.db, tenant, periodStart)
	if err != nil {
		return nil, err
	}
	if !p.Closed {
		return nil, newError(CodePeriodOpen,
			"period starting at %s for tenant %q is still open and has no draft",
			periodStart.Format(time.RFC3339Nano), tenant)
	}
	version, err := currentDraftVersion(ctx, s.db, tenant, p.Start.UnixNano())
	if err != nil {
		return nil, err
	}
	return loadDraft(ctx, s.db, tenant, p.Start.UnixNano(), version)
}

// GetDraftVersion 读取指定版本的草稿（含明细），包括已作废的历史版本。
func (s *Service) GetDraftVersion(ctx context.Context, tenant string, periodStart time.Time, version int32) (*Draft, error) {
	if tenant == "" || periodStart.IsZero() || version <= 0 {
		return nil, newError(CodeInvalidArgument, "tenant, period start and positive version are required")
	}
	return loadDraft(ctx, s.db, tenant, periodStart.UTC().UnixNano(), version)
}

// ListDraftVersions 列出周期的全部草稿版本头（不含明细），按版本号升序，
// 已作废版本的 Status 为 DraftStatusVoided 且 VoidedAt 非零。
func (s *Service) ListDraftVersions(ctx context.Context, tenant string, periodStart time.Time) ([]Draft, error) {
	if tenant == "" || periodStart.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant and period start are required")
	}
	startNS := periodStart.UTC().UnixNano()
	if _, err := getPeriod(ctx, s.db, tenant, periodStart.UTC()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT version, status, boundary_seq, total_amount, currency, created_at_unix, voided_at_unix
FROM drafts WHERE tenant = ? AND period_start_unix = ? ORDER BY version`,
		tenant, startNS)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list draft versions")
	}
	defer func() { _ = rows.Close() }()

	var out []Draft
	for rows.Next() {
		var d Draft
		var version int32
		var boundary int64
		var total, currency, status string
		var createdNS, voidedNS int64
		if err := rows.Scan(&version, &status, &boundary, &total, &currency, &createdNS, &voidedNS); err != nil {
			return nil, wrapError(CodeInternal, err, "scan draft version")
		}
		amount, err := decimal.NewFromString(total)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse draft total")
		}
		d = Draft{
			Tenant:      tenant,
			PeriodStart: periodStart.UTC(),
			Version:     version,
			Status:      status,
			BoundarySeq: boundary,
			Currency:    currency,
			TotalAmount: amount,
			CreatedAt:   nsTime(createdNS),
			VoidedAt:    nsTime(voidedNS),
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDraftLines 查询当前草稿的计价明细，稳定排序（正常项在前、调整项在后）。
// onlyAdjustment 为 true 时只返回迟到调整项明细（草稿中的单列部分）。
func (s *Service) ListDraftLines(ctx context.Context, tenant string, periodStart time.Time, onlyAdjustment bool) ([]DraftLine, error) {
	draft, err := s.GetDraft(ctx, tenant, periodStart)
	if err != nil {
		return nil, err
	}
	if !onlyAdjustment {
		return draft.Lines, nil
	}
	out := make([]DraftLine, 0, len(draft.Lines))
	for _, l := range draft.Lines {
		if l.Adjustment {
			out = append(out, l)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// collectSnapshotRows 取出周期快照范围内（承载周期为本周期、commit_seq <= 关账边界）
// 的全部事件与修正，排序稳定。修正行的取费率时间（pricingAtNS）为其原事件的发生时间：
// 迟到调整修正也按原事件发生时的费率版本计价。
func collectSnapshotRows(ctx context.Context, tx *sql.Tx, tenant string, p *Period) ([]ratedRow, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT kind, ref_id, event_id, meter, is_adjustment, origin_period_start_unix, occurred_at_unix, pricing_at_unix, quantity FROM (
	SELECT 'event'      AS kind, event_id      AS ref_id, event_id AS event_id, meter,
	       is_adjustment, origin_period_start_unix, occurred_at_unix, occurred_at_unix AS pricing_at_unix, quantity
	FROM events
	WHERE tenant = ? AND period_start_unix = ? AND commit_seq <= ?
	UNION ALL
	SELECT 'correction', c.correction_id,      c.event_id,        c.meter,
	       c.is_adjustment, c.origin_period_start_unix, c.occurred_at_unix, e.occurred_at_unix, c.delta
	FROM corrections c JOIN events e ON e.tenant = c.tenant AND e.event_id = c.event_id
	WHERE c.tenant = ? AND c.period_start_unix = ? AND c.commit_seq <= ?
)
ORDER BY meter, is_adjustment, kind, occurred_at_unix, ref_id`,
		tenant, p.Start.UnixNano(), p.CloseBoundarySeq,
		tenant, p.Start.UnixNano(), p.CloseBoundarySeq)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "collect snapshot rows")
	}
	defer func() { _ = rows.Close() }()

	var out []ratedRow
	for rows.Next() {
		var r ratedRow
		var kind, refID, eventID, meter, qtyText string
		var isAdj, originNS, occNS, pricingNS int64
		if err := rows.Scan(&kind, &refID, &eventID, &meter, &isAdj, &originNS, &occNS, &pricingNS, &qtyText); err != nil {
			return nil, wrapError(CodeInternal, err, "scan rated row")
		}
		q, err := decimal.NewFromString(qtyText)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse rated row quantity")
		}
		r = ratedRow{
			kind:         kind,
			refID:        refID,
			eventID:      eventID,
			meter:        meter,
			isAdjustment: isAdj == 1,
			originNS:     originNS,
			occurredNS:   occNS,
			pricingAtNS:  pricingNS,
			quantity:     q,
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func currentDraftVersion(ctx context.Context, q rowQuerier, tenant string, startNS int64) (int32, error) {
	var version int32
	err := q.QueryRowContext(ctx,
		`SELECT version FROM drafts WHERE tenant = ? AND period_start_unix = ? AND status = ?`,
		tenant, startNS, DraftStatusCurrent).Scan(&version)
	if isNoRows(err) {
		return 0, newError(CodeNotFound,
			"no current draft for tenant %q period starting at %s (never generated or all versions voided)",
			tenant, nsTime(startNS).Format(time.RFC3339Nano))
	}
	if err != nil {
		return 0, wrapError(CodeInternal, err, "load current draft version")
	}
	return version, nil
}

func loadDraft(ctx context.Context, q rowQuerier, tenant string, startNS int64, version int32) (*Draft, error) {
	var d Draft
	var status, total, currency string
	var boundary, createdNS, voidedNS int64
	err := q.QueryRowContext(ctx, `
SELECT version, status, boundary_seq, total_amount, currency, created_at_unix, voided_at_unix
FROM drafts WHERE tenant = ? AND period_start_unix = ? AND version = ?`,
		tenant, startNS, version).
		Scan(&d.Version, &status, &boundary, &total, &currency, &createdNS, &voidedNS)
	if isNoRows(err) {
		return nil, newError(CodeNotFound, "draft version %d not found for tenant %q period starting at %s",
			version, tenant, nsTime(startNS).Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load draft header")
	}
	totalAmount, err := decimal.NewFromString(total)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "parse draft total")
	}

	p, err := getPeriod(ctx, q, tenant, nsTime(startNS))
	if err != nil {
		return nil, err
	}

	lines, err := loadDraftLines(ctx, q, tenant, startNS, version)
	if err != nil {
		return nil, err
	}
	normalAmount := decimal.Zero
	adjustmentAmount := decimal.Zero
	for _, l := range lines {
		if l.Adjustment {
			adjustmentAmount = adjustmentAmount.Add(l.Amount)
		} else {
			normalAmount = normalAmount.Add(l.Amount)
		}
	}

	d = Draft{
		Tenant:           tenant,
		PeriodStart:      p.Start,
		PeriodEnd:        p.End,
		Version:          d.Version,
		Status:           status,
		BoundarySeq:      boundary,
		Currency:         currency,
		TotalAmount:      totalAmount,
		NormalAmount:     normalAmount,
		AdjustmentAmount: adjustmentAmount,
		Lines:            lines,
		CreatedAt:        nsTime(createdNS),
		VoidedAt:         nsTime(voidedNS),
	}
	return &d, nil
}

func loadDraftLines(ctx context.Context, q rowQuerier, tenant string, startNS int64, version int32) ([]DraftLine, error) {
	rows, err := q.QueryContext(ctx, `
SELECT kind, ref_id, event_id, meter, is_adjustment, origin_period_start_unix, occurred_at_unix, quantity,
       rate_version, rate_effective_at_unix, unit_amount, amount
FROM draft_lines
WHERE tenant = ? AND period_start_unix = ? AND draft_version = ?
ORDER BY line_seq`,
		tenant, startNS, version)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load draft lines")
	}
	defer func() { _ = rows.Close() }()

	var out []DraftLine
	for rows.Next() {
		var l DraftLine
		var kind, refID, eventID, meter, qty, unit, amount string
		var isAdj, originNS, occNS, rateEffNS int64
		var rateVersion int32
		if err := rows.Scan(&kind, &refID, &eventID, &meter, &isAdj, &originNS, &occNS, &qty,
			&rateVersion, &rateEffNS, &unit, &amount); err != nil {
			return nil, wrapError(CodeInternal, err, "scan draft line")
		}
		q, err := decimal.NewFromString(qty)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse draft line quantity")
		}
		u, err := decimal.NewFromString(unit)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse draft line unit amount")
		}
		a, err := decimal.NewFromString(amount)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse draft line amount")
		}
		l = DraftLine{
			Kind:              kind,
			RefID:             refID,
			EventID:           eventID,
			Meter:             meter,
			Adjustment:        isAdj == 1,
			OriginPeriodStart: nsTime(originNS),
			OccurredAt:        nsTime(occNS),
			Quantity:          q,
			RateVersion:       rateVersion,
			RateEffectiveFrom: nsTime(rateEffNS),
			UnitAmount:        u,
			Amount:            a,
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInternal, err, "iterate draft lines")
	}
	return out, nil
}
