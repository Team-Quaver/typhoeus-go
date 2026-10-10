package server

// 播放流端点：Typhoeus 接线（provider 组装 + 流中继 token 表）。
//
// - resolve → 协商出可播流（会员门控 + rank 回退 + 首块嗅探；加密档在内存解密）
// - 中继端点持 token 透传 Range（vkey 不出后端；token 有 TTL，播放中命中即续期）
//
// 加密边界：解密仅驻内存。ekey 只在协商时短暂存在；中继按 Range 分片解密直推，
// 全链路没有「解密后整文件」的内存峰值，也没有任何磁盘写入。
import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// streamEntry 一条已协商的播放流。
type streamEntry struct {
	url       string
	mime      string
	total     int64
	filename  string
	tier      string
	encrypted bool
	cipher    qmc.Cipher // 仅加密流非 nil；按绝对偏移解密，无跨请求状态
	expiresAt time.Time
	hits      int
	degraded  bool
	requested string
}

const streamTTL = 2 * time.Hour // 与上游 vkey expiration 同量级；播放中命中即续期（滑动过期）

// purgeExpired 清理过期 token。
func (a *App) purgeExpired() {
	now := time.Now()
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	for tok, e := range a.streams {
		if e.expiresAt.Before(now) {
			// 解密器随条目一起释放；它本身不含可落盘状态
			delete(a.streams, tok)
		}
	}
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
	Tier         string   `json:"tier"`         // typhoeus 档位 id
	Auto         bool     `json:"auto"`         // true=自动音质模式（会员不足向下降档，不报 403）
	Deprioritize []string `json:"deprioritize"` // 回退链降权档位（UI「回退排序」开关）
	Platform     string   `json:"platform"`     // 取链平台身份：android(缺省)/web/desktop——严格曲库的版权判定与此相关
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
	a.streamMu.Lock()
	a.streams[token] = &streamEntry{
		url:       resolved.URL,
		mime:      resolved.Tier.Mime,
		total:     total,
		filename:  resolved.Filename,
		tier:      resolved.Tier.ID,
		encrypted: resolved.Encrypted,
		cipher:    resolved.Cipher,
		expiresAt: time.Now().Add(streamTTL),
		degraded:  resolved.Degraded,
		requested: resolved.RequestedTier.ID,
	}
	a.streamMu.Unlock()

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

// handleStreamProxy Range 透传中继（<audio> src 指这里；vkey/ekey 留在后端）。
func (a *App) handleStreamProxy(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	a.streamMu.Lock()
	entry, ok := a.streams[token]
	if ok {
		entry.hits++
		entry.expiresAt = time.Now().Add(streamTTL) // 滑动续期：活跃播放不过期
	}
	a.streamMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, -1, "播放流不存在或已过期，请重新播放")
		return
	}

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
	up, err := typhoeus.OpenRange(r.Context(), entry.url, rng)
	if err != nil {
		writeError(w, err)
		return
	}
	defer up.Body.Close()

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
	written, err := typhoeus.CopyRange(r.Context(), discardLogger{w}, entry.url, up,
		entry.cipher, start, end, entry.total > 0, resume)
	if err != nil {
		// 区分良性断开与真故障：播放器切歌/预加载 abort 同样表现为写端 reset；
		// 早期断开（几乎没发数据）多为良性，大量传输后断开才值得追查。
		logWarn("回源流提前结束（已发 %d 字节，起点 %d）: %v", written, start, err)
	}
}

// discardLogger 包装 writer（占位，保持 io.Writer 语义）。
type discardLogger struct{ http.ResponseWriter }
