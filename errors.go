package usagemetering

import (
	"errors"
	"fmt"
)

// ErrorKind 对业务错误进行清晰分类，便于调用方按类型处理。
type ErrorKind string

const (
	// KindValidation 输入不合法（空字段、数量格式错误、时间无效等）。
	KindValidation ErrorKind = "validation"
	// KindNotFound 引用的对象不存在（原事件、修正号、周期等）。
	KindNotFound ErrorKind = "not_found"
	// KindConflict 幂等号已存在但内容与本次请求不一致。
	KindConflict ErrorKind = "conflict"
	// KindBelowFloor 修正后累计数量低于业务允许的下限。
	KindBelowFloor ErrorKind = "below_floor"
	// KindStorage 持久化层错误（磁盘 IO、数据损坏等）。
	KindStorage ErrorKind = "storage"
)

// Error 携带分类信息的业务错误。使用 errors.As 解包：
//
//	var e *usagemetering.Error
//	if errors.As(err, &e) && e.Kind == usagemetering.KindConflict { ... }
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string {
	return string(e.Kind) + ": " + e.Msg
}

func newError(kind ErrorKind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// AsError 尝试把 err 转为 *Error。
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
