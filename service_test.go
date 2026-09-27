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

// mustDec 解析 decimal，失败即终止测试。
func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return d
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	// 每个用例独立内存库；连接池限制为单连接，库随该连接存活。
	svc, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func mkTime(sec int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// 事件上报：幂等与冲突
// ---------------------------------------------------------------------------

func TestReportEvent_IdempotentSameContent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := EventInput{
		EventID: "evt-1", Tenant: "tenant-a", Meter: "storage_gb",
		OccurredAt: mkTime(10), Quantity: mustDec(t, "12.5"),
	}
	first, err := svc.ReportEvent(ctx, in)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	if first.CommitSeq <= 0 {
		t.Fatalf("commit seq should be positive, got %d", first.CommitSeq)
	}

	second, err := svc.ReportEvent(ctx, in)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if second.CommitSeq != first.CommitSeq {
		t.Fatalf("replay must return original commit seq: first=%d second=%d", first.CommitSeq, second.CommitSeq)
	}
	if !second.CommittedAt.Equal(first.CommittedAt) {
		t.Fatalf("replay must return original committed_at")
	}
}

func TestReportEvent_ConflictOnDifferentContent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := EventInput{
		EventID: "evt-1", Tenant: "tenant-a", Meter: "storage_gb",
		OccurredAt: mkTime(10), Quantity: mustDec(t, "1"),
	}
	if _, err := svc.ReportEvent(ctx, base); err != nil {
		t.Fatalf("first report: %v", err)
	}

	cases := map[string]EventInput{
		"meter 不同": {EventID: "evt-1", Tenant: "tenant-a", Meter: "network_gb", OccurredAt: mkTime(10), Quantity: mustDec(t, "1")},
		"发生时间不同":   {EventID: "evt-1", Tenant: "tenant-a", Meter: "storage_gb", OccurredAt: mkTime(11), Quantity: mustDec(t, "1")},
		"数量不同":     {EventID: "evt-1", Tenant: "tenant-a", Meter: "storage_gb", OccurredAt: mkTime(10), Quantity: mustDec(t, "2")},
	}
	for name, changed := range cases {
		_, err := svc.ReportEvent(ctx, changed)
		if !IsCode(err, CodeConflict) {
			t.Fatalf("%s: want CodeConflict, got %v", name, err)
		}
	}

	// 数值相等的不同写法（"1.0" vs "1"）视为相同内容，幂等返回原结果。
	equiv, err := svc.ReportEvent(ctx, EventInput{
		EventID: "evt-1", Tenant: "tenant-a", Meter: "storage_gb",
		OccurredAt: mkTime(10), Quantity: mustDec(t, "1.0"),
	})
	if err != nil {
		t.Fatalf("numerically equivalent quantity should replay idempotently: %v", err)
	}
	if !equiv.Quantity.Equal(mustDec(t, "1")) {
		t.Fatalf("replayed quantity = %s", equiv.Quantity)
	}

	// 冲突不得改写首次内容：原事件仍可按原内容幂等取回。
	got, err := svc.ReportEvent(ctx, base)
	if err != nil {
		t.Fatalf("original content replay: %v", err)
	}
	if got.Meter != "storage_gb" || !got.Quantity.Equal(mustDec(t, "1")) {
		t.Fatalf("original event content was mutated: %+v", got)
	}
}

