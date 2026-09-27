package usagemetering

import "context"

// Storage 是服务的持久化接口。所有方法必须对并发安全；
// Update 由存储层保证“读取-修改-写回”的串行化（互斥/事务），
// 服务层据此实现上报与周期关闭之间的线性一致性。
type Storage interface {
	// Update 在单个原子事务内执行 fn：fn 基于读到的 State 计算新 State，
	// 并可通过 result 把事务结果带回调用方。
	// fn 必须是纯函数式的：只通过返回值改变状态，不做带外副作用，
	// 以便持久化实现需要重试时可以安全重放。
	Update(ctx context.Context, fn func(s State) (newState State, result any, err error)) (any, error)

	// View 在与 Update 互斥的只读事务内执行 fn。fn 不得修改 State
	// （State 中的切片/Map 也不应被改动），实现不会触发任何写盘。
	View(ctx context.Context, fn func(s State) (result any, err error)) (any, error)
}

// State 是持久化的完整状态，JSON 序列化后落盘。
type State struct {
	// Events 保存全部记录（原始事件与修正），按提交顺序排列。
	Events []Event
	// Snapshots 按周期 ID 保存已关闭周期的不可变快照。
	Snapshots map[string]Snapshot
}

func newState() State {
	return State{Snapshots: map[string]Snapshot{}}
}
