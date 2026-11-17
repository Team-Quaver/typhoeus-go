package server

import (
	"bytes"
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
// 位置：缓存根目录下的 music/（CacheDir，见 session.go）。Linux 走 XDG_CACHE_HOME，
// Windows/macOS 是与 Chromium 的 Cache 明确区分开的 Quaver Cache / Caches；
// 用户可在设置页自定义位置（主进程经 QUAVER_CACHE_DIR 下传，或 POST /cache/music/dir 热切换）。
// 旧版落在配置目录 cache/music 下，首次打开时整目录搬家（adoptLegacyDirLocked）。
//
// 布局（全部在该目录下）：
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

// musicCacheDir 音乐缓存目录 = 缓存根目录/music。
func musicCacheDir() string {
	return filepath.Join(CacheDir(), "music")
}

func newMusicCache() *musicCache {
	return &musicCache{
		dir:     musicCacheDir(),
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
	c.adoptLegacyDirLocked()
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return false, fmt.Errorf("创建缓存目录失败: %w", err)
	}
	// 上次进程在写缓存中途退出会留下 .*.tmp 残片（cacheStore 的清理
	// defer 只在正常返回时执行）。这些文件不在索引里，用户「清理缓存」
	// 后仍白占磁盘，所以进程内第一次打开缓存时先扫掉。
	c.removeTempsLocked()
	path := filepath.Join(c.dir, cacheIndexName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c.cleanupLocked()
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
	c.cleanupLocked()
	return true, nil
}

// adoptLegacyDirLocked 把旧版落在配置目录里的缓存整目录搬到默认缓存目录。
//
// 只在「当前目录就是平台默认缓存目录」时做：自定义位置是用户显式指定的空目录，
// 不该被旧数据塞满（也不该把别处的缓存搬走）。只做一次、只在目标还没有索引时做。
//
// **搬家按文件确认，不是「尽力而为后清场」**：只有真的落到新目录的文件才从旧目录删除。
// 搬不动（目标只读 / 配额满 / 单文件权限）就原样留在旧目录 —— 宁可留一份用户还能切回去
// 用的缓存，也绝不能「新目录空着、旧目录被清空」（那等于升级白删几个 GiB）。
// 索引最后搬：它在，才代表这份缓存完整可用；先搬索引会在中途失败时留下指向空气的索引。
func (c *musicCache) adoptLegacyDirLocked() {
	if c.dir != musicCacheDir() {
		return
	}
	legacy := LegacyMusicCacheDir()
	if legacy == c.dir {
		return
	}
	if _, err := os.Stat(filepath.Join(legacy, cacheIndexName)); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(c.dir, cacheIndexName)); err == nil {
		// 新目录已有完整的一份：旧目录是上一轮搬完剩下的残片，收掉。
		if sweepCacheDir(legacy) {
			_ = os.Remove(legacy)
			_ = os.Remove(filepath.Dir(legacy))
		}
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return
	}
	moved := 0
	failed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !cacheOwnedName(name) || name == cacheIndexName {
			continue
		}
		if err := moveCacheFile(filepath.Join(legacy, name), filepath.Join(c.dir, name)); err != nil {
			failed++
			continue
		}
		_ = os.Remove(filepath.Join(legacy, name)) // 复制分支的收尾；rename 分支已删过
		moved++
	}
	if failed > 0 {
		logWarn("音乐缓存搬家未完成：%d 个文件搬不动，旧目录内容保留（%s）", failed, legacy)
		return
	}
	if err := moveCacheFile(filepath.Join(legacy, cacheIndexName), filepath.Join(c.dir, cacheIndexName)); err != nil {
		logWarn("音乐缓存索引搬家失败，旧目录内容保留（%s）: %v", legacy, err)
		return
	}
	_ = os.Remove(filepath.Join(legacy, cacheIndexName))
	if moved > 0 {
		logInfo("音乐缓存已迁至缓存目录：%s（%d 个文件）", c.dir, moved)
	}
	if sweepCacheDir(legacy) {
		_ = os.Remove(legacy)
		_ = os.Remove(filepath.Dir(legacy)) // cache/：只有本缓存用过，空了就收
	}
}

