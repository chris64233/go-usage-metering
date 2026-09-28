package usagemetering

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func decp(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func closedTier(t *testing.T, upper, price string) TierInput {
	t.Helper()
	return TierInput{UpperQuantity: decp(upper), UnitPrice: mustDec(t, price)}
}

func openTierP(t *testing.T, price string) TierInput {
	t.Helper()
	return TierInput{UnitPrice: mustDec(t, price)}
}

func publishRate(t *testing.T, svc *Service, in RateInput) *RateVersion {
	t.Helper()
	rv, err := svc.PublishRate(context.Background(), in)
	if err != nil {
		t.Fatalf("publish rate %q: %v", in.VersionID, err)
	}
	return rv
}

// flatRateInput 生成一个只有开口分段的单一费率。
func flatRateInput(id, tenant, meter string, eff time.Time, price string, scale int32) RateInput {
	return RateInput{
		VersionID: id, Tenant: tenant, Meter: meter, EffectiveAt: eff,
		Tiers: []TierInput{{UnitPrice: decimal.RequireFromString(price)}}, AmountScale: scale,
	}
}

func findBillLine(lines []BillLine, kind, refID string) *BillLine {
	for i := range lines {
		if lines[i].Kind == kind && lines[i].RefID == refID {
			return &lines[i]
		}
	}
	return nil
}

func decFromLines(lines []BillLine) decimal.Decimal {
	sum := decimal.Zero
	for _, l := range lines {
		sum = sum.Add(l.Amount)
	}
	return sum
}

// ---------------------------------------------------------------------------
// 费率发布：幂等、冲突、重叠
// ---------------------------------------------------------------------------

func TestPublishRate_IdempotentConflictAndOverlap(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := flatRateInput("r1", "ta", "m", mkTime(0), "1.5", 2)
	first := publishRate(t, svc, in)
	if first.PublishedAt.IsZero() {
		t.Fatalf("published_at should be set")
	}

	// 同号同内容：幂等返回。
	again, err := svc.PublishRate(ctx, in)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !again.PublishedAt.Equal(first.PublishedAt) {
		t.Fatalf("replay must return the original published_at")
	}
	if len(again.Tiers) != 1 || !again.Tiers[0].UnitPrice.Equal(mustDec(t, "1.5")) {
		t.Fatalf("replay tiers wrong: %+v", again.Tiers)
	}

	// 同号不同内容：冲突。
	bad := in
	bad.AmountScale = 4
	if _, err := svc.PublishRate(ctx, bad); !IsCode(err, CodeConflict) {
		t.Fatalf("same version id with different content: want CodeConflict, got %v", err)
	}

	// 同计量项、同一生效时间的另一版本：费率重叠。
	overlap := flatRateInput("r2", "ta", "m", mkTime(0), "2.0", 2)
	if _, err := svc.PublishRate(ctx, overlap); !IsCode(err, CodeRateOverlap) {
		t.Fatalf("same effective instant: want CodeRateOverlap, got %v", err)
	}

	// 不同计量项或不同生效时间：允许。
	publishRate(t, svc, flatRateInput("r3", "ta", "m2", mkTime(0), "3", 2))
	publishRate(t, svc, flatRateInput("r4", "ta", "m", mkTime(50), "2.0", 2))

	got, err := svc.GetRateVersion(ctx, "ta", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.EffectiveAt.Equal(mkTime(0)) || got.AmountScale != 2 {
		t.Fatalf("get rate wrong: %+v", got)
	}
	if _, err := svc.GetRateVersion(ctx, "ta", "ghost"); !IsCode(err, CodeNotFound) {
		t.Fatalf("missing rate: want CodeNotFound, got %v", err)
	}
}

func TestPublishRate_Validation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	valid := flatRateInput("r1", "ta", "m", mkTime(0), "1", 2)

	cases := map[string]RateInput{
		"缺版本号":   func() RateInput { x := valid; x.VersionID = ""; return x }(),
		"缺租户":    func() RateInput { x := valid; x.Tenant = ""; return x }(),
		"缺计量项":   func() RateInput { x := valid; x.Meter = ""; return x }(),
		"缺生效时间":  func() RateInput { x := valid; x.EffectiveAt = time.Time{}; return x }(),
		"精度越界负数": func() RateInput { x := valid; x.AmountScale = -1; return x }(),
		"精度越界过大": func() RateInput { x := valid; x.AmountScale = 19; return x }(),
		"无分段":    func() RateInput { x := valid; x.Tiers = nil; return x }(),
		"负单价": func() RateInput {
			x := valid
			x.Tiers = []TierInput{{UnitPrice: mustDec(t, "-0.01")}}
			return x
		}(),
		"末段闭口": func() RateInput {
			x := valid
			x.Tiers = []TierInput{closedTier(t, "10", "1")}
			return x
		}(),
		"非末段开口": func() RateInput {
			x := valid
			x.Tiers = []TierInput{
				openTierP(t, "1"),
				openTierP(t, "2"),
			}
			return x
		}(),
		"上限非递增": func() RateInput {
			x := valid
			x.Tiers = []TierInput{
				closedTier(t, "10", "1"),
				closedTier(t, "10", "2"),
				openTierP(t, "3"),
			}
			return x
		}(),
	}
	for name, in := range cases {
		if _, err := svc.PublishRate(ctx, in); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("%s: want CodeInvalidArgument, got %v", name, err)
		}
	}
}

