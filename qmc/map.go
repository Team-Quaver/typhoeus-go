package qmc

// QMC 静态映射类流密码：
//   - V1 固定 128 字节密钥（老 qmcflac/qmc0 等本地文件）
//   - QMCv2 Map：主密钥压缩成 128 字节后按同一套变换异或
//
// 变换语义（务必与 unlock-music 对齐）：字节绝对偏移 offset 超过 0x7FFF 时
// 先对 0x7FFF 取模，再对 128 取模得密钥索引。因此索引 0x7FFF→127、0x8000→1，
// 周期在 0x7FFF 处「跳变」一次，是逆向出的原始行为，不能「优化」掉。

const (
	v1KeySize       = 128
	v1OffsetBoundary = 0x7FFF
)

// v1StaticKey QMCv1 全局静态密钥（Rust lib_um_crypto_rust: qmc::v1::V1_STATIC_KEY）。
var v1StaticKey = [v1KeySize]byte{
	0xc3, 0x4a, 0xd6, 0xca, 0x90, 0x67, 0xf7, 0x52, 0xd8, 0xa1, 0x66, 0x62, 0x9f, 0x5b, 0x09, 0x00,
	0xc3, 0x5e, 0x95, 0x23, 0x9f, 0x13, 0x11, 0x7e, 0xd8, 0x92, 0x3f, 0xbc, 0x90, 0xbb, 0x74, 0x0e,
	0xc3, 0x47, 0x74, 0x3d, 0x90, 0xaa, 0x3f, 0x51, 0xd8, 0xf4, 0x11, 0x84, 0x9f, 0xde, 0x95, 0x1d,
	0xc3, 0xc6, 0x09, 0xd5, 0x9f, 0xfa, 0x66, 0xf9, 0xd8, 0xf0, 0xf7, 0xa0, 0x90, 0xa1, 0xd6, 0xf3,
	0xc3, 0xf3, 0xd6, 0xa1, 0x90, 0xa0, 0xf7, 0xf0, 0xd8, 0xf9, 0x66, 0xfa, 0x9f, 0xd5, 0x09, 0xc6,
	0xc3, 0x1d, 0x95, 0xde, 0x9f, 0x84, 0x11, 0xf4, 0xd8, 0x51, 0x3f, 0xaa, 0x90, 0x3d, 0x74, 0x47,
	0xc3, 0x0e, 0x74, 0xbb, 0x90, 0xbc, 0x3f, 0x92, 0xd8, 0x7e, 0x11, 0x13, 0x9f, 0x23, 0x95, 0x5e,
	0xc3, 0x00, 0x09, 0x5b, 0x9f, 0x62, 0x66, 0xa1, 0xd8, 0x52, 0xf7, 0x67, 0x90, 0xca, 0xd6, 0x4a,
}

// mapByteAt 静态映射单字节变换（key 长度必须为 128）。
func mapByteAt(key *[v1KeySize]byte, value byte, offset int64) byte {
	off := offset
	if off > v1OffsetBoundary {
		off %= v1OffsetBoundary
	}
	return value ^ key[off%v1KeySize]
}

// mapTransform 对 data（绝对起点 offset）整段做静态映射变换，返回新切片。
func mapTransform(data []byte, offset int64, key *[v1KeySize]byte) []byte {
	out := make([]byte, len(data))
	pos := offset
	for i := 0; i < len(data); {
		var take int
		var phase int64
		switch {
		case pos <= v1OffsetBoundary:
			// [0, 0x7FFF]：索引 = pos % 128（含 0x7FFF -> 127）
			take = min(int(0x8000-pos), len(data)-i)
			phase = pos % v1KeySize
		default:
			// (0x7FFF, …)：索引 = (pos % 0x7FFF) % 128
			r := pos % v1OffsetBoundary
			take = min(int(v1OffsetBoundary-r), len(data)-i)
			phase = r % v1KeySize
		}
		for j := 0; j < take; j++ {
			out[i+j] = data[i+j] ^ key[(phase+int64(j))%v1KeySize]
		}
		i += take
		pos += int64(take)
	}
	return out
}

// key_compress 对应 Rust v2_map/key.rs：任意长度主密钥压缩成 128 字节映射密钥。
// 注意 (key<<shift)|(key>>shift) 不是循环移位——保持原样以对拍 Rust。
func keyCompress(longKey []byte) [v1KeySize]byte {
	var result [v1KeySize]byte
	n := len(longKey)
	for i := 0; i < v1KeySize; i++ {
		idx := (i*i + 71214) % n
		key := uint32(longKey[idx])
		shift := uint32((idx + 4) % 8)
		result[i] = byte(((key << shift) | (key >> shift)) & 0xFF)
	}
	return result
}

// MapCipher QMCv2 短密钥（1..300 字节）Map 流密码。
type MapCipher struct {
	key [v1KeySize]byte
}

// NewMapCipher 由主密钥构造 Map 密码。
func NewMapCipher(masterKey []byte) (*MapCipher, error) {
	if len(masterKey) == 0 {
		return nil, ErrEmptyKey
	}
	return &MapCipher{key: keyCompress(masterKey)}, nil
}

// Decrypt 解密 data（对应密文在文件中的绝对偏移 offset），返回新切片。
func (c *MapCipher) Decrypt(data []byte, offset int64) []byte {
	return mapTransform(data, offset, &c.key)
}