// sweepCacheDir 删掉 dir 下属于本缓存的文件（索引 / 分片 / .tmp），别人的文件一个都不碰。
// 返回「目录是否已经空了」——调用方据此决定要不要把目录本身也收掉。
func sweepCacheDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	rest := 0
	for _, entry := range entries {
		if entry.IsDir() || !cacheOwnedName(entry.Name()) {
			rest++
			continue
		}
		if os.Remove(filepath.Join(dir, entry.Name())) != nil {
			rest++
		}
	}
	return rest == 0
}

// cacheOwnedName 这个文件名是否属于本缓存（索引 / 分片 / 写盘中断的 .tmp）。
func cacheOwnedName(name string) bool {
	return name == cacheIndexName || strings.HasSuffix(name, cacheFileExt) || strings.HasSuffix(name, ".tmp")
}

// moveCacheFile 先 rename（同盘瞬时），跨盘再退回「复制 + 删源」。
// 复制走 .tmp 中转再改名：中途断电不会在目标留下一个看似完整的半截分片。
func moveCacheFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := to + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, to); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(from)
	return nil
}

// cleanupLocked removes interrupted cache writes and data files that are no
// longer referenced by the index. Without this, a failed cache store leaves a
// multi-megabyte .tmp forever, and seed resets leave full tracks orphaned.
func (c *musicCache) cleanupLocked() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	valid := map[string]bool{cacheIndexName: true}
	for _, item := range c.idx.Items {
		valid[item.ID+cacheFileExt] = true
	}
	var freed int64
	var removed int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		drop := strings.HasSuffix(name, ".tmp")
		if !drop && strings.HasSuffix(name, cacheFileExt) && !valid[name] {
			drop = true
		}
		if !drop {
			continue
		}
		if info, ierr := entry.Info(); ierr == nil {
			freed += info.Size()
		}
		if os.Remove(filepath.Join(c.dir, name)) == nil {
			removed++
		}
	}
	if removed > 0 {
		logInfo("音乐缓存清理临时/孤立文件：%d 个，释放 %.1f MiB", removed, float64(freed)/(1<<20))
	}
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
	// 按目录内容清，而不是只清索引里的条目：写盘中途退出留下的 .tmp、
	// 以及索引损坏后来不及登记的孤儿分片都占磁盘，用户点「清理缓存」
	// 必须真的归零。目录里只有本缓存的 index / 分片 / 临时文件。
	c.removeFilesLocked(func(name string) bool {
		return strings.HasSuffix(name, cacheFileExt) || strings.HasSuffix(name, ".tmp")
	})
	c.idx = cacheIndex{Format: cacheFormat, Seed: c.seed, Items: []cacheEntry{}}
	logInfo("音乐缓存已重置：%s", reason)
	return nil
}

// removeTempsLocked 清理未提交的临时文件（cacheStore 中断残留）。
func (c *musicCache) removeTempsLocked() {
	c.removeFilesLocked(func(name string) bool { return strings.HasSuffix(name, ".tmp") })
}