func TestListRateVersions_Ordered(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	publishRate(t, svc, flatRateInput("r2", "ta", "m", mkTime(50), "2", 2))
	publishRate(t, svc, flatRateInput("r1", "ta", "m", mkTime(0), "1", 2))
	publishRate(t, svc, flatRateInput("o1", "ta", "other", mkTime(0), "9", 2))

	all, err := svc.ListRateVersions(ctx, "ta", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rates, got %d", len(all))
	}
	// (meter, effective_at) 排序：m@0, m@50, other@0。
	want := []struct {
		id  string
		eff time.Time
	}{
		{"r1", mkTime(0)}, {"r2", mkTime(50)}, {"o1", mkTime(0)},
	}
	for i, w := range want {
		if all[i].VersionID != w.id || !all[i].EffectiveAt.Equal(w.eff) {
			t.Fatalf("rate[%d] = %s@%s, want %s@%s", i, all[i].VersionID, all[i].EffectiveAt, w.id, w.eff)
		}
	}

	one, err := svc.ListRateVersions(ctx, "ta", "m")
	if err != nil || len(one) != 2 {
		t.Fatalf("filter by meter: %v %+v", err, one)
	}
}

// ---------------------------------------------------------------------------
// 分段计价引擎
// ---------------------------------------------------------------------------

