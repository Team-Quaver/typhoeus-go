package typhoeus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// countingHandler 一个支持 Range 的回源模拟；可注入「客户端断开」与「上游截断」。
type countingHandler struct {
	total    int64
	truncate bool // true：声明定长却少写，模拟 CDN 半路掐线

	mu       sync.Mutex
	requests int
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests++
	h.mu.Unlock()

	start, end := int64(0), h.total-1
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
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, h.total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)

	bodyLen := length
	if h.truncate {
		bodyLen = length / 2 // 每一段都少写一半：续传也补不齐，最终必须报真故障
	}
	// 分两块写，确保 pump 至少成功一轮后才可能失败
	half := bodyLen / 2
	w.Write(make([]byte, half))
	w.Write(make([]byte, bodyLen-half))
	if h.truncate {
		panic(http.ErrAbortHandler) // 声明的长度没写满 → 客户端读到 unexpected EOF
	}
}

// failingWriter 写了 limit 字节后开始报错（模拟对端已关闭的响应体）。
type failingWriter struct {
	limit   int
	written int
	err     error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written+len(p) > f.limit {
		return 0, f.err
	}
	f.written += len(p)
	return len(p), nil
}

func TestCopyRangeClientGoneDoesNotRetry(t *testing.T) {
	h := &countingHandler{total: 4 << 20}
	srv := httptest.NewServer(h)
	defer srv.Close()

	up, err := OpenRange(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer up.Body.Close()

	w := &failingWriter{limit: 300 * 1024, err: syscall.ECONNRESET}
	done := make(chan struct{})
	var gotErr error
	var gotWritten int64
	go func() {
		gotWritten, gotErr = CopyRange(context.Background(), w, srv.URL, up, nil,
			0, h.total-1, true, ResumeRetries)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端断开后续传仍在重试（应当立刻返回）")
	}
	if gotErr == nil {
		t.Fatal("期望返回写端错误")
	}
	if !IsClientAbort(gotErr) {
		t.Fatalf("写端 reset 应归类为客户端断开，实际: %v", gotErr)
	}
	// 写出量取决于网络分片边界，只要求与 writer 实际收下的一致
	if gotWritten != int64(w.written) || gotWritten == 0 {
		t.Fatalf("已发字节统计错误: got %d，writer 实收 %d", gotWritten, w.written)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("客户端已离开时不该再回源，实际请求数 %d", n)
	}
}

func TestCopyRangeCanceledContextDoesNotRetry(t *testing.T) {
	h := &countingHandler{total: 4 << 20}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	up, err := OpenRange(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer up.Body.Close()
	cancel() // 播放器关掉请求：http.Server 取消 r.Context()

	start := time.Now()
	_, gotErr := CopyRange(ctx, discardWriter{}, srv.URL, up, nil, 0, h.total-1, true, ResumeRetries)
	if time.Since(start) > time.Second {
		t.Fatalf("ctx 取消后仍走了退避/续传（耗时 %v）", time.Since(start))
	}
	if !IsClientAbort(gotErr) {
		t.Fatalf("context canceled 应归类为客户端断开，实际: %v", gotErr)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("ctx 取消后不该再回源，实际请求数 %d", n)
	}
}

func TestCopyRangeUpstreamTruncationStillRetries(t *testing.T) {
	h := &countingHandler{total: 1 << 20, truncate: true} // 每段都截断，续传次数用尽后必须报错
	srv := httptest.NewServer(h)
	defer srv.Close()

	up, err := OpenRange(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer up.Body.Close()

	_, gotErr := CopyRange(context.Background(), discardWriter{}, srv.URL, up, nil,
		0, h.total-1, true, ResumeRetries)
	if gotErr == nil {
		t.Fatal("上游截断且续传补不上时应报错")
	}
	if IsClientAbort(gotErr) {
		t.Fatalf("上游侧故障不能误判为客户端断开（会吞掉告警）：%v", gotErr)
	}
	if n := h.count(); n < 2 {
		t.Fatalf("真故障应当尝试续传，实际请求数 %d", n)
	}
}

func TestIsClientAbortIgnoresUpstreamReset(t *testing.T) {
	// 上游自己 reset 时错误文本同样是 connection reset by peer，
	// 但没有写端标记、ctx 也活着 → 必须仍按真故障处理。
	if IsClientAbort(fmt.Errorf("read tcp %w", syscall.ECONNRESET)) {
		t.Fatal("上游读侧 ECONNRESET 被误判为客户端断开")
	}
	if IsClientAbort(nil) {
		t.Fatal("nil 不能判为客户端断开")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// —— 续传路径的连接回收 ——

type trackedBody struct {
	io.ReadCloser
	id     int
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return b.ReadCloser.Close()
}

type bodyTracker struct {
	next   http.RoundTripper
	mu     sync.Mutex
	bodies []*trackedBody
}

func (t *bodyTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	t.mu.Lock()
	b := &trackedBody{ReadCloser: resp.Body, id: len(t.bodies)}
	t.bodies = append(t.bodies, b)
	t.mu.Unlock()
	resp.Body = b
	return resp, nil
}

func (t *bodyTracker) unclosed() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, b := range t.bodies {
		if !b.closed {
			n++
		}
	}
	return n
}

// 续传之后必须把「自己打开的那一段」关掉：first 由调用方负责，其余一条都不能漏。
func TestCopyRangeClosesReopenedBodies(t *testing.T) {
	srv := httptest.NewServer(&countingHandler{total: 1 << 20, truncate: true})
	defer srv.Close()

	tracker := &bodyTracker{next: http.DefaultTransport}
	old := upstreamClient
	upstreamClient = &http.Client{Transport: tracker}
	defer func() { upstreamClient = old }()

	up, err := OpenRange(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	if _, err = CopyRange(context.Background(), discardWriter{}, srv.URL, up, nil,
		0, (1<<20)-1, true, ResumeRetries); err == nil {
		t.Fatal("截断流应报错")
	}
	_ = up.Body.Close() // 调用方收回它自己的那一段

	// 用例必须真的走过续传（否则测不到回收路径）
	tracker.mu.Lock()
	totalBodies := len(tracker.bodies)
	tracker.mu.Unlock()
	if totalBodies < 2 {
		t.Fatalf("没有发生续传，用例失效（响应数 %d）", totalBodies)
	}
	// CopyRange 打开的每一段（除调用方持有的 first 外）此刻必须全部已关
	if n := tracker.unclosed(); n != 0 {
		t.Fatalf("续传泄漏了 %d 条上游连接未关闭", n)
	}
}
