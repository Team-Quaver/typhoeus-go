// Package qmc 实现 QQ 音乐 QMC 加密音频的流式解密。
//
// 仅保留内存语义：本包不落盘、不缓存明文，全部解密按「数据切片 + 绝对偏移」进行，
// 上层可将任意 Range 的密文块解成明文块后直接推给播放端。
//
// 覆盖算法（与 unlock-music 官方 Rust 实现 lib_um_crypto_rust 对拍）：
//   - tc_tea：腾讯 16 轮 TEA + 变体 CBC（EKey 解密用）
//   - EKey：V1 单层 TEA 与 V2 "QQMusic EncV2,Key:" 双层 TEA
//   - QMCv2 Map：短密钥（≤300 字节）静态映射流密码
//   - QMCv2 RC4：长密钥（>300 字节）分段 RC4 流密码
//   - QMCv1：老静态密钥（qmcflac/qmc0 等本地文件，本服务端主要不涉及，保留备用）
package qmc

import "encoding/binary"

// tc_tea 常量：16 轮 TEA，delta 与标准 TEA 相同。
const (
	teaDelta  = 0x9E3779B9
	teaRounds = 16
	// 加密侧的结构：1 字节 pad 长度 + pad + 2 字节 salt | 明文 | 7 字节零尾巴。
	teaSaltLen = 2
	teaZeroLen = 7
)

// teaEcbDecrypt 标准 16 轮 TEA 解密一个 64bit 块（大端）。
func teaEcbDecrypt(block uint64, k [4]uint32) uint64 {
	y := uint32(block >> 32)
	z := uint32(block)
	sum := uint32(teaDelta*teaRounds & 0xFFFFFFFF)
	for i := 0; i < teaRounds; i++ {
		z -= teaSingleRound(y, sum, k[2], k[3])
		y -= teaSingleRound(z, sum, k[0], k[1])
		sum -= teaDelta
	}
	return uint64(y)<<32 | uint64(z)
}

// teaEcbEncrypt 标准 16 轮 TEA 加密一个 64bit 块（大端）。
func teaEcbEncrypt(block uint64, k [4]uint32) uint64 {
	y := uint32(block >> 32)
	z := uint32(block)
	var sum uint32
	for i := 0; i < teaRounds; i++ {
		sum += teaDelta
		y += teaSingleRound(z, sum, k[0], k[1])
		z += teaSingleRound(y, sum, k[2], k[3])
	}
	return uint64(y)<<32 | uint64(z)
}

// teaSingleRound TEA 单轮混合函数。
func teaSingleRound(value, sum, key1, key2 uint32) uint32 {
	left := (value << 4) + key1
	right := (value >> 5) + key2
	mid := sum + value
	return left ^ mid ^ right
}

// TeaCBCEncrypt tc_tea 加密（对应 Rust tc_tea::cbc::encrypt）。
// 测试与向量构造用；正常业务只解密。
func TeaCBCEncrypt(plain, key16, salt []byte) []byte {
	var k [4]uint32
	for i := range k {
		k[i] = binary.BigEndian.Uint32(key16[i*4:])
	}
	outLen := 10 + len(plain)
	padLen := (8 - outLen&7) & 7
	headerLen := 1 + padLen + teaSaltLen
	outLen += padLen

	// 头部：salt 填充，首字节低 3 位写 pad 长度，随后紧跟明文前缀。
	header := make([]byte, 16)
	copy(header, salt)
	header[0] = header[0]&^7 | byte(padLen)
	copyLen := min(16-headerLen, len(plain))
	copy(header[headerLen:], plain[:copyLen])
	rest := plain[copyLen:]

	iv1, iv2 := uint64(0), uint64(0)
	out := make([]byte, outLen)
	pos := 0
	encBlock := func(block []byte) []byte {
		b := binary.BigEndian.Uint64(block)
		iv2Next := b ^ iv1
		c := teaEcbEncrypt(iv2Next, k) ^ iv2
		iv1, iv2 = c, iv2Next
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], c)
		return buf[:]
	}
	copy(out[pos:pos+8], encBlock(header[0:8]))
	pos += 8
	copy(out[pos:pos+8], encBlock(header[8:16]))
	pos += 8
	for len(rest) >= 8 {
		copy(out[pos:pos+8], encBlock(rest[:8]))
		rest = rest[8:]
		pos += 8
	}
	if len(rest) > 0 {
		var last [8]byte
		copy(last[:], rest)
		copy(out[pos:pos+8], encBlock(last[:]))
	}
	return out[:outLen]
}

// TeaCBCDecrypt tc_tea 解密（对应 Rust tc_tea::cbc::decrypt）。
// 返回去除 padding/salt/零尾后的明文。
func TeaCBCDecrypt(cipherText, key16 []byte) ([]byte, error) {
	var k [4]uint32
	for i := range k {
		k[i] = binary.BigEndian.Uint32(key16[i*4:])
	}
	if len(cipherText)%8 != 0 || len(cipherText) < 10 {
		return nil, errInvalid("TEA: 无效密文长度")
	}
	plain := make([]byte, 0, len(cipherText))
	var iv1, iv2 uint64
	for i := 0; i < len(cipherText); i += 8 {
		block := binary.BigEndian.Uint64(cipherText[i:])
		iv2Next := teaEcbDecrypt(block^iv2, k)
		p := iv2Next ^ iv1
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], p)
		plain = append(plain, buf[:]...)
		iv1, iv2 = block, iv2Next
	}
	padSize := plain[0] & 0b111
	start := 1 + int(padSize) + teaSaltLen
	end := len(cipherText) - teaZeroLen
	if start < 0 || start > end || end > len(plain) {
		return nil, errInvalid("TEA: 无效 padding")
	}
	for _, b := range plain[end:] {
		if b != 0 {
			return nil, errInvalid("TEA: 尾部非零，padding 校验失败")
		}
	}
	return plain[start:end], nil
}
