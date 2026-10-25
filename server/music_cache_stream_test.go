package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"io"
	"testing"
	"time"
)

func TestCacheDecryptReaderSeeks(t *testing.T) {
	seed := "0123456789abcdef0123456789abcdef"
	entryID := randomTag()
	key := cacheKey(seed, seed, "data:"+entryID)
	if key == nil {
		t.Fatal("nil key")
	}
	plain := bytes.Repeat([]byte("quaver-cache!"), 100)
	block, _ := aes.NewCipher(key)
	enc := cipher.NewCTR(block, cacheNonce("data:"+entryID, 0))
	cipherText := append([]byte(nil), plain...)
	enc.XORKeyStream(cipherText, cipherText)

	for _, offset := range []int64{0, 1, 7, 15, 16, 17, 99, 1200} {
		want := plain[offset:]
		got := make([]byte, len(want))
		r := &cacheDecryptReader{
			r: bytes.NewReader(cipherText[offset:]), key: key, entryID: entryID, off: offset,
		}
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("offset %d mismatch", offset)
		}
	}

	// reader 播放态滑动续期：30 分钟内重复取用不失效
	app := &App{musicCache: &musicCache{readers: map[string]*cacheReader{}}}
	app.musicCache.readers["tok"] = &cacheReader{expires: time.Now().Add(time.Minute)}
	if app.cacheResolvePlayback("tok") == nil {
		t.Fatal("live token rejected")
	}
}
