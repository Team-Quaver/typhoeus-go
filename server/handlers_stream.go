package server

// 播放流端点：Typhoeus 接线（provider 组装 + 流中继 token 表）。
//
// - resolve → 协商出可播流（会员门控 + rank 回退 + 首块嗅探；加密档在内存解密）
// - 中继端点持 token 透传 Range（vkey 不出后端；token 有 TTL，播放中命中即续期）
//
// 加密边界：ekey 只在协商时短暂存在；中继按 Range 分片解密直推，全链路没有
// 「解密后整文件」的内存峰值。除 cache/music/*.quaverlocalcache 的本地加密缓存外，
// 播放链路没有任何明文磁盘写入。
import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// streamEntry 一条已协商的播放流。
type streamEntry struct {
	mid        string
	url        string
	mime       string
	total      int64
	filename   string
	tier       string
	vkey       string
	encrypted  bool
	cipher     qmc.Cipher // 仅加密流非 nil；按绝对偏移解密，无跨请求状态
	expiresAt  time.Time
	createdAt  time.Time // 绝对上限锚点（滑动续期不能无限续命）
	hits       int
	degraded   bool
	requested  string
	durationMs int64 // 客户端声明的曲长（限速推算用；0=未知）
	probeBytes int64 // fetch 探测类请求已发放的额度（分类见 stream_guard.go）
	// opener 非空时由微云下载链接提供；否则使用 Typhoeus 的 CDN opener。
	opener streamOpener
	// cacheSeed starts the encrypted music cache only after playback has
	// delivered bytes. Starting it in resolve would open a second full-file
	// request while the player is still buffering the first one.
	cacheSeed    string
	cacheStarted bool
	// activeRequests counts in-flight playback reads. The background cache waits
	// for this to reach zero before opening its own full-file response.
	activeRequests int
}

type streamOpener interface {
	OpenRange(ctx context.Context, url string, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error)
}

// streamSourceRefresh lets ticket-bearing sources refresh before cache writes.
type streamSourceRefresh interface {
	Refresh(ctx context.Context) (streamOpener, error)
}

const (
	streamIdleTTL = 30 * time.Minute // 滑动空闲过期：离开播放即回收（旧值 2h 让复制走的 URL 长期有效）
	streamMaxAge  = 3 * time.Hour    // 绝对上限：滑动续期不能无限续命（vkey 自身也有过期）
)

// purgeExpired 清理过期 token。
func (a *App) purgeExpired() {
	now := time.Now()
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	for tok, e := range a.streams {
		if e.expiresAt.Before(now) || now.Sub(e.createdAt) > streamMaxAge {
			// 解密器随条目一起释放；它本身不含可落盘状态
			delete(a.streams, tok)
		}
	}
	a.dropIdlePace(now)
}

