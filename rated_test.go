package usagemetering

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// flatTiers 构造单一开放式分段（单价 price）。
func flatTiers(price string) []PriceTier {
	return []PriceTier{{UpTo: decimal.Zero, UnitPrice: decimal.RequireFromString(price)}}
}

// tiered 构造 [(0,u1):p1, (u1,u2):p2, ...] 分段，最后一段开放（最后一个上限传 "0"）。
func tiered(spec ...string) []PriceTier {
	if len(spec)%2 != 0 {
		panic("tiered spec must be up_to/price pairs")
	}
	var tiers []PriceTier
	for i := 0; i < len(spec); i += 2 {
		tiers = append(tiers, PriceTier{
			UpTo:      decimal.RequireFromString(spec[i]),
			UnitPrice: decimal.RequireFromString(spec[i+1]),
		})
	}
	return tiers
}

func publishFlat(t *testing.T, svc *Service, tenant, meter string, effective time.Time, price string) *RateVersion {
	t.Helper()
	rv, err := svc.PublishRate(context.Background(), PublishRateInput{
		Tenant: tenant, Meter: meter, EffectiveFrom: effective,
		Tiers: flatTiers(price), Currency: "USD", QuantityScale: 3, AmountScale: 2,
	})
	if err != nil {
		t.Fatalf("publish rate %s@%s: %v", price, effective.Format(time.RFC3339), err)
	}
	return rv
}

// ---------------------------------------------------------------------------
// 费率发布：版本号、幂等、重叠、不可变
// ---------------------------------------------------------------------------

