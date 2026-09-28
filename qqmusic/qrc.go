package qqmusic

// QRC/LRC 歌词密文解密（对标 qqmusic_api/algorithms/__init__.py:qrc_decrypt）：
// hex 解码 → 自定义 3DES 变体（见 qrc_3des.go）→ zlib 解压 → UTF-8。
// 解不开时返回空串（调用方保留原值，语义与 SDK 的 contextlib.suppress 一致）。

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"io"
	"strings"
)

var qrcTripleDESKey = []byte("!@#)(*$%123ZXC!@!@#)(NHL")

// DecryptQRC 解密歌词密文。
func DecryptQRC(encrypted string) string {
	if encrypted == "" {
		return ""
	}
	cipherText, err := hex.DecodeString(strings.TrimSpace(encrypted))
	if err != nil || len(cipherText) == 0 || len(cipherText)%8 != 0 {
		return ""
	}
	schedule := qrcTripledesKeySetup(qrcTripleDESKey, qrcDecrypt)
	plain := make([]byte, 0, len(cipherText))
	for off := 0; off+8 <= len(cipherText); off += 8 {
		plain = append(plain, qrcTripledesCrypt(cipherText[off:off+8], schedule)...)
	}
	zr, err := zlib.NewReader(bytes.NewReader(plain))
	if err != nil {
		return ""
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return ""
	}
	return string(out)
}
