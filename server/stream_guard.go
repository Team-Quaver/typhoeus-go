package server

// 播放流版权保护闸门（与 ui/electron/stream-guard.ts 构成两层防线）。
//
// 事件起点：用户报告「设置 → 通用 → 调试 → 开发者模式」打开 DevTools 后，网络面板里
// 复制 /stream/<token>（新标签页打开 / 复制为 cURL / 控制台 fetch）就能整首拖走。
// 加密档在后端已解密成明文再出流，这条口子等于把「仅提供流媒体、不提供下载」的应用
// 立场（README 协议节）直接击穿 —— 对版权方不可接受。
//
// 本文件的姿态（诚实的边界：**本地播放的字节终归要交给本地播放器，100% 防录防提取是
// 不可能任务**；闸门要消灭的是「顺手批量下整曲」这种形态）：
//
//  1. 客户端类别（Sec-Fetch-Dest，浏览器内核自己盖的章，页面脚本改不了）：
//     - audio / video：页内 <audio> 元素 —— 正常播放，放行
//     - （缺头）：原生客户端（mpv、quaver-astra、无中继的自研前端）—— 放行，进第 2 层
//     - empty：渲染层 fetch/XHR —— 只许「小的闭合 Range 探测」（正在播放页音质浮窗取
//       容器头），单段 ≤64KiB、每个 token 累计 ≤2MiB；整段拉流 403
//     - 其余（document / frame / …）：地址栏粘贴、新标签页、另存为 —— 一律 403
//     注：本机 curl 能伪造任何头，所以第 2 层才是兜底；分类闸门的靶子是 DevTools
//     与浏览器级重放（"Copy as cURL" 的默认头里没有 Sec-Fetch-*，Chromium 把
//     Sec-Fetch-* 列为 forbidden header，页面脚本也无法 setRequestHeader 伪造）。
//
//  2. mid 粒度令牌桶限速：每首歌的回写带宽封顶 ≈ 3× 实时码率（16MiB 突发额度覆盖
//     起播预缓冲与数次 seek）。播放与拖进度条无感；「一条 URL 秒整曲」被拖成
//     「下完一首 ≈ 听完一首」，批量抓取失去意义。桶按 **mid 跨 token 共享**：
//     反复 resolve 换新 token 刷不出额度（Electron 层的 webRequest 闸门挡不住
//     「resolve 循环 + 64KiB 分块 probe」，挡得住它的正是这里的累计速率）。
//
// 加密边界不变：vkey/ekey 依旧不出后端；本文件只约束「流出去的方式与速率」。

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clientClass 流端点请求的客户端类别。
type clientClass int

const (
	classPlayer  clientClass = iota // 缺 Sec-Fetch-Dest：mpv / 原生客户端
	classMedia                      // audio|video：页内媒体元素（正常播放通路）
	classProbe                      // empty：渲染层 fetch —— 降级为小 Range 探测专用
	classBlocked                    // 其余：地址栏 / 新标签页 / 另存为
)

func classifyStreamClient(r *http.Request) clientClass {
	switch dest := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest"))); dest {
	case "":
		return classPlayer
	case "audio", "video":
		return classMedia
	case "empty":
		return classProbe
	default:
		return classBlocked
	}
}

// 探测额度：音质浮窗实际只要 bytes=0-4095 + 一段 ≤256B 的补刀；预算留百倍余量。
const (
	probeMaxSpan   = 64 << 10
	probeMaxBudget = 2 << 20
)

// closedRangeSpan 解析「单段、闭合区间」的 Range（bytes=start-end），返回字节数。
// 多段、开区间（bytes=0-）、后缀式（bytes=-N）一律 false —— 探测必须显式给出小窗口。
func closedRangeSpan(header string) (int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.ContainsRune(spec, ',') {
		return 0, false
	}
	startS, endS, ok := strings.Cut(spec, "-")
	if !ok || startS == "" || endS == "" {
		return 0, false
	}
	start, err1 := strconv.ParseInt(startS, 10, 64)
	end, err2 := strconv.ParseInt(endS, 10, 64)
	if err1 != nil || err2 != nil || end < start {
		return 0, false
	}
	return end - start + 1, true
}

// denyBrowserClients 裸链类端点（/song/urls、/song/{mid}/url 返回带 vkey 的直连地址）
// 对携带 Sec-Fetch-Dest 的请求一律关门：Electron 中继会把渲染层请求的该头透传过来，
// 所以「渲染层控制台手动取裸链」= 403；原生客户端（无此头）不受影响。
func denyBrowserClients(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Dest") != "" {
		writeErr(w, http.StatusForbidden, -1, "该端点不服务页面直连，请使用 /stream 播放流")
		return true
	}
	return false
}

