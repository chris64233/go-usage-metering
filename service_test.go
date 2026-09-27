package usagemetering

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	store := NewFileStorage(t.TempDir() + "/state.json")
	sched, err := NewMonthlySchedule(1, 0, 0, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	return NewService(store, sched, WithClock(clk.now)), clk
}

func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func q(s string) Amount { return MustParseAmount(s) }

func eventInput(id string, at string, quantity string) EventInput {
	return EventInput{
		ExternalID: id,
		TenantID:   "tenant-a",
		MeterID:    "api-calls",
		OccurredAt: mustParseTime(at),
		Quantity:   q(quantity),
	}
}

func TestReportEventAndSnapshot(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	r1, err := svc.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "100"))
	if err != nil || r1.Replayed {
		t.Fatalf("report evt-1: %+v, %v", r1, err)
	}
	if r1.Record.PeriodID != "2026-09" {
		t.Fatalf("period = %q", r1.Record.PeriodID)
	}

	if _, err := svc.ReportEvent(ctx, eventInput("evt-2", "2026-09-20T08:00:00Z", "50.5")); err != nil {
		t.Fatal(err)
	}

	// 关闭 9 月周期（时钟拨到 10 月）。
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	res, err := svc.ClosePeriod(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed {
		t.Error("first close should not be replay")
	}
	if len(res.Snapshot.Lines) != 1 {
		t.Fatalf("lines = %+v", res.Snapshot.Lines)
	}
	line := res.Snapshot.Lines[0]
	if line.Quantity.String() != "150.5" || line.EventCount != 2 {
		t.Errorf("line = %+v", line)
	}
}

func TestReportEventIdempotentReplay(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	in := eventInput("evt-1", "2026-09-10T08:00:00Z", "100.00")

	first, err := svc.ReportEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// 数量文本不同但数值相同（100.00 vs 100），内容一致 -> 幂等重放。
	in.Quantity = q("100")
	second, err := svc.ReportEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed {
		t.Error("identical repeat should replay")
	}
	if !second.Record.Delta.Equal(first.Record.Delta) ||
		!second.Record.SubmittedAt.Equal(first.Record.SubmittedAt) {
		t.Error("replay should return the original result")
	}
}

func TestReportEventConflictOnChangedContent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	in := eventInput("evt-1", "2026-09-10T08:00:00Z", "100")
	if _, err := svc.ReportEvent(ctx, in); err != nil {
		t.Fatal(err)
	}

	changes := []func(EventInput) EventInput{
		func(e EventInput) EventInput { e.TenantID = "tenant-b"; return e },
		func(e EventInput) EventInput { e.MeterID = "other"; return e },
		func(e EventInput) EventInput { e.OccurredAt = e.OccurredAt.Add(time.Minute); return e },
		func(e EventInput) EventInput { e.Quantity = q("101"); return e },
	}
	for i, ch := range changes {
		if _, err := svc.ReportEvent(ctx, ch(in)); err == nil || !isKind(err, KindConflict) {
			t.Errorf("change %d: want conflict, got %v", i, err)
		}
	}
}

func TestReportEventValidation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	good := eventInput("evt-1", "2026-09-10T08:00:00Z", "100")

	bad := good
	bad.ExternalID = ""
	if _, err := svc.ReportEvent(ctx, bad); !isKind(err, KindValidation) {
		t.Errorf("empty id: %v", err)
	}
	bad = good
	bad.TenantID = ""
	if _, err := svc.ReportEvent(ctx, bad); !isKind(err, KindValidation) {
		t.Errorf("empty tenant: %v", err)
	}
	bad = good
	bad.Quantity = Amount{}
	if _, err := svc.ReportEvent(ctx, bad); !isKind(err, KindValidation) {
		t.Errorf("zero amount: %v", err)
	}
}

