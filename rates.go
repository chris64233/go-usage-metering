package usagemetering

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// PriceTier 是一个分段价格：用量落在 [前一段上限, UpTo) 区间内的部分按 UnitPrice 计价；
// UpTo 为 0 表示无上限的最后一段。
type PriceTier struct {
	// UpTo 该段上限（不含）；0 表示无上限，且只能出现在最后一段。
	UpTo decimal.Decimal `json:"up_to"`
	// UnitPrice 该段内每单位用量的单价，必须非负。
	UnitPrice decimal.Decimal `json:"unit_price"`
}

// RateVersion 是一份不可变费率版本。发布后任何内容都不会被修改。
type RateVersion struct {
	Tenant string
	Meter  string
	// Version 在 (租户, 计量项) 内单调分配，从 1 开始。
	Version int32
	// EffectiveFrom 生效起点（含）。按用量发生时间选择版本：
	// effective_from <= 发生时间 的最新版本即为适用费率。
	EffectiveFrom time.Time
	// Tiers 按上限升序排列的分段价格，最后一段无上限。
	Tiers []PriceTier
	// Currency ISO 币种代码（如 "USD"）。同一草稿内全部明细必须币种一致。
	Currency string
	// QuantityScale 数量计价精度（保留的小数位数），计价前先按此精度取整。
	QuantityScale int32
	// AmountScale 金额计价精度（保留的小数位数），每行明细金额按此精度取整。
	AmountScale int32
	// PublishedAt 发布时间（UTC）。
	PublishedAt time.Time
}

// PublishRateInput 是发布费率版本的入参。
type PublishRateInput struct {
	Tenant        string
	Meter         string
	EffectiveFrom time.Time
	Tiers         []PriceTier
	Currency      string
	// QuantityScale / AmountScale 为计价精度（小数位），必须非负。
	QuantityScale int32
	AmountScale   int32
}

// frozenRate 是费率在草稿明细中的不可变副本；后续费率发布或修订都不影响已冻结草稿。
type frozenRate struct {
	Version       int32       `json:"version"`
	EffectiveNS   int64       `json:"effective_from_unix"`
	Tiers         []PriceTier `json:"tiers"`
	Currency      string      `json:"currency"`
	QuantityScale int32       `json:"quantity_scale"`
	AmountScale   int32       `json:"amount_scale"`
}

// 草稿/版本状态常量。
const (
	DraftStatusCurrent = "current"
	DraftStatusVoided  = "voided"
)

