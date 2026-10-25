package server

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// handleMusicCacheStream GET /cache/music/stream/{token}
//
// 与 /stream/{token} 同一套 Sec-Fetch 分类：document/新标签页/整段 fetch 一律 403，
// 页面 fetch 只允许 ≤64KiB 的闭合 Range（音质信息浮窗探测）。命中只读本地密文，
// 不访问网络，也不暴露缓存条目 ID / MID。
func (a *App) handleMusicCacheStream(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	cls := classifyStreamClient(r)
	var probeSpan int64
	switch cls {
	case classBlocked:
		writeErr(w, http.StatusForbidden, -1, "缓存播放流仅允许应用内播放器取用")
		return
	case classProbe:
		span, ok := closedRangeSpan(r.Header.Get("Range"))
		if !ok || span > probeMaxSpan {
			writeErr(w, http.StatusForbidden, -1, "页面 fetch 仅允许小范围探测（取容器头），不支持整段读取缓存")
			return
		}
		probeSpan = span
	case classPlayer, classMedia:
	}
	reader := a.cacheResolvePlayback(token)
	if reader == nil {
		writeErr(w, http.StatusNotFound, -1, "缓存播放流不存在或已过期，请重新播放")
		return
	}
	_ = probeSpan // 放行额度与在线流保持同一语义；缓存读取没有网络回源成本

	rng := parseSimpleRange(r.Header.Get("Range"), reader.entry.Size)
	if rng == nil {
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, -1, "缓存 Range 无效")
		return
	}
	f, err := os.Open(reader.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, -1, "缓存数据已被清理，请重新播放")
			return
		}
		writeErr(w, http.StatusInternalServerError, -1, "打开缓存文件失败")
		return
	}
	defer f.Close()
	if _, err := f.Seek(rng.start, 0); err != nil {
		writeErr(w, http.StatusInternalServerError, -1, "读取缓存失败")
		return
	}
	headers := w.Header()
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Content-Type", cachePlainMime(reader.entry.Mime))
	headers.Set("Cache-Control", "no-store")
	headers.Set("Content-Length", fmt.Sprintf("%d", rng.length))
	if rng.start != 0 || rng.length != reader.entry.Size {
		headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, reader.entry.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.CopyN(w, &cacheDecryptReader{r: f, key: reader.dataKey, entryID: reader.entry.ID, off: rng.start}, rng.length)
}

// cacheDecryptReader 按绝对偏移解密缓存密文。AES-CTR 可从任意块边界重启：
// 先消费掉块内偏移，再进入常规流式解密，seek 无需重放前文。
type cacheDecryptReader struct {
	r       io.Reader
	key     []byte
	entryID string
	off     int64
	ready   bool
	stream  cipher.Stream
	block   cipher.Block
}

func (c *cacheDecryptReader) init() error {
	if c.ready {
		return nil
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return err
	}
	c.block = block
	nonce := cacheNonce("data:"+c.entryID, 0)
	skip := int(c.off % 16)
	iv := make([]byte, len(nonce))
	copy(iv, nonce)
	for i := int64(0); i < c.off/16; i++ {
		ivInc(iv)
	}
	base := cipher.NewCTR(block, iv)
	if skip > 0 {
		// CTR keystream 按块推进；跳过块内偏移后再解密后续读到的数据。
		c.stream = &skipStream{inner: base, n: skip}
	} else {
		c.stream = base
	}
	c.ready = true
	return nil
}

func ivInc(iv []byte) {
	for i := len(iv) - 1; i >= 0; i-- {
		iv[i]++
		if iv[i] != 0 {
			break
		}
	}
}

func (c *cacheDecryptReader) Read(p []byte) (int, error) {
	if err := c.init(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.stream.XORKeyStream(p[:n], p[:n])
		c.off += int64(n)
	}
	return n, err
}

type simpleRange struct {
	start, length int64
}

type skipStream struct {
	inner cipher.Stream
	n     int
}

func (s *skipStream) XORKeyStream(dst, src []byte) {
	if s.n > 0 {
		skip := make([]byte, s.n)
		s.inner.XORKeyStream(skip, skip)
		s.n = 0
	}
	s.inner.XORKeyStream(dst, src)
}

func parseSimpleRange(header string, total int64) *simpleRange {
	if strings.TrimSpace(header) == "" {
		return &simpleRange{start: 0, length: total}
	}
	r := typhoeus.ParseRange(header)
	if r == nil || r.Start == nil {
		return nil
	}
	start := *r.Start
	if start < 0 || start >= total {
		return nil
	}
	end := total - 1
	if r.End != nil && *r.End < end {
		end = *r.End
	}
	if end < start {
		return nil
	}
	return &simpleRange{start: start, length: end - start + 1}
}
