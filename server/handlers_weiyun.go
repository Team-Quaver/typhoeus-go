package server

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/typhoeus"
	"github.com/team-quaver/typhoeus-go/weiyun"
)

const (
	weiyunTicketTTL    = 20 * time.Minute
	weiyunIndexTTL     = 30 * time.Minute
	weiyunAuthCodeTTL  = 5 * time.Minute
	weiyunAPIBase      = "https://www.weiyun.com"
	weiyunAuthCodePath = "/api/v3/mcp/token/code"

	// 微云 CDN 对单条 HTTPS 连接大约只给 256KiB/s。24bit/48kHz 立体声 WAV
	// 本身就要约 288KB/s，单连接回源必然低于实时码率，表现为「播一段、卡一下」。
	// 多连接分段回源把吞吐拉回实时以上；分段仍按原始字节序拼接，QMC 解密偏移
	// 保持不变。256KiB 的分段让首段尽快到齐，后续分段在后台并行预取。
	weiyunParallelChunk    = 256 << 10
	weiyunParallelConns    = 6
	weiyunParallelMinBytes = 512 << 10
)

type weiyunTicket struct {
	URL     string
	Size    int64
	Cookie  string
	Name    string
	PdirKey string
	FileID  string
	Expires time.Time
}

type weiyunStreamSource struct {
	mu          sync.Mutex
	ticket      *weiyunTicket
	app         *App
	fileID      string
	refreshTest func(context.Context, string, string, string) (*weiyunTicket, error)
}

var weiyunCDNClient = newWeiyunCDNClient()

func newWeiyunCDNClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Timeout: 0}
	}
	clone := transport.Clone()
	clone.MaxIdleConns = 32
	clone.MaxIdleConnsPerHost = weiyunParallelConns + 2
	clone.IdleConnTimeout = 90 * time.Second

	return &http.Client{Timeout: 0, Transport: clone}
}

// weiyunCipher adapts the stateless QMCv1 transform to the generic stream
// entry without exposing a public qmc.Cipher constructor.
type weiyunCipher struct{}

func (weiyunCipher) Decrypt(data []byte, offset int64) []byte {
	out := append([]byte(nil), data...)
	qmc.DecryptV1InPlace(out, offset)
	return out
}

func (s *weiyunStreamSource) ticketCopy() *weiyunTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ticket == nil {
		return nil
	}
	cp := *s.ticket
	return &cp
}

// renew forces a new Weiyun download link. The cached link can still be
// syntactically valid while its CDN URL has already returned 403, so expiry
// alone is not enough to decide when to refresh.
func (s *weiyunStreamSource) renew(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ticket == nil {
		return errors.New("微云下载票据缺失")
	}
	pdirKey, name := s.ticket.PdirKey, s.ticket.Name
	var ticket *weiyunTicket
	var err error
	if s.refreshTest != nil {
		ticket, err = s.refreshTest(ctx, s.fileID, pdirKey, name)
	} else {
		s.app.invalidateWeiyunTicket(s.fileID, pdirKey)
		ticket, err = s.app.weiyunDownloadTicketNamed(ctx, s.fileID, pdirKey, name)
	}
	if err != nil {
		return err
	}
	s.ticket = ticket
	return nil
}

func (s *weiyunStreamSource) Refresh(ctx context.Context) (streamOpener, error) {
	ticket := s.ticketCopy()
	if ticket == nil {
		return s, errors.New("微云下载票据缺失")
	}
	// Avoid refreshing on every chunk; only react when the ticket is near expiry.
	if time.Until(ticket.Expires) > time.Minute {
		return s, nil
	}
	return s, s.renew(ctx)
}

func (weiyunCipher) DecryptInPlace(data []byte, offset int64) {
	qmc.DecryptV1InPlace(data, offset)
}

// Weiyun links can require a download cookie. This adapter lets the generic
// stream proxy/cache path use the same Typhoeus opener without leaking that
// cookie beyond the sidecar.
func (s *weiyunStreamSource) OpenRange(ctx context.Context, _ string, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	resp, err := s.open(ctx, rng)
	if err != nil || (resp.Status != http.StatusForbidden && resp.Status != http.StatusNotFound) {
		return resp, err
	}
	_ = resp.Body.Close()
	if rerr := s.renew(ctx); rerr != nil {
		return nil, rerr
	}
	return s.open(ctx, rng)
}

func (s *weiyunStreamSource) open(ctx context.Context, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	t := s.ticketCopy()
	if t == nil {
		return nil, errors.New("微云下载票据缺失")
	}

	return openWeiyunTicketRange(ctx, t, rng)
}

