package engine

import (
	"context"
	"errors"
	"fmt"
)

// recoverableError 标记一个"重跑就有希望成功"的失败：网络抖动、provider 侧
// 的临时错误、本次时间预算耗尽等等。它跟"消息历史本身已经坏了"的失败（比如
// 模型撞满轮数上限）语义相反——后者重跑只会再撞一次同一堵墙。
//
// 这个区分决定了运行记录的最终状态：可恢复的失败让记录留在 in_progress，
// 下次重跑沿用已落盘的消息历史接着跑；不可恢复的失败标记 failed，下次重开一局。
type recoverableError struct {
	err error
}

func (e *recoverableError) Error() string { return e.err.Error() }
func (e *recoverableError) Unwrap() error { return e.err }

// recoverable 把 err 标记为可恢复。err 为 nil 时返回 nil，方便直接包在
// 可能没有错误的返回值上。
func recoverable(err error) error {
	if err == nil {
		return nil
	}
	return &recoverableError{err: err}
}

// isRecoverable 判断 err 是否值得靠重跑来解决。
//
// 除了显式被 recoverable 标记过的错误，context 的取消与超时也一律算可恢复：
// 它们表示"这次没时间/被叫停了"，而不是"这份消息历史有问题"，下次重跑会拿到
// 全新的时间预算。provider 的 SDK 常常把 context 错误包在自己的错误里返回，
// 所以这里用 errors.Is 而不是等值比较。
func isRecoverable(err error) bool {
	if err == nil {
		return false
	}
	var re *recoverableError
	if errors.As(err, &re) {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// wrapRecoverable 按 err 是否可恢复，决定包装后的错误要不要保留可恢复标记。
// 用在需要给错误加一层上下文说明、又不想丢掉这个标记的地方。
func wrapRecoverable(err error, format string, args ...any) error {
	wrapped := fmt.Errorf(format, append(args, err)...)
	if isRecoverable(err) {
		return recoverable(wrapped)
	}
	return wrapped
}
