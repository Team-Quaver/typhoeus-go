// Package qqmusic 是 QQ 音乐服务端接口的 Go 客户端（对标 vendor/QQMusicApi 的核心层）。
//
// 设计约定：
//   - 上游 CGI（musicu.fcg / musics.fcg）的参数与响应都是 JSON；为了让「签名」
//     与「实际发送的字节」严格一致，请求体由本包的有序 JSON 构建器（JObj/JArr）
//     序列化，序列化结果既用于计算 zzc 签名也用于发送。
//   - 响应一律以 json.RawMessage 透传给上层（业务模块只挑自己关心的字段解析），
//     不做完整的强类型建模 —— 服务端是薄适配层，透传可让前端拿到最全的信息，
//     也避免每个字段的双重维护。
package qqmusic

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// JObj 有序 JSON 对象（键按插入顺序输出，保证与签名一致）。
type JObj struct {
	keys []string
	vals map[string]any
}

// NewJObj 构造空对象。
func NewJObj() *JObj { return &JObj{vals: map[string]any{}} }

// Set 写入键值（已存在则覆盖，位置不变）。
func (o *JObj) Set(key string, v any) *JObj {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
	return o
}

// Get 读取键值。
func (o *JObj) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Delete 删除键（不存在则无操作）。
func (o *JObj) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len 键数量。
func (o *JObj) Len() int { return len(o.keys) }

// JArr JSON 数组。
type JArr []any

// Marshal 输出紧凑 JSON（与 orjson 语义对齐：不转义 <>&，UTF-8 原样输出）。
func (o *JObj) Marshal() []byte {
	var b strings.Builder
	o.write(&b)
	return []byte(b.String())
}

// Marshal 输出紧凑 JSON。
func (a *JArr) Marshal() []byte {
	var b strings.Builder
	a.write(&b)
	return []byte(b.String())
}

func (o *JObj) write(b *strings.Builder) {
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(b, k)
		b.WriteByte(':')
		writeJSONValue(b, o.vals[k])
	}
	b.WriteByte('}')
}

func (a *JArr) write(b *strings.Builder) {
	b.WriteByte('[')
	for i, v := range *a {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONValue(b, v)
	}
	b.WriteByte(']')
}

func writeJSONValue(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float64:
		// 与 Python float 序列化接近：整数化时省去小数点
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case float32:
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	case string:
		writeJSONString(b, x)
	case *JObj:
		x.write(b)
	case *JArr:
		x.write(b)
	case JArr:
		x.write(b)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, s)
		}
		b.WriteByte(']')
	case []int:
		b.WriteByte('[')
		for i, n := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(n))
		}
		b.WriteByte(']')
	default:
		panic("ordered: 不支持的 JSON 值类型")
	}
}

// writeJSONString 按 RFC 8259 写字符串：仅转义必要字符，不转义 <>&（orjson 语义）。
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hexd = "0123456789abcdef"
				b.WriteByte(hexd[r>>4])
				b.WriteByte(hexd[r&0xF])
			} else if r == utf8.RuneError {
				b.WriteString(`\ufffd`)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ensureSurrogates 预留：Go range 已按 rune 处理，无效 UTF-8 会变 RuneError 并按
// \ufffd 输出，这与 orjson（严格 UTF-8，会在无效字节处报错）略有差异；
// 本项目的请求参数不含无效 UTF-8，故不做严格校验。
var _ = utf16.Encode

// BoolToIntParams 递归把参数对象里的 bool → 0/1（对齐上游 bool_to_int 默认行为）。
func BoolToIntParams(param *JObj) *JObj {
	out := NewJObj()
	for _, k := range param.Keys() {
		v, _ := param.Get(k)
		out.Set(k, convertBoolValue(v))
	}
	return out
}

func convertBoolValue(v any) any {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case *JObj:
		return BoolToIntParams(x)
	case *JArr:
		out := &JArr{}
		for _, item := range *x {
			*out = append(*out, convertBoolValue(item))
		}
		return out
	default:
		return v
	}
}

// Keys 返回键序快照（遍历用）。
func (o *JObj) Keys() []string {
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}
