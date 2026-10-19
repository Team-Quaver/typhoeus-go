package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// origin 支持 Range 的回源模拟；truncate=true 时每段都少写一半（真故障）。
type origin struct {
	total    int64
	truncate bool
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start, end := int64(0), o.total-1
	if spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes="); spec != "" {
		segs := strings.SplitN(spec, "-", 2)
		if segs[0] != "" {
			start, _ = strconv.ParseInt(segs[0], 10, 64)
		}
		if len(segs) == 2 && segs[1] != "" {
			end, _ = strconv.ParseInt(segs[1], 10, 64)
		}
	}
	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, o.total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	if o.truncate {
		w.Write(make([]byte, length/2))
		panic(http.ErrAbortHandler)
	}
	w.Write(make([]byte, length))
}

// fakeResp 可控写端的 ResponseWriter（reset = 客户端（主进程中继）把连接掐了）。
type fakeResp struct {
	hdr      http.Header
	status   int
	limit    int
	written  int
	failWith error
}

func (f *fakeResp) Header() http.Header { return f.hdr }
func (f *fakeResp) WriteHeader(c int)   { f.status = c }
func (f *fakeResp) Write(p []byte) (int, error) {
	if f.failWith != nil && f.written+len(p) > f.limit {
		return 0, f.failWith
	}
	f.written += len(p)
	return len(p), nil
}

// captureStderr 抓住 sidecar 的 stderr 一行不落——日志是用户可见行为，就该被测。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wr
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	fn()
	_ = wr.Close()
	os.Stderr = old
	return <-done
}

// withToken 模拟 mux 注入的路径参数（/stream/{token}）。
func withToken(r *http.Request) *http.Request {
	r.SetPathValue("token", "tok")
	return r
}

func newStreamApp(t *testing.T, url string, total int64) *App {
	t.Helper()
	return &App{streams: map[string]*streamEntry{
		"tok": {url: url, mime: "audio/flac", total: total, tier: "flac", expiresAt: time.Now().Add(time.Hour)},
	}}
}

func TestStreamProxyClientAbortIsNotWarn(t *testing.T) {
	srv := httptest.NewServer(&origin{total: 4 << 20})
	defer srv.Close()

	// 默认等级（info）：播放器 seek/切歌造成的断开必须完全安静
	defer func(l severity) { minLevel = l }(minLevel)
	minLevel = sevInfo

	app := newStreamApp(t, srv.URL, 4<<20)
	req := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	req.Header.Set("Range", "bytes=0-")
	rec := &fakeResp{hdr: http.Header{}, limit: 256 * 1024, failWith: syscall.ECONNRESET}

	out := captureStderr(t, func() { app.handleStreamProxy(rec, req) })
	if strings.Contains(out, "WARN") {
		t.Fatalf("客户端断开被记成 WARN（就是用户看到的刷屏）：%s", out)
	}
	if rec.written == 0 {
		t.Fatal("一个字节都没写出，中继路径可能没跑起来")
	}

	// 放开 debug 时仍应留下线索，便于排查「为什么这段没播完」
	minLevel = sevDebug
	rec2 := &fakeResp{hdr: http.Header{}, limit: 256 * 1024, failWith: syscall.ECONNRESET}
	req2 := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	req2.Header.Set("Range", "bytes=0-")
	out2 := captureStderr(t, func() { app.handleStreamProxy(rec2, req2) })
	if !strings.Contains(out2, "DEBUG") || !strings.Contains(out2, "播放流由客户端结束") {
		t.Fatalf("debug 等级下应保留良性断开的线索，实际：%q", out2)
	}
	if strings.Contains(out2, "WARN") {
		t.Fatalf("debug 也不该升级为 WARN：%s", out2)
	}
}

func TestStreamProxyUpstreamFailureStillWarns(t *testing.T) {
	srv := httptest.NewServer(&origin{total: 1 << 20, truncate: true})
	defer srv.Close()

	defer func(l severity) { minLevel = l }(minLevel)
	minLevel = sevInfo

	app := newStreamApp(t, srv.URL, 1<<20)
	req := withToken(httptest.NewRequest("GET", "/stream/tok", nil))
	req.Header.Set("Range", "bytes=0-")
	rec := &fakeResp{hdr: http.Header{}}

	var mu sync.Mutex
	out := captureStderr(t, func() {
		mu.Lock()
		defer mu.Unlock()
		app.handleStreamProxy(rec, req)
	})
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "回源流提前结束") {
		t.Fatalf("真故障必须留在 WARN，否则无声丢流：%q", out)
	}
}

func TestLogLevelFiltering(t *testing.T) {
	defer func(l severity) { minLevel = l }(minLevel)
	if got := parseSeverity(""); got != sevInfo {
		t.Fatalf("默认等级应为 info，实际 %v", got)
	}
	if got := parseSeverity("DEBUG"); got != sevDebug {
		t.Fatalf("大小写不敏感失败：%v", got)
	}
	minLevel = sevWarn
	out := captureStderr(t, func() { logInfo("不该出现") })
	if strings.Contains(out, "不该出现") {
		t.Fatal("等级过滤未生效：info 在 warn 阈值下仍打印")
	}
	out = captureStderr(t, func() { logWarn("应当出现") })
	if !strings.Contains(out, "应当出现") {
		t.Fatal("WARN 被误吞")
	}
	out = captureStderr(t, func() { logDebug("静默") })
	if strings.Contains(out, "静默") {
		t.Fatal("DEBUG 在 warn 阈值下必须静默")
	}
}