func openWeiyunTicketRange(ctx context.Context, ticket *weiyunTicket, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	if !weiyunParallelEligible(rng) {
		return openWeiyunTicketSingle(ctx, ticket, rng)
	}

	firstStart := *rng.Start
	firstEnd := firstStart + weiyunParallelChunk - 1
	if rng.End != nil && firstEnd > *rng.End {
		firstEnd = *rng.End
	}
	firstRange := &typhoeus.ByteRange{Start: &firstStart, End: &firstEnd}
	first, err := openWeiyunTicketSingle(ctx, ticket, firstRange)
	if err != nil || first.Status != http.StatusPartialContent {
		return first, err
	}
	if got := rangeContentStart(first.ContentRange); got >= 0 && got != firstStart {
		_ = first.Body.Close()
		return nil, fmt.Errorf("微云回源起点不符：期望 %d，上游给 %d", firstStart, got)
	}
	total := weiyunContentRangeTotal(first.ContentRange)
	if total <= 0 {
		return first, nil
	}
	last := total - 1
	if rng.End != nil && *rng.End < last {
		last = *rng.End
	}
	if last-firstStart+1 < weiyunParallelMinBytes || firstEnd >= last {
		return first, nil
	}

	body := newWeiyunParallelBody(ctx, ticket, first, firstStart, last, total)
	header := first.Header.Clone()
	header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", firstStart, last, total))
	header.Set("Content-Length", strconv.FormatInt(last-firstStart+1, 10))
	header.Del("Transfer-Encoding")

	return &typhoeus.UpstreamResponse{
		Status:       http.StatusPartialContent,
		Header:       header,
		ContentRange: header.Get("Content-Range"),
		Total:        total,
		Body:         body,
	}, nil
}

func weiyunParallelEligible(rng *typhoeus.ByteRange) bool {
	if rng == nil || rng.Start == nil || *rng.Start < 0 {
		return false
	}
	if rng.End != nil && *rng.End >= *rng.Start && *rng.End-*rng.Start+1 < weiyunParallelMinBytes {
		return false
	}

	return true
}