func TestClosePeriodValidationAndIdempotency(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	// 周期未结束不能关闭。
	if _, err := svc.ClosePeriod(ctx, "2026-09"); !isKind(err, KindValidation) {
		t.Errorf("close open period: %v", err)
	}
	if _, err := svc.ClosePeriod(ctx, "bad-id"); !isKind(err, KindValidation) {
		t.Errorf("bad period id: %v", err)
	}
	if _, err := svc.GetSnapshot(ctx, "2026-09"); !isKind(err, KindNotFound) {
		t.Errorf("snapshot before close: %v", err)
	}

	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	first, err := svc.ClosePeriod(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ClosePeriod(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed {
		t.Error("repeat close should replay")
	}
	if !snapshotsEqual(first.Snapshot, second.Snapshot) {
		t.Error("repeated close returned different snapshot")
	}

	got, err := svc.GetSnapshot(ctx, "2026-09")
	if err != nil || !snapshotsEqual(got, first.Snapshot) {
		t.Errorf("get snapshot = %+v, %v", got, err)
	}
}

func TestCorrectionsAreIncremental(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	if _, err := svc.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "100")); err != nil {
		t.Fatal(err)
	}

	// 第一次修正 -30。
	c1, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-1", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("-30"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c1.Record.Type != KindCorrection || c1.Record.OriginEventID != "evt-1" {
		t.Fatalf("correction record wrong: %+v", c1.Record)
	}

	// 第二次独立修正 -70，累计恰好 0，达到默认下限。
	if _, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-2", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-12T08:00:00Z"), Delta: q("-70"),
	}); err != nil {
		t.Fatal(err)
	}

	// 再 -0.01，累计 -0.01，低于下限 0。
	_, err = svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-3", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-13T08:00:00Z"), Delta: q("-0.01"),
	})
	if !isKind(err, KindBelowFloor) {
		t.Fatalf("want below_floor, got %v", err)
	}

	// 被拒修正未落库：corr-3 查不到。
	if _, err := svc.GetCorrection(ctx, "corr-3"); !isKind(err, KindNotFound) {
		t.Errorf("rejected correction persisted: %v", err)
	}

	// 修正幂等：相同内容重放。
	replay, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-1", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("-30"),
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("correction replay: %+v, %v", replay, err)
	}

	// 修正号内容变化 -> 冲突。
	_, err = svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-1", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("-31"),
	})
	if !isKind(err, KindConflict) {
		t.Fatalf("want conflict, got %v", err)
	}

	// 引用不存在的原事件 -> not found。
	_, err = svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-x", OriginID: "missing",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("1"),
	})
	if !isKind(err, KindNotFound) {
		t.Fatalf("want not_found, got %v", err)
	}

	// 快照：原始 100 - 30 - 70 = 0，三条记录。
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	res, err := svc.ClosePeriod(ctx, "2026-08") // 先关 8 月以保证顺序
	_ = res
	if err != nil {
		t.Fatal(err)
	}
	sep, err := svc.ClosePeriod(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := sep.Snapshot.Lines[0].Quantity.String(); got != "0" {
		t.Errorf("snapshot quantity = %s, want 0", got)
	}
	if sep.Snapshot.Lines[0].EventCount != 3 {
		t.Errorf("event count = %d, want 3", sep.Snapshot.Lines[0].EventCount)
	}
}

