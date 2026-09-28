package qqmusic

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

// zzc 签名算法（QQ 音乐客户端请求签名，逆向自官方 JS）。
var (
	signPart1Indexes = []int{23, 14, 6, 36, 16, 7, 19}
	signPart2Indexes = []int{16, 1, 32, 12, 19, 27, 8, 5}
	signScramble     = []byte{89, 39, 179, 150, 218, 82, 58, 252, 177, 52, 186, 123,
		120, 64, 242, 133, 143, 161, 121, 179}
	signB64Strip = regexp.MustCompile(`[\\/+=]`)
)

// ZzcSign 计算 QQ 音乐客户端请求的 zzc 签名。
// payload 必须是「将要原样发送」的请求体字节（本包用 JObj.Marshal 产出）。
func ZzcSign(payload []byte) string {
	sum := sha1.Sum(payload)
	hashHex := hex.EncodeToString(sum[:])
	hashHex = strings.ToUpper(hashHex)

	var p1, p2 strings.Builder
	for _, i := range signPart1Indexes {
		p1.WriteByte(hashHex[i])
	}
	for _, i := range signPart2Indexes {
		p2.WriteByte(hashHex[i])
	}

	part3 := make([]byte, 20)
	for i, v := range signScramble {
		hi, err := hexByte(hashHex[i*2], hashHex[i*2+1])
		if err != nil {
			return ""
		}
		part3[i] = v ^ hi
	}
	b64 := base64.StdEncoding.EncodeToString(part3)
	b64 = signB64Strip.ReplaceAllString(b64, "")
	// 最终整体转小写（对齐 Python f-string 的 .lower()，作用于全串）
	return strings.ToLower("zzc" + p1.String() + b64 + p2.String())
}

func hexByte(a, b byte) (byte, error) {
	var out byte
	for _, c := range [2]byte{a, b} {
		out <<= 4
		switch {
		case c >= '0' && c <= '9':
			out |= c - '0'
		case c >= 'A' && c <= 'F':
			out |= c - 'A' + 10
		case c >= 'a' && c <= 'f':
			out |= c - 'a' + 10
		default:
			return 0, errInvalidHex
		}
	}
	return out, nil
}
