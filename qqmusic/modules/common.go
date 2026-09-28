package modules

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// randomGUID 32 位随机 hex（取链/CDN 调度的 guid 参数）。
func randomGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomSearchID 随机 searchID（搜索接口防重用，与上游算法一致）。
func randomSearchID() string {
	e := 1 + randIntN(20)
	t := e * 18014398509481984
	n := randIntN(4194305) * 4294967296
	r := (time.Now().UnixMilli()) % (24 * 60 * 60 * 1000)
	return strconv.FormatInt(t+n+r, 10)
}

func randIntN(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// jsonStr 从原始 JSON 取字符串字段（缺省空串）。
func jsonStr(raw map[string]any, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

// jsonInt 从原始 JSON 取整数字段（兼容 float64 / json.Number / 字符串数字）。
func jsonInt(raw map[string]any, key string) int64 {
	switch v := raw[key].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

// jsonArr 从原始 JSON 取数组字段。
func jsonArr(raw map[string]any, key string) []any {
	if v, ok := raw[key].([]any); ok {
		return v
	}
	return nil
}

// jsonObj 从原始 JSON 取对象字段。
func jsonObj(raw map[string]any, key string) map[string]any {
	if v, ok := raw[key].(map[string]any); ok {
		return v
	}
	return nil
}

// strList 从原始 JSON 的数组里取字符串列表（v_songId 这类 int 列表转字符串）。
func int64List(items []any) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		switch v := it.(type) {
		case float64:
			out = append(out, int64(v))
		case string:
			n, _ := strconv.ParseInt(v, 10, 64)
			out = append(out, n)
		}
	}
	return out
}

// BoolResult 把 CGI 返回的 {"retCode":0} / {"result":0} 归一为 bool。
func BoolResult(data json.RawMessage, key string) (bool, error) {
	var raw map[string]any
	if err := jsonUnmarshal(data, &raw); err != nil {
		return false, qqmusic.AsAPIError(err)
	}
	return jsonInt(raw, key) == 0, nil
}
