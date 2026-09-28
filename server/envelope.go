package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// 统一响应信封（对齐上游 QQMusicApi web 层约定）：
//
//	成功 {code:0, msg:"ok", data:...}
//	失败 {code:-1|-<status>, msg:...} + 对应 HTTP 状态码
func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": data})
}

func writeErr(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg})
}

// writeError 统一错误映射：
//   - Typhoeus 错误族自带 status（403 会员门控 / 451 加密档拒绝 / 502 provider / 422 未知档），
//     code = -status；
//   - 上游风控 429；凭证无效 401；其余上游错误 400；
//   - 本地错误（请求体解析等）400/422。
func writeError(w http.ResponseWriter, err error) {
	var tErr *typhoeus.Error
	if errors.As(err, &tErr) {
		writeErr(w, tErr.Status, -tErr.Status, tErr.Message)
		return
	}
	apiErr := qqmusic.AsAPIError(err)
	switch apiErr.Kind {
	case qqmusic.ErrKindRatelimited:
		writeErr(w, http.StatusTooManyRequests, -http.StatusTooManyRequests, apiErr.Message)
	case qqmusic.ErrKindCredentialInvalid, qqmusic.ErrKindCredentialExpired:
		writeErr(w, http.StatusUnauthorized, -http.StatusUnauthorized, apiErr.Message)
	default:
		writeErr(w, http.StatusBadRequest, -1, apiErr.Message)
	}
}

// decodeBody 解析 JSON 请求体（失败 → 400）。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, -1, "请求体解析失败")
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, http.StatusBadRequest, -1, "请求体 JSON 格式错误")
		return false
	}
	return true
}

// queryInt 读取 int 查询参数（缺省/非法用默认值）。
func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func queryInt64(r *http.Request, key string, def int64) int64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// fmtUnused 防止 fmt 依赖遗漏（保留错误格式化能力）。
var _ = fmt.Sprintf

func queryBool(r *http.Request, key string, def bool) bool {
	v := r.URL.Query().Get(key)
	switch v {
	case "1", "true", "True":
		return true
	case "0", "false", "False":
		return false
	}
	return def
}
