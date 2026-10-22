package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassifyStreamClient(t *testing.T) {
	cases := []struct {
		dest string // "" = 不发头（原生客户端）
		want clientClass
	}{
		{"", classPlayer},
		{"audio", classMedia},
		{"VIDEO", classMedia}, // 头值大小写不敏感
		{"empty", classProbe},
		{"document", classBlocked},
		{"frame", classBlocked},
		{" object", classBlocked}, // 前导空白不干扰判定
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/stream/tok", nil)
		if c.dest != "" {
			r.Header.Set("Sec-Fetch-Dest", c.dest)
		}
		if got := classifyStreamClient(r); got != c.want {
			t.Errorf("dest=%q 分类成 %v，期望 %v", c.dest, got, c.want)
		}
	}
}

func TestClosedRangeSpan(t *testing.T) {
	cases := []struct {
		header string
		span   int64
		ok     bool
	}{
		{"bytes=0-4095", 4096, true},
		{"bytes=100-199", 100, true},
		{"bytes=0-", 0, false},        // 开区间 = 整段意图
		{"bytes=-512", 0, false},      // 后缀式
		{"bytes=0-9,20-29", 0, false}, // 多段
		{"", 0, false},                // 无 Range = 整段 GET
		{"bytes=abc-def", 0, false},
		{"bytes=10-5", 0, false}, // 倒挂
		{"items=0-10", 0, false}, // 非 bytes 单元
	}
	for _, c := range cases {
		span, ok := closedRangeSpan(c.header)
		if ok != c.ok || (ok && span != c.span) {
			t.Errorf("Range %q → (%d, %v)，期望 (%d, %v)", c.header, span, ok, c.span, c.ok)
		}
	}
}

func TestPaceRateFor(t *testing.T) {
	// 128k mp3（≈2.9MB / 3min）：理论 3× ≈ 48KB/s，应被下限抬到 256KiB/s
	if r := paceRateFor(2_900_000, 180_000); r != paceMinRate {
		t.Errorf("低码率没吃到下限：%f", r)
	}
	// 母带 60MB / 5min：3× = 360KB/s → 仍在区间内（下限之上）
	if r := paceRateFor(60_000_000, 300_000); r < paceMinRate || r > paceMaxRate {
		t.Errorf("母带倍率出界：%f", r)
	}
	// 极短高码率：应被上限夹住
	if r := paceRateFor(200_000_000, 10_000); r != paceMaxRate {
		t.Errorf("没被上限夹住：%f", r)
	}
	// 时长未知：按整曲 ≥2 分钟兜底（60MB/120s = 500KB/s，仍在区间内）
	if r := paceRateFor(60_000_000, 0); r != 60_000_000.0/paceUnknownDiv {
		t.Errorf("未知时长兜底错：%f", r)
	}
	// 时长未知且是小文件：兜底速率也被下限抬住（播放体验优先）
	if r := paceRateFor(12_000_000, 0); r != paceMinRate {
		t.Errorf("小文件未知时长没吃到下限：%f", r)
	}
	// 总长未知（探测失败）：0 = 不限速信号
	if r := paceRateFor(0, 180_000); r != 0 {
		t.Errorf("total=0 应返回 0：%f", r)
	}
}

func TestByteBucketDrainsAndWaits(t *testing.T) {
	b := &byteBucket{rate: 1 << 20, burst: 1 << 20, avail: 1 << 10, last: time.Now()}

	start := time.Now()
	if err := b.reserve(context.Background(), 4096); err != nil {
		t.Fatal(err)
	}
	// 只有 1KiB 存量、缺 3KiB @1MiB/s ≈ 3ms：应明显小于一次网络往返，但也确实等了
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("等待过久： %v", d)
	}
}

