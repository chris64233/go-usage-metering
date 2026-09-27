package usagemetering

import (
	"context"
	"testing"
	"time"
)

// 自定义业务下限（非 0）。
func TestCorrection_CustomFloor(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(1), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	floor5 := mustDec(t, "5")

	// 10 - 6 = 4 < 5：拒绝。
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(2), Delta: mustDec(t, "-6"), Floor: &floor5,
	}); !IsCode(err, CodeBelowFloor) {
		t.Fatalf("want CodeBelowFloor with custom floor, got %v", err)
	}
	// 10 - 5 = 5 == 5：允许。
	c, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(2), Delta: mustDec(t, "-5"), Floor: &floor5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Floor.Equal(floor5) {
		t.Fatalf("stored floor = %s, want 5", c.Floor)
	}

	// 负下限非法。
	negFloor := mustDec(t, "-1")
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c2", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(3), Delta: mustDec(t, "0"), Floor: &negFloor,
	}); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("want CodeInvalidArgument for negative floor, got %v", err)
	}
}

// 上报前没有任何周期时，以发生时间自动建立首个周期。
func TestReportEvent_AutoCreateFirstPeriod(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	e, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m", OccurredAt: at, Quantity: mustDec(t, "1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.GetPeriod(ctx, "ta", at)
	if err != nil {
		t.Fatalf("first period should be auto-created at occurrence time: %v", err)
	}
	if p.Closed {
		t.Fatalf("auto-created period should be open")
	}
	if e.CommitSeq != 1 {
		t.Fatalf("first commit seq = %d, want 1", e.CommitSeq)
	}
}

// 发生时间早于系统内最早周期：进入当前开放周期的调整项，来源周期为零值。
func TestEventBeforeEarliestPeriod_IsAdjustmentWithZeroOrigin(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(100), mkTime(200)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "ancient", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(0), Quantity: mustDec(t, "3"),
	}); err != nil {
		t.Fatal(err)
	}
	adjs, err := svc.ListAdjustments(ctx, "ta", t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("want 1 adjustment, got %+v", adjs)
	}
	if !adjs[0].OriginPeriodStart.IsZero() {
		t.Fatalf("origin should be zero for data predating all periods, got %s", adjs[0].OriginPeriodStart)
	}
	if adjs[0].PeriodStart != t2 {
		t.Fatalf("should be carried by P2, got %s", adjs[0].PeriodStart)
	}
}

// 租户隔离：同一事件号/修正号在不同租户互不干扰。
func TestTenantIsolation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	in := func(tenant string) EventInput {
		return EventInput{
			EventID: "same-id", Tenant: tenant, Meter: "m",
			OccurredAt: mkTime(1), Quantity: mustDec(t, "1"),
		}
	}
	if _, err := svc.ReportEvent(ctx, in("ta")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, in("tb")); err != nil {
		t.Fatalf("same event id in another tenant should be independent: %v", err)
	}
	// tb 的事件引用不到 ta 的原事件。
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "c1", Tenant: "tb", EventID: "same-id",
		OccurredAt: mkTime(2), Delta: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetCorrection(ctx, "ta", "c1"); !IsCode(err, CodeNotFound) {
		t.Fatalf("correction must not leak across tenants, got %v", err)
	}
}

// 多条调整项按提交序号排序并保留各自来源。
func TestListAdjustments_OrderedAndLinked(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t1, t2 := mkTime(0), mkTime(100)
	if _, err := svc.EnsurePeriod(ctx, "ta", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "e1", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(5), Quantity: mustDec(t, "10"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, "ta", t1, t2); err != nil {
		t.Fatal(err)
	}

	// 交错提交：迟到事件、迟到修正、又一条迟到事件。
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late-e1", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(6), Quantity: mustDec(t, "1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyCorrection(ctx, CorrectionInput{
		CorrectionID: "late-c1", Tenant: "ta", EventID: "e1",
		OccurredAt: mkTime(7), Delta: mustDec(t, "-1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportEvent(ctx, EventInput{
		EventID: "late-e2", Tenant: "ta", Meter: "m",
		OccurredAt: mkTime(8), Quantity: mustDec(t, "2"),
	}); err != nil {
		t.Fatal(err)
	}

	adjs, err := svc.ListAdjustments(ctx, "ta", t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 3 {
		t.Fatalf("want 3 adjustments, got %d: %+v", len(adjs), adjs)
	}
	wantOrder := []struct {
		kind string
		ref  string
	}{
		{KindEvent, "late-e1"},
		{KindCorrection, "late-c1"},
		{KindEvent, "late-e2"},
	}
	for i, w := range wantOrder {
		if adjs[i].Kind != w.kind || adjs[i].RefID != w.ref {
			t.Fatalf("adjustment[%d] = (%s,%s), want (%s,%s)",
				i, adjs[i].Kind, adjs[i].RefID, w.kind, w.ref)
		}
		if adjs[i].CommitSeq <= 0 {
			t.Fatalf("adjustment %s missing commit seq", w.ref)
		}
		if adjs[i].CommittedAt.IsZero() {
			t.Fatalf("adjustment %s missing committed_at", w.ref)
		}
	}
}

// Open 入参校验。
func TestOpen_EmptyDSN(t *testing.T) {
	if _, err := Open(""); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// GetEvent/GetCorrection 参数校验。
func TestGetters_Validation(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.GetEvent(ctx, "", "e1"); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("GetEvent empty tenant: %v", err)
	}
	if _, err := svc.GetCorrection(ctx, "ta", ""); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("GetCorrection empty id: %v", err)
	}
	if _, err := svc.GetPeriod(ctx, "ta", time.Time{}); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("GetPeriod zero start: %v", err)
	}
	if _, err := svc.GetSnapshot(ctx, "ta", time.Time{}); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("GetSnapshot zero start: %v", err)
	}
	if _, err := svc.ListAdjustments(ctx, "", mkTime(0)); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("ListAdjustments empty tenant: %v", err)
	}
}