func TestPublishRate_VersioningAndIdempotency(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := PublishRateInput{
		Tenant: "ta", Meter: "m", EffectiveFrom: mkTime(0),
		Tiers:    tiered("10", "1.00", "0", "2.00"),
		Currency: "USD", QuantityScale: 3, AmountScale: 2,
	}
	v1, err := svc.PublishRate(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || !v1.PublishedAt.After(time.Time{}) {
		t.Fatalf("first version bad: %+v", v1)
	}
	if len(v1.Tiers) != 2 {
		t.Fatalf("tiers not stored: %+v", v1.Tiers)
	}

	// 同一生效时刻、完全相同内容（单价 1 与 1.00 数值相等）-> 幂等返回原版本。
	same := in
	same.Tiers = tiered("10", "1", "0", "2.0")
	again, err := svc.PublishRate(ctx, same)
	if err != nil {
		t.Fatalf("idempotent republish: %v", err)
	}
	if again.Version != v1.Version || !again.PublishedAt.Equal(v1.PublishedAt) {
		t.Fatalf("republish must return the same immutable version")
	}

	// 同一生效时刻、不同内容 -> 费率重叠冲突。
	diff := in
	diff.Tiers = tiered("10", "1.50", "0", "2.00")
	if _, err := svc.PublishRate(ctx, diff); !IsCode(err, CodeRateOverlap) {
		t.Fatalf("want CodeRateOverlap, got %v", err)
	}

	// 另一个生效时刻 -> 新版本号。
	v2, err := svc.PublishRate(ctx, PublishRateInput{
		Tenant: "ta", Meter: "m", EffectiveFrom: mkTime(50),
		Tiers:    flatTiers("3.00"),
		Currency: "USD", QuantityScale: 3, AmountScale: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("want version 2, got %d", v2.Version)
	}

	// 租户与计量项维度独立。
	vOther, err := svc.PublishRate(ctx, PublishRateInput{
		Tenant: "tb", Meter: "m", EffectiveFrom: mkTime(0),
		Tiers:    flatTiers("9"),
		Currency: "USD", QuantityScale: 0, AmountScale: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vOther.Version != 1 {
		t.Fatalf("other tenant should have its own version sequence, got %d", vOther.Version)
	}
}

func TestPublishRate_Validation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	valid := PublishRateInput{
		Tenant: "ta", Meter: "m", EffectiveFrom: mkTime(0),
		Tiers: tiered("10", "1", "0", "2"), Currency: "USD",
		QuantityScale: 2, AmountScale: 2,
	}
	cases := map[string]PublishRateInput{
		"缺租户":   func() PublishRateInput { x := valid; x.Tenant = ""; return x }(),
		"缺计量项":  func() PublishRateInput { x := valid; x.Meter = ""; return x }(),
		"缺生效时间": func() PublishRateInput { x := valid; x.EffectiveFrom = time.Time{}; return x }(),
		"缺币种":   func() PublishRateInput { x := valid; x.Currency = ""; return x }(),
		"空分段":   func() PublishRateInput { x := valid; x.Tiers = nil; return x }(),
		"负单价":   func() PublishRateInput { x := valid; x.Tiers = tiered("10", "-1", "0", "2"); return x }(),
		"非末段开放": func() PublishRateInput { x := valid; x.Tiers = tiered("0", "1", "10", "2"); return x }(),
		"末段非开放": func() PublishRateInput { x := valid; x.Tiers = tiered("10", "1", "20", "2"); return x }(),
		"上限不递增": func() PublishRateInput { x := valid; x.Tiers = tiered("10", "1", "10", "2", "0", "3"); return x }(),
		"负精度":   func() PublishRateInput { x := valid; x.QuantityScale = -1; return x }(),
	}
	for name, in := range cases {
		if _, err := svc.PublishRate(ctx, in); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("%s: want CodeInvalidArgument, got %v", name, err)
		}
	}
}

func TestEffectiveRate_SelectionByOccurrenceTime(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	// 允许补发生效时间更早的版本。
	publishFlat(t, svc, "ta", "m", mkTime(10), "2.00")
	publishFlat(t, svc, "ta", "m", mkTime(0), "1.00")

	r, err := svc.GetEffectiveRate(ctx, "ta", "m", mkTime(5))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Tiers[0].UnitPrice.Equal(mustDec(t, "1.00")) {
		t.Fatalf("at t=5 want v1 price 1.00, got %s", r.Tiers[0].UnitPrice)
	}
	r, err = svc.GetEffectiveRate(ctx, "ta", "m", mkTime(10))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Tiers[0].UnitPrice.Equal(mustDec(t, "2.00")) {
		t.Fatalf("at t=10 (boundary inclusive) want 2.00, got %s", r.Tiers[0].UnitPrice)
	}
	// 早于任何生效时间 -> 无适用费率。
	if _, err := svc.GetEffectiveRate(ctx, "ta", "m", mkTime(0).Add(-time.Second)); !IsCode(err, CodeNoApplicableRate) {
		t.Fatalf("want CodeNoApplicableRate, got %v", err)
	}
	// 该计量项从未发布费率。
	if _, err := svc.GetEffectiveRate(ctx, "ta", "other", mkTime(10)); !IsCode(err, CodeNoApplicableRate) {
		t.Fatalf("want CodeNoApplicableRate for unknown meter, got %v", err)
	}

	// 版本号按发布顺序分配，列表按生效时间排序：补发的 t=0 是 v2，排在最前。
	list, err := svc.ListRateVersions(ctx, "ta", "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 rate versions, got %+v", list)
	}
	if !list[0].EffectiveFrom.Equal(mkTime(0)) || list[0].Version != 2 ||
		!list[0].Tiers[0].UnitPrice.Equal(mustDec(t, "1.00")) {
		t.Fatalf("list[0] should be backfilled rate at t=0 (v2): %+v", list[0])
	}
	if !list[1].EffectiveFrom.Equal(mkTime(10)) || list[1].Version != 1 {
		t.Fatalf("list[1] should be first published rate at t=10 (v1): %+v", list[1])
	}
	if _, err := svc.GetRateVersion(ctx, "ta", "m", 1); err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if _, err := svc.GetRateVersion(ctx, "ta", "m", 99); !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound for missing version, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 计价口径：分段 + 精度 + 负增量
// ---------------------------------------------------------------------------

func TestPriceQuantity_TieredAndRounding(t *testing.T) {
	// 0-10 单价 1；10-20 单价 2；20+ 单价 3；数量精度 0、金额精度 2。
	fr := &frozenRate{
		Tiers:         tiered("10", "1", "20", "2", "0", "3"),
		QuantityScale: 0, AmountScale: 2,
	}
	cases := map[string]string{
		"5":   "5.00",   // 全在第一段
		"10":  "10.00",  // 边界：10*1
		"15":  "20.00",  // 10*1 + 5*2
		"25":  "45.00",  // 10*1 + 10*2 + 5*3
		"-15": "-20.00", // 修正负增量按同费率表计价后恢复符号
	}
	for qty, want := range cases {
		if got := priceQuantity(mustDec(t, qty), fr); !got.Equal(mustDec(t, want)) {
			t.Fatalf("price(%s) = %s, want %s", qty, got, want)
		}
	}

	// 数量精度 1：0.05 进位到 0.1 后计价（单价 1）-> 0.10。
	fr2 := &frozenRate{Tiers: flatTiers("1"), QuantityScale: 1, AmountScale: 2}
	if got := priceQuantity(mustDec(t, "0.05"), fr2); !got.Equal(mustDec(t, "0.10")) {
		t.Fatalf("quantity rounding: got %s", got)
	}
	// 金额精度 0：单价 0.333 * 3 = 0.999 -> 1。
	fr3 := &frozenRate{Tiers: flatTiers("0.333"), QuantityScale: 3, AmountScale: 0}
	if got := priceQuantity(mustDec(t, "3"), fr3); !got.Equal(mustDec(t, "1")) {
		t.Fatalf("amount rounding half away from zero: got %s", got)
	}
	// 半数远离零：2.5 在金额精度 0 下进位为 3，-2.5 进位为 -3。
	if got := roundAwayFromZero(mustDec(t, "2.5"), 0); !got.Equal(mustDec(t, "3")) {
		t.Fatalf("round +2.5 = %s", got)
	}
	if got := roundAwayFromZero(mustDec(t, "-2.5"), 0); !got.Equal(mustDec(t, "-3")) {
		t.Fatalf("round -2.5 = %s", got)
	}
}

// ---------------------------------------------------------------------------
// 草稿生成：冻结、幂等、适用费率按发生时间
// ---------------------------------------------------------------------------

// seedRatedPeriod 建一个已关闭周期 [t0,t1)，含两条不同发生时间的事件与一条修正。
func seedRatedPeriod(t *testing.T, svc *Service, tenant string, t0, t1 time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.EnsurePeriod(ctx, tenant, t0); err != nil {
		t.Fatal(err)
	}
	report := func(id string, at time.Time, q string) {
		if _, err := svc.ReportEvent(ctx, EventInput{
			EventID: id, Tenant: tenant, Meter: "m", OccurredAt: at, Quantity: mustDec(t, q),
		}); err != nil {
			t.Fatal(err)
		}
	}
	report("e-old", t0.Add(10*time.Second), "5") // 适用 v1 单价 1
	report("e-new", t0.Add(60*time.Second), "5") // 适用 v2 单价 2
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: tenant, EventID: "e-old",
		OccurredAt: t0.Add(80 * time.Second), Delta: mustDec(t, "-2"), // 仍按原事件发生时间的 v1 计价
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, tenant, t0, t1); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateDraft_PricingFreezeAndIdempotency(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	seedRatedPeriod(t, svc, "ta", t0, t1)
	publishFlat(t, svc, "ta", "m", t0, "1.00")
	publishFlat(t, svc, "ta", "m", t0.Add(50*time.Second), "2.00")

	// 周期未关闭不能出草稿（这里已关闭；另用开放周期单独验证）。
	draft, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Version != 1 || draft.Status != DraftStatusCurrent || draft.Currency != "USD" {
		t.Fatalf("bad draft header: %+v", draft)
	}
	// 5*1(v1) + 5*2(v2) + (-2)*1(修正按原事件发生时间 v1) = 5 + 10 - 2 = 13。
	if !draft.TotalAmount.Equal(mustDec(t, "13")) {
		t.Fatalf("total = %s, want 13", draft.TotalAmount)
	}
	if !draft.NormalAmount.Equal(mustDec(t, "13")) || !draft.AdjustmentAmount.IsZero() {
		t.Fatalf("amount split wrong: normal=%s adjustment=%s", draft.NormalAmount, draft.AdjustmentAmount)
	}
	// 明细逐行断言适用版本与金额。
	wantLines := map[string]struct {
		version int32
		amount  string
	}{
		"e-old": {1, "5.00"},
		"e-new": {2, "10.00"},
		"c1":    {1, "-2.00"},
	}
	if len(draft.Lines) != len(wantLines) {
		t.Fatalf("want %d lines, got %d: %+v", len(wantLines), len(draft.Lines), draft.Lines)
	}
	for _, l := range draft.Lines {
		w, ok := wantLines[l.RefID]
		if !ok {
			t.Fatalf("unexpected line %+v", l)
		}
		if l.RateVersion != w.version || !l.Amount.Equal(mustDec(t, w.amount)) {
			t.Fatalf("line %s = (v%d,%s), want (v%d,%s)", l.RefID, l.RateVersion, l.Amount, w.version, w.amount)
		}
	}

	// 草稿冻结：补发一个覆盖全部历史时间的更早日费率，再重复生成，必须原样返回。
	publishFlat(t, svc, "ta", "m", mkTime(0).Add(-365*24*time.Hour), "9.99")
	again, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != draft.Version || !again.TotalAmount.Equal(draft.TotalAmount) {
		t.Fatalf("frozen draft changed after a new rate publication: %+v", again)
	}
	for i := range again.Lines {
		if !again.Lines[i].Amount.Equal(draft.Lines[i].Amount) ||
			again.Lines[i].RateVersion != draft.Lines[i].RateVersion {
			t.Fatalf("line %d changed on frozen draft", i)
		}
	}
	got, err := svc.GetDraft(ctx, "ta", t0)
	if err != nil || got.Version != 1 {
		t.Fatalf("GetDraft: %v %+v", err, got)
	}
}

func TestGenerateDraft_RequiresClosedPeriod(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0 := mkTime(0)
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateDraft(ctx, "ta", t0); !IsCode(err, CodePeriodOpen) {
		t.Fatalf("want CodePeriodOpen for open period, got %v", err)
	}
	if _, err := svc.GetDraft(ctx, "ta", t0); !IsCode(err, CodePeriodOpen) {
		t.Fatalf("GetDraft on open period: want CodePeriodOpen, got %v", err)
	}
}

func TestGenerateDraft_NoApplicableRateRollsBack(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	seedRatedPeriod(t, svc, "ta", t0, t1)
	// 只为 t=50 之后发布费率：e-old 与修正（发生时间 t=10）无适用费率。
	publishFlat(t, svc, "ta", "m", t0.Add(50*time.Second), "2.00")

	if _, err := svc.GenerateDraft(ctx, "ta", t0); !IsCode(err, CodeNoApplicableRate) {
		t.Fatalf("want CodeNoApplicableRate, got %v", err)
	}
	// 整笔回滚：不存在任何草稿版本。
	if versions, err := svc.ListDraftVersions(ctx, "ta", t0); err != nil || len(versions) != 0 {
		t.Fatalf("failed generation must not leave partial draft: versions=%v err=%v", versions, err)
	}
	if _, err := svc.GetDraft(ctx, "ta", t0); !IsCode(err, CodeNotFound) {
		t.Fatalf("want no current draft after failed generation, got %v", err)
	}
}

func TestGenerateDraft_TotalEqualsSumOfLines(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	// 分段费率制造非整齐金额，验证精确求和。
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	for i, q := range []string{"0.1", "0.2", "3.333"} {
		if _, err := svc.ReportEvent(ctx, EventInput{
			EventID: fmt.Sprintf("e%d", i), Tenant: "ta", Meter: "m",
			OccurredAt: t0.Add(time.Duration(i+1) * time.Second), Quantity: mustDec(t, q),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t0, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishRate(ctx, PublishRateInput{
		Tenant: "ta", Meter: "m", EffectiveFrom: t0,
		Tiers:    tiered("1", "0.1", "0", "0.2"),
		Currency: "USD", QuantityScale: 3, AmountScale: 2,
	}); err != nil {
		t.Fatal(err)
	}
	draft, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	sum := decimal.Zero
	for _, l := range draft.Lines {
		sum = sum.Add(l.Amount)
	}
	if !draft.TotalAmount.Equal(sum) {
		t.Fatalf("total %s != sum of line amounts %s", draft.TotalAmount, sum)
	}
	// 重新加载后仍然相等（落库文本无精度损失）。
	reloaded, err := svc.GetDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	sum2 := decimal.Zero
	for _, l := range reloaded.Lines {
		sum2 = sum2.Add(l.Amount)
	}
	if !reloaded.TotalAmount.Equal(sum2) || !reloaded.TotalAmount.Equal(draft.TotalAmount) {
		t.Fatalf("persisted total/line invariant broken: total=%s sum=%s", reloaded.TotalAmount, sum2)
	}
}

// 并发重复生成：只能有一个 current 版本，所有调用返回同一版本。
func TestGenerateDraft_ConcurrentReturnsSameVersion(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	seedRatedPeriod(t, svc, "ta", t0, t1)
	publishFlat(t, svc, "ta", "m", t0, "1.00")

	const n = 24
	var wg sync.WaitGroup
	versions := make([]int32, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			d, err := svc.GenerateDraft(ctx, "ta", t0)
			if err == nil {
				versions[i] = d.Version
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent generate[%d]: %v", i, err)
		}
		if versions[i] != 1 {
			t.Fatalf("concurrent generate[%d] got version %d, want 1", i, versions[i])
		}
	}
	list, err := svc.ListDraftVersions(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != DraftStatusCurrent {
		t.Fatalf("exactly one current version expected: %+v", list)
	}
}

// ---------------------------------------------------------------------------
// 作废与重算：版本保留、当前唯一、乐观版本控制
// ---------------------------------------------------------------------------

func TestVoidAndRegenerate_KeepsHistory(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1, t2 := mkTime(0), mkTime(100), mkTime(200)
	seedRatedPeriod(t, svc, "ta", t0, t1)
	publishFlat(t, svc, "ta", "m", t0, "1.00")

	d1, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Version != 1 {
		t.Fatalf("first version = %d", d1.Version)
	}

	// 作废时校验版本号：传错版本号 -> 版本冲突，不作废。
	if _, err := svc.VoidDraft(ctx, "ta", t0, 99); !IsCode(err, CodeVersionConflict) {
		t.Fatalf("want CodeVersionConflict, got %v", err)
	}
	if _, err := svc.GetDraft(ctx, "ta", t0); err != nil {
		t.Fatalf("draft must remain current after failed void: %v", err)
	}

	voided, err := svc.VoidDraft(ctx, "ta", t0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if voided.Status != DraftStatusVoided || voided.VoidedAt.IsZero() {
		t.Fatalf("voided draft bad: %+v", voided)
	}
	// 当前草稿不存在（但历史版本可读）。
	if _, err := svc.GetDraft(ctx, "ta", t0); !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound with no current draft, got %v", err)
	}
	old, err := svc.GetDraftVersion(ctx, "ta", t0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != DraftStatusVoided || len(old.Lines) != len(d1.Lines) || !old.TotalAmount.Equal(d1.TotalAmount) {
		t.Fatalf("voided version and its lines must be retained: %+v", old)
	}
	// 重复作废已无当前版本 -> NotFound。
	if _, err := svc.VoidDraft(ctx, "ta", t0, 0); !IsCode(err, CodeNotFound) {
		t.Fatalf("void with no current draft: want CodeNotFound, got %v", err)
	}

	// 重新生成 -> 新版本；当前唯一；旧版本继续保留。发布新费率不影响旧版本，
	// 但新版本按生成时最新规则计价（这里费率未变，金额相同）。
	d2, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Version != 2 || d2.Status != DraftStatusCurrent {
		t.Fatalf("regenerated version bad: %+v", d2)
	}
	if !d2.TotalAmount.Equal(d1.TotalAmount) || len(d2.Lines) != len(d1.Lines) {
		t.Fatalf("v2 should recompute same data: %+v vs %+v", d2, d1)
	}
	versions, err := svc.ListDraftVersions(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[0].Status != DraftStatusVoided ||
		versions[1].Version != 2 || versions[1].Status != DraftStatusCurrent {
		t.Fatalf("version history wrong: %+v", versions)
	}
	if versions[0].VoidedAt.IsZero() || !versions[1].VoidedAt.IsZero() {
		t.Fatalf("voided timestamps wrong: %+v", versions)
	}

	// 作废未知周期。
	if _, err := svc.VoidDraft(ctx, "ta", t2, 0); !IsCode(err, CodeNotFound) {
		t.Fatalf("void unknown period: want CodeNotFound, got %v", err)
	}
}

// 并发作废+重算：任何时刻至多一个 current，版本号严格递增，每个版本明细完整。
func TestVoidAndRegenerate_ConcurrentSingleCurrent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	seedRatedPeriod(t, svc, "ta", t0, t1)
	publishFlat(t, svc, "ta", "m", t0, "1.00")
	if _, err := svc.GenerateDraft(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}

	const roundsN = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < roundsN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_, _ = svc.VoidDraft(ctx, "ta", t0, 0)
			} else {
				_, _ = svc.GenerateDraft(ctx, "ta", t0)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	versions, err := svc.ListDraftVersions(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	currents := 0
	prev := int32(0)
	for _, v := range versions {
		if v.Version <= prev {
			t.Fatalf("versions must be strictly increasing: %+v", versions)
		}
		prev = v.Version
		if v.Status == DraftStatusCurrent {
			currents++
		}
	}
	if currents > 1 {
		t.Fatalf("at most one current version allowed, got %d: %+v", currents, versions)
	}
	// 每个保留版本的明细金额之和等于其头部总额。
	for _, v := range versions {
		full, err := svc.GetDraftVersion(ctx, "ta", t0, v.Version)
		if err != nil {
			t.Fatal(err)
		}
		sum := decimal.Zero
		for _, l := range full.Lines {
			sum = sum.Add(l.Amount)
		}
		if !sum.Equal(full.TotalAmount) {
			t.Fatalf("version %d total %s != lines sum %s", v.Version, full.TotalAmount, sum)
		}
	}
}

// ---------------------------------------------------------------------------
// 迟到调整进入下一周期草稿：单列、引用原事件、按原事件发生时间费率计价
// ---------------------------------------------------------------------------

func TestDraft_LateAdjustmentRatedAtOriginalEventTimeAndSeparated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200), mkTime(300)

	// P1：事件 e1 发生在 t=10；费率 v1@t0=1，v2@t50=2。
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: t0.Add(10 * time.Second), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	publishFlat(t, svc, "ta", "m", t0, "1.00")
	publishFlat(t, svc, "ta", "m", t0.Add(50*time.Second), "2.00")
	if _, err := svc.ClosePeriod(ctx, "ta", t0, t1); err != nil {
		t.Fatal(err)
	}
	d1, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !d1.TotalAmount.Equal(mustDec(t, "10.00")) {
		t.Fatalf("P1 draft = %s, want 10.00", d1.TotalAmount)
	}

	// P2：到达迟到事件（发生 t=20）与迟到修正（引用 e1，发生 t=30），
	// 以及一条 P2 正常事件（发生 t=150，适用 v2 单价 2）。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late-e", Tenant: "ta", Meter: "m", OccurredAt: t0.Add(20 * time.Second), Quantity: mustDec(t, "4"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "late-c", Tenant: "ta", EventID: "e1",
		OccurredAt: t0.Add(30 * time.Second), Delta: mustDec(t, "-3"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "normal-p2", Tenant: "ta", Meter: "m", OccurredAt: t1.Add(50 * time.Second), Quantity: mustDec(t, "4"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	d2, err := svc.GenerateDraft(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}

	// 迟到事件 4*v1(1) = 4；迟到修正 -3 按原事件 e1 发生时间 t=10 的 v1 计价 = -3；
	// 正常事件 4*v2(2) = 8。总额 9，其中调整项单列合计 1。
	if !d2.AdjustmentAmount.Equal(mustDec(t, "1.00")) {
		t.Fatalf("adjustment subtotal = %s, want 1.00", d2.AdjustmentAmount)
	}
	if !d2.NormalAmount.Equal(mustDec(t, "8.00")) {
		t.Fatalf("normal subtotal = %s, want 8.00", d2.NormalAmount)
	}
	if !d2.TotalAmount.Equal(mustDec(t, "9.00")) {
		t.Fatalf("P2 total = %s, want 9.00", d2.TotalAmount)
	}
	byRef := map[string]DraftLine{}
	for _, l := range d2.Lines {
		byRef[l.RefID] = l
	}
	lateEvent := byRef["late-e"]
	if !lateEvent.Adjustment || lateEvent.RateVersion != 1 || !lateEvent.Amount.Equal(mustDec(t, "4.00")) {
		t.Fatalf("late event line wrong: %+v", lateEvent)
	}
	if lateEvent.OriginPeriodStart != t0 {
		t.Fatalf("late event must reference original period %s, got %s", t0, lateEvent.OriginPeriodStart)
	}
	lateCorr := byRef["late-c"]
	if !lateCorr.Adjustment || lateCorr.EventID != "e1" || lateCorr.RateVersion != 1 ||
		!lateCorr.Amount.Equal(mustDec(t, "-3.00")) {
		t.Fatalf("late correction must be priced with the original event's rate v1: %+v", lateCorr)
	}
	if lateCorr.OriginPeriodStart != t0 {
		t.Fatalf("late correction must reference original period %s, got %s", t0, lateCorr.OriginPeriodStart)
	}
	normal := byRef["normal-p2"]
	if normal.Adjustment || normal.RateVersion != 2 || !normal.Amount.Equal(mustDec(t, "8.00")) {
		t.Fatalf("normal P2 line wrong: %+v", normal)
	}
	if !normal.OriginPeriodStart.IsZero() {
		t.Fatalf("normal line must carry no origin period, got %s", normal.OriginPeriodStart)
	}

	// 单列查询只返回调整项。
	adjOnly, err := svc.ListDraftLines(ctx, "ta", t1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjOnly) != 2 {
		t.Fatalf("want 2 adjustment-only lines, got %d: %+v", len(adjOnly), adjOnly)
	}
	all, err := svc.ListDraftLines(ctx, "ta", t1, false)
	if err != nil || len(all) != len(d2.Lines) {
		t.Fatalf("ListDraftLines all: %v %d", err, len(all))
	}

	// 旧周期草稿不因迟到数据改写。
	d1Again, err := svc.GetDraftVersion(ctx, "ta", t0, d1.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !d1Again.TotalAmount.Equal(mustDec(t, "10.00")) || len(d1Again.Lines) != 1 {
		t.Fatalf("P1 draft mutated by late data: %+v", d1Again)
	}

	// 关闭 P3 前不能出 P3 草稿（周期状态区分）。
	if _, err := svc.GenerateDraft(ctx, "ta", t2); !IsCode(err, CodePeriodOpen) {
		t.Fatalf("P3 open: want CodePeriodOpen, got %v", err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t2, t3); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 数量修正与草稿生成并发：每笔调整只归入一个草稿版本
// ---------------------------------------------------------------------------

func TestConcurrentCorrectionAndDraft_EachAdjustmentInOneVersion(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	t2 := t1.Add(365 * 24 * time.Hour)

	// P1 关闭并先出草稿（冻结）；锚点事件在 P1。
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "anchor", Tenant: "ta", Meter: "m",
		OccurredAt: t0.Add(5 * time.Second), Quantity: mustDec(t, "100"),
	}); err != nil {
		t.Fatal(err)
	}
	publishFlat(t, svc, "ta", "m", t0, "1.00")
	if _, err := svc.ClosePeriod(ctx, "ta", t0, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateDraft(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}

	// 第一阶段（确定性拆分）：
	//   先并发提交一半迟到修正（顺延到开放的 P2），随后关闭 P2；
	//   再并发提交另一半迟到修正（P2 已关闭，顺延到 P3）。
	const corrN = 24
	const half = corrN / 2
	var wg sync.WaitGroup
	submit := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _ = svc.ApplyCorrection(ctx, CorrectionInput{
					CorrectionID: fmt.Sprintf("lc-%d", i), Tenant: "ta", EventID: "anchor",
					OccurredAt: t0.Add(10 * time.Second), Delta: mustDec(t, "1"), // 正增量，避开下限
				})
			}(i)
		}
	}
	submit(0, half)
	wg.Wait()
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	submit(half, corrN)
	wg.Wait()

	// 关账边界决定的 P2/P3 归属集合（来自不可变快照口径）。
	p2Refs := map[string]bool{}
	rows, err := svc.db.QueryContext(ctx,
		`SELECT correction_id FROM corrections WHERE tenant='ta' AND period_start_unix=?`, t1.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		p2Refs[id] = true
	}
	rows.Close()
	var p3Count int
	if err := svc.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corrections WHERE tenant='ta' AND period_start_unix=?`, t2.UnixNano()).
		Scan(&p3Count); err != nil {
		t.Fatal(err)
	}
	if len(p2Refs)+p3Count != corrN {
		t.Fatalf("correction partition broken: P2=%d P3=%d total want %d",
			len(p2Refs), p3Count, corrN)
	}
	if len(p2Refs) == 0 || p3Count == 0 {
		t.Fatalf("test timing produced no split: P2=%d P3=%d", len(p2Refs), p3Count)
	}

	// 第二阶段：并发作废/重算 P2 草稿。
	const genN = 20
	for i := 0; i < genN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = svc.VoidDraft(ctx, "ta", t1, 0)
			} else {
				_, _ = svc.GenerateDraft(ctx, "ta", t1)
			}
		}(i)
	}
	wg.Wait()
	_, _ = svc.VoidDraft(ctx, "ta", t1, 0)
	final, err := svc.GenerateDraft(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}

	// 最终草稿恰好覆盖归属 P2 的修正；归入 P3 的修正绝不渗入。
	if len(final.Lines) != len(p2Refs) {
		t.Fatalf("final draft lines=%d, want P2 corrections=%d", len(final.Lines), len(p2Refs))
	}
	finalRefs := map[string]bool{}
	for _, l := range final.Lines {
		finalRefs[l.RefID] = true
		if !p2Refs[l.RefID] {
			t.Fatalf("line %s belongs to P3 but leaked into P2 draft", l.RefID)
		}
		if !l.Adjustment || l.RateVersion != 1 || !l.Amount.Equal(mustDec(t, "1.00")) {
			t.Fatalf("late correction must use original event's rate v1: %+v", l)
		}
	}
	for ref := range p2Refs {
		if !finalRefs[ref] {
			t.Fatalf("P2 correction %s missing from final draft", ref)
		}
	}

	// 每个保留版本内：同一修正至多出现一次（明细唯一约束 + 重算全量复制），
	// 版本总额等于明细之和，且只包含归属 P2 的修正。
	versions, err := svc.ListDraftVersions(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatalf("expected draft versions")
	}
	for _, v := range versions {
		full, err := svc.GetDraftVersion(ctx, "ta", t1, v.Version)
		if err != nil {
			t.Fatal(err)
		}
		inVersion := map[string]bool{}
		sum := decimal.Zero
		for _, l := range full.Lines {
			if inVersion[l.RefID] {
				t.Fatalf("correction %s duplicated within draft version %d", l.RefID, v.Version)
			}
			inVersion[l.RefID] = true
			if !p2Refs[l.RefID] {
				t.Fatalf("version %d contains non-P2 correction %s", v.Version, l.RefID)
			}
			sum = sum.Add(l.Amount)
		}
		if !sum.Equal(full.TotalAmount) {
			t.Fatalf("version %d total %s != lines sum %s", v.Version, full.TotalAmount, sum)
		}
	}
}

// ---------------------------------------------------------------------------
// 币种不一致
// ---------------------------------------------------------------------------

func TestGenerateDraft_CurrencyMismatch(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m1",
		OccurredAt: t0.Add(1 * time.Second), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e2", Tenant: "ta", Meter: "m2",
		OccurredAt: t0.Add(2 * time.Second), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t0, t1); err != nil {
		t.Fatal(err)
	}
	publishFlat(t, svc, "ta", "m1", t0, "1.00")
	if _, err := svc.PublishRate(ctx, PublishRateInput{
		Tenant: "ta", Meter: "m2", EffectiveFrom: t0,
		Tiers: flatTiers("1"), Currency: "EUR", QuantityScale: 2, AmountScale: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateDraft(ctx, "ta", t0); !IsCode(err, CodeConflict) {
		t.Fatalf("mixed currencies in one draft: want CodeConflict, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 持久化：费率、草稿版本与明细重开后完整保留
// ---------------------------------------------------------------------------

func TestRatedPersistence_AcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "meter.db")
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)

	func() {
		svc, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = svc.Close() }()
		seedRatedPeriod(t, svc, "ta", t0, t1)
		publishFlat(t, svc, "ta", "m", t0, "1.00")
		d, err := svc.GenerateDraft(ctx, "ta", t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.VoidDraft(ctx, "ta", t0, d.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.GenerateDraft(ctx, "ta", t0); err != nil {
			t.Fatal(err)
		}
	}()

	svc2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc2.Close() }()

	r, err := svc2.GetRateVersion(ctx, "ta", "m", 1)
	if err != nil || !r.Tiers[0].UnitPrice.Equal(mustDec(t, "1.00")) {
		t.Fatalf("rate lost after reopen: %v %+v", err, r)
	}
	versions, err := svc2.ListDraftVersions(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Status != DraftStatusVoided || versions[1].Status != DraftStatusCurrent {
		t.Fatalf("draft version history lost: %+v", versions)
	}
	d, err := svc2.GetDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 2 || !d.TotalAmount.Equal(mustDec(t, "8")) || len(d.Lines) != 3 {
		t.Fatalf("current draft after reopen wrong: %+v", d)
	}
	old, err := svc2.GetDraftVersion(ctx, "ta", t0, 1)
	if err != nil || old.Status != DraftStatusVoided || len(old.Lines) != 3 {
		t.Fatalf("voided version lines lost after reopen: %v %+v", err, old)
	}
}

// 空快照周期也能出草稿（无数据、无费率要求），重复生成仍幂等。
func TestGenerateDraft_EmptyPeriod(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t0, t1 := mkTime(0), mkTime(100)
	if _, err := svc.EnsurePeriod(ctx, "ta", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t0, t1); err != nil {
		t.Fatal(err)
	}
	d, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || !d.TotalAmount.IsZero() || len(d.Lines) != 0 {
		t.Fatalf("empty draft bad: %+v", d)
	}
	again, err := svc.GenerateDraft(ctx, "ta", t0)
	if err != nil || again.Version != 1 {
		t.Fatalf("empty draft idempotent: %v %+v", err, again)
	}
}

// 错误可被 errors.As 取出且带可读信息（新增错误码冒烟）。
func TestRatedErrors_AreStructured(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GenerateDraft(context.Background(), "ta", mkTime(0))
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeNotFound || se.Message == "" {
		t.Fatalf("want structured CodeNotFound, got %v", err)
	}
}