func TestByteBucketCtxCancel(t *testing.T) {
	b := &byteBucket{rate: 16, burst: 16, avail: 0, last: time.Now()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.reserve(ctx, 4096); err == nil {
		t.Fatal("已取消的 ctx 必须立刻返回错误")
	}
}

func TestPaceBucketSharedByMid(t *testing.T) {
	app := &App{streams: map[string]*streamEntry{}}

	b1 := app.paceBucket("midA", 1000)
	b2 := app.paceBucket("midA", 2000) // 换档：同一 mid 复用桶、覆盖速率
	if b1 != b2 {
		t.Fatal("同 mid 拿到了两个桶（跨 token 绕额度）")
	}
	if b2.rate != 2000 {
		t.Errorf("速率没被最新 resolve 覆盖：%f", b2.rate)
	}
	if app.paceBucket("midB", 1000) == b1 {
		t.Error("不同 mid 共用了桶")
	}
}

func TestStreamProxyClientGate(t *testing.T) {
	srv := httptest.NewServer(&origin{total: 1 << 20})
	defer srv.Close()
	app := newStreamApp(t, srv.URL, 1<<20)

	// ① 浏览器导航类（地址栏 / 新标签页 / 另存为）：403
	nav := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	nav.Header.Set("Sec-Fetch-Dest", "document")
	rec := &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, nav)
	if rec.status != http.StatusForbidden {
		t.Fatalf("document 类请求应 403，实际 %d", rec.status)
	}

	// ② 渲染层整段 fetch：403
	big := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	big.Header.Set("Sec-Fetch-Dest", "empty")
	big.Header.Set("Range", "bytes=0-100000000")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, big)
	if rec.status != http.StatusForbidden {
		t.Fatalf("页面 fetch 整段应 403，实际 %d", rec.status)
	}

	// ③ 渲染层小 Range 探测（音质浮窗）：放行 206
	probe := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	probe.Header.Set("Sec-Fetch-Dest", "empty")
	probe.Header.Set("Range", "bytes=0-4095")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, probe)
	if rec.status != http.StatusPartialContent || rec.written == 0 {
		t.Fatalf("小探测应 206 且有数据：%d / %d", rec.status, rec.written)
	}

	// ④ <audio> 元素（dest=audio）：放行
	media := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	media.Header.Set("Sec-Fetch-Dest", "audio")
	media.Header.Set("Range", "bytes=0-")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, media)
	if rec.written == 0 {
		t.Fatal("媒体元素请求没拿到数据")
	}

	// ⑤ 原生客户端（mpv：缺 Sec-Fetch-Dest）：放行
	native := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	native.Header.Set("Range", "bytes=0-")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, native)
	if rec.written == 0 {
		t.Fatal("原生客户端请求没拿到数据")
	}

	// ⑥ 探测预算耗尽后，fetch 分块搬运也被拒（403）；媒体元素不受牵连
	app.streamMu.Lock()
	app.streams["tok"].probeBytes = probeMaxBudget
	app.streamMu.Unlock()
	probe2 := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	probe2.Header.Set("Sec-Fetch-Dest", "empty")
	probe2.Header.Set("Range", "bytes=0-4095")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, probe2)
	if rec.status != http.StatusForbidden {
		t.Fatalf("超预算探测应 403，实际 %d", rec.status)
	}
	media2 := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	media2.Header.Set("Sec-Fetch-Dest", "audio")
	media2.Header.Set("Range", "bytes=0-4095")
	rec = &fakeResp{hdr: http.Header{}}
	app.handleStreamProxy(rec, media2)
	if rec.status != http.StatusPartialContent {
		t.Fatalf("预算耗尽不该牵连 <audio>：%d", rec.status)
	}
}

func TestDenyBrowserClients(t *testing.T) {
	if !denyBrowserClients(&fakeResp{hdr: http.Header{}}, func() *http.Request {
		r := httptest.NewRequest("POST", "/song/urls", nil)
		r.Header.Set("Sec-Fetch-Dest", "empty")
		return r
	}()) {
		t.Error("带 Sec-Fetch-Dest 的裸链请求没被拒")
	}
	native := httptest.NewRequest("POST", "/song/urls", nil)
	if denyBrowserClients(&fakeResp{hdr: http.Header{}}, native) {
		t.Error("无 Sec-Fetch-Dest 的原生客户端应放行")
	}
}