func TestPriceQuantity_TieredProgressiveAndRounding(t *testing.T) {
	rate := loadedRate{
		versionID:   "r1",
		amountScale: 2,
		tiers: []PriceTier{
			{UpperQuantity: decp("10"), UnitPrice: mustDec(t, "1")}, // (0,10]
			{UpperQuantity: decp("20"), UnitPrice: mustDec(t, "2")}, // (10,20]
			{UnitPrice: mustDec(t, "3")},                            // (20,+∞)
		},
	}

	segs, amount := priceQuantity(mustDec(t, "25"), rate)
	// 10*1 + 10*2 + 5*3 = 45。
	if !amount.Equal(mustDec(t, "45")) {
		t.Fatalf("amount = %s, want 45", amount)
	}
	if len(segs) != 3 {
		t.Fatalf("want 3 segments, got %+v", segs)
	}
	if !segs[0].Quantity.Equal(mustDec(t, "10")) || !segs[0].RawAmount.Equal(mustDec(t, "10")) {
		t.Fatalf("seg0 wrong: %+v", segs[0])
	}
	if !segs[1].Quantity.Equal(mustDec(t, "10")) || !segs[1].RawAmount.Equal(mustDec(t, "20")) {
		t.Fatalf("seg1 wrong: %+v", segs[1])
	}
	if !segs[2].Quantity.Equal(mustDec(t, "5")) || !segs[2].RawAmount.Equal(mustDec(t, "15")) {
		t.Fatalf("seg2 wrong: %+v", segs[2])
	}

	// 精度：单价 0.1 无法用二进制浮点表示，数量 3 -> 精确 0.30。
	rate.tiers = []PriceTier{{UnitPrice: mustDec(t, "0.1")}}
	_, amount = priceQuantity(mustDec(t, "3"), rate)
	if !amount.Equal(mustDec(t, "0.30")) {
		t.Fatalf("exact decimal pricing failed: %s", amount)
	}

	// 舍入：0.005 在 2 位精度下四舍五入（半数远离零）= 0.01。
	rate.tiers = []PriceTier{{UnitPrice: mustDec(t, "0.005")}}
	_, amount = priceQuantity(mustDec(t, "1"), rate)
	if !amount.Equal(mustDec(t, "0.01")) {
		t.Fatalf("rounding = %s, want 0.01", amount)
	}

	// 负数量：按绝对值分段计价后恢复符号。
	rate.tiers = []PriceTier{
		{UpperQuantity: decp("10"), UnitPrice: mustDec(t, "1")},
		{UnitPrice: mustDec(t, "2")},
	}
	segs, amount = priceQuantity(mustDec(t, "-15"), rate)
	if !amount.Equal(mustDec(t, "-20")) { // -(10*1 + 5*2)
		t.Fatalf("negative amount = %s, want -20", amount)
	}
	for _, sg := range segs {
		if !sg.Quantity.IsNegative() || !sg.RawAmount.IsNegative() {
			t.Fatalf("negative quantity must keep signed segments: %+v", sg)
		}
	}
}

// ---------------------------------------------------------------------------
// 账单草稿：基本计价、费率版本选择
// ---------------------------------------------------------------------------

func TestGenerateBill_BasicPricingAndRateSelection(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)

	// 两版费率：t0 起单价 1；t50 起单价 2。
	publishRate(t, svc, flatRateInput("r-v1", "ta", "m", t1, "1", 2))
	publishRate(t, svc, flatRateInput("r-v2", "ta", "m", mkTime(50), "2", 2))

	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e2", Tenant: "ta", Meter: "m", OccurredAt: mkTime(60), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(20), Delta: mustDec(t, "-2"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	bill, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bill.Version != 1 || bill.Status != StatusCurrent || !bill.PeriodClosed {
		t.Fatalf("bill meta wrong: %+v", bill)
	}
	if !bill.PeriodEnd.Equal(t2) {
		t.Fatalf("frozen period end = %s", bill.PeriodEnd)
	}
	// 数量快照冻结：10 + 10 - 2 = 18。
	if !bill.SnapshotQuantities["m"].Equal(mustDec(t, "18")) {
		t.Fatalf("snapshot qty = %v", bill.SnapshotQuantities)
	}
	// 金额：e1@10=10（v1），e2@60=20（v2），c1=-2（发生在 t20，v1）= -2，合计 28。
	if !bill.TotalAmount.Equal(mustDec(t, "28")) {
		t.Fatalf("total = %s, want 28", bill.TotalAmount)
	}
	if l := findBillLine(bill.Lines, KindEvent, "e2"); l.RateVersionID != "r-v2" ||
		!l.RateEffectiveAt.Equal(mkTime(50)) {
		t.Fatalf("e2 must use v2: %+v", l)
	}
	if l := findBillLine(bill.Lines, KindCorrection, "c1"); l.RateVersionID != "r-v1" ||
		!l.Amount.Equal(mustDec(t, "-2")) {
		t.Fatalf("c1 must use v1 and be negative: %+v", l)
	}
	// 汇总必须精确等于全部明细之和。
	if sum := decFromLines(bill.Lines); !sum.Equal(bill.TotalAmount) {
		t.Fatalf("total %s != sum of lines %s", bill.TotalAmount, sum)
	}
}

