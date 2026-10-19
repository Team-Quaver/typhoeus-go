package typhoeus

import (
	"context"
	"errors"
	"fmt"
)

// Error Typhoeus 错误族；Status 直接映射 HTTP 状态码。
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// 常用错误构造（状态码语义与 Python 版一致）：
//
//	422 未知档位 / 403 会员门控 / 451 加密档拒绝 / 502 provider 或回源失败。
func errUnknownTier(id string) *Error {
	return &Error{Status: 422, Message: fmt.Sprintf("未知音质档位: %s", id)}
}

func errMembershipRequired(msg string) *Error {
	return &Error{Status: 403, Message: msg}
}

func errTierNotPlayable(msg string) *Error {
	return &Error{Status: 451, Message: msg}
}

func errProvider(msg string) *Error {
	return &Error{Status: 502, Message: msg}
}

func errStream(msg string) *Error {
	return &Error{Status: 502, Message: msg}
}

// —— 断开来源分类 ——
//
// 中继的写端是播放器（经 Electron 主进程转发），断开的来源有两种，语义完全不同：
//
//	client gone  ：seek / 切歌 / 暂停 / 预加载被丢弃 → Chromium 关掉请求，
//	               主进程随即 abort 它对 sidecar 的 fetch（native-server.ts 的
//	               onClientGone）。表现即 r.Context() canceled 或
//	               write tcp 127.0.0.1:<sidecar>->…: connection reset by peer。
//	               完全良性：已送出的字节没问题，播放器会用新偏移重开一个 Range。
//	upstream dead：CDN 掐连接 / 读超时 / 提前 EOF。真故障，值得退避续传并告警。
//
// 只看 errno 分不开这两类——上游 reset 同样是 ECONNRESET。所以按「错误来自哪一侧」
// 打标记：pump 写客户端失败时包一层 clientGone，读上游失败保持原样。

// clientGone 标记「写客户端时失败」，保留原始错误文本供日志。
type clientGone struct{ err error }

func (e *clientGone) Error() string { return e.err.Error() }
func (e *clientGone) Unwrap() error { return e.err }

// wrapClientGone 由 pump 使用：写端错误一律视为客户端已离开。
func wrapClientGone(err error) error {
	if err == nil {
		return nil
	}
	return &clientGone{err}
}

// IsClientAbort 判断错误是否由客户端主动离开引起（而非回源失败）。
//
// 调用方据此把「播放器正常操作」降到低噪级别，同时保留真故障的告警。
func IsClientAbort(err error) bool {
	if err == nil {
		return false
	}
	var gone *clientGone
	if errors.As(err, &gone) {
		return true
	}
	// 请求上下文取消 = 客户端连接结束（http.Server 在连接关闭时取消 r.Context）。
	// 上游侧用的是同一个 ctx，因此上游自己 reset 不会走到这里——它表现为读错误。
	return errors.Is(err, context.Canceled)
}
