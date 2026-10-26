package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// 音乐磁盘缓存（弱网优化；不是下载功能）。
//
// 布局（全部在配置目录 cache/music/ 下）：
//
//	index.quaverlocalcache   加密索引（首 64 字节是混淆噪声，JSON 之后 AEAD 加密）
//	<32hex>.quaverlocalcache 每首歌一个随机名分片文件（AES-CTR 密文；不含 MID/歌名）
//
// 密钥拆成两半：磁盘种子（索引头）+ 前端 seed（localStorage）。只拿磁盘（包括整个索引
// 与所有数据文件）解不开任何一首歌；只拿前端 seed 同样不行。索引里的 MID 也加密存储，
// 数据文件与文件名都不出现 QQ MID。
const (
	cacheIndexName = "index.quaverlocalcache"
	cacheFileExt   = ".quaverlocalcache"
	cacheHeaderLen = 64
	cacheChunkSize = 512 * 1024
	cacheMinBytes  = 256 << 20
	cacheMaxBytes  = 32 << 30
	cacheFormat    = 1
)

var (
	errCacheSeed  = errors.New("缓存密钥种子无效，请重新登录后再试")
	errCacheEntry = errors.New("缓存条目不存在或已过期")
)

type cacheEntry struct {
	ID       string    `json:"id"`
	UserID   int64     `json:"uid"`
	MidEnc   []byte    `json:"m"`
	MidMAC   []byte    `json:"x"`
	Tier     string    `json:"tier"`
	Mime     string    `json:"mime"`
	Size     int64     `json:"size"`
	Chunks   int       `json:"chunks"`
	Duration int64     `json:"duration_ms"`
	Played   int64     `json:"played"`
	At       time.Time `json:"at"`
}

type cacheIndex struct {
	Format   int          `json:"v"`
	Seed     string       `json:"s"`
	FrontMAC []byte       `json:"f"`
	Bytes    int64        `json:"b"`
	Items    []cacheEntry `json:"i"`
}

type cacheReader struct {
	entry    cacheEntry
	dataKey  []byte
	filePath string
	expires  time.Time
}

type musicCache struct {
	mu      sync.Mutex
	dir     string
	seed    string
	idx     cacheIndex
	readers map[string]*cacheReader
}

func newMusicCache() *musicCache {
	return &musicCache{
		dir:     filepath.Join(ConfigDir(), "cache", "music"),
		readers: map[string]*cacheReader{},
	}
}

func cacheKey(diskSeed, otherSeed, domain string) []byte {
	if len(diskSeed) < 32 || len(otherSeed) < 32 {
		return nil
	}
	h := sha256.New()
	_, _ = h.Write([]byte("quaver-localcache-v1\x00"))
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(diskSeed))
	_, _ = h.Write([]byte(otherSeed))
	return h.Sum(nil)
}

func cacheNonce(domain string, n uint64) []byte {
	var tail [8]byte
	binary.LittleEndian.PutUint64(tail[:], n)
	body := append(append([]byte(domain), 0), tail[:]...)
	sum := sha256.Sum256(body)
	return sum[len(sum)-16:]
}

func randomTag() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 不可用属于运行环境故障，快速失败优于落一个可预测缓存
	}
	return fmt.Sprintf("%x", b)
}

func cacheEntryIdentity(seed, mid string) (enc, mac []byte, err error) {
	key := cacheKey(seed, seed, "identity")
	if key == nil {
		return nil, nil, errCacheSeed
	}
	enc = make([]byte, len(mid)+16)
	if _, err := rand.Read(enc[:16]); err != nil {
		return nil, nil, err
	}
	copy(enc[16:], mid)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	cipher.NewCTR(block, cacheNonce("identity-enc", 0)).XORKeyStream(enc, enc)
	return enc, cacheMidMAC(seed, mid), nil
}

func cacheMidMAC(seed, mid string) []byte {
	key := cacheKey(seed, seed, "identity-mac")
	if key == nil {
		return nil
	}
	h := sha256.New()
	_, _ = h.Write(key)
	_, _ = h.Write([]byte(mid))
	return h.Sum(nil)
}