// —— 令牌桶 ——

const (
	paceMinRate    = 256 << 10 // 256KiB/s 下限：低码率档也够起播预缓冲与 seek，不至于饿死
	paceMaxRate    = 4 << 20   // 4MiB/s 上限：超高采样率母带 ≈3× 实时的顶格也够用
	paceBurst      = 16 << 20  // 突发额度：Chromium/mpv 首帧缓冲 + 数次 seek 重拉的余量
	paceUnknownDiv = 120       // 时长未知时按「整曲全速下载也 ≥2 分钟」限速
	paceIdleDrop   = 6 * time.Hour
)

// byteBucket 极简令牌桶：按 rate 字节/秒回填，容量 burst；reserve 等待额度、可被 ctx 打断。
type byteBucket struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	avail float64
	last  time.Time
	now   func() time.Time // 测试注入时钟；缺省 time.Now
}

func (b *byteBucket) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// reserve 为 n 字节放行；额度不足则按缺口休眠（切片 ≤200ms 保持对取消的响应）。
// rate<=0 视为不限速（total 探测失败的极端情形，别让闸门把播放掐死）。
func (b *byteBucket) reserve(ctx context.Context, n int) error {
	want := float64(n)
	b.mu.Lock()
	if b.rate <= 0 {
		b.mu.Unlock()
		return nil
	}
	b.pace(b.clock())
	if b.avail >= want {
		b.avail -= want
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		b.pace(b.clock())
		if b.avail >= want {
			b.avail -= want
			b.mu.Unlock()
			return nil
		}
		deficit := want - b.avail
		rate := b.rate
		b.mu.Unlock()

		sleep := time.Duration(deficit / rate * float64(time.Second))
		if sleep > 200*time.Millisecond {
			sleep = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// pace 回填令牌（调用方持锁）。
func (b *byteBucket) pace(now time.Time) {
	if now.After(b.last) {
		b.avail += now.Sub(b.last).Seconds() * b.rate
		if b.avail > b.burst {
			b.avail = b.burst
		}
		b.last = now
	}
}

// paceRateFor 由曲长与时长推限速：3× 实时码率，夹进 [256KiB/s, 4MiB/s]。
// total<=0（上游探长失败）返回 0 = 不限速（分类闸门仍然生效；这是可用性优先的例外）。
func paceRateFor(total, durationMs int64) float64 {
	if total <= 0 {
		return 0
	}
	rate := float64(total) / paceUnknownDiv
	if durationMs > 0 {
		rate = float64(total) / (float64(durationMs) / 1000) * 3
	}
	if rate < paceMinRate {
		return paceMinRate
	}
	if rate > paceMaxRate {
		return paceMaxRate
	}
	return rate
}

// pacedWriter 把桶挂在响应写上：CopyRange 的每一次 Write 先领额度。
type pacedWriter struct {
	w   io.Writer
	b   *byteBucket
	ctx context.Context
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	if err := p.b.reserve(p.ctx, len(b)); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}

// paceBucket 取（或建）mid 的限速桶。resolve 时用最新协商值刷新速率；
// 「后写覆盖」是有意的：切低码率档时收紧限速同样成立（播放只吃 1×）。
func (a *App) paceBucket(mid string, rate float64) *byteBucket {
	a.paceMu.Lock()
	defer a.paceMu.Unlock()
	if a.paceMap == nil {
		a.paceMap = map[string]*byteBucket{}
	}
	b, ok := a.paceMap[mid]
	if !ok {
		b = &byteBucket{rate: rate, burst: paceBurst, avail: paceBurst}
		a.paceMap[mid] = b
		return b
	}
	b.mu.Lock()
	b.rate = rate
	b.mu.Unlock()
	return b
}

// dropIdlePace 清掉长期不用的桶（与 purgeExpired 搭车；桶只存计数，不存内容）。
func (a *App) dropIdlePace(now time.Time) {
	a.paceMu.Lock()
	defer a.paceMu.Unlock()
	for mid, b := range a.paceMap {
		b.mu.Lock()
		idle := now.Sub(b.last)
		b.mu.Unlock()
		if idle > paceIdleDrop {
			delete(a.paceMap, mid)
		}
	}
}