func TestLateEventBecomesAdjustment(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	// 9 月 10 日的事件在 9 月周期关闭后才上报。
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	if _, err := svc.ClosePeriod(ctx, "2026-09"); err != nil {
		t.Fatal(err)
	}

	r, err := svc.ReportEvent(ctx, eventInput("late-1", "2026-09-15T08:00:00Z", "10"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Record.PeriodID != "2026-10" || r.Record.SourcePeriodID != "2026-09" {
		t.Fatalf("late event attribution = %q source=%q", r.Record.PeriodID, r.Record.SourcePeriodID)
	}

	// 旧快照不可变：没有这条事件，也没有调整项。
	old, err := svc.GetSnapshot(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Lines) != 0 || len(old.Adjustments) != 0 {
		t.Fatalf("old snapshot mutated: %+v", old)
	}

	// 10 月快照把它计为数量并列为调整项。
	clk.t = mustParseTime("2026-11-02T00:00:00Z")
	oct, err := svc.ClosePeriod(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if oct.Snapshot.Lines[0].Quantity.String() != "10" {
		t.Errorf("Oct quantity = %s", oct.Snapshot.Lines[0].Quantity)
	}
	adjs, err := svc.ListAdjustments(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("adjustments = %+v", adjs)
	}
	a := adjs[0]
	if a.Kind != "adjustment_event" || a.RecordID != "late-1" ||
		a.OriginEventID != "late-1" || a.SourcePeriodID != "2026-09" ||
		a.Delta.String() != "10" {
		t.Errorf("adjustment wrong: %+v", a)
	}

	// 已关闭周期不能再有调整项查询以外的写影响——9 月快照依旧为空。
	old2, _ := svc.GetSnapshot(ctx, "2026-09")
	if len(old2.Lines) != 0 {
		t.Error("September snapshot changed after late event")
	}
}

func TestLateCorrectionBecomesAdjustment(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	// 9 月事件先上报，9 月关闭后才提交修正。
	if _, err := svc.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "100")); err != nil {
		t.Fatal(err)
	}
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	if _, err := svc.ClosePeriod(ctx, "2026-09"); err != nil {
		t.Fatal(err)
	}

	r, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-late", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-10-03T08:00:00Z"), Delta: q("-15"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Record.PeriodID != "2026-10" || r.Record.SourcePeriodID != "2026-09" {
		t.Fatalf("late correction attribution = %q source=%q", r.Record.PeriodID, r.Record.SourcePeriodID)
	}

	// 旧快照保持 100，不受修正影响。
	old, _ := svc.GetSnapshot(ctx, "2026-09")
	if old.Lines[0].Quantity.String() != "100" {
		t.Fatalf("old snapshot = %s", old.Lines[0].Quantity)
	}

	// 下限校验对迟到修正仍然生效：100-15=85，再来 -90 -> 低于 0。
	_, err = svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "corr-late-2", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-10-04T08:00:00Z"), Delta: q("-90"),
	})
	if !isKind(err, KindBelowFloor) {
		t.Fatalf("want below_floor, got %v", err)
	}

	// 10 月快照数量 -15，调整项保留来源事件关联。
	clk.t = mustParseTime("2026-11-02T00:00:00Z")
	oct, err := svc.ClosePeriod(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if oct.Snapshot.Lines[0].Quantity.String() != "-15" {
		t.Errorf("Oct quantity = %s", oct.Snapshot.Lines[0].Quantity)
	}
	if len(oct.Snapshot.Adjustments) != 1 {
		t.Fatalf("adjustments = %+v", oct.Snapshot.Adjustments)
	}
	a := oct.Snapshot.Adjustments[0]
	if a.Kind != "adjustment_correction" || a.RecordID != "corr-late" ||
		a.OriginEventID != "evt-1" || a.SourcePeriodID != "2026-09" ||
		a.Delta.String() != "-15" {
		t.Errorf("adjustment wrong: %+v", a)
	}
}

