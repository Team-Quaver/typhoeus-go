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
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/weiyun"
)

const (
	weiyunTicketTTL    = 20 * time.Minute
	weiyunIndexTTL     = 30 * time.Minute
	weiyunAuthCodeTTL  = 5 * time.Minute
	weiyunAPIBase      = "https://www.weiyun.com"
	weiyunAuthCodePath = "/api/v3/mcp/token/code"
)

type weiyunTicket struct {
	URL     string
	Size    int64
	Cookie  string
	Name    string
	PdirKey string
	Expires time.Time
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
	a.invalidateWeiyunIndex()
	writeOK(w, map[string]any{"configured": false})
}

func (a *App) serveWeiyunTicket(w http.ResponseWriter, r *http.Request, ticket *weiyunTicket, fileID, pdirKey string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, ticket.URL, nil)
	if err != nil {
		writeErr(w, 502, -1, "下载链接无效")
		return
	}
	if ticket.Cookie != "" {
		req.Header.Set("Cookie", ticket.Cookie)
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	up, err := (&http.Client{Timeout: 0}).Do(req)
	if err == nil && (up.StatusCode == http.StatusForbidden || up.StatusCode == http.StatusNotFound) {
		_ = up.Body.Close()
		a.weiyunTicketsMu.Lock()
		delete(a.weiyunTickets, weiyunTicketID(fileID, pdirKey))
		a.weiyunTicketsMu.Unlock()
		ticket, err = a.weiyunDownloadTicket(r.Context(), fileID, pdirKey)
		if err == nil {
			req, err = http.NewRequestWithContext(r.Context(), r.Method, ticket.URL, nil)
			if err == nil {
				if ticket.Cookie != "" {
					req.Header.Set("Cookie", ticket.Cookie)
				}
				if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
					req.Header.Set("Range", rangeHeader)
				}
				req.Header.Set("User-Agent", "Mozilla/5.0")
				up, err = (&http.Client{Timeout: 0}).Do(req)
			}
		}
	}
	if err != nil {
		writeErr(w, 502, -1, "微云下载失败: "+err.Error())
		return
	}
	defer up.Body.Close()
	decrypt := r.URL.Query().Get("raw") != "1" && weiyunShouldDecrypt(ticket.Name)
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
	if r.URL.Query().Get("download") == "1" && ticket.Name != "" {
		name := ticket.Name
		if decrypt && strings.Contains(strings.ToLower(name), "[aq].") {
			// The stored name encodes the container as hex ("6f6767" = "ogg").
			// Present the decrypted stream under its real audio extension.
			name = strings.TrimSuffix(name, filepath.Ext(name)) + weiyunAudioExt(name)
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	for _, h := range []string{"Content-Length", "Content-Range"} {
		if v := up.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(up.StatusCode)
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
		PdirKey: pdirKey,
		Expires: now.Add(weiyunTicketTTL),
	}
	a.weiyunTicketsMu.Lock()
	a.weiyunTickets[id] = t
	a.weiyunTicketsMu.Unlock()
	cp := *t
	return &cp, nil
}

func (a *App) weiyunDownloadTicketNamed(ctx context.Context, fileID, pdirKey, name string) (*weiyunTicket, error) {
	if pdirKey == "" {
		f, err := a.weiyunIndexedFile(ctx, fileID)
		if err != nil {
			return nil, err
		}
		pdirKey = f.PdirKey
		name = f.FileName
	}
	t, err := a.weiyunDownloadTicket(ctx, fileID, pdirKey)
	if err != nil {
		return nil, err
	}
	if name != "" {
		t.Name = name
	}
	return t, nil
}

// handleWeiyunMusicFile resolves a QQ Music cloud row to a Weiyun file and
// streams it through the same QMC-decrypting relay. This is the music-only
// bridge; it deliberately has no directory-management operations.
func (a *App) handleWeiyunMusicFile(w http.ResponseWriter, r *http.Request) {
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

func weiyunAudioExt(name string) string {
	switch weiyunAudioMIME(name) {
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	default:
		return filepath.Ext(name)
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