func TestReportEvent_Validation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	valid := EventInput{
		EventID: "e", Tenant: "t", Meter: "m",
		OccurredAt: mkTime(1), Quantity: mustDec(t, "1"),
	}
	cases := map[string]EventInput{
		"缺事件号":  func() EventInput { x := valid; x.EventID = ""; return x }(),
		"缺租户":   func() EventInput { x := valid; x.Tenant = ""; return x }(),
		"缺计量项":  func() EventInput { x := valid; x.Meter = ""; return x }(),
		"缺发生时间": func() EventInput { x := valid; x.OccurredAt = time.Time{}; return x }(),
		"数量为负":  func() EventInput { x := valid; x.Quantity = mustDec(t, "-0.01"); return x }(),
	}
	for name, in := range cases {
		if _, err := svc.ReportEvent(ctx, in); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("%s: want CodeInvalidArgument, got %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 修正：增量、幂等、冲突、下限、原事件引用
// ---------------------------------------------------------------------------

func TestCorrection_IncrementalAndFloor(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "evt-1", Tenant: "ta", Meter: "api_calls",
		OccurredAt: mkTime(5), Quantity: mustDec(t, "100"),
	}); err != nil {
		t.Fatal(err)
	}

	// 独立修正号 -> 多次增量修正累计：100 - 30 - 80 = -10，低于下限 0，应拒绝。
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-1", Tenant: "ta", EventID: "evt-1",
		OccurredAt: mkTime(6), Delta: mustDec(t, "-30"),
	}); err != nil {
		t.Fatalf("corr-1: %v", err)
	}
	_, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-2", Tenant: "ta", EventID: "evt-1",
		OccurredAt: mkTime(7), Delta: mustDec(t, "-80"),
	})
	if !IsCode(err, CodeBelowFloor) {
		t.Fatalf("want CodeBelowFloor, got %v", err)
	}
	var se *Error
	if errors.As(err, &se) && se.Message == "" {
		t.Fatalf("below-floor error should carry a descriptive message")
	}

	// 被拒修正不得落库：修正号不存在。
	if _, err := svc.GetCorrection(ctx, "ta", "corr-2"); !IsCode(err, CodeNotFound) {
		t.Fatalf("rejected correction must not be persisted, got %v", err)
	}

	// 改用 -70：累计恰好 0，允许。
	c2, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-2", Tenant: "ta", EventID: "evt-1",
		OccurredAt: mkTime(7), Delta: mustDec(t, "-70"),
	})
	if err != nil {
		t.Fatalf("corr-2 at floor 0: %v", err)
	}
	if c2.Meter != "api_calls" {
		t.Fatalf("correction should inherit meter from original event, got %q", c2.Meter)
	}
	if !c2.Floor.Equal(decimal.Zero) {
		t.Fatalf("default floor should be zero, got %s", c2.Floor)
	}
}

func TestCorrection_IdempotentAndConflict(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "evt-1", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(5), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	in := CorrectionInput{
		CorrectionID: "corr-1", Tenant: "ta", EventID: "evt-1",
		OccurredAt: mkTime(6), Delta: mustDec(t, "2.5"),
	}
	first, err := svc.ApplyCorrection(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.ApplyCorrection(ctx, in)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if again.CommitSeq != first.CommitSeq {
		t.Fatalf("replay commit seq mismatch: %d != %d", again.CommitSeq, first.CommitSeq)
	}

	// 同修正号、不同增量 -> 冲突。
	_, err = svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-1", Tenant: "ta", EventID: "evt-1",
		OccurredAt: mkTime(6), Delta: mustDec(t, "3.5"),
	})
	if !IsCode(err, CodeConflict) {
		t.Fatalf("want CodeConflict, got %v", err)
	}

	// 修正必须引用存在的原事件。
	_, err = svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-x", Tenant: "ta", EventID: "ghost",
		OccurredAt: mkTime(6), Delta: mustDec(t, "1"),
	})
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound for missing original event, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 周期关闭、快照、重复/并发关闭
// ---------------------------------------------------------------------------

func TestClosePeriod_BasicSnapshot(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)

	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	report := func(id string, at time.Time, q string) {
		t.Helper()
		if _, err := svc.ReportEvent(ctx, EventInput{
			EventID: id, Tenant: "ta", Meter: "m", OccurredAt: at, Quantity: mustDec(t, q),
		}); err != nil {
			t.Fatal(err)
		}
	}
	report("e1", mkTime(10), "0.1")
	report("e2", mkTime(20), "0.2")
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(30), Delta: mustDec(t, "0.3"),
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := svc.ClosePeriod(ctx, "ta", t1, t2)
	if err != nil {
		t.Fatal(err)
	}
	// 精确计算：0.1 + 0.2 + 0.3 = 0.6，不允许二进制浮点误差。
	if got := snap.Totals["m"]; !got.Equal(mustDec(t, "0.6")) {
		t.Fatalf("snapshot total = %s, want 0.6 (events=%v corrections=%v)",
			got, snap.Events, snap.Corrections)
	}
	if !snap.Events["m"].Equal(mustDec(t, "0.3")) {
		t.Fatalf("events sum = %s, want 0.3", snap.Events["m"])
	}
	if !snap.Corrections["m"].Equal(mustDec(t, "0.3")) {
		t.Fatalf("corrections sum = %s, want 0.3", snap.Corrections["m"])
	}
	if snap.PeriodEnd != t2 || snap.PeriodStart != t1 {
		t.Fatalf("snapshot period bounds wrong: %+v", snap)
	}

	// 未关闭周期没有快照。
	if _, err := svc.GetSnapshot(ctx, "ta", t2); !IsCode(err, CodeNotFound) {
		t.Fatalf("open period snapshot should be CodeNotFound, got %v", err)
	}
}

