package qqmusic

import (
	"errors"
	"strconv"
)

var errInvalidHex = errors.New("sign: 非法 hex 字符")

// APIError 是 QQ 音乐上游业务异常的统一载体。
// Kind 决定 HTTP 层的映射状态码（见 server 包的错误映射表）。
type APIError struct {
	Kind    ErrorKind
	Code    int // 上游业务码（CGI code / 登录 code），无则 0
	Message string
	Data    string // 上游原始 data（文本形式，诊断用）
}

// ErrorKind 错误分类（对标 qqmusic_api.core.exceptions）。
type ErrorKind int

const (
	// ErrKindHTTP HTTP 状态码异常。
	ErrKindHTTP ErrorKind = iota
	// ErrKindNetwork 网络异常（断网/超时）。
	ErrKindNetwork
	// ErrKindTimeout 网络超时（与一般网络故障区分，供长轮询按超时解释）。
	ErrKindTimeout
	// ErrKindData 响应载荷解析失败或关键数据缺失。
	ErrKindData
	// ErrKindGlobal 网关级错误（顶层 code != 0）。
	ErrKindGlobal
	// ErrKindCGI 单项 CGI 业务异常。
	ErrKindCGI
	// ErrKindCredentialInvalid 凭证缺失或格式损坏（发起请求前即发现）。
	ErrKindCredentialInvalid
	// ErrKindCredentialExpired 服务端拦截的凭证过期（1000/104400/104401）。
	ErrKindCredentialExpired
	// ErrKindRatelimited 触发风控或频率限制（2001）。
	ErrKindRatelimited
	// ErrKindSignature 请求需要签名（2000）。
	ErrKindSignature
	// ErrKindLogin 登录域业务异常（含各细分码）。
	ErrKindLogin
)

func (e *APIError) Error() string { return e.Message }

// APIError 构造快捷函数。
func errHTTP(message string, status int) *APIError {
	return &APIError{Kind: ErrKindHTTP, Message: "HTTP " + strconv.Itoa(status) + ": " + message, Code: status}
}

func errNetwork(message string) *APIError { return &APIError{Kind: ErrKindNetwork, Message: message} }
func errTimeout(message string) *APIError { return &APIError{Kind: ErrKindTimeout, Message: message} }
func errData(message string) *APIError {
	return &APIError{Kind: ErrKindData, Message: "API Data Error: " + message}
}
func errGlobal(code int, data string) *APIError {
	return &APIError{Kind: ErrKindGlobal, Code: code, Message: "请求被网关拒绝 (code=" + strconv.Itoa(code) + ")", Data: data}
}
func errCGI(code int, data string) *APIError {
	return &APIError{Kind: ErrKindCGI, Code: code, Message: "CGI 请求错误 (code=" + strconv.Itoa(code) + ")", Data: data}
}
func errCGIKind(kind ErrorKind, code int, message, data string) *APIError {
	return &APIError{Kind: kind, Code: code, Message: message, Data: data}
}
func errCredentialInvalid(message string) *APIError {
	return &APIError{Kind: ErrKindCredentialInvalid, Message: message}
}
func errLogin(code int, message, data string) *APIError {
	return &APIError{Kind: ErrKindLogin, Code: code, Message: message, Data: data}
}

// AsAPIError 把任意 error 还原成 *APIError（其他错误归为网络错误）。
func AsAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return &APIError{Kind: ErrKindNetwork, Message: err.Error()}
}

// errorsIsKind 判断错误是否属于某类（供上层按类处理）。
func errorsIsKind(err error, kind ErrorKind) bool {
	e := AsAPIError(err)
	return e.Kind == kind
}

// cgiErrorMap 服务端拦截码 → 错误分类（对标 core/response.py CGI_ERROR_MAP）。
var cgiErrorMap = map[int]ErrorKind{
	2000:   ErrKindSignature,
	2001:   ErrKindRatelimited,
	1000:   ErrKindCredentialExpired,
	104400: ErrKindCredentialExpired,
	104401: ErrKindCredentialExpired,
	// 10004：上游要求登录态（如每日三十首 dirid=202 匿名请求），视作凭证缺失。
	10004: ErrKindCredentialInvalid,
}

// ratelimitedMessage 风控提示文案（与上游一致）。
const ratelimitedMessage = "触发风控, 需登录或者安全验证"

// expiredMessage 凭证过期提示文案。
const expiredMessage = "登录凭证已过期, 请重新登录"

// NewDataError 构造数据解析错误（供 modules 子包使用）。
func NewDataError(message string) *APIError { return errData(message) }

// NewLoginError 构造登录域错误（供 modules 子包使用）。
func NewLoginError(code int, message, data string) *APIError { return errLogin(code, message, data) }

// NewCredentialInvalidError 构造凭证缺失错误。
func NewCredentialInvalidError(message string) *APIError { return errCredentialInvalid(message) }
