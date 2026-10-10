package qqmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	musicuURL = "https://u.y.qq.com/cgi-bin/musicu.fcg"
	musicsURL = "https://u.y.qq.com/cgi-bin/musics.fcg"
)

// Client QQ 音乐 API 客户端：凭证 + 设备指纹 + 会话 + HTTP 池的一站式封装。
//
// 并发安全：Credential 采用整体替换；设备指纹启动时加载一次；
// QIMEI / Android 会话按需懒加载并带缓存。
type Client struct {
	mu   sync.RWMutex
	cred *Credential

	device         *Device
	cachePath      string // device.cache.json 路径（空 = 不持久化）
	qimeiMu        sync.Mutex
	qimei          *qimeiResult
	qimeiAt        int64
	sessionMu      sync.Mutex
	androidSession *AndroidSession

	http     *http.Client
	httpOnce sync.Once
}

// ClientOption 允许嵌入方提供 HTTP 客户端（也用于离线协议回归测试）。
type ClientOption func(*Client)

// WithHTTPClient 在构造时指定 HTTP 客户端；调用方负责其 Transport/超时配置。
func WithHTTPClient(client *http.Client) ClientOption {
	return func(cl *Client) { cl.http = client }
}

// NewClient 构造客户端。devicePath 为空则生成一次性内存设备；
// 设备缓存路径缺省由 devicePath 派生（device.json → device.cache.json）。
func NewClient(cred *Credential, devicePath, deviceCachePath string, options ...ClientOption) (*Client, error) {
	if cred == nil {
		cred = &Credential{}
	}
	if deviceCachePath == "" && devicePath != "" {
		deviceCachePath = strings.TrimSuffix(devicePath, ".json") + ".cache.json"
	}
	cl := &Client{
		cred:      cred,
		cachePath: deviceCachePath,
	}
	dev, err := loadOrInitDevice(devicePath)
	if err != nil {
		return nil, err
	}
	cl.device = dev
	for _, option := range options {
		option(cl)
	}
	return cl, nil
}

