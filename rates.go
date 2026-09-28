package usagemetering

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// TierInput 是发布费率时的一个分段价格入参。
type TierInput struct {
	// UpperQuantity 分段数量上限（含）；nil 表示开口分段，必须位于末段。
	UpperQuantity *decimal.Decimal
	// UnitPrice 分段单价，不得为负。
	UnitPrice decimal.Decimal
}

// RateInput 是发布一个费率版本的入参。
type RateInput struct {
	// VersionID 外部费率版本号，租户内唯一、幂等。
	VersionID string
	Tenant    string
	Meter     string
	// EffectiveAt 生效时间（含）；该计量项在某时刻的适用费率取
	// “生效时间最大且不晚于该时刻”的版本。
	EffectiveAt time.Time
	// Tiers 分段价格，按上限严格递增；末段必须开口（UpperQuantity 为 nil）。
	Tiers []TierInput
	// AmountScale 计价金额保留的小数位数（四舍五入，半数远离零），取值 [0, 18]。
	AmountScale int32
}

// PublishRate 发布一个不可变费率版本。
//
// 规则：
//   - 版本号租户内幂等：同号同内容返回首次结果，同号不同内容返回 CodeConflict；
//   - 同一 (租户, 计量项) 在同一生效时间只允许一个版本，
//     否则两个版本会在同一时刻同时有效（时间区间重叠），返回 CodeRateOverlap；
//   - 版本一经发布即不可修改：没有任何更新或删除入口。
func (s *Service) PublishRate(ctx context.Context, in RateInput) (*RateVersion, error) {
	if err := validateRateInput(in); err != nil {
		return nil, err
	}
	in.EffectiveAt = in.EffectiveAt.UTC()
	tiers := make([]PriceTier, len(in.Tiers))
	for i := range in.Tiers {
		tiers[i] = PriceTier{UpperQuantity: in.Tiers[i].UpperQuantity, UnitPrice: in.Tiers[i].UnitPrice}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := getRateVersionTx(ctx, tx, in.Tenant, in.VersionID); err == nil {
		if !sameRateContent(existing, in) {
			return nil, conflictRate(existing, in)
		}
		return existing, nil // 幂等重放
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO rate_versions(version_id, tenant, meter, effective_at_unix, amount_scale, published_at_unix)
VALUES(?, ?, ?, ?, ?, ?)`,
		in.VersionID, in.Tenant, in.Meter, in.EffectiveAt.UnixNano(), in.AmountScale, now.UnixNano()); err != nil {
		if isUniqueErr(err) {
			// (tenant, meter, effective_at) 唯一：同一生效时间已存在版本 = 费率重叠。
			return nil, newError(CodeRateOverlap,
				"an effective rate for tenant %q meter %q at %s already exists; "+
					"two rates must not be valid at the same instant",
				in.Tenant, in.Meter, in.EffectiveAt.Format(time.RFC3339Nano))
		}
		return nil, wrapError(CodeInternal, err, "insert rate version %q", in.VersionID)
	}

	for i, t := range tiers {
		var upper any
		if t.UpperQuantity != nil {
			upper = t.UpperQuantity.String()
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO rate_tiers(tenant, version_id, tier_index, upper_quantity, unit_price)
VALUES(?, ?, ?, ?, ?)`,
			in.Tenant, in.VersionID, i, upper, t.UnitPrice.String()); err != nil {
			return nil, wrapError(CodeInternal, err, "insert rate tier %d", i)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit rate")
	}
	return &RateVersion{
		VersionID:   in.VersionID,
		Tenant:      in.Tenant,
		Meter:       in.Meter,
		EffectiveAt: in.EffectiveAt,
		Tiers:       tiers,
		AmountScale: in.AmountScale,
		PublishedAt: now,
	}, nil
}

// GetRateVersion 按版本号查询费率；不存在返回 CodeNotFound。
func (s *Service) GetRateVersion(ctx context.Context, tenant, versionID string) (*RateVersion, error) {
	if tenant == "" || versionID == "" {
		return nil, newError(CodeInvalidArgument, "tenant and version id are required")
	}
	return getRateVersionTx(ctx, s.db, tenant, versionID)
}

// ListRateVersions 列出某计量项（meter 为空则该租户全部计量项）已发布的费率版本，
// 按 (计量项, 生效时间) 稳定排序。
func (s *Service) ListRateVersions(ctx context.Context, tenant, meter string) ([]RateVersion, error) {
	if tenant == "" {
		return nil, newError(CodeInvalidArgument, "tenant is required")
	}
	q := `
SELECT version_id, tenant, meter, effective_at_unix, amount_scale, published_at_unix
FROM rate_versions WHERE tenant = ?`
	args := []any{tenant}
	if meter != "" {
		q += ` AND meter = ?`
		args = append(args, meter)
	}
	q += ` ORDER BY meter, effective_at_unix, version_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list rate versions")
	}
	defer func() { _ = rows.Close() }()

	var out []RateVersion
	for rows.Next() {
		var rv RateVersion
		var effNS, publishedNS int64
		if err := rows.Scan(&rv.VersionID, &rv.Tenant, &rv.Meter, &effNS, &rv.AmountScale, &publishedNS); err != nil {
			return nil, wrapError(CodeInternal, err, "scan rate version")
		}
		rv.EffectiveAt = nsTime(effNS)
		rv.PublishedAt = nsTime(publishedNS)
		out = append(out, rv)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInternal, err, "iterate rate versions")
	}
	_ = rows.Close()

	// 单连接池下不能在游标未关时发起嵌套查询，分段在游标关闭后逐版本加载。
	for i := range out {
		tiers, err := loadTiers(ctx, s.db, out[i].Tenant, out[i].VersionID)
		if err != nil {
			return nil, err
		}
		out[i].Tiers = tiers
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 计价引擎
// ---------------------------------------------------------------------------

// loadedRate 是计价时使用的费率版本投影。
type loadedRate struct {
	versionID   string
	effectiveNS int64
	amountScale int32
	tiers       []PriceTier
}

// priceQuantity 按分段累进价格计算数量金额（全部精确十进制）。
//
// 数量按其绝对值切分到各分段（落入 (上段上限, 本段上限] 的部分按本段单价），
// 负数量先按绝对值计价再恢复符号；分段金额精确求和后，按费率精度一次性四舍五入。
func priceQuantity(qty decimal.Decimal, rate loadedRate) ([]PricedSegment, decimal.Decimal) {
	sign := decimal.NewFromInt(1)
	abs := qty
	if qty.IsNegative() {
		sign = decimal.NewFromInt(-1)
		abs = qty.Neg()
	}
	var segs []PricedSegment
	var rawSum decimal.Decimal
	prev := decimal.Zero
	for i, t := range rate.tiers {
		if abs.Cmp(prev) <= 0 {
			break
		}
		segQty := abs.Sub(prev)
		if t.UpperQuantity != nil && abs.Cmp(*t.UpperQuantity) > 0 {
			segQty = t.UpperQuantity.Sub(prev)
		}
		segQty = segQty.Mul(sign)
		raw := segQty.Mul(t.UnitPrice)
		rawSum = rawSum.Add(raw)
		segs = append(segs, PricedSegment{
			TierIndex: i,
			Quantity:  segQty,
			UnitPrice: t.UnitPrice,
			RawAmount: raw,
		})
		if t.UpperQuantity != nil {
			prev = *t.UpperQuantity
		} else {
			break
		}
	}
	return segs, rawSum.Round(rate.amountScale)
}

// rateCatalog 是一次账单计算内的费率目录：按计量项缓存其全部已发布版本。
type rateCatalog struct {
	byMeter map[string][]loadedRate // 各计量项版本按生效时间升序
}

// loadRateCatalog 为给定计量项集合加载费率目录。必须在事务内调用。
func loadRateCatalog(ctx context.Context, q rowQuerier, tenant string, meters map[string]struct{}) (*rateCatalog, error) {
	cat := &rateCatalog{byMeter: map[string][]loadedRate{}}
	for meter := range meters {
		rows, err := q.QueryContext(ctx, `
SELECT version_id, effective_at_unix, amount_scale
FROM rate_versions WHERE tenant = ? AND meter = ?
ORDER BY effective_at_unix, version_id`, tenant, meter)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "load rates for meter %q", meter)
		}
		var versions []loadedRate
		for rows.Next() {
			var r loadedRate
			if err := rows.Scan(&r.versionID, &r.effectiveNS, &r.amountScale); err != nil {
				_ = rows.Close()
				return nil, wrapError(CodeInternal, err, "scan rate version")
			}
			versions = append(versions, r)
		}
		_ = rows.Close()
		for i := range versions {
			tiers, err := loadTiers(ctx, q, tenant, versions[i].versionID)
			if err != nil {
				return nil, err
			}
			versions[i].tiers = tiers
		}
		cat.byMeter[meter] = versions
	}
	return cat, nil
}

// lookup 返回计量项在 atNS 时刻适用的费率：生效时间最大且 <= atNS 的版本。
func (c *rateCatalog) lookup(meter string, atNS int64) (loadedRate, bool) {
	versions := c.byMeter[meter]
	// 升序排列，取最后一个生效时间 <= atNS 的版本。
	idx := sort.Search(len(versions), func(i int) bool {
		return versions[i].effectiveNS > atNS
	}) - 1
	if idx < 0 {
		return loadedRate{}, false
	}
	return versions[idx], true
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

func validateRateInput(in RateInput) error {
	if in.Tenant == "" {
		return newError(CodeInvalidArgument, "tenant is required")
	}
	if in.VersionID == "" {
		return newError(CodeInvalidArgument, "rate version id is required")
	}
	if in.Meter == "" {
		return newError(CodeInvalidArgument, "meter is required")
	}
	if in.EffectiveAt.IsZero() {
		return newError(CodeInvalidArgument, "effective_at is required")
	}
	if in.AmountScale < 0 || in.AmountScale > 18 {
		return newError(CodeInvalidArgument, "amount_scale must be within [0, 18], got %d", in.AmountScale)
	}
	if len(in.Tiers) == 0 {
		return newError(CodeInvalidArgument, "at least one price tier is required")
	}
	var prev *decimal.Decimal
	for i, t := range in.Tiers {
		if t.UnitPrice.IsNegative() {
			return newError(CodeInvalidArgument, "tier %d unit price must not be negative, got %s", i, t.UnitPrice)
		}
		isLast := i == len(in.Tiers)-1
		if t.UpperQuantity == nil {
			if !isLast {
				return newError(CodeInvalidArgument, "only the last tier may be open-ended, tier %d is not last", i)
			}
		} else {
			if isLast {
				return newError(CodeInvalidArgument, "the last tier must be open-ended (UpperQuantity = nil)")
			}
			if t.UpperQuantity.IsNegative() {
				return newError(CodeInvalidArgument, "tier %d upper quantity must not be negative", i)
			}
			if prev != nil && t.UpperQuantity.Cmp(*prev) <= 0 {
				return newError(CodeInvalidArgument,
					"tier upper quantities must be strictly increasing: %s is not greater than %s",
					t.UpperQuantity, prev)
			}
			v := *t.UpperQuantity
			prev = &v
		}
	}
	return nil
}

func sameRateContent(stored *RateVersion, in RateInput) bool {
	if stored.Meter != in.Meter ||
		!stored.EffectiveAt.Equal(in.EffectiveAt.UTC()) ||
		stored.AmountScale != in.AmountScale ||
		len(stored.Tiers) != len(in.Tiers) {
		return false
	}
	for i := range in.Tiers {
		a, b := stored.Tiers[i], in.Tiers[i]
		if (a.UpperQuantity == nil) != (b.UpperQuantity == nil) {
			return false
		}
		if a.UpperQuantity != nil && !a.UpperQuantity.Equal(*b.UpperQuantity) {
			return false
		}
		if !a.UnitPrice.Equal(b.UnitPrice) {
			return false
		}
	}
	return true
}

func conflictRate(stored *RateVersion, in RateInput) *Error {
	return newError(CodeConflict,
		"rate version %q already published for tenant %q with different content: "+
			"stored meter=%q effective_at=%s tiers=%d scale=%d; submitted meter=%q effective_at=%s tiers=%d scale=%d",
		in.VersionID, in.Tenant,
		stored.Meter, stored.EffectiveAt.Format(time.RFC3339Nano), len(stored.Tiers), stored.AmountScale,
		in.Meter, in.EffectiveAt.UTC().Format(time.RFC3339Nano), len(in.Tiers), in.AmountScale)
}

// rateCols 为费率版本主表的列清单，供单点查询复用。
const rateCols = `version_id, tenant, meter, effective_at_unix, amount_scale, published_at_unix`

func getRateVersionTx(ctx context.Context, q rowQuerier, tenant, versionID string) (*RateVersion, error) {
	var rv RateVersion
	var effNS, publishedNS int64
	err := q.QueryRowContext(ctx,
		`SELECT `+rateCols+` FROM rate_versions WHERE tenant = ? AND version_id = ?`,
		tenant, versionID).Scan(&rv.VersionID, &rv.Tenant, &rv.Meter, &effNS, &rv.AmountScale, &publishedNS)
	if isNoRows(err) {
		return nil, newError(CodeNotFound, "rate version %q not found for tenant %q", versionID, tenant)
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load rate version")
	}
	rv.EffectiveAt = nsTime(effNS)
	rv.PublishedAt = nsTime(publishedNS)
	tiers, err := loadTiers(ctx, q, tenant, versionID)
	if err != nil {
		return nil, err
	}
	rv.Tiers = tiers
	return &rv, nil
}

func loadTiers(ctx context.Context, q rowQuerier, tenant, versionID string) ([]PriceTier, error) {
	rows, err := q.QueryContext(ctx, `
SELECT upper_quantity, unit_price FROM rate_tiers
WHERE tenant = ? AND version_id = ? ORDER BY tier_index`, tenant, versionID)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "load rate tiers")
	}
	defer func() { _ = rows.Close() }()
	var tiers []PriceTier
	for rows.Next() {
		var upper, price sql.NullString
		if err := rows.Scan(&upper, &price); err != nil {
			return nil, wrapError(CodeInternal, err, "scan rate tier")
		}
		p, err := decimal.NewFromString(price.String)
		if err != nil {
			return nil, wrapError(CodeInternal, err, "parse tier unit price")
		}
		t := PriceTier{UnitPrice: p}
		if upper.Valid {
			u, err := decimal.NewFromString(upper.String)
			if err != nil {
				return nil, wrapError(CodeInternal, err, "parse tier upper quantity")
			}
			t.UpperQuantity = &u
		}
		tiers = append(tiers, t)
	}
	return tiers, rows.Err()
}
