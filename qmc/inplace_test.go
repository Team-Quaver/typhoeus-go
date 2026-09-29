package qmc

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestInPlaceMatchesDecrypt 就地解密与返回新切片的 Decrypt 必须逐字节一致：
// 两条路径共享同一套算法语义，不同步就是静默坏流（_range 对拍）。
func TestInPlaceMatchesDecrypt(t *testing.T) {
	rng := rand.New(rand.NewSource(20260929))
	cases := []struct {
		name string
		key  []byte
	}{
		{"map-short", bytes.Repeat([]byte("mapkey"), 3)[:16]},        // 1..300 → Map
		{"map-300", bytes.Repeat([]byte("qwertyuiop"), 30)},          // 300 → Map
		{"rc4-512", bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz"), 20)[:512]}, // >300 → RC4
	}
	offsets := []int64{0, 1, 0x7F, 0x80, 0x1400, 0x2801, 32760, 32768, 999_999}
	for _, tc := range cases {
		c, err := NewCipher(tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, off := range offsets {
			data := make([]byte, 4096+64)
			rng.Read(data)
			want := c.Decrypt(data, off)
			got := append([]byte{}, data...)
			inplace, ok := c.(InPlaceCipher)
			if !ok {
				t.Fatalf("%s: 未实现 InPlaceCipher", tc.name)
			}
			inplace.DecryptInPlace(got, off)
			if !bytes.Equal(want, got) {
				t.Fatalf("%s @%d: 就地解密与 Decrypt 不一致", tc.name, off)
			}
			// XOR 变换是对合的：再解一次应回到原文（顺带验证无累积状态）
			inplace.DecryptInPlace(got, off)
			if !bytes.Equal(got, data) {
				t.Fatalf("%s @%d: 二次解密未还原明文（存在跨调用状态）", tc.name, off)
			}
		}
	}
}

func BenchmarkMapCipherDecrypt(b *testing.B) {
	key := bytes.Repeat([]byte("x"), 296)
	c, _ := NewMapCipher(key)
	data := make([]byte, 256*1024)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c.Decrypt(data, int64(i)*int64(len(data)))
	}
}

func BenchmarkMapCipherInPlace(b *testing.B) {
	key := bytes.Repeat([]byte("x"), 296)
	c, _ := NewMapCipher(key)
	data := make([]byte, 256*1024)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c.DecryptInPlace(data, int64(i)*int64(len(data)))
	}
}

func BenchmarkRC4Decrypt(b *testing.B) {
	key := bytes.Repeat([]byte("y"), 512)
	c, _ := NewRC4Cipher(key)
	data := make([]byte, 256*1024)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c.Decrypt(data, int64(i)*int64(len(data)))
	}
}

func BenchmarkRC4InPlace(b *testing.B) {
	key := bytes.Repeat([]byte("y"), 512)
	c, _ := NewRC4Cipher(key)
	data := make([]byte, 256*1024)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c.DecryptInPlace(data, int64(i)*int64(len(data)))
	}
}

// BenchmarkRC4SeekAhead 模拟 mpv 拖动进度条：从远端偏移开始解密一块。
// RC4 分段以绝对偏移计算，随机 seek 与顺序读成本相同（无重放惩罚）——
// 基准固定它不随 offset 退化，防将来有人把状态化实现引进来。
func BenchmarkRC4SeekAhead(b *testing.B) {
	key := bytes.Repeat([]byte("y"), 512)
	c, _ := NewRC4Cipher(key)
	data := make([]byte, 256*1024)
	b.SetBytes(int64(len(data)))
	const far = int64(1) << 30 // 1GiB 处
	for i := 0; i < b.N; i++ {
		c.DecryptInPlace(data, far+int64(i)*int64(len(data)))
	}
}