func TestConcurrentReportAndCloseLinearizes(t *testing.T) {
	store := NewFileStorage(t.TempDir() + "/state.json")
	sched, _ := NewMonthlySchedule(1, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: mustParseTime("2026-10-01T00:00:00Z")}
	svc := NewService(store, sched, WithClock(clk.now))
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	// 所有事件都发生在 9 月，恰在 9 月/10 月边界并发上报与关闭。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := eventInput(fmt.Sprintf("evt-%03d", i), "2026-09-15T08:00:00Z", "1")
			if _, err := svc.ReportEvent(ctx, in); err != nil {
				t.Errorf("report %d: %v", i, err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ClosePeriod(ctx, "2026-09")
	}()
	wg.Wait()

	sep, err := svc.GetSnapshot(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	sepCount := int64(0)
	for _, l := range sep.Lines {
		sepCount += l.EventCount
	}

	// 统计进入 10 月的迟到事件（先关 10 月再读快照）。
	clk.t = mustParseTime("2026-11-02T00:00:00Z")
	octRes, err := svc.ClosePeriod(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	octCount := int64(0)
	var lateCount int
	for _, l := range octRes.Snapshot.Lines {
		octCount += l.EventCount
	}
	lateCount = len(octRes.Snapshot.Adjustments)

	// 关键不变量：不遗漏（总数 n）、不重复（只在一个周期）。
	if sepCount+octCount != n {
		t.Fatalf("events lost or duplicated: sep=%d oct=%d, want total %d", sepCount, octCount, n)
	}
	if int64(lateCount) != octCount {
		t.Errorf("every October event should be a late adjustment: octCount=%d adjustments=%d", octCount, lateCount)
	}
	sepQty := MustParseAmount("0")
	for _, l := range sep.Lines {
		sepQty = sepQty.Add(l.Quantity)
	}
	if sepQty.String() != fmt.Sprintf("%d", sepCount) {
		t.Errorf("Sep quantity inconsistent: %s vs %d", sepQty, sepCount)
	}
}

func TestConcurrentCloseProducesOneSnapshot(t *testing.T) {
	store := NewFileStorage(t.TempDir() + "/state.json")
	sched, _ := NewMonthlySchedule(1, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: mustParseTime("2026-10-02T00:00:00Z")}
	svc := NewService(store, sched, WithClock(clk.now))
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := svc.ClosePeriod(ctx, "2026-09")
			if err != nil {
				t.Errorf("close %d: %v", i, err)
				return
			}
			results[i] = res.Replayed
		}(i)
	}
	wg.Wait()

	replays := 0
	for _, r := range results {
		if r {
			replays++
		}
	}
	if replays != n-1 {
		t.Errorf("want exactly 1 creator and %d replays, got %d replays", n-1, replays)
	}
}

func TestSnapshotImmutableAfterReturn(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()
	if _, err := svc.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "10")); err != nil {
		t.Fatal(err)
	}
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	res, _ := svc.ClosePeriod(ctx, "2026-09")
	res.Snapshot.Lines[0].TenantID = "hacked"
	res.Snapshot.Lines = append(res.Snapshot.Lines, SnapshotLine{TenantID: "x"})

	got, _ := svc.GetSnapshot(ctx, "2026-09")
	if got.Lines[0].TenantID != "tenant-a" || len(got.Lines) != 1 {
		t.Fatalf("stored snapshot was mutated through returned value: %+v", got)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/state.json"
	sched, _ := NewMonthlySchedule(1, 0, 0, 0, time.UTC)
	ctx := context.Background()

	clk := &fakeClock{t: mustParseTime("2026-09-15T12:00:00Z")}
	svc1 := NewService(NewFileStorage(path), sched, WithClock(clk.now))
	if _, err := svc1.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "100")); err != nil {
		t.Fatal(err)
	}
	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	if _, err := svc1.ClosePeriod(ctx, "2026-09"); err != nil {
		t.Fatal(err)
	}

	// 用同一文件重新打开服务，数据应完整保留，关闭结果幂等。
	svc2 := NewService(NewFileStorage(path), sched, WithClock(clk.now))
	r, err := svc2.ReportEvent(ctx, eventInput("evt-1", "2026-09-10T08:00:00Z", "100"))
	if err != nil || !r.Replayed {
		t.Fatalf("event replay after restart: %+v, %v", r, err)
	}
	c, err := svc2.ClosePeriod(ctx, "2026-09")
	if err != nil || !c.Replayed {
		t.Fatalf("close replay after restart: %+v, %v", c, err)
	}
	if c.Snapshot.Lines[0].Quantity.String() != "100" {
		t.Errorf("snapshot after restart = %s", c.Snapshot.Lines[0].Quantity)
	}

	// 迟到事件在重启后依然正确进入下一周期。
	if _, err := svc2.ReportEvent(ctx, eventInput("evt-late", "2026-09-20T08:00:00Z", "5")); err != nil {
		t.Fatal(err)
	}
	e, err := svc2.GetEvent(ctx, "evt-late")
	if err != nil {
		t.Fatal(err)
	}
	if e.PeriodID != "2026-10" || e.SourcePeriodID != "2026-09" {
		t.Errorf("late attribution after restart: %+v", e)
	}
}

