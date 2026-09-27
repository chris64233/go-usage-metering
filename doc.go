// Package usagemetering 提供带修正记录与结算截点的用量计量能力。
//
// 核心模型：
//   - 用量事件（Event）：由外部事件号幂等标识，包含租户、计量项、发生时间与精确数量。
//   - 修正（Correction）：必须引用一条原事件，以有符号增量（delta）记账，永不覆盖历史；
//     同一原事件可以有多次独立修正，修正号本身幂等。
//   - 结算周期（Period）：租户内首尾相接、互不重叠的半开时间区间 [Start, End)。
//   - 快照（Snapshot）：周期关闭时生成，内容不可变；关闭之后才到达的迟到事件或修正
//     不会改写旧快照，而是落到下一个开放周期，作为带来源关联的调整项（Adjustment）。
//
// 所有数量均使用 github.com/shopspring/decimal 精确计算并以规范文本持久化。
package usagemetering