func TestGenerateBill_NoApplicableRate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	_, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "run-1",
	})
	if !IsCode(err, CodeNoRate) {
		t.Fatalf("want CodeNoRate, got %v", err)
	}
	// 失败不得留下任何草稿版本。
	if _, err := svc.GetCurrentBill(ctx, "ta", t1); !IsCode(err, CodeNotFound) {
		t.Fatalf("failed generation must not create a draft, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 草稿幂等、冻结与不可改写
// ---------------------------------------------------------------------------

func TestGenerateBill_IdempotentAndFrozen(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	publishRate(t, svc, flatRateInput("r1", "ta", "m", t1, "1", 2))
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}

	in := GenerateBillInput{Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1"}
	bill := publishBill(t, svc, in)

	// 同键重复生成：返回同一版本（同一 CreatedAt / 金额 / 明细）。
	again, err := svc.GenerateBill(ctx, in)
	if err != nil {
		t.Fatalf("idempotent regenerate: %v", err)
	}
	if again.Version != bill.Version || !again.CreatedAt.Equal(bill.CreatedAt) ||
		!again.TotalAmount.Equal(bill.TotalAmount) || len(again.Lines) != len(bill.Lines) {
		t.Fatalf("idempotent regenerate changed the draft:\n%+v\n%+v", bill, again)
	}

	// 不同幂等键：冲突，避免无意中触发重算。
	if _, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k2",
	}); !IsCode(err, CodeBillConflict) {
		t.Fatalf("different idempotency key: want CodeBillConflict, got %v", err)
	}

	// 草稿生成之后发布的新费率（下一时刻生效，不重叠）不得改写已冻结草稿。
	publishRate(t, svc, flatRateInput("r2", "ta", "m", t2, "9", 2))
	reloaded, err := svc.GenerateBill(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if l := findBillLine(reloaded.Lines, KindEvent, "e1"); l.RateVersionID != "r1" ||
		!l.Amount.Equal(mustDec(t, "10")) {
		t.Fatalf("frozen draft was rewritten by a later rate: %+v", l)
	}

	// 关闭后到达的迟到用量进入下一周期，不得塞进旧草稿。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late", Tenant: "ta", Meter: "m", OccurredAt: mkTime(20), Quantity: mustDec(t, "7"),
	}); err != nil {
		t.Fatal(err)
	}
	oldAgain, err := svc.GenerateBill(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if findBillLine(oldAgain.Lines, KindEvent, "late") != nil {
		t.Fatalf("late usage must not be injected into the frozen old draft")
	}
	if len(oldAgain.Lines) != 1 {
		t.Fatalf("old draft line count changed: %d", len(oldAgain.Lines))
	}
}

func publishBill(t *testing.T, svc *Service, in GenerateBillInput) *BillVersion {
	t.Helper()
	bv, err := svc.GenerateBill(context.Background(), in)
	if err != nil {
		t.Fatalf("generate bill: %v", err)
	}
	return bv
}