// httpClient 懒初始化 HTTP 池。
func (cl *Client) httpClient() *http.Client {
	cl.httpOnce.Do(func() {
		if cl.http != nil {
			return
		}
		cl.http = &http.Client{
			Timeout: 35 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	})
	return cl.http
}

// Credential 当前凭证快照。
func (cl *Client) Credential() *Credential {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.cred
}

// SetCredential 整体替换凭证（登录/刷新/登出后调用）。
func (cl *Client) SetCredential(c *Credential) {
	if c == nil {
		c = &Credential{}
	}
	if c.LoginType == 0 {
		c.LoginType = inferLoginType(c.MusicKey)
	}
	cl.mu.Lock()
	cl.cred = c
	cl.mu.Unlock()
}

// Device 返回设备指纹。
func (cl *Client) Device() *Device { return cl.device }

// ---- QIMEI / Android 会话 ----

// qimei36 返回缓存的 QIMEI36（拿不到时为空串，不阻塞业务请求）。
func (cl *Client) qimei36() string {
	if q, err := cl.qimeiCached(); err == nil && q != nil {
		return q.Q36
	}
	return ""
}

// qimeiCached 获取 QIMEI（磁盘缓存 24h）。
func (cl *Client) qimeiCached() (*qimeiResult, error) {
	cl.qimeiMu.Lock()
	defer cl.qimeiMu.Unlock()
	now := time.Now().Unix()
	if cl.qimei != nil && now-cl.qimeiAt < 86400 {
		return cl.qimei, nil
	}
	if cl.cachePath != "" {
		if c := readQimeiCache(cl.cachePath); c != nil && now-c.SavedAt < 86400 {
			cl.qimei = &qimeiResult{Q16: c.Q16, Q36: c.Q36}
			cl.qimeiAt = c.SavedAt
			return cl.qimei, nil
		}
	}
	q, err := fetchQimei(cl, cl.device, defaultVersionProfiles[PlatformAndroid])
	if err != nil {
		return nil, err
	}
	cl.qimei = q
	cl.qimeiAt = now
	if cl.cachePath != "" {
		writeQimeiCache(cl.cachePath, q)
	}
	return q, nil
}

// ---- HTTP 基础设施 ----

// newRequest 构造带默认头的请求。
func newRequest(method, rawURL string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, errNetwork(err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doRequest 执行请求并读取完整响应体，返回响应（Body 已读完但未 Close，调用方 Close）。
// 网络错误按超时/其他归类；HTTP 状态码交由调用方按场景处理。
func (cl *Client) doRequest(req *http.Request, timeout time.Duration, noRedirect bool) (*http.Response, []byte, error) {
	client := cl.httpClient()
	if noRedirect {
		// 一次性 client 复用同一 Transport，只改重定向策略（并发安全）
		cp := *client
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &cp
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "Client.Timeout") {
			return nil, nil, errTimeout(msg)
		}
		return nil, nil, errNetwork(msg)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, errNetwork("读取响应失败: " + err.Error())
	}
	return resp, body, nil
}

// ---- CGI 请求 ----

// CGIOption CGI 请求可选项。
type CGIOption struct {
	Platform        Platform // 缺省 ANDROID
	Comm            *JObj    // 请求级 comm 覆盖
	Credential      *Credential
	RequireLogin    bool
	Sign            bool // 走 musics.fcg 带 zzc 签名
	AllowErrorCodes []int
	// PreserveBool=true 时参数里的布尔保持 JSON true/false；
	// 缺省按上游 bool_to_int 语义递归转成 0/1（与 vendor 实现一致）。
	PreserveBool bool
	Cookies      map[string]string // 请求级 Cookie（QQ OAuth 换码使用，不写入共享客户端）
	Headers      map[string]string
}

// CgiCall 发起一次 CGI 调用，返回子响应原文（json.RawMessage，含 code/data）。
//
// 上游 SDK 的「多请求合并」在服务端场景收益极小（每个用户请求通常 1 次 CGI），
// 这里保持一调用一请求的简单模型。
func (cl *Client) CgiCall(module, method string, param *JObj, opt CGIOption) (json.RawMessage, error) {
	cred := opt.Credential
	if cred == nil {
		cred = cl.Credential()
	}
	if opt.RequireLogin && !cred.HasLogin() {
		return nil, errCredentialInvalid("请求需要登录, 未提供有效的登录凭证")
	}
	platform := opt.Platform
	if platform == "" {
		platform = PlatformAndroid
	}
	if platform == PlatformAndroid {
		// Android 平台需要匿名会话；失败不阻塞（uid/sid 缺省跳过）
		_, _ = cl.ensureAndroidSession()
	}

	comm := cl.BuildComm(platform, cred)
	mergeCommOverrides(comm, opt.Comm)

	if !opt.PreserveBool {
		param = BoolToIntParams(param)
	}
	payload := NewJObj().
		Set("comm", comm).
		Set("req_0", NewJObj().
			Set("module", module).
			Set("method", method).
			Set("param", param))

	body := payload.Marshal()
	if os.Getenv("QSG_DEBUG_CGI") != "" {
		fmt.Fprintln(os.Stderr, "[CGI]", module, method, "payload_bytes=", len(body))
	}
	rawURL := musicuURL
	if opt.Sign {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		rawURL = musicsURL + "?_=" + ts + "&sign=" + ZzcSign(body)
	}
	req, err := newRequest("POST", rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", cl.UserAgent(platform))
	for k, v := range opt.Headers {
		req.Header.Set(k, v)
	}
	// Web/desktop CGI 依赖 Cookie 鉴权；只有 comm.uin/g_tk 并不携带登录凭证。
	// 请求级 Cookie 覆盖同名值，QQ OAuth 用空 Credential，避免串入旧账号。
	cookies := map[string]string{}
	if platform != PlatformAndroid {
		cookies = credentialCookies(cred)
	}
	for k, v := range opt.Cookies {
		cookies[k] = v
	}
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, respBody, err := cl.doRequest(req, 15*time.Second, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errHTTP(fmt.Sprintf("CGI 上游返回 %s", resp.Status), resp.StatusCode)
	}
	return unwrapCGIResponse(respBody, 0)
}

// unwrapCGIResponse 解开 {code, req_N:{...}} 信封，返回第 idx 个子响应原文。
func unwrapCGIResponse(body []byte, idx int) (json.RawMessage, error) {
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errData("CGI 响应非 JSON: " + err.Error())
	}
	if envelope.Code != 0 {
		return nil, errGlobal(envelope.Code, string(body))
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, errData("CGI 响应格式异常: " + err.Error())
	}
	item, ok := keys[fmt.Sprintf("req_%d", idx)]
	if !ok {
		return nil, errData("CGI 响应缺少 req_" + strconv.Itoa(idx))
	}
	return item, nil
}

// ParseCGIData 校验子响应业务码并返回 data 原始字节（透传语义）。
func ParseCGIData(item json.RawMessage, allowErrorCodes []int) (json.RawMessage, error) {
	var sub struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(item, &sub); err != nil {
		return nil, errData("CGI 子响应格式异常: " + err.Error())
	}
	code := 0
	if sub.Code != nil {
		code = *sub.Code
	}
	if code == 0 || containsInt(allowErrorCodes, code) {
		if len(sub.Data) == 0 {
			return json.RawMessage("{}"), nil
		}
		return sub.Data, nil
	}
	if kind, ok := cgiErrorMap[code]; ok {
		msg := "CGI 请求错误"
		switch kind {
		case ErrKindSignature:
			msg = "请求需要签名"
		case ErrKindRatelimited:
			msg = ratelimitedMessage
		case ErrKindCredentialExpired:
			msg = expiredMessage
		}
		return nil, errCGIKind(kind, code, msg, string(sub.Data))
	}
	return nil, errCGI(code, string(sub.Data))
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- 普通 HTTP 请求（登录流程用）----

// RawResponse 普通 HTTP 响应快照（对标 RawPayload）。
type RawResponse struct {
	StatusCode int
	Headers    http.Header
	Cookies    map[string]string // 响应 Set-Cookie 名值快照
	Text       string
	Body       []byte
}

// BodyBytes 响应体字节。
func (r *RawResponse) BodyBytes() []byte { return r.Body }

// HTTPParams / HTTPForm 查询串与表单的类型别名。
type (
	HTTPParams = url.Values
	HTTPForm   = url.Values
)

// P 快捷构造 HTTPParams/HTTPForm：P("k1", v1, "k2", v2 ...)。
func P(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// HTTPOption 普通 HTTP 请求可选项。
type HTTPOption struct {
	Params         url.Values
	FormBody       url.Values        // application/x-www-form-urlencoded 请求体
	Body           []byte            // 原始请求体（优先级低于 FormBody）
	Cookies        map[string]string // 追加 Cookie（覆盖凭证 Cookie）
	Headers        map[string]string
	Credential     *Credential
	NoRedirect     bool
	TimeoutSeconds float64
}

// DoHTTPPlain 发起普通 HTTP 请求。
//
// 与上游 HttpExecutor 一致：凭证非空时自动携带 uin/qqmusic_uin/qm_keyst/qqmusic_key
// 四枚 Cookie（登录态接口靠它），请求级 Cookie 可覆盖。
func (cl *Client) DoHTTPPlain(method, rawURL string, opt HTTPOption) (*RawResponse, error) {
	return cl.DoHTTPPlainContext(context.Background(), method, rawURL, opt)
}

// DoHTTPPlainContext 带 ctx 的普通 HTTP 请求（可取消/超时）。
func (cl *Client) DoHTTPPlainContext(ctx context.Context, method, rawURL string, opt HTTPOption) (*RawResponse, error) {
	if len(opt.Params) > 0 {
		rawURL += "?" + opt.Params.Encode()
	}
	var body []byte
	switch {
	case opt.FormBody != nil:
		body = []byte(opt.FormBody.Encode())
		opt.Headers = orInit(opt.Headers)
		opt.Headers["Content-Type"] = "application/x-www-form-urlencoded"
	case opt.Body != nil:
		body = opt.Body
	}
	req, err := newRequest(method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	for k, v := range opt.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", cl.UserAgent(PlatformWeb))
	}
	cred := opt.Credential
	if cred == nil {
		cred = cl.Credential()
	}
	cookies := credentialCookies(cred)
	for k, v := range opt.Cookies {
		cookies[k] = v
	}
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	timeout := 15 * time.Second
	if opt.TimeoutSeconds > 0 {
		timeout = time.Duration(opt.TimeoutSeconds * float64(time.Second))
	}
	resp, respBody, err := cl.doRequest(req, timeout, opt.NoRedirect)
	if err != nil {
		return nil, err
	}
	snap := &RawResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Cookies:    map[string]string{},
		Text:       string(respBody),
		Body:       respBody,
	}
	for _, c := range resp.Cookies() {
		snap.Cookies[c.Name] = c.Value
	}
	return snap, nil
}

// credentialCookies 返回独立 map，不修改共享凭证，不保留请求级 OAuth Cookie。
func credentialCookies(cred *Credential) map[string]string {
	cookies := map[string]string{}
	if cred.MusicID != 0 {
		uin := cred.StrMusicID
		if uin == "" {
			uin = strconv.FormatInt(cred.MusicID, 10)
		}
		cookies["uin"], cookies["qqmusic_uin"] = uin, uin
	}
	if cred.MusicKey != "" {
		cookies["qm_keyst"], cookies["qqmusic_key"] = cred.MusicKey, cred.MusicKey
	}
	if cred.LoginType != 0 {
		cookies["tmeLoginType"] = strconv.FormatInt(cred.LoginType, 10)
	}
	return cookies
}

func orInit(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