func cacheSameMAC(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (c *musicCache) openLocked(lazy bool) (bool, error) {
	if c.seed != "" {
		return true, nil
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return false, fmt.Errorf("创建缓存目录失败: %w", err)
	}
	path := filepath.Join(c.dir, cacheIndexName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if lazy {
			return false, nil
		}
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return false, err
		}
		c.seed = base64.RawStdEncoding.EncodeToString(seed)
		c.idx = cacheIndex{Format: cacheFormat, Seed: c.seed, Items: []cacheEntry{}}
		if err := c.saveLocked(); err != nil {
			c.seed = ""
			c.idx = cacheIndex{}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(raw) <= cacheHeaderLen {
		return false, errors.New("缓存索引损坏")
	}
	header := raw[:cacheHeaderLen]
	seed := base64.RawStdEncoding.EncodeToString(header[16:48])
	body, err := decryptCacheBlob(raw[cacheHeaderLen:], cacheKey(seed, seed, "index"))
	if err != nil {
		return false, errors.New("缓存索引损坏或密钥不匹配")
	}
	var idx cacheIndex
	if err := json.Unmarshal(body, &idx); err != nil || idx.Format != cacheFormat {
		return false, errors.New("缓存索引格式不支持")
	}
	c.idx = idx
	c.seed = idx.Seed
	return true, nil
}

func (c *musicCache) saveLocked() error {
	body, err := json.Marshal(c.idx)
	if err != nil {
		return err
	}
	key := cacheKey(c.seed, c.seed, "index")
	if key == nil {
		return errCacheSeed
	}
	blob, err := encryptCacheBlob(body, key)
	if err != nil {
		return err
	}
	header := make([]byte, cacheHeaderLen)
	if _, err := rand.Read(header); err != nil {
		return err
	}
	seed, err := base64.RawStdEncoding.DecodeString(c.seed)
	if err != nil || len(seed) != 32 {
		return errCacheSeed
	}
	copy(header[16:48], seed)
	out := append(header, blob...)
	tmp := filepath.Join(c.dir, ".index.tmp")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, cacheIndexName)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func encryptCacheBlob(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func decryptCacheBlob(blob, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("密文过短")
	}
	ns := gcm.NonceSize()
	return gcm.Open(nil, blob[:ns], blob[ns:], nil)
}

func (a *App) cacheSetSeed(seed string) error {
	if len(seed) < 32 {
		return errCacheSeed
	}
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.openLocked(false); err != nil {
		return err
	}
	frontMAC := cacheMidMAC(c.seed, seed)
	if cacheSameMAC(frontMAC, c.idx.FrontMAC) {
		return nil
	}
	_ = c.resetLocked("账号缓存种子变更")
	c.idx.FrontMAC = frontMAC
	return c.saveLocked()
}

func (c *musicCache) resetLocked(reason string) error {
	for _, e := range c.idx.Items {
		_ = os.Remove(c.fileOf(e))
	}
	_ = os.Remove(filepath.Join(c.dir, cacheIndexName))
	c.idx = cacheIndex{Format: cacheFormat, Seed: c.seed, Items: []cacheEntry{}}
	logInfo("音乐缓存已重置：%s", reason)
	return nil
}

func (c *musicCache) fileOf(e cacheEntry) string {
	return filepath.Join(c.dir, e.ID+cacheFileExt)
}

// cacheTryResolve 只返回与目标档位相同的缓存。
// 缓存的是某一档的完整音频，不是“这首歌随便一份”；若忽略 tier，用户切到 320/SQ/母带
// 仍会命中旧的 128/320 文件，UI 看似换了档、实际音质完全没变。
func (a *App) cacheTryResolve(mid, frontendSeed, tier string) (string, *cacheEntry, error) {
	if frontendSeed == "" {
		return "", nil, nil
	}
	uid := a.cacheUserID()
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	ok, err := c.openLocked(true)
	if err != nil || !ok {
		return "", nil, err
	}
	for _, e := range c.idx.Items {
		if e.UserID != uid || e.Tier != tier || !cacheSameMAC(cacheMidMAC(c.seed, mid), e.MidMAC) {
			continue
		}
		key := cacheKey(c.seed, frontendSeed, "data:"+e.ID)
		if key == nil {
			return "", nil, errCacheSeed
		}
		tok := newToken()
		c.readers[tok] = &cacheReader{
			entry: e, dataKey: key, filePath: c.fileOf(e),
			expires: time.Now().Add(streamIdleTTL),
		}
		return tok, &e, nil
	}
	return "", nil, nil
}

func (a *App) cacheResolvePlayback(token string) *cacheReader {
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.readers[token]
	if r == nil {
		return nil
	}
	if time.Now().After(r.expires) {
		delete(c.readers, token)
		return nil
	}
	r.expires = time.Now().Add(streamIdleTTL)
	return r
}

func (a *App) cacheStore(entry *streamEntry, mid, frontendSeed string, durationMs int64) {
	if frontendSeed == "" || entry.total <= 0 {
		return
	}
	uid := a.cacheUserID()
	c := a.musicCache
	c.mu.Lock()
	if _, err := c.openLocked(true); err != nil || c.seed == "" {
		c.mu.Unlock()
		return
	}
	mac := cacheMidMAC(c.seed, mid)
	for i := range c.idx.Items {
		e := &c.idx.Items[i]
		if e.UserID == uid && e.Tier == entry.tier && cacheSameMAC(mac, e.MidMAC) {
			e.Played++
			e.At = time.Now()
			_ = c.saveLocked()
			c.mu.Unlock()
			return
		}
	}
	id := randomTag()
	midEnc, midMAC, err := cacheEntryIdentity(c.seed, mid)
	if err != nil {
		c.mu.Unlock()
		return
	}
	key := cacheKey(c.seed, frontendSeed, "data:"+id)
	block, err := aes.NewCipher(key)
	if err != nil || key == nil {
		c.mu.Unlock()
		return
	}
	ctr := cipher.NewCTR(block, cacheNonce("data:"+id, 0))
	chunks := int((entry.total + cacheChunkSize - 1) / cacheChunkSize)
	bucket := a.paceBucket(entry.mid, paceRateFor(entry.total, durationMs))
	c.mu.Unlock()

	tmp := filepath.Join(c.dir, "."+id+".tmp")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	buf := make([]byte, cacheChunkSize)
	for start := int64(0); start < entry.total; {
		end := start + int64(len(buf)) - 1
		if end >= entry.total {
			end = entry.total - 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		up, err := typhoeus.OpenRange(ctx, entry.url, &typhoeus.ByteRange{Start: &start, End: &end})
		if err != nil {
			cancel()
			return
		}
		n, copyErr := copyEncrypting(&pacedWriter{w: f, b: bucket, ctx: ctx}, up.Body, entry.cipher, ctr, buf, start)
		closeErr := up.Body.Close()
		cancel()
		if copyErr != nil || closeErr != nil || n != end-start+1 {
			return
		}
		start = end + 1
	}
	if err := f.Sync(); err != nil {
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	item := cacheEntry{
		ID: id, UserID: uid, MidEnc: midEnc, MidMAC: midMAC, Tier: entry.tier,
		Mime: entry.mime, Size: entry.total, Chunks: chunks,
		Duration: durationMs, Played: 1, At: time.Now(),
	}
	if err := os.Rename(tmp, c.fileOf(item)); err != nil {
		_ = os.Remove(tmp)
		return
	}
	committed = true

	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.idx.Items {
		e := &c.idx.Items[i]
		if e.UserID == uid && e.Tier == item.Tier && cacheSameMAC(cacheMidMAC(c.seed, mid), e.MidMAC) {
			_ = os.Remove(c.fileOf(item))
			return
		}
	}
	c.idx.Items = append(c.idx.Items, item)
	c.idx.Bytes += item.Size
	c.evictLocked(a.cacheLimit.Load(), item.ID)
	if err := c.saveLocked(); err != nil {
		return
	}
	logInfo("音乐缓存已落盘：%s（%.1f MiB）", id, float64(item.Size)/(1<<20))
}

func copyEncrypting(dst io.Writer, src io.Reader, upstream qmc.Cipher, ctr cipher.Stream, buf []byte, start int64) (int64, error) {
	var total int64
	pos := start
	inplace, _ := upstream.(qmc.InPlaceCipher)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			switch {
			case inplace != nil:
				inplace.DecryptInPlace(chunk, pos)
			case upstream != nil:
				chunk = upstream.Decrypt(chunk, pos)
			}
			ctr.XORKeyStream(chunk, chunk)
			wn, werr := dst.Write(chunk)
			total += int64(wn)
			pos += int64(wn)
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

func (c *musicCache) evictLocked(limit int64, protectID string) {
	if limit < cacheMinBytes {
		limit = cacheMinBytes
	}
	if c.idx.Bytes <= limit {
		return
	}
	removed := map[string]bool{}
	items := append([]cacheEntry{}, c.idx.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].ID == protectID {
			return false
		}
		if items[j].ID == protectID {
			return true
		}
		return items[i].At.Before(items[j].At)
	})
	for _, e := range items {
		if c.idx.Bytes <= limit {
			break
		}
		if e.ID == protectID {
			continue
		}
		if err := os.Remove(c.fileOf(e)); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		c.idx.Bytes -= e.Size
		removed[e.ID] = true
	}
	if len(removed) == 0 {
		return
	}
	next := c.idx.Items[:0]
	for _, e := range c.idx.Items {
		if !removed[e.ID] {
			next = append(next, e)
		}
	}
	c.idx.Items = next
}

func (a *App) cacheUserID() int64 {
	cred := a.session.Credential()
	if cred != nil && cred.HasLogin() {
		return cred.MusicID
	}
	return -1
}

func (a *App) handleCacheSeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed string `json:"seed"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.cacheSetSeed(strings.TrimSpace(body.Seed)); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": true})
}

func (a *App) handleMusicCacheStatus(w http.ResponseWriter, _ *http.Request) {
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	ok, err := c.openLocked(true)
	if err != nil {
		writeError(w, err)
		return
	}
	if !ok {
		writeOK(w, map[string]any{"items": 0, "bytes": int64(0), "limit_bytes": a.cacheLimit.Load()})
		return
	}
	writeOK(w, map[string]any{"items": len(c.idx.Items), "bytes": c.idx.Bytes, "limit_bytes": a.cacheLimit.Load()})
}

func (a *App) handleMusicCacheClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed string `json:"seed"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.openLocked(false); err != nil {
		writeError(w, err)
		return
	}
	if err := c.resetLocked("用户清理缓存"); err != nil {
		writeError(w, err)
		return
	}
	if err := c.saveLocked(); err != nil {
		writeError(w, err)
		return
	}
	for tok := range c.readers {
		delete(c.readers, tok)
	}
	writeOK(w, map[string]any{"items": 0, "bytes": int64(0)})
}

func (a *App) handleMusicCacheSetLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bytes int64 `json:"bytes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Bytes != 0 && (body.Bytes < cacheMinBytes || body.Bytes > cacheMaxBytes) {
		writeErr(w, 422, -1, "缓存上限须在 256 MiB 到 32 GiB 之间")
		return
	}
	a.cacheLimit.Store(body.Bytes)
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.openLocked(true); err != nil || c.seed == "" {
		writeOK(w, map[string]any{"bytes": body.Bytes})
		return
	}
	c.evictLocked(body.Bytes, "")
	_ = c.saveLocked()
	writeOK(w, map[string]any{"items": len(c.idx.Items), "bytes": c.idx.Bytes})
}

func cachePlainMime(v string) string {
	mt, _, err := mime.ParseMediaType(v)
	if err != nil || !strings.Contains(mt, "/") {
		return "application/octet-stream"
	}
	return mt
}
