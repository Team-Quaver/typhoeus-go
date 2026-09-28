package qmc

// Cipher 是「按绝对偏移可解密」的流密码抽象。
//
// 之所以用绝对偏移而不是顺序状态：播放端随时会 Range 跳转/重连，
// 解密必须能从任意字节偏移重启，且不维护任何跨请求可变状态——
// 这也是「仅内存、无落盘缓存」的天然配合。
type Cipher interface {
	// Decrypt 解密 data，data 对应密文文件中的绝对字节偏移 offset。
	// 返回新分配的明文切片；data 不被修改。
	Decrypt(data []byte, offset int64) []byte
}

// 主密钥长度分界：≤300 走 Map，>300 走 RC4（与官方客户端一致）。
const mapKeyMaxLen = 300

// NewCipher 按主密钥长度选择流密码（对应 Rust QMCv2Cipher::new）。
func NewCipher(masterKey []byte) (Cipher, error) {
	if len(masterKey) == 0 {
		return nil, ErrEmptyKey
	}
	if len(masterKey) <= mapKeyMaxLen {
		return NewMapCipher(masterKey)
	}
	return NewRC4Cipher(masterKey)
}

// NewCipherFromEkey 直接从 API 下发的 EKey 字符串构造流密码。
func NewCipherFromEkey(ekey string) (Cipher, error) {
	masterKey, err := DecryptEkey(ekey)
	if err != nil {
		return nil, err
	}
	return NewCipher(masterKey)
}

// DecryptFile 整文件解密（本地小文件场景/测试用；服务端流播走 Cipher.Decrypt）。
func DecryptFile(data []byte, masterKey []byte) ([]byte, error) {
	c, err := NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	return c.Decrypt(data, 0), nil
}

// DecryptV1File 用全局静态密钥整文件解密（QMCv1 老格式）。
func DecryptV1File(data []byte) []byte {
	return mapTransform(data, 0, &v1StaticKey)
}