// newToken 生成 url-safe 随机 token。
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// streamTiersBody GET /stream/tiers 响应。
//
// 当前账号可播档位（会员门控数据源）。加密档位会出现（本后端支持解密播放），
// 用 encrypted 字段区分——UI 可标「解密播放」徽标。
func (a *App) handleStreamTiers(w http.ResponseWriter, r *http.Request) {
	membership, err := a.provider.Membership(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	available := typhoeus.AvailableFor(membership)
	tiers := make([]map[string]any, 0, len(available))
	for _, t := range available {
		tiers = append(tiers, tierJSON(t, false))
	}
	all := typhoeus.Tiers()
	allTiers := make([]map[string]any, 0, len(all))
	for _, t := range all {
		item := tierJSON(t, true)
		item["locked"] = membership < typhoeus.Membership(t.Requires)
		item["requires"] = int(t.Requires)
		allTiers = append(allTiers, item)
	}
	max := ""
	if len(available) > 0 {
		max = available[len(available)-1].ID
	}
	writeOK(w, map[string]any{
		"membership":       int(membership),
		"membership_label": membership.String(),
		"tiers":            tiers,
		"all_tiers":        allTiers,
		"max":              max,
	})
}

func tierJSON(t *typhoeus.Tier, withMeta bool) map[string]any {
	m := map[string]any{
		"id":        t.ID,
		"label":     t.Label,
		"rank":      t.Rank,
		"hi_res":    t.Rank >= 40,
		"encrypted": t.Encrypted,
	}
	if withMeta {
		m["mime"] = t.Mime
		m["ext"] = t.Ext
		m["requires"] = int(t.Requires)
	}
	return m
}

// streamResolveBody POST /stream/resolve 请求体。
type streamResolveBody struct {
	Mid          string   `json:"mid"`
	MediaMid     string   `json:"media_mid"`
	SongType     int      `json:"song_type"`
	Tier         string   `json:"tier"`          // typhoeus 档位 id
	Auto         bool     `json:"auto"`          // true=自动音质模式（会员不足向下降档，不报 403）
	Deprioritize []string `json:"deprioritize"`  // 回退链降权档位（UI「回退排序」开关）
	Platform     string   `json:"platform"`      // 取链平台身份：android(缺省)/web/desktop——严格曲库的版权判定与此相关
	DurationMs   int64    `json:"duration_ms"`   // 曲长提示（秒×1000）：中继限速的实时倍率据此推算；缺省按整曲 ≥2 分钟兜底
	CacheSeed    string   `json:"cache_seed"`    // 前端缓存密钥半种；空 = 本次播放不参与磁盘缓存
	CloudFileID  string   `json:"cloud_file_id"` // 微云条目：网盘播放也走同一条缓存/令牌链路
}

func (a *App) handleStreamResolve(w http.ResponseWriter, r *http.Request) {
	var body streamResolveBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Mid == "" {
		writeErr(w, 422, -1, "缺少 mid")
		return
	}
	if body.Tier == "" {
		body.Tier = "128"
	}
	if body.CloudFileID != "" {
		if body.CacheSeed != "" {
			if err := a.cacheSetSeed(body.CacheSeed); err == nil {
				if tok, item, cerr := a.cacheTryResolve(body.Mid, body.CacheSeed, "weiyun"); cerr == nil && item != nil {
					writeOK(w, map[string]any{
						"token":      tok,
						"path":       "/cache/music/stream/" + tok,
						"tier":       item.Tier,
						"tier_label": tierLabel(item.Tier),
						"mime":       cachePlainMime(item.Mime),
						"size":       item.Size,
						"cached":     true,
						"degraded":   false,
					})
					return
				}
			}
		}
		a.handleWeiyunStreamResolve(w, r, body)
		return
	}
	if body.CacheSeed != "" {
		if err := a.cacheSetSeed(body.CacheSeed); err == nil {
			if tok, item, cerr := a.cacheTryResolve(body.Mid, body.CacheSeed, body.Tier); cerr == nil && item != nil {
				writeOK(w, map[string]any{
					"token":      tok,
					"path":       "/cache/music/stream/" + tok,
					"tier":       item.Tier,
					"tier_label": tierLabel(item.Tier),
					"mime":       cachePlainMime(item.Mime),
					"size":       item.Size,
					"cached":     true,
					"degraded":   item.Tier != body.Tier,
				})
				return
			}
		}
	}
	resolved, err := a.resolver.Resolve(r.Context(), body.Mid, body.MediaMid, body.Tier,
		body.Auto, body.Deprioritize, body.Platform, body.SongType)
	if err != nil {
		writeError(w, err)
		return
	}
	// 探测总长：对 CDN 发 bytes=0-0，从 Content-Range 解析（不下载实体）。
	// CDN 抖动不该推翻已协商成功的流：total 未知时中继按流式透传照样可播
	// （此前探测失败会让整次 resolve 报错 → 前端兜底标准音质）。
	total, err := typhoeus.ProbeTotal(r.Context(), resolved.URL)
	if err != nil {
		logWarn("探测总长失败（按未知长度继续）: %v", err)
		total = 0
	}
	a.purgeExpired()
	token := newToken()
	now := time.Now()
	entry := &streamEntry{
		mid:        resolved.Mid,
		url:        resolved.URL,
		mime:       resolved.Tier.Mime,
		total:      total,
		filename:   resolved.Filename,
		tier:       resolved.Tier.ID,
		vkey:       resolved.VKey,
		encrypted:  resolved.Encrypted,
		cipher:     resolved.Cipher,
		expiresAt:  now.Add(streamIdleTTL),
		createdAt:  now,
		degraded:   resolved.Degraded,
		requested:  resolved.RequestedTier.ID,
		durationMs: body.DurationMs,
		cacheSeed:  body.CacheSeed,
	}
	a.streamMu.Lock()
	a.streams[token] = entry
	a.streamMu.Unlock()
	// mid 粒度限速桶：resolve 即建档（换档刷新倍率），跨 token 共享，重取链接刷不出额度
	a.paceBucket(resolved.Mid, paceRateFor(total, body.DurationMs))

	writeOK(w, map[string]any{
		"token":          token,
		"path":           "/stream/" + token,
		"tier":           resolved.Tier.ID,
		"tier_label":     resolved.Tier.Label,
		"requested_tier": resolved.RequestedTier.ID,
		"degraded":       resolved.Degraded,
		"mime":           resolved.Tier.Mime,
		"size":           total,
		"filename":       resolved.Filename,
		"encrypted":      resolved.Encrypted,
	})
}

// tierLabel 缓存条目只存稳定 id，标签按当前档位表取；未知 id 原样返回便于排障。
func tierLabel(id string) string {
	t, err := typhoeus.TierByID(id)
	if err != nil {
		return id
	}
	return t.Label
}

// handleStreamProxy Range 透传中继（<audio> src 指这里；vkey/ekey 留在后端）。
//
// 版权闸门（类别判定与额度语义见 stream_guard.go）：
//   - document/frame 等浏览器导航类 → 403（堵「复制 URL 到新标签页/另存为」）
//   - empty（渲染层 fetch）→ 只允许小闭合 Range，且每 token 累计 ≤2MiB（音质浮窗探测）
//   - audio/video 与原生客户端（缺头）→ 放行，但统一过 mid 限速桶
func (a *App) handleStreamProxy(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	cls := classifyStreamClient(r)
	var probeSpan int64
	switch cls {
	case classBlocked:
		writeErr(w, http.StatusForbidden, -1, "播放流仅允许应用内播放器取用")
		return
	case classProbe:
		span, ok := closedRangeSpan(r.Header.Get("Range"))
		if !ok || span > probeMaxSpan {
			writeErr(w, http.StatusForbidden, -1, "页面 fetch 仅允许小范围探测（取容器头），不支持整段取流")
			return
		}
		probeSpan = span
	case classPlayer, classMedia:
	}

	a.streamMu.Lock()
	entry, ok := a.streams[token]
	probeOver := false
	if ok && cls == classProbe {
		if entry.probeBytes+probeSpan > probeMaxBudget {
			probeOver = true // 探测预算耗尽：同一 token 上「分块搬运整曲」的形态
		} else {
			entry.probeBytes += probeSpan
		}
	}
	if ok && !probeOver {
		entry.hits++
		entry.expiresAt = time.Now().Add(streamIdleTTL) // 滑动续期：活跃播放不过期
		entry.activeRequests++
	}
	a.streamMu.Unlock()
	if probeOver {
		writeErr(w, http.StatusForbidden, -1, "取流探测超出预算，请重新播放")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, -1, "播放流不存在或已过期，请重新播放")
		return
	}
	defer func() {
		a.streamMu.Lock()
		if entry.activeRequests > 0 {
			entry.activeRequests--
		}
		a.streamMu.Unlock()
	}()

	// 解析客户端 Range
	rng := typhoeus.ParseRange(r.Header.Get("Range"))
	if rng != nil && entry.total > 0 {
		rng = rng.Clamp(entry.total)
		if rng != nil && rng.Start != nil && *rng.Start >= entry.total {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.total))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	// 回源
	up, err := a.openStreamRange(r.Context(), entry, rng)
	if err != nil {
		writeError(w, err)
		return
	}
	defer up.Body.Close()

	if rng != nil && entry.encrypted {
		// An encrypted range is only meaningful when the body starts at the
		// requested absolute byte. A 200 response (or a shifted Content-Range)
		// means the keystream would be aligned to the wrong offset: reject it
		// instead of streaming noise to the player.
		wantStart := int64(0)
		if rng.Start != nil {
			wantStart = *rng.Start
		}
		if err := validateRangeResponse(up, wantStart); err != nil {
			writeError(w, err)
			return
		}
	}

	headers := w.Header()
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Content-Type", entry.mime)
	headers.Set("Cache-Control", "no-store")
	if up.ContentRange != "" {
		headers.Set("Content-Range", up.ContentRange)
	}
	contentLength := up.Header.Get("Content-Length")
	if contentLength != "" {
		headers.Set("Content-Length", contentLength)
	} else if up.Status == 200 && entry.total > 0 {
		headers.Set("Content-Length", fmt.Sprintf("%d", entry.total))
	}
	w.WriteHeader(up.Status)

	// 续传参数：无 Range（整段）和带 start 的 Range 都知道绝对起点；
	// 后缀 Range（bytes=-N）只在文件长度未知时无从推导偏移 → 不续传。
	var start, end int64
	var resume int
	switch {
	case rng == nil:
		start, end, resume = 0, entry.total-1, typhoeus.ResumeRetries
	case rng.Start == nil:
		start, end, resume = 0, -1, 0
	default:
		start = *rng.Start
		if rng.End != nil {
			end = *rng.End
		} else {
			end = entry.total - 1
		}
		resume = typhoeus.ResumeRetries
	}

	// 断流续传：CDN 掐连接/读超时把「播放中断」降级为「一次短暂停顿」。
	// 限速：写出经 mid 令牌桶（≈3× 实时码率）——「整曲一把抓」被拖成接近实时，
	// 正常播放与 seek 只吃突发额度，无感。
	bucket := a.paceBucket(entry.mid, paceRateFor(entry.total, entry.durationMs))
	written, err := copyStreamRange(r.Context(), &pacedWriter{w: discardLogger{w}, b: bucket, ctx: r.Context()}, entry, up,
		entry.cipher, start, end, entry.total > 0, resume)
	if written > 0 {
		a.cacheAfterPlayback(entry)
	}
	if err == nil {
		return
	}
	if typhoeus.IsClientAbort(err) {
		// 播放器 seek / 切歌 / 暂停 / 预加载被丢弃：Chromium 关掉请求后，主进程的中继
		// 立刻 abort 它对 sidecar 的 fetch（native-server.ts: onClientGone）——于是这里
		// 看到 context canceled 或写端 connection reset by peer。
		//
		// 完全良性：已送出的字节没问题，播放器会带着新偏移重开一个 Range；预加载读掉
		// 十几 MB 再丢弃是 Chromium 媒体栈的常规行为。真能看到问题的是下面那条 WARN。
		logDebug("播放流由客户端结束（已发 %d 字节，起点 %d）: %v", written, start, err)
		return
	}
	// 到这里才是真故障：上游 reset / 提前 EOF / 续传失败（重连后仍没补完）。
	logWarn("回源流提前结束（已发 %d 字节，起点 %d）: %v", written, start, err)
}