// 开放周期也可生成草稿：冻结当时边界；之后的新数据不进旧草稿，作废重算后才纳入。
func TestGenerateBill_OpenPeriodThenVoidAndRegenerate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	publishRate(t, svc, flatRateInput("r1", "ta", "m", t1, "1", 2))
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}

	v1, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v1.PeriodClosed || !v1.PeriodEnd.IsZero() {
		t.Fatalf("open-period draft should record PeriodClosed=false: %+v", v1)
	}
	if !v1.TotalAmount.Equal(mustDec(t, "10")) {
		t.Fatalf("v1 total = %s", v1.TotalAmount)
	}

	// 草稿冻结后新到达的数据不会改变它。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e2", Tenant: "ta", Meter: "m", OccurredAt: mkTime(20), Quantity: mustDec(t, "5"),
	}); err != nil {
		t.Fatal(err)
	}
	same, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1",
	})
	if err != nil || same.Version != 1 || len(same.Lines) != 1 {
		t.Fatalf("frozen open-period draft changed: %v %+v", err, same)
	}

	// 作废后重算：新版本纳入新数据并冻结关闭状态；旧版本保留。
	voided, err := svc.VoidBill(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	if voided.Status != StatusVoided || voided.VoidedAt.IsZero() {
		t.Fatalf("voided draft wrong: %+v", voided)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	v2, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || v2.Status != StatusCurrent || !v2.PeriodClosed {
		t.Fatalf("regenerated draft wrong: %+v", v2)
	}
	if !v2.TotalAmount.Equal(mustDec(t, "15")) || len(v2.Lines) != 2 {
		t.Fatalf("v2 should include the later event: total=%s lines=%d", v2.TotalAmount, len(v2.Lines))
	}

	// 旧版本与明细仍可查询。
	old, err := svc.GetBillVersion(ctx, "ta", t1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusVoided || len(old.Lines) != 1 {
		t.Fatalf("voided v1 must be retained with its lines: %+v", old)
	}
	cur, err := svc.GetCurrentBill(ctx, "ta", t1)
	if err != nil || cur.Version != 2 {
		t.Fatalf("current should be v2: %v %+v", err, cur)
	}
	versions, err := svc.ListBillVersions(ctx, "ta", t1)
	if err != nil || len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("versions history wrong: %v %+v", err, versions)
	}

	// 作废当前版本（v2）成功；没有当前版本后再次作废：冲突。
	if _, err := svc.VoidBill(ctx, "ta", t1); err != nil {
		t.Fatalf("void current v2: %v", err)
	}
	if _, err := svc.VoidBill(ctx, "ta", t1); !IsCode(err, CodeBillConflict) {
		t.Fatalf("void with no current draft: want CodeBillConflict, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 迟到调整：单列、引用原事件/原周期、按原事件发生时费率计价
// ---------------------------------------------------------------------------

func TestBill_LateCorrectionAdjustmentPricedAtOriginalEventRate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200)

	// v1 自 t0 单价 1；v2 自 t50 单价 2（覆盖修正自身发生时间 t60）。
	publishRate(t, svc, flatRateInput("r-v1", "ta", "m", t1, "1", 2))
	publishRate(t, svc, flatRateInput("r-v2", "ta", "m", mkTime(50), "2", 2))

	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	// P1 先关闭并出草稿：e1 适用 v1，金额 10。
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	p1 := publishBill(t, svc, GenerateBillInput{Tenant: "ta", PeriodStart: t1, IdempotencyKey: "p1"})
	if !p1.TotalAmount.Equal(mustDec(t, "10")) {
		t.Fatalf("P1 total = %s", p1.TotalAmount)
	}

	// 关闭后到达的迟到修正：发生时间 t60（其自身时刻 v2 有效），引用旧事件 e1。
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "late-c", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(60), Delta: mustDec(t, "-3"),
	}); err != nil {
		t.Fatal(err)
	}
	// 迟到事件调整行：发生在 t20（v1 有效）。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late-e", Tenant: "ta", Meter: "m", OccurredAt: mkTime(20), Quantity: mustDec(t, "4"),
	}); err != nil {
		t.Fatal(err)
	}

	// 旧周期草稿不被改写。
	p1Again, err := svc.GetBillVersion(ctx, "ta", t1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1Again.Lines) != 1 || !p1Again.TotalAmount.Equal(mustDec(t, "10")) {
		t.Fatalf("frozen P1 draft mutated: %+v", p1Again)
	}

	// 关闭 P2 并出草稿。
	if _, err := svc.ClosePeriod(ctx, "ta", t2, t3); err != nil {
		t.Fatal(err)
	}
	p2 := publishBill(t, svc, GenerateBillInput{Tenant: "ta", PeriodStart: t2, IdempotencyKey: "p2"})

	cl := findBillLine(p2.Lines, KindCorrection, "late-c")
	if cl == nil {
		t.Fatalf("late correction missing from P2 draft: %+v", p2.Lines)
	}
	if !cl.Adjustment || cl.OriginPeriodStart != t1 || cl.EventID != "e1" {
		t.Fatalf("late correction must be a separately-listed adjustment linked to origin: %+v", cl)
	}
	// 关键断言：按原事件发生时间 t10 选费率 -> v1 单价 1，金额 -3；
	// 若误用修正自身时间 t60 则会选 v2 得到 -6。
	if cl.RateVersionID != "r-v1" || !cl.Amount.Equal(mustDec(t, "-3")) {
		t.Fatalf("late correction must be priced at the ORIGINAL event's rate v1: %+v", cl)
	}
	el := findBillLine(p2.Lines, KindEvent, "late-e")
	if el == nil || !el.Adjustment || el.OriginPeriodStart != t1 {
		t.Fatalf("late event adjustment missing or not linked: %+v", el)
	}
	if !el.Amount.Equal(mustDec(t, "4")) {
		t.Fatalf("late event amount = %s", el.Amount)
	}
	// 调整行单列，且汇总精确等于明细之和。
	if len(p2.Lines) != 2 {
		t.Fatalf("P2 should contain exactly the two adjustment lines, got %+v", p2.Lines)
	}
	if !p2.TotalAmount.Equal(decFromLines(p2.Lines)) || !p2.TotalAmount.Equal(mustDec(t, "1")) {
		t.Fatalf("P2 total = %s, want 1 = sum of lines", p2.TotalAmount)
	}

	// 计价明细查询返回冻结的费率副本与分段过程。
	listed, err := svc.ListBillLines(ctx, "ta", t2, p2.Version)
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListBillLines: %v %+v", err, listed)
	}
	gotCL := findBillLine(listed, KindCorrection, "late-c")
	if gotCL == nil || len(gotCL.Tiers) != 1 || len(gotCL.Segments) != 1 ||
		!gotCL.Segments[0].UnitPrice.Equal(mustDec(t, "1")) {
		t.Fatalf("frozen pricing detail wrong: %+v", gotCL)
	}
}

