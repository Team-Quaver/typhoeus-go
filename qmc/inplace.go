package qmc

// InPlaceCipher 就地解密扩展（可选实现）。
//
// 流式中继在复用缓冲上解密时走这条路径：data 被原地改写，全程零额外分配。
// Cipher.Decrypt（返回新切片、不改输入）保持原语义不变——嗅探等调用方仍用它。
// 两种实现（MapCipher / RC4Cipher）都支持；用类型断言按需升级。
type InPlaceCipher interface {
	DecryptInPlace(data []byte, offset int64)
}

var _ InPlaceCipher = (*MapCipher)(nil)
var _ InPlaceCipher = (*RC4Cipher)(nil)

// DecryptInPlace 就地解密 Map 流密码。
func (c *MapCipher) DecryptInPlace(data []byte, offset int64) {
	xorMapInPlace(data, offset, c.key2)
}

// DecryptInPlace 就地解密 RC4 流密码。
func (c *RC4Cipher) DecryptInPlace(data []byte, offset int64) {
	c.decryptRC4(data, offset)
}
