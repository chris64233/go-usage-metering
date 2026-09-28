package usagemetering

import (
	"errors"
	"fmt"
)

// ErrorCode 对错误进行清晰分类，调用方可通过 errors.As 取得 *Error 后按 Code 分支处理。
type ErrorCode string

const (
	// CodeInvalidArgument 入参校验失败（字段缺失、时间非法、数量无法解析等）。
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound 引用的对象不存在，例如原事件或周期不存在。
	CodeNotFound ErrorCode = "not_found"
	// CodeConflict 幂等键（事件号/修正号/周期）已存在，但提交内容与首次不一致。
	CodeConflict ErrorCode = "conflict"
	// CodeBelowFloor 修正累计后数量低于业务允许的下限。
	CodeBelowFloor ErrorCode = "below_floor"
	// CodePeriodClosed 目标周期已经关闭，不能再被改写（迟到数据请走下一周期调整项）。
	CodePeriodClosed ErrorCode = "period_closed"
	// CodePeriodOpen 目标周期尚未关闭；账单草稿只能在周期关闭（快照定稿）后生成。
	CodePeriodOpen ErrorCode = "period_open"
	// CodeRateOverlap 同一租户、同一计量项在同一生效时刻已存在费率版本；
	// 任一时刻适用费率必须唯一，不允许两份费率在同一时刻同时生效。
	CodeRateOverlap ErrorCode = "rate_overlap"
	// CodeNoApplicableRate 草稿计价时，某计量项在用量发生时刻没有任何已生效费率版本。
	CodeNoApplicableRate ErrorCode = "no_applicable_rate"
	// CodeVersionConflict 草稿版本冲突：例如要求作废的版本与当前版本不一致。
	CodeVersionConflict ErrorCode = "version_conflict"
	// CodeInternal 存储层或其它内部故障。
	CodeInternal ErrorCode = "internal"
)

// Error 是本服务返回的结构化错误。
type Error struct {
	Code    ErrorCode
	Message string
	// Cause 保留底层错误（如数据库错误），可通过 errors.Unwrap/errors.Is 继续判断。
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("usagemetering(%s): %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("usagemetering(%s): %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapError(code ErrorCode, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// AsError 尝试把任意错误转换为 *Error。
func AsError(err error) (*Error, bool) {
	var e *Error
	return e, errors.As(err, &e)
}

// IsCode 判断错误是否属于指定错误码。
func IsCode(err error, code ErrorCode) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}