func TestClosePeriod_RepeatAndConcurrentReturnSameSnapshot(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := svc.ClosePeriod(ctx, "ta", t1, t2)
	if err != nil {
		t.Fatal(err)
	}

	// 重复关闭返回原快照。
	again, err := svc.ClosePeriod(ctx, "ta", t1, t2)
	if err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	if !snapshotEqual(first, again) {
		t.Fatalf("repeat close changed snapshot:\nfirst=%+v\nagain=%+v", first, again)
	}

	// 并发关闭也只能得到同一个结果。
	var wg sync.WaitGroup
	snaps := make([]*Snapshot, 16)
	errs := make([]error, 16)
	start := make(chan struct{})
	for i := range snaps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			snaps[i], errs[i] = svc.ClosePeriod(ctx, "ta", t1, t2)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent close[%d]: %v", i, e)
		}
		if !snapshotEqual(first, snaps[i]) {
			t.Fatalf("concurrent close[%d] produced a different snapshot", i)
		}
	}

	// 关闭后再向该周期起点 EnsurePeriod 返回同一已关闭周期（幂等）。
	p, err := svc.EnsurePeriod(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Closed {
		t.Fatalf("EnsurePeriod on closed start should return the closed period")
	}

	// 用不同的终点重复关闭：冲突，避免调用方误以为改了周期边界。
	if _, err := svc.ClosePeriod(ctx, "ta", t1, mkTime(150)); !IsCode(err, CodeConflict) {
		t.Fatalf("repeat close with different end should be CodeConflict, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 迟到事件/修正 -> 下一周期调整项，旧快照不可变
// ---------------------------------------------------------------------------

func TestLateEvent_BecomesNextPeriodAdjustment(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200)

	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "5"),
	}); err != nil {
		t.Fatal(err)
	}
	snap1, err := svc.ClosePeriod(ctx, "ta", t1, t2)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap1.Totals["m"]; !got.Equal(mustDec(t, "5")) {
		t.Fatalf("snapshot1 total = %s, want 5", got)
	}

	// 关闭之后到达、发生时间仍在 P1 内的迟到事件。
	late, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late-1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(20), Quantity: mustDec(t, "7"),
	})
	if err != nil {
		t.Fatalf("late event: %v", err)
	}
	if late.CommitSeq <= snap1.CloseBoundarySeq {
		t.Fatalf("late event commit seq %d should be past boundary %d", late.CommitSeq, snap1.CloseBoundarySeq)
	}

	// 旧快照不可变。
	snap1Again, err := svc.GetSnapshot(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotEqual(snap1, snap1Again) {
		t.Fatalf("closed snapshot was mutated by a late event: before=%+v after=%+v", snap1, snap1Again)
	}

	// 调整项落在下一周期并保留来源关联。
	adjs, err := svc.ListAdjustments(ctx, "ta", t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("want 1 adjustment in P2, got %d: %+v", len(adjs), adjs)
	}
	a := adjs[0]
	if a.Kind != KindEvent || a.RefID != "late-1" || a.EventID != "late-1" {
		t.Fatalf("adjustment identity wrong: %+v", a)
	}
	if a.OriginPeriodStart != t1 {
		t.Fatalf("origin period should be P1 %s, got %s", t1, a.OriginPeriodStart)
	}
	if !a.Quantity.Equal(mustDec(t, "7")) {
		t.Fatalf("adjustment quantity = %s", a.Quantity)
	}

	// 下一周期关闭时调整项计入其快照，且与正常数据分列。
	snap2, err := svc.ClosePeriod(ctx, "ta", t2, t3)
	if err != nil {
		t.Fatal(err)
	}
	if !snap2.AdjustmentEvents["m"].Equal(mustDec(t, "7")) {
		t.Fatalf("P2 adjustment events = %v, want 7", snap2.AdjustmentEvents)
	}
	if _, ok := snap2.Events["m"]; ok {
		t.Fatalf("P2 should have no normal events, got %v", snap2.Events)
	}
	if !snap2.Totals["m"].Equal(mustDec(t, "7")) {
		t.Fatalf("P2 total = %s, want 7", snap2.Totals["m"])
	}
}