func openWeiyunTicketSingle(ctx context.Context, ticket *weiyunTicket, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	if ticket == nil {
		return nil, errors.New("微云下载票据缺失")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ticket.URL, nil)
	if err != nil {
		return nil, err
	}
	if ticket.Cookie != "" {
		req.Header.Set("Cookie", ticket.Cookie)
	}
	if rng != nil && (rng.Start != nil || rng.End != nil) {
		req.Header.Set("Range", rng.Header())
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := weiyunCDNClient.Do(req)
	if err != nil {
		return nil, err
	}
	cr := resp.Header.Get("Content-Range")
	return &typhoeus.UpstreamResponse{
		Status: resp.StatusCode, Header: resp.Header, ContentRange: cr,
		Total: weiyunContentRangeTotal(cr), Body: resp.Body,
	}, nil
}

type weiyunChunkResult struct {
	data []byte
	err  error
}

// weiyunParallelBody 把一段大 Range 拆成固定大小的子 Range 并行回源，再按
// 原始字节序串行交给播放器/缓存。首段保持流式，不等整段下载完，所以起播
// 延迟仍然只取决于首段；后续段由 worker 预取，播放器不会追着单连接跑。
type weiyunParallelBody struct {
	ctx     context.Context
	cancel  context.CancelFunc
	ticket  *weiyunTicket
	start   int64
	end     int64
	total   int64
	chunks  int
	first   io.ReadCloser
	readIdx int
	nextIdx int
	current []byte
	results map[int]*weiyunChunkResult
	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
}

func newWeiyunParallelBody(parent context.Context, ticket *weiyunTicket, first *typhoeus.UpstreamResponse,
	start, end, total int64) *weiyunParallelBody {
	ctx, cancel := context.WithCancel(parent)
	span := end - start + 1
	chunks := int((span + weiyunParallelChunk - 1) / weiyunParallelChunk)
	b := &weiyunParallelBody{
		ctx: ctx, cancel: cancel, ticket: ticket, start: start, end: end, total: total,
		chunks: chunks, first: first.Body, readIdx: 0, nextIdx: 1,
		results: map[int]*weiyunChunkResult{},
	}
	b.cond = sync.NewCond(&b.mu)
	for i := 0; i < weiyunParallelConns; i++ {
		go b.worker()
	}

	return b
}

func (b *weiyunParallelBody) worker() {
	for {
		b.mu.Lock()
		for !b.closed && (b.nextIdx >= b.chunks || b.nextIdx >= b.readIdx+weiyunParallelConns) {
			b.cond.Wait()
		}
		if b.closed {
			b.mu.Unlock()
			return
		}
		idx := b.nextIdx
		b.nextIdx++
		b.mu.Unlock()

		result := b.fetch(idx)

		b.mu.Lock()
		if !b.closed {
			b.results[idx] = result
			b.cond.Broadcast()
		}
		b.mu.Unlock()
	}
}

func (b *weiyunParallelBody) fetch(idx int) *weiyunChunkResult {
	start := b.start + int64(idx)*weiyunParallelChunk
	end := start + weiyunParallelChunk - 1
	if end > b.end {
		end = b.end
	}
	up, err := openWeiyunTicketSingle(b.ctx, b.ticket, &typhoeus.ByteRange{Start: &start, End: &end})
	if err != nil {
		return &weiyunChunkResult{err: err}
	}
	defer up.Body.Close()
	if up.Status != http.StatusPartialContent {
		return &weiyunChunkResult{err: fmt.Errorf("微云分段回源 HTTP %d", up.Status)}
	}
	if got := rangeContentStart(up.ContentRange); got >= 0 && got != start {
		return &weiyunChunkResult{err: fmt.Errorf("微云分段回源起点不符：期望 %d，上游给 %d", start, got)}
	}
	want := end - start + 1
	data, err := io.ReadAll(io.LimitReader(up.Body, want))
	if err != nil {
		return &weiyunChunkResult{err: err}
	}
	if int64(len(data)) != want {
		return &weiyunChunkResult{err: fmt.Errorf("微云分段回源提前结束（%d/%d 字节）", len(data), want)}
	}

	return &weiyunChunkResult{data: data}
}

func (b *weiyunParallelBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		first := b.first
		b.mu.Unlock()
		if first == nil {
			break
		}
		n, err := first.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			b.mu.Lock()
			if b.first == first {
				b.first = nil
				_ = first.Close()
				b.readIdx = 1
				b.cond.Broadcast()
			}
			b.mu.Unlock()
			return b.Read(p)
		}
		if err != nil {
			_ = b.Close()
			return 0, err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		if len(b.current) > 0 {
			n := copy(p, b.current)
			b.current = b.current[n:]
			if len(b.current) == 0 {
				b.current = nil
				b.readIdx++
				b.cond.Broadcast()
			}
			return n, nil
		}
		if b.readIdx >= b.chunks {
			return 0, io.EOF
		}
		if result, ok := b.results[b.readIdx]; ok {
			delete(b.results, b.readIdx)
			if result.err != nil {
				b.closed = true
				b.cancel()
				b.cond.Broadcast()
				return 0, result.err
			}
			b.current = result.data
			continue
		}
		b.cond.Broadcast()
		b.cond.Wait()
	}
}

func (b *weiyunParallelBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	first := b.first
	b.first = nil
	b.cancel()
	b.cond.Broadcast()
	b.mu.Unlock()
	if first != nil {
		return first.Close()
	}

	return nil
}

