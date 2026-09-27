// Package usagemetering 实现带修正记录与结算截点的用量计量。
//
// # 核心能力
//
//   - 原始用量事件上报：每条事件带外部事件号（幂等键）、租户、计量项、
//     发生时间与精确数量。同号同内容重放返回原结果；同号内容变化报冲突。
//   - 增量修正：修正只引用原事件并记录增量，从不覆盖历史。同一原事件
//     支持多次独立修正，每个修正号自身幂等；原事件数量与全部修正增量
//     之和不得低于业务下限（FloorPolicy，默认 0）。
//   - 结算周期关闭：生成按 (租户, 计量项) 聚合的不可变快照。重复关闭与
//     并发关闭都只产生同一份快照。
//   - 迟到处理：周期关闭后到达的事件/修正不改动旧快照，而是进入提交时
//     的当前打开周期，在该周期快照中作为调整项（Adjustment）单列，
//     并保留来源周期与来源事件关联。
//
// # 并发与提交边界
//
// 所有写操作都在 Storage 的单个原子事务内完成。上报与关闭并发时，
// 以事务提交顺序作为唯一归属依据：
//
//   - 先提交的上报：周期尚未关闭 -> 计入该周期快照；
//   - 先提交的关闭：后提交的记录按发生时间发现周期已关闭 -> 作为调整项
//     进入下一周期。
//
// 因此事件既不会遗漏，也不可能同时进入两个周期。
//
// # 精确数量
//
// 数量使用 Amount（math/big.Int 定点十进制），全程不经过 float64，
// JSON 以字符串形式传输，适合计量计费场景。
//
// # 典型用法
//
//	store := usagemetering.NewFileStorage("data/state.json")
//	sched, _ := usagemetering.NewMonthlySchedule(1, 0, 0, 0, time.UTC)
//	svc := usagemetering.NewService(store, sched)
//
//	r, err := svc.ReportEvent(ctx, usagemetering.EventInput{
//	    ExternalID: "evt-1",
//	    TenantID:   "tenant-a",
//	    MeterID:    "api-calls",
//	    OccurredAt: time.Now(),
//	    Quantity:   usagemetering.MustParseAmount("100.25"),
//	})
//
//	res, err := svc.ClosePeriod(ctx, "2026-09")
package usagemetering