func TestLateCorrection_BecomesAdjustmentAndFloorStillEnforced(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "5"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}

	// 迟到修正引用旧周期原事件、发生时间在旧周期内：顺延到 P2 调整项。
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "late-c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(15), Delta: mustDec(t, "-2"),
	}); err != nil {
		t.Fatalf("late correction: %v", err)
	}

	// 全局累计下限仍然生效（5 - 2 - 4 < 0），即使修正落在新周期。
	_, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "late-c2", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(16), Delta: mustDec(t, "-4"),
	})
	if !IsCode(err, CodeBelowFloor) {
		t.Fatalf("want CodeBelowFloor across periods, got %v", err)
	}

	adjs, err := svc.ListAdjustments(ctx, "ta", t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 || adjs[0].Kind != KindCorrection || adjs[0].RefID != "late-c1" {
		t.Fatalf("want exactly the late correction adjustment, got %+v", adjs)
	}
	if adjs[0].EventID != "e1" || adjs[0].OriginPeriodStart != t1 {
		t.Fatalf("late correction source link wrong: %+v", adjs[0])
	}
	if !adjs[0].Quantity.Equal(mustDec(t, "-2")) {
		t.Fatalf("late correction delta = %s", adjs[0].Quantity)
	}

	snap2, err := svc.ClosePeriod(ctx, "ta", t2, t3)
	if err != nil {
		t.Fatal(err)
	}
	if !snap2.AdjustmentCorrections["m"].Equal(mustDec(t, "-2")) {
		t.Fatalf("P2 adjustment corrections = %v, want -2", snap2.AdjustmentCorrections)
	}
	if !snap2.Totals["m"].Equal(mustDec(t, "-2")) {
		t.Fatalf("P2 total = %s, want -2", snap2.Totals["m"])
	}

	// 旧快照保持为原始 5。
	snap1, err := svc.GetSnapshot(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	if !snap1.Totals["m"].Equal(mustDec(t, "5")) {
		t.Fatalf("P1 snapshot changed: %s", snap1.Totals["m"])
	}
}

func TestFutureEvent_MovedToNextPeriodAtClose(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "in-1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(10), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	// 开放周期右端未定时，提前上报发生时间在“未来”的事件。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "future-1", Tenant: "ta", Meter: "m", OccurredAt: mkTime(150), Quantity: mustDec(t, "9"),
	}); err != nil {
		t.Fatal(err)
	}

	snap1, err := svc.ClosePeriod(ctx, "ta", t1, t2)
	if err != nil {
		t.Fatal(err)
	}
	if !snap1.Totals["m"].Equal(mustDec(t, "1")) {
		t.Fatalf("P1 must not contain the future event, total=%s", snap1.Totals["m"])
	}
	if adjs, _ := svc.ListAdjustments(ctx, "ta", t2); len(adjs) != 0 {
		t.Fatalf("future event is normal data, must not be flagged as adjustment: %+v", adjs)
	}
	snap2, err := svc.ClosePeriod(ctx, "ta", t2, t3)
	if err != nil {
		t.Fatal(err)
	}
	if !snap2.Events["m"].Equal(mustDec(t, "9")) {
		t.Fatalf("future event should land normally in P2 events, got %v", snap2.Events)
	}
}

// ---------------------------------------------------------------------------
// 上报与关闭并发：不重不漏、归属唯一
// ---------------------------------------------------------------------------