func weiyunContentRangeTotal(contentRange string) int64 {
	idx := strings.LastIndex(contentRange, "/")
	if idx < 0 {
		return 0
	}
	n, err := strconv.ParseInt(contentRange[idx+1:], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

type weiyunIndexCache struct {
	Files     map[string]weiyun.IndexedFile
	Expires   time.Time
	Generated uint64
}

type weiyunAuthFlow struct {
	Code      string
	ExpiresAt time.Time
}

func (a *App) weiyunClient() (*weiyun.Client, error) {
	auth := a.weiyunStore.load()
	if auth == nil {
		return nil, errors.New("尚未配置微云授权令牌")
	}
	return weiyun.NewClient(auth.Token, auth.MCPURL), nil
}

func (a *App) invalidateWeiyunIndex() {
	a.weiyunIndexMu.Lock()
	a.weiyunIndex = nil
	a.weiyunIndexGen++
	a.weiyunIndexMu.Unlock()
}

// weiyunFileIndex scans the directory tree once and reuses the file_id →
// pdir_key map for later Range requests. Concurrent first plays wait for the
// same scan instead of each walking the whole drive.
func (a *App) weiyunFileIndex(ctx context.Context) (*weiyunIndexCache, error) {
	a.weiyunIndexMu.Lock()
	if a.weiyunIndex != nil && time.Now().Before(a.weiyunIndex.Expires) {
		idx := a.weiyunIndex
		a.weiyunIndexMu.Unlock()
		return idx, nil
	}
	if wait := a.weiyunIndexLoad; wait != nil {
		a.weiyunIndexMu.Unlock()
		select {
		case <-wait:
			return a.weiyunFileIndex(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	wait := make(chan struct{})
	a.weiyunIndexLoad = wait
	gen := a.weiyunIndexGen
	a.weiyunIndexMu.Unlock()

	c, err := a.weiyunClient()
	var idx *weiyun.IndexResult
	if err == nil {
		idx, err = c.IndexFiles(ctx, weiyun.IndexOptions{})
	}
	if err == nil && idx != nil {
		// Persist the whole scan once in the background. Otherwise every app
		// restart throws away the in-memory location map and the first cloud
		// play pays for a full drive walk again.
		go a.rememberCloudLocations(weiyunLocationsOf(idx.Files))
	}

	a.weiyunIndexMu.Lock()
	if err == nil && gen == a.weiyunIndexGen {
		a.weiyunIndex = &weiyunIndexCache{
			Files: idx.Files, Expires: time.Now().Add(weiyunIndexTTL), Generated: gen,
		}
	}
	if a.weiyunIndexLoad == wait {
		a.weiyunIndexLoad = nil
		close(wait)
	}
	cache := a.weiyunIndex
	a.weiyunIndexMu.Unlock()
	if err != nil {
		return nil, err
	}
	if cache == nil || cache.Generated != gen {
		return a.weiyunFileIndex(ctx)
	}
	return cache, nil
}

func weiyunLocationsOf(files map[string]weiyun.IndexedFile) map[string]cloudLocation {
	locations := make(map[string]cloudLocation, len(files))
	for fileID, f := range files {
		locations[fileID] = cloudLocation{PdirKey: f.PdirKey, FileName: f.FileName}
	}
	return locations
}

func (a *App) weiyunIndexedFile(ctx context.Context, fileID string) (*weiyun.IndexedFile, error) {
	idx, err := a.weiyunFileIndex(ctx)
	if err != nil {
		return nil, err
	}
	f, ok := idx.Files[fileID]
	if !ok {
		return nil, errors.New("微云目录中找不到该文件")
	}
	return &f, nil
}

// warmWeiyunTickets fetches short-lived download URLs for the whole cloud page
// in one batch. Resolving each track separately costs one MCP round trip on the
// first switch; batching moves that latency to the page-open background work.
func (a *App) warmWeiyunTickets(fileIDs []string) {
	if len(fileIDs) == 0 {
		return
	}
	c, err := a.weiyunClient()
	if err != nil {
		return
	}
	index := a.loadCloudIndex()
	a.cloudMu.RLock()
	locations := make(map[string]cloudLocation, len(fileIDs))
	for _, fileID := range fileIDs {
		if loc, ok := index.Locations[fileID]; ok && loc.PdirKey != "" {
			locations[fileID] = loc
		}
	}
	a.cloudMu.RUnlock()

	items := make([]weiyun.DownloadItem, 0, len(locations))
	for _, fileID := range fileIDs {
		if _, ok := locations[fileID]; !ok {
			continue
		}
		if a.cachedWeiyunTicket(fileID) != nil {
			continue
		}
		items = append(items, weiyun.DownloadItem{FileID: fileID, PdirKey: locations[fileID].PdirKey})
		if len(items) >= 50 {
			break
		}
	}
	if len(items) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, err := c.Download(ctx, items)
	if err != nil {
		logDebug("微云下载票据预热失败：%v", err)
		return
	}
	now := time.Now()
	a.weiyunTicketsMu.Lock()
	for i, result := range results {
		fileID := result.FileID
		if fileID == "" && i < len(items) {
			fileID = items[i].FileID
		}
		loc, ok := locations[fileID]
		if !ok || result.HTTPSDownloadURL == "" {
			continue
		}
		a.weiyunTickets[weiyunTicketID(fileID, loc.PdirKey)] = &weiyunTicket{
			URL: result.HTTPSDownloadURL, Size: result.FileSize, Cookie: result.Cookie,
			Name: loc.FileName, PdirKey: loc.PdirKey, FileID: fileID,
			Expires: now.Add(weiyunTicketTTL),
		}
	}
	a.weiyunTicketsMu.Unlock()
	logDebug("微云下载票据已预热：%d 个", len(results))
}

func weiyunStatusError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "尚未配置"), strings.Contains(msg, "令牌未配置"):
		writeErr(w, http.StatusUnauthorized, -http.StatusUnauthorized, msg)
	case strings.Contains(msg, "not authorized"), strings.Contains(msg, "token"), strings.Contains(msg, "权限"):
		writeErr(w, http.StatusUnauthorized, -http.StatusUnauthorized, msg)
	default:
		writeErr(w, http.StatusBadGateway, -http.StatusBadGateway, msg)
	}
}

func (a *App) handleWeiyunStatus(w http.ResponseWriter, _ *http.Request) {
	auth := a.weiyunStore.load()
	hasQQ := a.session != nil && a.session.Credential() != nil && a.session.Credential().HasLogin()
	writeOK(w, map[string]any{
		"configured":      auth != nil,
		"endpoint":        weiyun.DefaultMCPURL,
		"hasQqCredential": hasQQ,
	})
}

func (a *App) handleWeiyunAuthSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		MCPURL string `json:"mcpUrl"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeErr(w, 422, -1, "缺少微云令牌")
		return
	}
	if err := a.weiyunStore.save(body.Token, body.MCPURL); err != nil {
		writeErr(w, 500, -1, "保存微云授权失败")
		return
	}
	if a.session.Mode() == ModeExternal {
		a.session.emitWeiyunAuth(body.Token, body.MCPURL)
	}
	a.invalidateWeiyunIndex()
	c, err := a.weiyunClient()
	if err != nil {
		writeErr(w, 500, -1, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if _, err := c.List(ctx, weiyun.ListOptions{Limit: 1}); err != nil {
		_ = a.weiyunStore.clear()
		a.invalidateWeiyunIndex()
		weiyunStatusError(w, fmt.Errorf("令牌校验失败: %w", err))
		return
	}
	writeOK(w, map[string]any{"configured": true})
}

func (a *App) handleWeiyunAuthStart(w http.ResponseWriter, _ *http.Request) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		writeErr(w, http.StatusInternalServerError, -1, "生成微云授权码失败")
		return
	}
	code := hex.EncodeToString(buf)
	expires := time.Now().Add(weiyunAuthCodeTTL)
	a.weiyunAuthMu.Lock()
	a.weiyunAuthFlow = &weiyunAuthFlow{Code: code, ExpiresAt: expires}
	a.weiyunAuthMu.Unlock()
	writeOK(w, map[string]any{
		"url":          weiyunAPIBase + "/authorize/token?code=" + code,
		"expiresInSec": int(weiyunAuthCodeTTL / time.Second),
	})
}

func (a *App) currentWeiyunAuthCode() (string, error) {
	a.weiyunAuthMu.Lock()
	defer a.weiyunAuthMu.Unlock()
	flow := a.weiyunAuthFlow
	if flow == nil {
		return "", errors.New("尚未发起微云授权")
	}
	if time.Now().After(flow.ExpiresAt) {
		a.weiyunAuthFlow = nil
		return "", errors.New("微云授权码已过期，请重新发起授权")
	}
	return flow.Code, nil
}

func exchangeWeiyunToken(ctx context.Context, apiBase, code string) (string, int, string, error) {
	raw, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+weiyunAuthCodePath, strings.NewReader(string(raw)))
	if err != nil {
		return "", 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Quaver/1.7 WeiyunAuth")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", 0, "", fmt.Errorf("连接微云授权服务失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, "", fmt.Errorf("读取微云授权响应失败: %w", err)
	}
	var out struct {
		Token   string `json:"token"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, "", fmt.Errorf("微云授权响应格式异常")
	}
	if out.Token != "" {
		return out.Token, 0, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Code == 0 {
			out.Code = resp.StatusCode
		}
		if out.Message == "" {
			out.Message = http.StatusText(resp.StatusCode)
		}
	}
	return "", out.Code, out.Message, nil
}