// PublishRate 发布一份不可变费率版本。
//
// 规则：
//   - 费率按 (租户, 计量项, 生效时间) 发布；同一 (租户, 计量项) 的版本号单调递增；
//   - 版本一经发布即不可修改，没有修订/删除接口；
//   - 同一 (租户, 计量项, 生效时间) 只允许一份版本：同内容重复发布幂等返回原版本，
//     内容不同返回 CodeRateOverlap（保证任一时刻至多一份有效费率）；
//   - 允许补发生效时间较早的版本，但不得与既有生效时间重合。
func (s *Service) PublishRate(ctx context.Context, in PublishRateInput) (*RateVersion, error) {
	if err := validateRateInput(in); err != nil {
		return nil, err
	}
	in.EffectiveFrom = in.EffectiveFrom.UTC()
	tiers := normalizeTiers(in.Tiers)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := loadRateAtExact(ctx, tx, in.Tenant, in.Meter, in.EffectiveFrom.UnixNano()); err == nil {
		if !sameRateContent(existing, in, tiers) {
			return nil, newError(CodeRateOverlap,
				"rate for tenant %q meter %q effective at %s already exists (version %d) with different content; "+
					"published rates are immutable and only one rate may be effective at a time",
				in.Tenant, in.Meter, in.EffectiveFrom.Format(time.RFC3339Nano), existing.Version)
		}
		return existing, nil // 幂等重放
	} else if !IsCode(err, CodeNotFound) {
		return nil, err
	}

	var next int32
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM rate_versions WHERE tenant = ? AND meter = ?`,
		in.Tenant, in.Meter).Scan(&next); err != nil {
		return nil, wrapError(CodeInternal, err, "allocate rate version")
	}
	tiersJSON, err := json.Marshal(tiers)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "encode tiers")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO rate_versions(tenant, meter, version, effective_from_unix, tiers_json,
                          currency, quantity_scale, amount_scale, published_at_unix)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Tenant, in.Meter, next, in.EffectiveFrom.UnixNano(), string(tiersJSON),
		in.Currency, in.QuantityScale, in.AmountScale, now.UnixNano()); err != nil {
		if isUniqueErr(err) {
			// 互斥 + 预查之下理论不会发生，防御性地回到重放/重叠判断。
			if existing, gerr := loadRateAtExact(ctx, tx, in.Tenant, in.Meter, in.EffectiveFrom.UnixNano()); gerr == nil {
				if sameRateContent(existing, in, tiers) {
					return existing, nil
				}
				return nil, newError(CodeRateOverlap,
					"rate for tenant %q meter %q effective at %s already exists (version %d)",
					in.Tenant, in.Meter, in.EffectiveFrom.Format(time.RFC3339Nano), existing.Version)
			}
		}
		return nil, wrapError(CodeInternal, err, "insert rate version")
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapError(CodeInternal, err, "commit rate version")
	}
	return &RateVersion{
		Tenant:        in.Tenant,
		Meter:         in.Meter,
		Version:       next,
		EffectiveFrom: in.EffectiveFrom,
		Tiers:         tiers,
		Currency:      in.Currency,
		QuantityScale: in.QuantityScale,
		AmountScale:   in.AmountScale,
		PublishedAt:   now,
	}, nil
}

// GetRateVersion 按版本号读取一份费率版本；不存在返回 CodeNotFound。
func (s *Service) GetRateVersion(ctx context.Context, tenant, meter string, version int32) (*RateVersion, error) {
	if tenant == "" || meter == "" || version <= 0 {
		return nil, newError(CodeInvalidArgument, "tenant, meter and positive version are required")
	}
	return loadRateVersion(ctx, s.db, tenant, meter, version)
}

// GetEffectiveRate 返回指定时刻对 (租户, 计量项) 适用的费率版本，
// 即 effective_from <= at 的最新版本；尚无任何生效版本时返回 CodeNoApplicableRate。
func (s *Service) GetEffectiveRate(ctx context.Context, tenant, meter string, at time.Time) (*RateVersion, error) {
	if tenant == "" || meter == "" || at.IsZero() {
		return nil, newError(CodeInvalidArgument, "tenant, meter and at are required")
	}
	r, _, err := effectiveRate(ctx, s.db, tenant, meter, at.UTC().UnixNano())
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListRateVersions 列出某 (租户, 计量项) 的全部费率版本，按生效时间升序返回。
func (s *Service) ListRateVersions(ctx context.Context, tenant, meter string) ([]RateVersion, error) {
	if tenant == "" || meter == "" {
		return nil, newError(CodeInvalidArgument, "tenant and meter are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+rateCols+`
FROM rate_versions WHERE tenant = ? AND meter = ? ORDER BY effective_from_unix, version`,
		tenant, meter)
	if err != nil {
		return nil, wrapError(CodeInternal, err, "list rate versions")
	}
	defer func() { _ = rows.Close() }()
	var out []RateVersion
	for rows.Next() {
		rv, _, err := scanRateVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rv)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 费率读取与计价
// ---------------------------------------------------------------------------

const rateCols = `tenant, meter, version, effective_from_unix, tiers_json,
                  currency, quantity_scale, amount_scale, published_at_unix`

// rowScanner 被 *sql.Row 与 *sql.Rows 同时满足。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRateVersion(row rowScanner) (*RateVersion, *frozenRate, error) {
	var rv RateVersion
	var effNS, publishedNS int64
	var tiersJSON, currency string
	var qScale, aScale int32
	if err := row.Scan(&rv.Tenant, &rv.Meter, &rv.Version, &effNS, &tiersJSON,
		&currency, &qScale, &aScale, &publishedNS); err != nil {
		return nil, nil, wrapError(CodeInternal, err, "scan rate version")
	}
	tiers, err := decodeTiers(tiersJSON)
	if err != nil {
		return nil, nil, err
	}
	rv.EffectiveFrom = nsTime(effNS)
	rv.Tiers = tiers
	rv.Currency = currency
	rv.QuantityScale = qScale
	rv.AmountScale = aScale
	rv.PublishedAt = nsTime(publishedNS)
	frozen := &frozenRate{
		Version:       rv.Version,
		EffectiveNS:   effNS,
		Tiers:         tiers,
		Currency:      currency,
		QuantityScale: qScale,
		AmountScale:   aScale,
	}
	return &rv, frozen, nil
}

func decodeTiers(tiersJSON string) ([]PriceTier, error) {
	var tiers []PriceTier
	if err := json.Unmarshal([]byte(tiersJSON), &tiers); err != nil {
		return nil, wrapError(CodeInternal, err, "decode tiers")
	}
	return tiers, nil
}

func loadRateVersion(ctx context.Context, q rowQuerier, tenant, meter string, version int32) (*RateVersion, error) {
	rv, _, err := scanRateVersion(q.QueryRowContext(ctx,
		`SELECT `+rateCols+` FROM rate_versions WHERE tenant = ? AND meter = ? AND version = ?`,
		tenant, meter, version))
	if err != nil {
		if IsCode(err, CodeInternal) && errors.Is(err, sql.ErrNoRows) {
			return nil, newError(CodeNotFound,
				"rate version %d not found for tenant %q meter %q", version, tenant, meter)
		}
		return nil, err
	}
	return rv, nil
}

// loadRateAtExact 读取生效时间恰好等于 atNS 的版本。
func loadRateAtExact(ctx context.Context, q rowQuerier, tenant, meter string, atNS int64) (*RateVersion, error) {
	rv, _, err := scanRateVersion(q.QueryRowContext(ctx,
		`SELECT `+rateCols+` FROM rate_versions
                 WHERE tenant = ? AND meter = ? AND effective_from_unix = ?`,
		tenant, meter, atNS))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, newError(CodeNotFound, "no rate published at that effective time")
		}
		return nil, err
	}
	return rv, nil
}

// effectiveRate 选择 atNS 时刻适用的费率：effective_from <= atNS 的最新版本。
// 必须保证同一时刻只有一份费率生效（生效时刻唯一），因此结果确定唯一。
func effectiveRate(ctx context.Context, q rowQuerier, tenant, meter string, atNS int64) (*RateVersion, *frozenRate, error) {
	rv, frozen, err := scanRateVersion(q.QueryRowContext(ctx,
		`SELECT `+rateCols+` FROM rate_versions
                 WHERE tenant = ? AND meter = ? AND effective_from_unix <= ?
                 ORDER BY effective_from_unix DESC, version DESC LIMIT 1`,
		tenant, meter, atNS))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, newError(CodeNoApplicableRate,
				"no applicable rate for tenant %q meter %q at %s (no rate version effective at that time)",
				tenant, meter, nsTime(atNS).Format(time.RFC3339Nano))
		}
		return nil, nil, err
	}
	return rv, frozen, nil
}

// priceQuantity 按分段价格与计价精度对数量计价。
//
// 计价口径：数量先按 QuantityScale 做四舍五入（远离零方向）取整；
// 分段按“落在每段内的数量 × 该段单价”累加（最后一段无上限）；
// 明细金额按 AmountScale 四舍五入取整。数量为负（修正增量）时按绝对值分段计价后
// 再恢复符号，即“按原事件发生时的费率表对本次增量本身计价”。
func priceQuantity(qty decimal.Decimal, fr *frozenRate) decimal.Decimal {
	q := roundAwayFromZero(qty, fr.QuantityScale)
	negative := q.IsNegative()
	if negative {
		q = q.Neg()
	}

	var amount decimal.Decimal
	prev := decimal.Zero
	for _, tier := range fr.Tiers {
		inTier := q.Sub(prev)
		if inTier.IsPositive() {
			if tier.UpTo.Sign() > 0 {
				width := tier.UpTo.Sub(prev)
				if inTier.GreaterThan(width) {
					inTier = width
				}
			}
			amount = amount.Add(inTier.Mul(tier.UnitPrice))
		}
		if tier.UpTo.Sign() == 0 {
			break
		}
		prev = tier.UpTo
	}
	amount = roundAwayFromZero(amount, fr.AmountScale)
	if negative {
		amount = amount.Neg()
	}
	return amount
}

// roundAwayFromZero 按指定小数位做四舍五入（半数远离零，与常见计价口径一致）。
func roundAwayFromZero(d decimal.Decimal, scale int32) decimal.Decimal {
	if scale < 0 {
		scale = 0
	}
	neg := d.Sign() < 0
	if neg {
		d = d.Neg()
	}
	pow := decimal.New(1, scale) // 10^scale
	scaled := d.Mul(pow)
	floor := scaled.IntPart()
	frac := scaled.Sub(decimal.NewFromInt(floor))
	if frac.GreaterThanOrEqual(decimal.New(5, -1)) { // >= 0.5
		floor++
	}
	// floor × 10^-scale，精确构造，避免除法。
	rounded := decimal.New(floor, -scale)
	if neg {
		rounded = rounded.Neg()
	}
	return rounded
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

func validateRateInput(in PublishRateInput) error {
	if in.Tenant == "" {
		return newError(CodeInvalidArgument, "tenant is required")
	}
	if in.Meter == "" {
		return newError(CodeInvalidArgument, "meter is required")
	}
	if in.EffectiveFrom.IsZero() {
		return newError(CodeInvalidArgument, "effective_from is required")
	}
	if in.Currency == "" {
		return newError(CodeInvalidArgument, "currency is required")
	}
	if in.QuantityScale < 0 || in.AmountScale < 0 {
		return newError(CodeInvalidArgument, "pricing scales must not be negative")
	}
	if len(in.Tiers) == 0 {
		return newError(CodeInvalidArgument, "at least one price tier is required")
	}
	for i, t := range in.Tiers {
		if t.UnitPrice.IsNegative() {
			return newError(CodeInvalidArgument, "tier %d unit price must not be negative, got %s", i, t.UnitPrice)
		}
		if t.UpTo.IsNegative() {
			return newError(CodeInvalidArgument, "tier %d upper bound must not be negative", i)
		}
	}
	// 上限必须严格递增；只允许最后一段无上限。
	var prevUpTo decimal.Decimal
	havePrev := false
	for i, t := range in.Tiers {
		if t.UpTo.Sign() == 0 {
			if i != len(in.Tiers)-1 {
				return newError(CodeInvalidArgument, "only the last tier may be open-ended (up_to = 0)")
			}
			continue
		}
		if !havePrev {
			if !t.UpTo.IsPositive() {
				return newError(CodeInvalidArgument, "first tier upper bound must be positive")
			}
		} else if !t.UpTo.GreaterThan(prevUpTo) {
			return newError(CodeInvalidArgument, "tier upper bounds must be strictly increasing")
		}
		prevUpTo = t.UpTo
		havePrev = true
	}
	if in.Tiers[len(in.Tiers)-1].UpTo.Sign() != 0 {
		return newError(CodeInvalidArgument, "last tier must be open-ended (up_to = 0) so any quantity is covered")
	}
	return nil
}

// normalizeTiers 返回按上限升序排列的分段副本（调用前已通过校验）。
func normalizeTiers(tiers []PriceTier) []PriceTier {
	out := make([]PriceTier, len(tiers))
	copy(out, tiers)
	sort.SliceStable(out, func(i, j int) bool {
		// 无上限段排最后。
		if out[i].UpTo.Sign() == 0 {
			return false
		}
		if out[j].UpTo.Sign() == 0 {
			return true
		}
		return out[i].UpTo.LessThan(out[j].UpTo)
	})
	return out
}

func sameRateContent(stored *RateVersion, in PublishRateInput, tiers []PriceTier) bool {
	if stored.Currency != in.Currency ||
		!stored.EffectiveFrom.Equal(in.EffectiveFrom.UTC()) ||
		stored.QuantityScale != in.QuantityScale ||
		stored.AmountScale != in.AmountScale ||
		len(stored.Tiers) != len(tiers) {
		return false
	}
	for i := range tiers {
		if !stored.Tiers[i].UpTo.Equal(tiers[i].UpTo) ||
			!stored.Tiers[i].UnitPrice.Equal(tiers[i].UnitPrice) {
			return false
		}
	}
	return true
}