// ---------------------------------------------------------------------------
// 并发重算：只有一个版本成为当前草稿
// ---------------------------------------------------------------------------

func TestGenerateBill_ConcurrentRegenerationSingleCurrent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	publishRate(t, svc, flatRateInput("r1", "ta", "m", t1, "1", 2))
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	// v1 先存在再作废，制造多个并发重算者竞争“当前版本”。
	publishBill(t, svc, GenerateBillInput{Tenant: "ta", PeriodStart: t1, IdempotencyKey: "first"})
	if _, err := svc.VoidBill(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}

	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	versions := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bv, err := svc.GenerateBill(ctx, GenerateBillInput{
				Tenant: "ta", PeriodStart: t1, IdempotencyKey: fmt.Sprintf("winner-%d", i),
			})
			if err == nil {
				versions[i] = bv.Version
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	winners, conflicts := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			if versions[i] != 2 {
				t.Fatalf("winner created version %d, want 2", versions[i])
			}
		case IsCode(err, CodeBillConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winners != 1 || conflicts != n-1 {
		t.Fatalf("want exactly 1 winner and %d conflicts, got winners=%d conflicts=%d",
			n-1, winners, conflicts)
	}
	cur, err := svc.GetCurrentBill(ctx, "ta", t1)
	if err != nil || cur.Status != StatusCurrent || cur.Version != 2 {
		t.Fatalf("current draft wrong after race: %v %+v", err, cur)
	}
}

// 数量修正与草稿生成并发：冻结边界把每笔修正二分到唯一一个草稿版本归属，
// 同一版本内每笔修正恰好出现一次；重算后的新版本才纳入全部修正。
func TestConcurrentCorrectionAndRegeneration_PartitionByBoundary(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1 := mkTime(0)
	publishRate(t, svc, flatRateInput("r1", "ta", "m", t1, "1", 2))
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "anchor", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(5), Quantity: mustDec(t, "100"),
	}); err != nil {
		t.Fatal(err)
	}

	const corrN = 64
	const billWorkers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})

	// 并发提交修正（周期仍开放，全部为正常修正行；+1 增量不触发下限）。
	for i := 0; i < corrN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _ = svc.ApplyCorrection(ctx, CorrectionInput{
				CorrectionID: fmt.Sprintf("corr-%02d", i), Tenant: "ta", EventID: "anchor",
				OccurredAt: mkTime(10 + (i % 9000)), Delta: mustDec(t, "1"),
			})
		}(i)
	}
	// 并发生成草稿：同键调用只有一个版本，其余返回同一冻结版本。
	bills := make([]*BillVersion, billWorkers)
	for w := 0; w < billWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			bv, err := svc.GenerateBill(ctx, GenerateBillInput{
				Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1",
			})
			if err == nil {
				bills[w] = bv
			}
		}(w)
	}
	close(start)
	wg.Wait()

	// 所有并发生成结果必须是同一个 v1。
	var frozen *BillVersion
	for _, bv := range bills {
		if bv == nil {
			continue
		}
		if frozen == nil {
			frozen = bv
		} else if bv.Version != frozen.Version || !bv.CreatedAt.Equal(frozen.CreatedAt) {
			t.Fatalf("concurrent generation produced different drafts: v%d vs v%d",
				bv.Version, frozen.Version)
		}
	}
	if frozen == nil {
		t.Fatal("no bill was generated")
	}

	// 边界二分：commit_seq <= 冻结边界的修正必须恰好出现一次，其余不出现。
	seen := map[string]int{}
	corrInV1 := 0
	var sumAmount decimal.Decimal
	for _, l := range frozen.Lines {
		if l.Kind == KindCorrection {
			seen[l.RefID]++
			corrInV1++
		}
		sumAmount = sumAmount.Add(l.Amount)
	}
	for ref, n := range seen {
		if n != 1 {
			t.Fatalf("correction %q appears %d times in one draft version, want exactly 1", ref, n)
		}
	}
	for _, l := range frozen.Lines {
		if l.Kind == KindCorrection && l.CommitSeq > frozen.BoundarySeq {
			t.Fatalf("line %s commit_seq %d past frozen boundary %d must not be included",
				l.RefID, l.CommitSeq, frozen.BoundarySeq)
		}
	}
	// 用数据库独立核验：边界内的修正总数 == v1 明细中的修正数。
	var wantCount int
	if err := svc.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM corrections