func (a *App) openStreamRange(ctx context.Context, entry *streamEntry, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	if entry.opener != nil {
		return entry.opener.OpenRange(ctx, entry.url, rng)
	}
	return typhoeus.OpenRange(ctx, entry.url, rng)
}

func copyStreamRange(ctx context.Context, w io.Writer, entry *streamEntry, first *typhoeus.UpstreamResponse,
	cipher qmc.Cipher, start, end int64, totalKnown bool, resume int) (int64, error) {
	if entry.opener == nil {
		return typhoeus.CopyRange(ctx, w, entry.url, first, cipher, start, end, totalKnown, resume)
	}
	// Weiyun needs its cookie-bearing opener for resume requests as well.
	// pos MUST start at the requested absolute offset: QMC is stateless, but
	// decrypting a range at N with the keystream for 0 turns valid audio into noise.
	pos := start
	var written int64
	resp := first
	left := resume
	for {
		n, err := typhoeus.CopyRange(ctx, w, entry.url, resp, cipher, pos, end, totalKnown, 0)
		written += n
		pos += n
		if err == nil {
			return written, nil
		}
		if typhoeus.IsClientAbort(err) || ctx.Err() != nil {
			return written, err
		}
		if left <= 0 {
			return written, err
		}
		left--
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		next, rerr := entry.opener.OpenRange(ctx, entry.url, &typhoeus.ByteRange{Start: &pos, End: &end})
		if rerr != nil {
			return written, err
		}
		if err := validateRangeResponse(next, pos); err != nil {
			_ = next.Body.Close()
			return written, err
		}
		resp = next
	}
}

// validateRangeResponse rejects an upstream that ignored (or shifted) the
// requested range. Decrypting such a body would silently produce noise rather
// than an obvious HTTP error, so the check is deliberately strict.
func validateRangeResponse(up *typhoeus.UpstreamResponse, wantStart int64) error {
	if up == nil {
		return fmt.Errorf("回源响应为空")
	}
	if up.Status != http.StatusPartialContent {
		return fmt.Errorf("回源未返回 206（status=%d）", up.Status)
	}
	if up.ContentRange != "" {
		got := rangeContentStart(up.ContentRange)
		if got >= 0 && got != wantStart {
			return fmt.Errorf("回源起点不符：期望 %d，上游给 %d", wantStart, got)
		}
	}
	return nil
}

func rangeContentStart(contentRange string) int64 {
	spec := strings.TrimSpace(contentRange)
	if !strings.HasPrefix(strings.ToLower(spec), "bytes ") {
		return -1
	}
	span, _, ok := strings.Cut(strings.TrimSpace(spec[len("bytes "):]), "/")
	if !ok {
		return -1
	}
	left, _, ok := strings.Cut(span, "-")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(left), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// discardLogger 包装 writer（占位，保持 io.Writer 语义）。
type discardLogger struct{ http.ResponseWriter }
