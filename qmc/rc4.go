package qmc

import "math"

// QMCv2 RC4（长密钥）流密码，对应 Rust lib_um_crypto_rust::qmc::v2_rc4。
//
// 文件按固定分段异或：
//   - 首段 0x80 字节：每字节独立按 get_segment_key(绝对偏移, key[偏移%len], hash) 取 key 索引
//   - 其余按 0x1400 字节分段：段内直接用预生成的 0x1600 长度 key_stream 切片异或，
//     每段跳过 skip = get_segment_key(段号, seed, hash) & 0x1FF 字节
//
// 两类分段都以「绝对偏移」为准，因此天然支持任意 Range 的按需解密。
const (
	rc4FirstSegmentSize = 0x0080
	rc4OtherSegmentSize = 0x1400
	// 预生成密钥流长度 = 一个完整段 + 512（skip 上限 0x1FF）。
	rc4StreamCacheSize = rc4OtherSegmentSize + 512
)

// modifiedRC4 变体 RC4：状态长度 = 密钥长度（非 256），状态元素按 u8 回卷。
type modifiedRC4 struct {
	state []byte
	i, j  int
	n     int
}

func newModifiedRC4(key []byte) *modifiedRC4 {
	n := len(key)
	state := make([]byte, n)
	for i := range state {
		state[i] = byte(i)
	}
	j := 0
	for i := 0; i < n; i++ {
		j = (j + int(state[i]) + int(key[i%n])) % n
		state[i], state[j] = state[j], state[i]
	}
	return &modifiedRC4{state: state, n: n}
}

// generate 产出一个密钥流字节。
func (r *modifiedRC4) generate() byte {
	n := r.n
	r.i = (r.i + 1) % n
	r.j = (r.j + int(r.state[r.i])) % n
	r.state[r.i], r.state[r.j] = r.state[r.j], r.state[r.i]
	idx := (int(r.state[r.i]) + int(r.state[r.j])) % n
	return r.state[idx]
}

// rc4Hash 对应 Rust v2_rc4/hash.rs：u32 逐步自乘，一旦不严格递增即停（f64 返回）。
func rc4Hash(key []byte) float64 {
	h := uint64(1)
	for _, v := range key {
		if v == 0 {
			continue
		}
		nxt := (h * uint64(v)) & 0xFFFFFFFF
		if nxt == 0 || nxt <= h {
			break
		}
		h = nxt
	}
	return float64(h)
}

// getSegmentKey 对应 Rust v2_rc4/segment_key.rs。
func getSegmentKey(id int64, seed byte, hash float64) float64 {
	if seed == 0 {
		return 0
	}
	denom := uint64(id+1) * uint64(seed)
	return math.Trunc(hash / float64(denom) * 100.0)
}

// RC4Cipher QMCv2 长密钥（>300 字节）RC4 流密码。
type RC4Cipher struct {
	key       []byte
	hash      float64
	keyStream [rc4StreamCacheSize]byte
}

// NewRC4Cipher 由主密钥构造 RC4 密码。
func NewRC4Cipher(masterKey []byte) (*RC4Cipher, error) {
	if len(masterKey) == 0 {
		return nil, ErrEmptyKey
	}
	c := &RC4Cipher{key: append([]byte{}, masterKey...), hash: rc4Hash(masterKey)}
	gen := newModifiedRC4(masterKey)
	for i := range c.keyStream {
		c.keyStream[i] = gen.generate()
	}
	return c, nil
}

// Decrypt 解密 data（对应密文在文件中的绝对偏移 offset），返回新切片。
func (c *RC4Cipher) Decrypt(data []byte, offset int64) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	n := len(out)
	pos := offset
	start := 0
	if pos < rc4FirstSegmentSize {
		take := min(int(rc4FirstSegmentSize-pos), n-start)
		c.xorFirstSegment(out[start:start+take], pos)
		start += take
		pos += int64(take)
	}
	if rem := pos % rc4OtherSegmentSize; rem != 0 {
		take := min(int(rc4OtherSegmentSize-rem), n-start)
		c.xorOtherSegment(out[start:start+take], pos)
		start += take
		pos += int64(take)
	}
	for start < n {
		take := min(rc4OtherSegmentSize, n-start)
		c.xorOtherSegment(out[start:start+take], pos)
		start += take
		pos += int64(take)
	}
	return out
}

// xorFirstSegment 首段（0x80 字节内）：逐字节独立取 key 索引。
func (c *RC4Cipher) xorFirstSegment(buf []byte, offset int64) {
	n := len(c.key)
	for j := range buf {
		o := offset + int64(j)
		idx := int(math.Mod(getSegmentKey(o, c.key[o%int64(n)], c.hash), float64(n)))
		buf[j] ^= c.key[idx]
	}
}

// xorOtherSegment 其余段：段内用 keyStream 的连续切片异或。
func (c *RC4Cipher) xorOtherSegment(buf []byte, offset int64) {
	n := len(c.key)
	segID := offset / rc4OtherSegmentSize
	blockOff := int(offset % rc4OtherSegmentSize)
	seed := c.key[int(segID)%n]
	skip := int(getSegmentKey(segID, seed, c.hash)) & 0x1FF
	for j := range buf {
		buf[j] ^= c.keyStream[skip+blockOff+j]
	}
}