func TestCustomFloorPolicy(t *testing.T) {
	store := NewFileStorage(t.TempDir() + "/state.json")
	sched, _ := NewMonthlySchedule(1, 0, 0, 0, time.UTC)
	// 自定义下限：api-calls 允许 -100，其它维度默认 0。
	policy := func(tenant, meter string) (Amount, bool) {
		if meter == "api-calls" {
			return q("-100"), true
		}
		return q("0"), true
	}
	svc := NewService(store, sched, WithFloorPolicy(policy))
	ctx := context.Background()

	in := eventInput("evt-1", "2026-09-10T08:00:00Z", "100")
	if _, err := svc.ReportEvent(ctx, in); err != nil {
		t.Fatal(err)
	}
	// 累计 -101 低于自定义下限 -100，拒绝。
	if _, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("-201"),
	}); !isKind(err, KindBelowFloor) {
		t.Fatalf("want below_floor at -101 < -100, got %v", err)
	}
	// 累计 -50 在自定义下限 -100 之上，允许。
	if _, err := svc.ReportCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", OriginID: "evt-1",
		OccurredAt: mustParseTime("2026-09-11T08:00:00Z"), Delta: q("-150"),
	}); err != nil {
		t.Fatalf("-50 with floor -100 should pass: %v", err)
	}
}

func TestSnapshotAggregatesDecimalsExactly(t *testing.T) {
	svc, clk := newTestService(t)
	ctx := context.Background()

	mk := func(id, tenant, meter, at, quantity string) {
		t.Helper()
		if _, err := svc.ReportEvent(ctx, EventInput{
			ExternalID: id, TenantID: tenant, MeterID: meter,
			OccurredAt: mustParseTime(at), Quantity: MustParseAmount(quantity),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 0.1 + 0.2 在浮点下不等于 0.3，这里必须精确相等。
	mk("e1", "t1", "m1", "2026-09-10T08:00:00Z", "0.1")
	mk("e2", "t1", "m1", "2026-09-11T08:00:00Z", "0.2")
	// 另一租户 / 计量项，验证分组与排序。
	mk("e3", "t1", "m2", "2026-09-11T08:00:00Z", "5")
	mk("e4", "t2", "m1", "2026-09-11T08:00:00Z", "7.77")

	clk.t = mustParseTime("2026-10-02T00:00:00Z")
	res, err := svc.ClosePeriod(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		tenant, meter, q string
		count            int64
	}{
		{"t1", "m1", "0.3", 2},
		{"t1", "m2", "5", 1},
		{"t2", "m1", "7.77", 1},
	}
	if len(res.Snapshot.Lines) != len(want) {
		t.Fatalf("lines = %+v", res.Snapshot.Lines)
	}
	for i, w := range want {
		got := res.Snapshot.Lines[i]
		if got.TenantID != w.tenant || got.MeterID != w.meter ||
			got.Quantity.String() != w.q || got.EventCount != w.count {
			t.Errorf("line %d = %+v, want %s/%s=%s (%d)", i, got, w.tenant, w.meter, w.q, w.count)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.GetEvent(context.Background(), "nope")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if e.Kind != KindNotFound {
		t.Errorf("kind = %q", e.Kind)
	}
	if e.Error() == "" {
		t.Error("empty error message")
	}
	if _, ok := AsError(err); !ok {
		t.Error("AsError failed")
	}
	if _, ok := AsError(nil); ok {
		t.Error("AsError(nil) should fail")
	}
}

func isKind(err error, kind ErrorKind) bool {
	e, ok := AsError(err)
	return ok && e.Kind == kind
}

func snapshotsEqual(a, b Snapshot) bool {
	if a.PeriodID != b.PeriodID || !a.ClosedAt.Equal(b.ClosedAt) {
		return false
	}
	if len(a.Lines) != len(b.Lines) || len(a.Adjustments) != len(b.Adjustments) {
		return false
	}
	for i := range a.Lines {
		if a.Lines[i] != b.Lines[i] && !a.Lines[i].Quantity.Equal(b.Lines[i].Quantity) {
			return false
		}
	}
	for i := range a.Adjustments {
		x, y := a.Adjustments[i], b.Adjustments[i]
		if x.Kind != y.Kind || x.RecordID != y.RecordID || x.OriginEventID != y.OriginEventID ||
			x.SourcePeriodID != y.SourcePeriodID || !x.Delta.Equal(y.Delta) ||
			!x.SubmittedAt.Equal(y.SubmittedAt) {
			return false
		}
	}
	return true
}