func TestConcurrentReportAndClose_PartitionIsComplete(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	t3 := t2.Add(365 * 24 * time.Hour)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	// 供并发修正引用的锚点事件。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "anchor", Tenant: "ta", Meter: "m", OccurredAt: mkTime(5), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}

	const eventN = 40
	const corrN = 16 // 需能被 workers 整除，保证每个 goroutine 提交数量一致
	const workers = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	report := func(worker int) {
		defer wg.Done()
		<-start
		for i := 0; i < eventN/workers; i++ {
			id := fmt.Sprintf("e-w%d-%d", worker, i)
			at := mkTime((i*7 + worker) % 90) // 全部发生在 [t1,t2)
			_, _ = svc.ReportEvent(ctx, EventInput{
				EventID: id, Tenant: "ta", Meter: "m",
				OccurredAt: at, Quantity: mustDec(t, "1"),
			})
		}
	}
	correct := func(worker int) {
		defer wg.Done()
		<-start
		for i := 0; i < corrN/workers; i++ {
			id := fmt.Sprintf("c-w%d-%d", worker, i)
			_, _ = svc.ApplyCorrection(ctx, CorrectionInput{
				CorrectionID: id, Tenant: "ta", EventID: "anchor",
				OccurredAt: mkTime((i*11 + worker) % 90),
				Delta:      mustDec(t, "0.5"), // 全为正增量，避免触发下限
			})
		}
	}
	closer := func() {
		defer wg.Done()
		<-start
		time.Sleep(2 * time.Millisecond) // 让部分上报先于关账提交
		_, _ = svc.ClosePeriod(ctx, "ta", t1, t2)
	}

	for w := 0; w < workers; w++ {
		wg.Add(2)
		go report(w)
		go correct(w)
	}
	wg.Add(1)
	go closer()
	close(start)
	wg.Wait()

	// 关闭 P2：所有剩余数据（含 P1 的迟到调整项）都应落入 P1/P2 两个快照之一。
	if _, err := svc.ClosePeriod(ctx, "ta", t2, t3); err != nil {
		t.Fatal(err)
	}
	snap1, err := svc.GetSnapshot(ctx, "ta", t1)
	if err != nil {
		t.Fatal(err)
	}
	snap2, err := svc.GetSnapshot(ctx, "ta", t2)
	if err != nil {
		t.Fatal(err)
	}

	// 期望总量 = 锚点 10 + 40 个事件各 1 + 20 条修正各 0.5 = 60。
	want := mustDec(t, "10").
		Add(mustDec(t, fmt.Sprintf("%d", eventN))).
		Add(mustDec(t, "0.5").Mul(mustDec(t, fmt.Sprintf("%d", corrN))))
	got := snap1.Totals["m"].Add(snap2.Totals["m"])
	if !got.Equal(want) {
		t.Fatalf("partition not balanced: got %s, want %s (P1=%s P2=%s)",
			got, want, snap1.Totals["m"], snap2.Totals["m"])
	}

	// 每条事件有且仅有一个归属：P1 正常事件 + P2 正常/调整事件合计恰为 eventN+锚点。
	countRows := func(sqlText string, args ...any) int {
		var n int
		row := svc.db.QueryRowContext(ctx, sqlText, args...)
		if err := row.Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	p1Normal := countRows(
		`SELECT COUNT(*) FROM events WHERE tenant='ta' AND period_start_unix=? AND is_adjustment=0`,
		t1.UnixNano())
	p2Any := countRows(
		`SELECT COUNT(*) FROM events WHERE tenant='ta' AND period_start_unix=?`, t2.UnixNano())
	if total := p1Normal + p2Any; total != eventN+1 {
		t.Fatalf("event assignment must be a partition: P1=%d P2=%d total=%d want=%d",
			p1Normal, p2Any, total, eventN+1)
	}
	c1 := countRows(
		`SELECT COUNT(*) FROM corrections WHERE tenant='ta' AND period_start_unix=?`, t1.UnixNano())
	c2 := countRows(
		`SELECT COUNT(*) FROM corrections WHERE tenant='ta' AND period_start_unix=?`, t2.UnixNano())
	if c1+c2 != corrN {
		t.Fatalf("correction assignment must be a partition: P1=%d P2=%d total=%d want=%d",
			c1, c2, c1+c2, corrN)
	}
}

// ---------------------------------------------------------------------------
// 周期管理与错误分类
// ---------------------------------------------------------------------------

func TestEnsurePeriod_Contiguity(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2, t3 := mkTime(0), mkTime(100), mkTime(200)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	// 已有开放周期时不能再建。
	if _, err := svc.EnsurePeriod(ctx, "ta", t2); !IsCode(err, CodeConflict) {
		t.Fatalf("want CodeConflict for second open period, got %v", err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	// 关账会自动建立从终点 t2 开始的下一开放周期，因此再建其它起点会冲突。
	if _, err := svc.EnsurePeriod(ctx, "ta", t3); !IsCode(err, CodeConflict) {
		t.Fatalf("want CodeConflict while next period is open, got %v", err)
	}
	// 显式确保同一周期：幂等返回自动建立的开放周期。
	p2, err := svc.EnsurePeriod(ctx, "ta", t2)
	if err != nil {
		t.Fatalf("contiguous period: %v", err)
	}
	if p2.Closed || !p2.Start.Equal(t2) {
		t.Fatalf("P2 wrong: %+v", p2)
	}

	// 上一周期已关闭且新起点不等于其终点：拒绝（白盒构造一条已关闭周期）。
	if _, err := svc.db.Exec(`
INSERT INTO periods(tenant, start_unix, end_unix, closed) VALUES('tb', ?, ?, 1)`,
		t1.UnixNano(), t2.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsurePeriod(ctx, "tb", t3); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("want CodeInvalidArgument for non-contiguous period, got %v", err)
	}
	if _, err := svc.EnsurePeriod(ctx, "tb", t2); err != nil {
		t.Fatalf("period starting exactly at previous end should be accepted: %v", err)
	}
}

func TestClosePeriod_Validation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.ClosePeriod(ctx, "ta", mkTime(100), mkTime(0)); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("end before start: want CodeInvalidArgument, got %v", err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", mkTime(0), mkTime(100)); !IsCode(err, CodeNotFound) {
		t.Fatalf("missing period: want CodeNotFound, got %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.GetEvent(context.Background(), "t", "missing"); !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
	var se *Error
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	_ = errors.As(fmt.Errorf("wrap: %w", newError(CodeConflict, "x")), &se)
	if se == nil || se.Code != CodeConflict {
		t.Fatalf("errors.As should expose *Error through wrapping")
	}
}

// ---------------------------------------------------------------------------
// 持久化：文件库关闭重开后数据仍在
// ---------------------------------------------------------------------------

func TestPersistence_AcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meter.db")
	dsn := "file:" + dbPath
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)

	func() {
		svc, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = svc.Close() }()
		if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ReportEvent(ctx, EventInput{
			EventID: "e1", Tenant: "ta", Meter: "m",
			OccurredAt: mkTime(10), Quantity: mustDec(t, "3.141592653589793238"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
			t.Fatal(err)
		}
	}()

	svc2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc2.Close() }()

	e, err := svc2.GetEvent(ctx, "ta", "e1")
	if err != nil {
		t.Fatalf("event lost after reopen: %v", err)
	}
	if !e.Quantity.Equal(mustDec(t, "3.141592653589793238")) {
		t.Fatalf("precision lost across persistence: %s", e.Quantity)
	}
	snap, err := svc2.GetSnapshot(ctx, "ta", t1)
	if err != nil {
		t.Fatalf("snapshot lost after reopen: %v", err)
	}
	if !snap.Totals["m"].Equal(mustDec(t, "3.141592653589793238")) {
		t.Fatalf("snapshot total after reopen = %s", snap.Totals["m"])
	}
	p, err := svc2.GetPeriod(ctx, "ta", t2)
	if err != nil {
		t.Fatalf("auto-created next period lost after reopen: %v", err)
	}
	if p.Closed {
		t.Fatalf("P2 should still be open after reopen")
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func snapshotEqual(a, b *Snapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Tenant != b.Tenant ||
		!a.PeriodStart.Equal(b.PeriodStart) ||
		!a.PeriodEnd.Equal(b.PeriodEnd) ||
		!a.ClosedAt.Equal(b.ClosedAt) ||
		a.CloseBoundarySeq != b.CloseBoundarySeq {
		return false
	}
	return decMapEqual(a.Totals, b.Totals) &&
		decMapEqual(a.Events, b.Events) &&
		decMapEqual(a.Corrections, b.Corrections) &&
		decMapEqual(a.AdjustmentEvents, b.AdjustmentEvents) &&
		decMapEqual(a.AdjustmentCorrections, b.AdjustmentCorrections)
}

func decMapEqual(a, b map[string]decimal.Decimal) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !va.Equal(vb) {
			return false
		}
	}
	return true
}
