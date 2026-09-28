package typhoeus

import "fmt"

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