WHERE tenant='ta' AND period_start_unix=? AND commit_seq <= ?`,
		t1.UnixNano(), frozen.BoundarySeq).Scan(&wantCount); err != nil {
		t.Fatal(err)
	}
	if wantCount != corrInV1 {
		t.Fatalf("boundary partition mismatch: db=%d draft=%d", wantCount, corrInV1)
	}
	// 金额汇总精确等于全部明细之和；数量快照 = 锚点 + 边界内修正。
	if !sumAmount.Equal(frozen.TotalAmount) {
		t.Fatalf("frozen total %s != sum of line amounts %s", frozen.TotalAmount, sumAmount)
	}
	wantQty := mustDec(t, fmt.Sprintf("%d", 100+corrInV1))
	if !frozen.SnapshotQuantities["m"].Equal(wantQty) {
		t.Fatalf("frozen snapshot qty = %s, want %s", frozen.SnapshotQuantities["m"], wantQty)
	}

	// 全部修正落库后作废重算：v2 纳入全部 corrN 笔修正，每笔仍恰好一次。
	if _, err := svc.VoidBill(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	v2, err := svc.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k2",
	})
	if err != nil {
		t.Fatal(err)
	}
	corrInV2 := 0
	allOnce := map[string]int{}
	var v2Sum decimal.Decimal
	for _, l := range v2.Lines {
		v2Sum = v2Sum.Add(l.Amount)
		if l.Kind == KindCorrection {
			corrInV2++
			allOnce[l.RefID]++
		}
	}
	if corrInV2 != corrN || len(allOnce) != corrN {
		t.Fatalf("v2 must include all %d corrections once each: lines=%d distinct=%d",
			corrN, corrInV2, len(allOnce))
	}
	for ref, n := range allOnce {
		if n != 1 {
			t.Fatalf("correction %q duplicated in v2: %d", ref, n)
		}
	}
	if !v2Sum.Equal(v2.TotalAmount) {
		t.Fatalf("v2 total %s != sum of lines %s", v2.TotalAmount, v2Sum)
	}
	wantV2 := mustDec(t, fmt.Sprintf("%d", 100+corrN))
	if !v2.SnapshotQuantities["m"].Equal(wantV2) {
		t.Fatalf("v2 qty = %s, want %s", v2.SnapshotQuantities["m"], wantV2)
	}
}

// 费率生效边界：发生时间恰好等于某版本生效时间时，应选取该版本（生效时间含该时刻）。
func TestRateSelection_EffectiveBoundaryInclusive(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	publishRate(t, svc, flatRateInput("r-v1", "ta", "m", t1, "1", 2))
	publishRate(t, svc, flatRateInput("r-v2", "ta", "m", mkTime(50), "2", 2))
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	// 恰好发生在 t50：应取 v2（含生效时刻）。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(50), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	bill := publishBill(t, svc, GenerateBillInput{Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k"})
	if l := findBillLine(bill.Lines, KindEvent, "e1"); l.RateVersionID != "r-v2" ||
		!l.Amount.Equal(mustDec(t, "2")) {
		t.Fatalf("usage exactly at effective time must pick the newer inclusive rate: %+v", l)
	}
}

// 费率版本与账单草稿（含明细）跨关闭重开持久化，且冻结内容不被重开改变。
func TestBilling_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + dir + "/meter.db"
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)

	func() {
		svc, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = svc.Close() }()
		publishRate(t, svc, flatRateInput("r1", "ta", "m", t1, "0.1", 2))
		if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ReportEvent(ctx, EventInput{
			EventID: "e1", Tenant: "ta", Meter: "m",
			OccurredAt: mkTime(10), Quantity: mustDec(t, "3"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
			t.Fatal(err)
		}
		publishBill(t, svc, GenerateBillInput{Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1"})
	}()

	svc2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc2.Close() }()

	rv, err := svc2.GetRateVersion(ctx, "ta", "r1")
	if err != nil {
		t.Fatalf("rate lost across reopen: %v", err)
	}
	if len(rv.Tiers) != 1 || !rv.Tiers[0].UnitPrice.Equal(mustDec(t, "0.1")) {
		t.Fatalf("rate tiers lost across reopen: %+v", rv.Tiers)
	}

	bill, err := svc2.GetCurrentBill(ctx, "ta", t1)
	if err != nil {
		t.Fatalf("bill lost across reopen: %v", err)
	}
	if !bill.TotalAmount.Equal(mustDec(t, "0.30")) || len(bill.Lines) != 1 {
		t.Fatalf("frozen bill changed across reopen: total=%s lines=%d", bill.TotalAmount, len(bill.Lines))
	}
	l := bill.Lines[0]
	if l.RateVersionID != "r1" || len(l.Segments) != 1 ||
		!l.Segments[0].RawAmount.Equal(mustDec(t, "0.3")) ||
		!l.Segments[0].UnitPrice.Equal(mustDec(t, "0.1")) {
		t.Fatalf("frozen pricing detail lost across reopen: %+v", l)
	}

	// 重开后同键生成仍是同一冻结版本。
	again, err := svc2.GenerateBill(ctx, GenerateBillInput{
		Tenant: "ta", PeriodStart: t1, IdempotencyKey: "k1",
	})
	if err != nil || again.Version != bill.Version {
		t.Fatalf("idempotent draft after reopen: %v %+v", err, again)
	}
}