func (a *App) handleWeiyunAuthPoll(w http.ResponseWriter, r *http.Request) {
	code, err := a.currentWeiyunAuthCode()
	if err != nil {
		writeErr(w, http.StatusGone, -1, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	token, apiCode, message, err := exchangeWeiyunToken(ctx, weiyunAPIBase, code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, -1, err.Error())
		return
	}
	if token == "" {
		if apiCode == 11510 {
			writeErr(w, http.StatusConflict, 11510, "尚未完成微云授权")
			return
		}
		if message == "" {
			message = "微云授权失败"
		}
		writeErr(w, http.StatusBadGateway, -1, fmt.Sprintf("%s（code=%d）", message, apiCode))
		return
	}

	c := weiyun.NewClient(token, "")
	if _, err := c.List(ctx, weiyun.ListOptions{Limit: 1}); err != nil {
		weiyunStatusError(w, fmt.Errorf("授权令牌校验失败: %w", err))
		return
	}
	if err := a.weiyunStore.save(token, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, -1, "保存微云授权失败")
		return
	}
	if a.session.Mode() == ModeExternal {
		a.session.emitWeiyunAuth(token, "")
	}
	a.invalidateWeiyunIndex()
	a.weiyunAuthMu.Lock()
	a.weiyunAuthFlow = nil
	a.weiyunAuthMu.Unlock()
	writeOK(w, map[string]any{"configured": true})
}