// removeFilesLocked 删除 dir 下满足条件的普通文件；目录不可读时静默跳过。
func (c *musicCache) removeFilesLocked(match func(name string) bool) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !match(entry.Name()) {
			continue
		}
		_ = os.Remove(filepath.Join(c.dir, entry.Name()))
	}
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
	// The cache deliberately has no separate bucket. It shares the playback
	// bucket, so cache traffic can never claim the 3x realtime allowance while
	// playback is consuming 1x. Once playback is buffered, the remaining
	// headroom fills the cache without causing stalls.
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
	// One sequential response covers the whole body. Issuing many adjacent
	// Range requests raced the playback relay and could hand the cache a
	// truncated segment; a single stream keeps the absolute QMC offset
	// contiguous and avoids extra CDN round trips on track changes.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if refreshable, ok := entry.opener.(streamSourceRefresh); ok {
		if next, rerr := refreshable.Refresh(ctx); rerr == nil {
			entry.opener = next
		}
	}
	var up *typhoeus.UpstreamResponse
	start, end := int64(0), entry.total-1
	if entry.opener != nil {
		up, err = entry.opener.OpenRange(ctx, entry.url, &typhoeus.ByteRange{Start: &start, End: &end})
	} else {
		up, err = typhoeus.OpenRange(ctx, entry.url, &typhoeus.ByteRange{Start: &start, End: &end})
	}
	if err != nil {
		return
	}
	if err := validateRangeResponse(up, start); err != nil {
		_ = up.Body.Close()
		logWarn("音乐缓存回源 Range 异常，放弃本条缓存: %v", err)
		return
	}
	n, copyErr := copyEncrypting(&pacedWriter{w: f, b: bucket, ctx: ctx}, up.Body, entry.cipher, ctr, buf, start)
	closeErr := up.Body.Close()
	if copyErr != nil || closeErr != nil || n != entry.total {
		logWarn("音乐缓存回源中断（%d/%d 字节）: %v", n, entry.total, copyErr)
		return
	}
	if err := f.Sync(); err != nil {
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	if err := validateCachedAudio(tmp, key, id, entry.mime); err != nil {
		logWarn("音乐缓存解密校验失败，丢弃本条: %v", err)
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

// cacheAfterPlayback starts the encrypted cache only once the playback relay
// has successfully delivered bytes. It is safe to call on every Range request:
// streamEntry.cacheStarted is guarded by streamMu and makes the start idempotent.
func (a *App) cacheAfterPlayback(entry *streamEntry) {
	if entry == nil || entry.cacheSeed == "" {
		return
	}
	a.streamMu.Lock()
	if entry.cacheStarted {
		a.streamMu.Unlock()
		return
	}
	entry.cacheStarted = true
	seed, mid, duration := entry.cacheSeed, entry.mid, entry.durationMs
	a.streamMu.Unlock()

	go func() {
		// Wait until no player request is reading this stream, then add a short
		// grace period so a seek/restart request is not missed. The cache is a
		// post-playback fill, not a second simultaneous reader competing for the
		// same upstream connection and rate bucket.
		for {
			a.streamMu.Lock()
			active := entry.activeRequests
			a.streamMu.Unlock()

			if active == 0 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)

		a.streamMu.Lock()
		active := entry.activeRequests
		a.streamMu.Unlock()
		if active == 0 {
			a.cacheStore(entry, mid, seed, duration)
		}
	}()
}

// validateCachedAudio decrypts the first bytes of a completed cache file and
// checks for a recognizable audio container. A wrong QMC offset can still
// produce a full-length file, so length alone is not a sufficient commit gate.
func validateCachedAudio(path string, key []byte, entryID, mime string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, err := io.ReadFull(&cacheDecryptReader{r: f, key: key, entryID: entryID}, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	head = head[:n]
	switch cachePlainMime(mime) {
	case "audio/ogg":
		if !bytes.HasPrefix(head, []byte("OggS")) {
			return errors.New("缓存解密结果不是 Ogg 容器")
		}
	case "audio/flac":
		if !bytes.HasPrefix(head, []byte("fLaC")) {
			return errors.New("缓存解密结果不是 FLAC 容器")
		}
	case "audio/wav":
		if !bytes.HasPrefix(head, []byte("RIFF")) {
			return errors.New("缓存解密结果不是 WAV 容器")
		}
	case "audio/mpeg":
		if !(bytes.HasPrefix(head, []byte{0xff, 0xfb}) || bytes.HasPrefix(head, []byte{0xff, 0xf3}) ||
			bytes.HasPrefix(head, []byte{0xff, 0xf2}) || bytes.HasPrefix(head, []byte("ID3"))) {
			return errors.New("缓存解密结果不是 MPEG 音频")
		}
	}
	return nil
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

// cacheSetDir 换音乐缓存目录（设置页「缓存位置」）。root 为空 = 回落平台默认缓存根目录。
//
// 已有缓存跟着搬：不搬就是「换个位置 = 几个 GiB 白占着旧盘」，而用户在界面上只看到
// 「0 首」。搬家在锁内做（与落盘/清理互斥），且**按文件确认**：只有真的搬成功的才删源，
// 有任何一个搬不动就整份留在旧目录并报错 —— 绝不「新目录空着、旧目录被清空」。
func (a *App) cacheSetDir(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root != "" && !filepath.IsAbs(root) {
		return "", errors.New("缓存位置必须是绝对路径")
	}
	dir := musicCacheDir()
	if root != "" {
		dir = filepath.Join(filepath.Clean(root), "music")
	}
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if dir == c.dir {
		return c.dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("缓存位置不可用: %w", err)
	}
	probe := filepath.Join(dir, ".write-probe.tmp")
	if err := os.WriteFile(probe, []byte("quaver"), 0o600); err != nil {
		return "", fmt.Errorf("缓存位置不可写: %w", err)
	}
	_ = os.Remove(probe)

	from := c.dir
	if _, err := os.Stat(filepath.Join(from, cacheIndexName)); err == nil {
		_, dstErr := os.Stat(filepath.Join(dir, cacheIndexName))
		freshTarget := errors.Is(dstErr, os.ErrNotExist)
		if freshTarget {
			entries, rerr := os.ReadDir(from)
			if rerr != nil {
				return "", fmt.Errorf("读取原缓存目录失败: %w", rerr)
			}
			failed := 0
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !cacheOwnedName(name) || name == cacheIndexName {
					continue
				}
				if err := moveCacheFile(filepath.Join(from, name), filepath.Join(dir, name)); err != nil {
					failed++
					continue
				}
				_ = os.Remove(filepath.Join(from, name))
			}
			if failed > 0 {
				// 索引最后搬：搬不动就整份留在旧目录，用户切回去仍能用。
				return "", fmt.Errorf("有 %d 个缓存文件搬不到新位置（磁盘满/权限不足？），已保留原位置的缓存", failed)
			}
			if err := moveCacheFile(filepath.Join(from, cacheIndexName), filepath.Join(dir, cacheIndexName)); err != nil {
				return "", fmt.Errorf("缓存索引搬不到新位置，已保留原位置的缓存: %w", err)
			}
			_ = os.Remove(filepath.Join(from, cacheIndexName))
			// 搬完把旧目录属于本缓存的字节收干净：留着就是「界面上 0 首、旧盘还压着几个 GiB」。
			if sweepCacheDir(from) {
				_ = os.Remove(from)
			}
		} else {
			// 目标已经有自己的一份缓存（用户切回之前用过的目录）：**两份都留着**。
			// 覆盖或清扫都会删掉用户另一处的真实缓存 —— 那是数据丢失，不是「整理」。
			// 旧目录的字节仍可经「切回去 + 清理缓存」正常清掉。
			logInfo("音乐缓存位置已切到既有缓存目录，原目录内容保留：%s", from)
		}
	}
	c.dir = dir
	c.seed = ""
	c.idx = cacheIndex{}
	// 令牌里存的是绝对路径：换目录后旧令牌指向的文件已经不在了，整表作废，
	// 让播放器重新 resolve（而不是拿到一个 404 的播放流）。
	c.readers = map[string]*cacheReader{}
	logInfo("音乐缓存位置已切换：%s", dir)
	return dir, nil
}

// cacheDir 当前生效的缓存目录（只读，不加锁也安全：切换在锁内整体替换字符串）。
func (a *App) cacheDir() string {
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir
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
		writeOK(w, map[string]any{"items": 0, "bytes": int64(0), "limit_bytes": a.cacheLimit.Load(), "dir": c.dir})
		return
	}
	writeOK(w, map[string]any{"items": len(c.idx.Items), "bytes": c.idx.Bytes, "limit_bytes": a.cacheLimit.Load(), "dir": c.dir})
}

// handleMusicCacheSetDir POST /cache/music/dir {dir}
//
// dir 为空 = 回到平台默认缓存位置。**只接受主进程调用**：路径是「在磁盘上写哪儿」的权限，
// 与 /api/bg 同一条「请求参数不参与拼路径」红线 —— 渲染层改的是 quaver.conf 里的配置键，
// 由主进程解析后下发。中继会透传 Sec-Fetch-Dest（渲染层伪造不了），带该头一律 403。
func (a *App) handleMusicCacheSetDir(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")) != "" {
		writeErr(w, http.StatusForbidden, -1, "缓存位置只能由桌面主进程按配置文件设定")
		return
	}
	var body struct {
		Dir string `json:"dir"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	dir, err := a.cacheSetDir(body.Dir)
	if err != nil {
		writeErr(w, 422, -1, err.Error())
		return
	}
	c := a.musicCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.openLocked(true); err != nil || c.seed == "" {
		writeOK(w, map[string]any{"dir": dir, "items": 0, "bytes": int64(0), "limit_bytes": a.cacheLimit.Load()})
		return
	}
	writeOK(w, map[string]any{"dir": dir, "items": len(c.idx.Items), "bytes": c.idx.Bytes, "limit_bytes": a.cacheLimit.Load()})
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
	writeOK(w, map[string]any{"items": 0, "bytes": int64(0), "dir": c.dir})
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
