package qmc

import (
	"encoding/base64"
	"errors"
	"math"
)

// errInvalid 统一的输入错误。
type errInvalid string

func (e errInvalid) Error() string { return string(e) }

// EncV2 前缀：base64("QQMusic EncV2,Key:")，出现即表示 EKey 需要先做双层 TEA 解密。
var ekeyV2Prefix = []byte("UVFNdXNpYyBFbmNWMixLZXk6")

// EncV2 双层 TEA 的两把固定密钥（对应 Rust ekey.rs KEY1/KEY2）。
var (
	ekeyV2Key1 = []byte{
		0x33, 0x38, 0x36, 0x5A, 0x4A, 0x59, 0x21, 0x40,
		0x23, 0x2A, 0x24, 0x25, 0x5E, 0x26, 0x29, 0x28,
	}
	ekeyV2Key2 = []byte{
		0x2A, 0x2A, 0x23, 0x21, 0x28, 0x23, 0x24, 0x25,
		0x26, 0x5E, 0x61, 0x31, 0x63, 0x5A, 0x2C, 0x54,
	}
)

// makeSimpleKey 对应 Rust ekey.rs 的 make_simple_key::<8>()。
// 由 tan(106 + i*0.1) 派生的 8 字节固定密钥，全部运算走 f32 语义（与 Rust 对齐）。
func makeSimpleKey() []byte {
	f01 := float32(0.1)
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		v := float32(106.0) + float32(float32(i)*f01)
		t := math.Abs(math.Tan(float64(v))) // tan 在 f64 上计算
		v = float32(t)
		v = float32(float64(v) * 100.0)
		iv := int64(v) // 向零截断
		if iv < 0 {
			iv = 0
		} else if iv > 255 {
			iv = 255
		}
		out[i] = byte(iv)
	}
	return out
}

var ekeySimpleKey = makeSimpleKey()

// ekeyDecryptV1 V1 形态：base64 → header(8 字节) + TEA 密文，
// TEA 密钥 = interleave(simple_key, header)（16 字节）。返回 header + 明文主密钥。
func ekeyDecryptV1(ekey []byte) ([]byte, error) {
	if len(ekey) < 12 {
		return nil, errInvalid("EKey 太短，无法解密")
	}
	decoded, err := base64.StdEncoding.DecodeString(string(ekey))
	if err != nil {
		return nil, err
	}
	if len(decoded) < 8 {
		return nil, errInvalid("EKey base64 解码后不足 8 字节")
	}
	header, cipherText := decoded[:8], decoded[8:]
	teaKey := make([]byte, 0, 16)
	for i := 0; i < 8; i++ {
		teaKey = append(teaKey, ekeySimpleKey[i], header[i])
	}
	plain, err := TeaCBCDecrypt(cipherText, teaKey)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, header...), plain...), nil
}

// ekeyDecryptV2 V2 形态："QQMusic EncV2,Key:" 前缀之后的 base64，
// 先用 KEY1/KEY2 各做一次 tc_tea 解密，截断到第一个 0 字节后再走 V1。
func ekeyDecryptV2(ekey []byte) ([]byte, error) {
	payload, err := base64.StdEncoding.DecodeString(string(ekey))
	if err != nil {
		return nil, err
	}
	if payload, err = TeaCBCDecrypt(payload, ekeyV2Key1); err != nil {
		return nil, err
	}
	if payload, err = TeaCBCDecrypt(payload, ekeyV2Key2); err != nil {
		return nil, err
	}
	for i, b := range payload {
		if b == 0 {
			payload = payload[:i]
			break
		}
	}
	return ekeyDecryptV1(payload)
}

// DecryptEkey 把 API 下发的 EKey 字符串解成「主密钥」（master key）。
// 主密钥长度决定流密码：1..300 字节走 Map，更长走 RC4（见 NewCipher）。
func DecryptEkey(ekey string) ([]byte, error) {
	raw := []byte(ekey)
	if len(raw) >= len(ekeyV2Prefix) && string(raw[:len(ekeyV2Prefix)]) == string(ekeyV2Prefix) {
		return ekeyDecryptV2(raw[len(ekeyV2Prefix):])
	}
	return ekeyDecryptV1(raw)
}

// ErrEmptyKey 主密钥为空。
var ErrEmptyKey = errors.New("qmc: 主密钥为空")