func (a *App) handleWeiyunAuthClear(w http.ResponseWriter, _ *http.Request) {
	if err := a.weiyunStore.clear(); err != nil {
		writeErr(w, 500, -1, "清除微云授权失败")
		return
	}
	if a.session.Mode() == ModeExternal {
		a.session.emitWeiyunAuth("", "")
	}
	a.invalidateWeiyunIndex()
	writeOK(w, map[string]any{"configured": false})
}

func (a *App) serveWeiyunTicket(w http.ResponseWriter, r *http.Request, ticket *weiyunTicket, fileID, pdirKey string) {
	rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
	rng := typhoeus.ParseRange(rangeHeader)
	if rangeHeader != "" && (rng == nil || (rng.Start != nil && *rng.Start < 0)) {
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, -1, "微云音频 Range 无效")
		return
	}
	up, err := openWeiyunTicketRange(r.Context(), ticket, rng)
	if err == nil && (up.Status == http.StatusForbidden || up.Status == http.StatusNotFound) {
		_ = up.Body.Close()
		name := ticket.Name
		a.invalidateWeiyunTicket(fileID, pdirKey)
		ticket, err = a.weiyunDownloadTicketNamed(r.Context(), fileID, pdirKey, name)
		if err == nil {
			up, err = openWeiyunTicketRange(r.Context(), ticket, rng)
		}
	}
	if err != nil {
		writeErr(w, 502, -1, "微云下载失败: "+err.Error())
		return
	}
	defer up.Body.Close()
	decrypt := weiyunShouldDecrypt(ticket.Name)
	contentType := up.Header.Get("Content-Type")
	if decrypt {
		contentType = weiyunAudioMIME(ticket.Name)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var decryptStart int64
	if decrypt {
		// Upstream Content-Range is authoritative for suffix/open ranges such as
		// "bytes=-4096" or "bytes=123-"; the request header alone cannot always
		// reveal the absolute start needed by the stateless QMCv1 transform.
		start, rangeErr := weiyunContentRangeStart(up.Header.Get("Content-Range"))
		if rangeErr != nil {
			start, rangeErr = weiyunRangeStart(r.Header.Get("Range"))
		}
		if rangeErr != nil {
			writeErr(w, http.StatusRequestedRangeNotSatisfiable, -1, "微云音频 Range 无法定位解密偏移")
			return
		}
		decryptStart = start
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	// This endpoint is playback-only. An attachment disposition would turn the
	// guarded relay into a browser download feature, so it is deliberately absent.
	w.Header().Set("Content-Disposition", "inline")
	for _, h := range []string{"Content-Length", "Content-Range"} {
		if v := up.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(up.Status)
	// Weiyun's music upload pipeline returns QMCv1-encrypted audio. Playback and
	// downloads go through this same endpoint, so decrypt transparently by the
	// absolute byte offset. Range seeks remain valid because QMCv1 is stateless.
	if decrypt {
		_, _ = io.Copy(w, &weiyunDecryptReader{r: up.Body, offset: decryptStart})
		return
	}
	_, _ = io.Copy(w, up.Body)
}

func (a *App) weiyunDownloadTicket(ctx context.Context, fileID, pdirKey string) (*weiyunTicket, error) {
	id := weiyunTicketID(fileID, pdirKey)
	now := time.Now()
	a.weiyunTicketsMu.Lock()
	t := a.weiyunTickets[id]
	a.weiyunTicketsMu.Unlock()
	if t != nil && t.Expires.After(now) {
		cp := *t
		return &cp, nil
	}
	c, err := a.weiyunClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	res, err := c.Download(ctx, []weiyun.DownloadItem{{FileID: fileID, PdirKey: pdirKey}})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 || res[0].HTTPSDownloadURL == "" {
		return nil, errors.New("没有拿到下载链接")
	}
	item := res[0]
	t = &weiyunTicket{
		URL: item.HTTPSDownloadURL, Size: item.FileSize, Cookie: item.Cookie,
		PdirKey: pdirKey, FileID: fileID,
		Expires: now.Add(weiyunTicketTTL),
	}
	a.weiyunTicketsMu.Lock()
	a.weiyunTickets[id] = t
	a.weiyunTicketsMu.Unlock()
	cp := *t
	return &cp, nil
}

func (a *App) weiyunDownloadTicketNamed(ctx context.Context, fileID, pdirKey, name string) (*weiyunTicket, error) {
	// A live ticket already contains the download URL, size and cookie; no
	// directory scan is needed just to find its name. This keeps repeat
	// playback instant even when the location cache is cold.
	if t := a.cachedWeiyunTicket(fileID); t != nil {
		a.nameWeiyunTicket(t, name)

		return t, nil
	}
	if pdirKey == "" {
		if loc, ok := a.cloudLocationOf(fileID); ok {
			pdirKey = loc.PdirKey
			if name == "" {
				name = loc.FileName
			}
		}
	}
	if pdirKey == "" {
		f, err := a.weiyunIndexedFile(ctx, fileID)
		if err != nil {
			return nil, err
		}
		pdirKey = f.PdirKey
		name = f.FileName
		// The caller (resolve) is on the playback hot path; writing library.json
		// synchronously here can add disk I/O before the first byte. The cloud
		// page warms the whole location map in one batched write instead.
	}
	t, err := a.weiyunDownloadTicket(ctx, fileID, pdirKey)
	if err != nil {
		return nil, err
	}
	a.nameWeiyunTicket(t, name)
	return t, nil
}

// nameWeiyunTicket keeps the cached ticket in sync with the caller-provided
// file name. The ticket was historically stored before Name was attached, so a
// later resolve could find a live ticket with an empty name and mark an
// encrypted OGG/FLAC upload as application/octet-stream + unencrypted.
func (a *App) nameWeiyunTicket(t *weiyunTicket, name string) {
	name = strings.TrimSpace(name)
	if t == nil || name == "" {
		return
	}
	t.Name = name
	a.weiyunTicketsMu.Lock()
	if cached := a.weiyunTickets[weiyunTicketID(t.FileID, t.PdirKey)]; cached != nil {
		cached.Name = name
	}
	a.weiyunTicketsMu.Unlock()
}

func (a *App) cachedWeiyunTicket(fileID string) *weiyunTicket {
	now := time.Now()
	a.weiyunTicketsMu.Lock()
	defer a.weiyunTicketsMu.Unlock()
	for _, t := range a.weiyunTickets {
		if t != nil && t.Expires.After(now) && t.FileID == fileID {
			cp := *t

			return &cp
		}
	}

	return nil
}

func (a *App) invalidateWeiyunTicket(fileID, pdirKey string) {
	a.weiyunTicketsMu.Lock()
	delete(a.weiyunTickets, weiyunTicketID(fileID, pdirKey))
	a.weiyunTicketsMu.Unlock()
}

// handleWeiyunMusicFile resolves a QQ Music cloud row to a Weiyun file and
// streams it through the same QMC-decrypting relay. This is the music-only
// bridge; it deliberately has no directory-management operations.
func (a *App) handleWeiyunMusicFile(w http.ResponseWriter, r *http.Request) {
	// StreamGuard parity with /stream and /cache/music/stream. Weiyun relay URLs
	// are stable because they contain no short-lived ticket; without this gate a
	// copied URL remains replayable for the ticket cache lifetime.
	if cls := classifyStreamClient(r); cls == classBlocked {
		writeErr(w, http.StatusForbidden, -1, "微云播放流仅允许应用内播放器取用")
		return
	}
	fileID := r.PathValue("fileID")
	if fileID == "" {
		writeErr(w, 422, -1, "缺少 file_id")
		return
	}
	name := r.URL.Query().Get("name")
	ticket, err := a.weiyunDownloadTicketNamed(r.Context(), fileID, "", name)
	if err != nil {
		weiyunStatusError(w, err)
		return
	}
	a.serveWeiyunTicket(w, r, ticket, fileID, ticket.PdirKey)
}

// handleWeiyunStreamResolve brings Weiyun playback into the same token/cache
// pipeline as QQ streams. It keeps LocalGuard on and gives StreamGuard a token
// URL instead of a stable file-id URL.
func (a *App) handleWeiyunStreamResolve(w http.ResponseWriter, r *http.Request, body streamResolveBody) {
	if strings.TrimSpace(body.CloudFileID) == "" {
		writeErr(w, 422, -1, "缺少微云 file_id")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	ticket, err := a.weiyunDownloadTicketNamed(r.Context(), body.CloudFileID, "", name)
	if err != nil {
		weiyunStatusError(w, err)
		return
	}
	source := &weiyunStreamSource{ticket: ticket, app: a, fileID: body.CloudFileID}
	// The download ticket already carries file_size. A second open here used to
	// issue a full-file GET with no Range, so the resolve call downloaded the
	// entire track before returning and the player appeared to hang. When the
	// ticket lacks a size, the first playback Range supplies Content-Range.
	//
	// The audio cache is also deliberately not started here: it would compete
	// with the first player request for the same mid rate bucket and halve the
	// playback bandwidth. It starts after the stream has actually delivered
	// data (see noteStreamProgress / cacheStoreIfIdle).
	a.purgeExpired()
	token := newToken()
	now := time.Now()
	entry := &streamEntry{
		mid:        body.Mid,
		url:        ticket.URL,
		mime:       weiyunAudioMIME(ticket.Name),
		total:      ticket.Size,
		filename:   ticket.Name,
		tier:       "weiyun",
		encrypted:  weiyunShouldDecrypt(ticket.Name),
		cipher:     weiyunCipher{},
		expiresAt:  now.Add(streamIdleTTL),
		createdAt:  now,
		degraded:   false,
		requested:  "weiyun",
		durationMs: body.DurationMs,
		opener:     source,
		cacheSeed:  body.CacheSeed,
	}
	a.streamMu.Lock()
	a.streams[token] = entry
	a.streamMu.Unlock()
	a.paceBucket(entry.mid, paceRateFor(entry.total, body.DurationMs))
	writeOK(w, map[string]any{
		"token":          token,
		"path":           "/stream/" + token,
		"tier":           "weiyun",
		"tier_label":     "微云文件",
		"requested_tier": "weiyun",
		"degraded":       false,
		"mime":           entry.mime,
		"size":           entry.total,
		"filename":       entry.filename,
		"encrypted":      entry.encrypted,
	})
}

func weiyunTicketID(fileID, pdirKey string) string {
	sum := sha1.Sum([]byte(fileID + "\x00" + pdirKey))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func weiyunShouldDecrypt(name string) bool {
	lower := strings.ToLower(name)
	// Weiyun's music pipeline names encrypted files "...[aq].6f6767";
	// 6f6767 is ASCII "ogg" in hex. Only explicit encrypted markers are
	// transformed: a normal ".ogg" upload must pass through untouched.
	if strings.Contains(lower, "[aq].") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mgg", ".mflac", ".6f6767", ".6d7033", ".666c6163", ".776176":
		return true
	default:
		return false
	}
}

func weiyunAudioMIME(name string) string {
	lower := strings.ToLower(name)
	hexExt := strings.TrimPrefix(filepath.Ext(lower), ".")
	switch hexExt {
	case "776176":
		return "audio/wav"
	case "6f6767":
		return "audio/ogg"
	case "666c6163":
		return "audio/flac"
	case "6d7033":
		return "audio/mpeg"
	}
	switch {
	case strings.HasSuffix(lower, ".ogg"), strings.HasSuffix(lower, ".oga"), strings.HasSuffix(lower, ".mgg"):
		return "audio/ogg"
	case strings.HasSuffix(lower, ".flac"), strings.HasSuffix(lower, ".mflac"):
		return "audio/flac"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// weiyunRangeStart parses only the single-range form emitted by <audio>/mpv.
func weiyunRangeStart(header string) (int64, error) {
	if header == "" {
		return 0, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, errors.New("不支持的 Range")
	}
	first := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	left, _, ok := strings.Cut(first, "-")
	if !ok || left == "" {
		return 0, errors.New("不支持的 Range")
	}
	n, err := strconv.ParseInt(left, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("不支持的 Range")
	}
	return n, nil
}

// weiyunContentRangeStart parses the absolute start of a single upstream
// Content-Range ("bytes 123-456/789"). It also covers suffix/open ranges that
// weiyunRangeStart cannot resolve from the request header alone.
func weiyunContentRangeStart(header string) (int64, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bytes ") {
		return 0, errors.New("缺少 Content-Range")
	}
	spec := strings.TrimSpace(header[len("bytes "):])
	if strings.HasPrefix(spec, "*/") || strings.Contains(spec, ",") {
		return 0, errors.New("不支持的 Content-Range")
	}
	span, _, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, errors.New("Content-Range 格式错误")
	}
	left, _, ok := strings.Cut(span, "-")
	if !ok || left == "" {
		return 0, errors.New("Content-Range 缺少起点")
	}
	n, err := strconv.ParseInt(left, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("Content-Range 起点无效")
	}
	return n, nil
}

type weiyunDecryptReader struct {
	r      io.Reader
	offset int64
}

func (d *weiyunDecryptReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if n > 0 {
		qmc.DecryptV1InPlace(p[:n], d.offset)
		d.offset += int64(n)
	}
	return n, err
}
