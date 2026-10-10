package modules

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/mqtt"
)

// LoginModule 登录域接口（QQ 二维码 / 微信二维码 / 手机客户端二维码 + 刷新/登出）。
type LoginModule struct {
	cl         *qqmusic.Client
	qqMu       sync.Mutex
	qqSessions map[string]*qqLoginSession

	// mqttNode 缓存上次成功的 MQTT 握手路径（含节点段），
	// 下次连接直接命中节点，省一次 0x9D 重定向往返。
	mqttMu   sync.Mutex
	mqttNode string
}

// NewLoginModule 构造。
func NewLoginModule(cl *qqmusic.Client) *LoginModule { return &LoginModule{cl: cl} }

// QRLoginType 二维码登录类型。
type QRLoginType string

// 三种登录通道。
const (
	QRTypeQQ     QRLoginType = "qq"
	QRTypeWX     QRLoginType = "wx"
	QRTypeMobile QRLoginType = "mobile"
)

// QRLoginEvent 二维码登录状态事件（对齐上游枚举命名）。
type QRLoginEvent string

// 事件值。
const (
	EventDone    QRLoginEvent = "DONE"
	EventScan    QRLoginEvent = "SCAN"
	EventConf    QRLoginEvent = "CONF"
	EventTimeout QRLoginEvent = "TIMEOUT"
	EventRefuse  QRLoginEvent = "REFUSE"
)

// QR 二维码信息（data 为图片二进制；identifier 是状态轮询/推送的凭据）。
type QR struct {
	Data       []byte
	Type       QRLoginType
	Mimetype   string
	Identifier string
}

// QRLoginResult 单次状态查询结果。
type QRLoginResult struct {
	Event      QRLoginEvent
	Done       bool
	Credential *qqmusic.Credential
	Error      string // 非空 = 流程内错误（cookies 解析失败 / Login CGI 失败等），UI 应展示
}

// 上游登录域错误码 → 文案（_validate_result 的映射表）。
var loginErrorText = map[int64]string{
	1000: "登录鉴权参数无效或已过期", 104401: "登录鉴权参数无效或已过期", 104400: "登录鉴权参数无效或已过期",
	20261: "登录参数错误", 20271: "验证码错误", 20272: "账号绑定异常", 20274: "账号绑定缺失",
	20277: "账号受限", 20278: "账号受限", 20279: "登录设备超限", 20450: "账号已被封禁", 104604: "操作过于频繁",
}

// allowLoginErrorCodes 登录 CGI 允许的业务码集合（拿到后本地归一为 Login 错误）。
var allowLoginErrorCodes = []int{
	1000, 104401, 104400, 20261, 20271, 20272, 20274, 20277, 20278, 20279, 20450, 104604,
}

// validateLoginResponse 校验登录响应：code=0 返回凭证；登录域错误码归一为 APIError。
func validateLoginResponse(item json.RawMessage) (*qqmusic.Credential, error) {
	var sub struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(item, &sub); err != nil {
		return nil, qqmusic.NewDataError("登录响应格式异常")
	}
	if sub.Code != 0 {
		msg, ok := loginErrorText[int64(sub.Code)]
		if !ok {
			msg = "登录失败"
		}
		return nil, qqmusic.NewLoginError(sub.Code, msg, string(sub.Data))
	}
	var cred qqmusic.Credential
	if err := json.Unmarshal(sub.Data, &cred); err != nil {
		return nil, qqmusic.NewDataError("凭证解析失败: " + err.Error())
	}
	if cred.LoginType == 0 {
		cred.LoginType = inferType(cred.MusicKey)
	}
	return &cred, nil
}

func inferType(musicKey string) int64 {
	if strings.HasPrefix(musicKey, "W_X") {
		return 1
	}
	return 2
}

// loginCgi 发起登录域 CGI（带登录错误码白名单）。
func (m *LoginModule) loginCgi(module, method string, param *qqmusic.JObj, comm *qqmusic.JObj) (json.RawMessage, error) {
	item, err := m.cl.CgiCall(module, method, param, qqmusic.CGIOption{
		Comm:            comm,
		AllowErrorCodes: allowLoginErrorCodes,
	})
	if err != nil {
		return nil, err
	}
	// 白名单命中时 CgiCall 直接返回子响应原文（含 code），由调用方校验
	return item, nil
}

// ===================== QQ 二维码 =====================

const qqLoginJump = "https://graph.qq.com/oauth2.0/login_jump"

// 每张二维码独占 cookie jar。pt_login_sig/qrsig/中间响应 Cookie 必须跨轮询保存；
// Cookie 的 Domain/Path/过期信息也必须保留，不能用全局或按名称合并的 map。
type qqLoginSession struct {
	mu        sync.Mutex
	jar       http.CookieJar
	created   time.Time
	referer   string
	jsVersion string
	result    *QRLoginResult
	err       error
}

func (s *qqLoginSession) cookies(rawURL string) map[string]string {
	u, _ := url.Parse(rawURL)
	cookies := map[string]string{}
	for _, c := range s.jar.Cookies(u) {
		cookies[c.Name] = c.Value
	}
	return cookies
}

func (m *LoginModule) qqRequest(ctx context.Context, session *qqLoginSession, method, rawURL string, opt qqmusic.HTTPOption) (*qqmusic.RawResponse, error) {
	opt.Cookies, opt.Credential, opt.NoRedirect = session.cookies(rawURL), &qqmusic.Credential{}, true
	resp, err := m.cl.DoHTTPPlainContext(ctx, method, rawURL, opt)
	if err != nil {
		e := qqmusic.AsAPIError(err)
		return nil, &qqmusic.APIError{Kind: e.Kind, Code: e.Code, Message: "QQ 登录请求失败，请重新生成二维码"}
	}
	u, _ := url.Parse(rawURL)
	// RawResponse.Cookies 按名字折叠，会丢掉重复名称、不同域和删除 Cookie；从头部解析完整集合。
	session.jar.SetCookies(u, (&http.Response{Header: resp.Headers}).Cookies())
	return resp, nil
}

// GetQQQR 从官方 xlogin 建立登录会话，再取二维码，保存本次会话供后续轮询。
func (m *LoginModule) GetQQQR(ctx context.Context) (*QR, error) {
	params := qqmusic.P("appid", "716027609", "daid", "383", "style", "40", "target", "self",
		"s_url", qqLoginJump, "pt_3rd_aid", "100497308", "hide_title_bar", "1", "hide_border", "1")
	jar, _ := cookiejar.New(nil)
	session := &qqLoginSession{jar: jar, created: time.Now(), jsVersion: "26092315",
		referer: "https://xui.ptlogin2.qq.com/cgi-bin/xlogin?" + params.Encode()}
	page, err := m.qqRequest(ctx, session, "GET", session.referer, qqmusic.HTTPOption{})
	if err != nil {
		return nil, err
	}
	if page.StatusCode != http.StatusOK {
		return nil, qqmusic.NewDataError(fmt.Sprintf("QQ 登录页返回 HTTP %d", page.StatusCode))
	}
	if match := qqJSVersionRe.FindStringSubmatch(page.Text); len(match) > 1 {
		session.jsVersion = match[1]
	}
	params = qqmusic.P("appid", "716027609", "e", "2", "l", "M", "s", "3", "d", "72", "v", "4",
		"t", fmt.Sprintf("0.%d", time.Now().UnixNano()%1e10), "u1", qqLoginJump,
		"daid", "383", "pt_3rd_aid", "100497308")
	resp, err := m.qqRequest(ctx, session, "GET", "https://ssl.ptlogin2.qq.com/ptqrshow", qqmusic.HTTPOption{
		Params: params, Headers: map[string]string{"Referer": session.referer},
	})
	if err != nil {
		return nil, err
	}
	qrsig := session.cookies("https://ssl.ptlogin2.qq.com/ptqrlogin")["qrsig"]
	if resp.StatusCode != http.StatusOK || qrsig == "" {
		return nil, qqmusic.NewDataError("获取 QQ 二维码失败")
	}
	m.qqMu.Lock()
	if m.qqSessions == nil {
		m.qqSessions = make(map[string]*qqLoginSession)
	}
	for id, old := range m.qqSessions {
		if time.Since(old.created) > 10*time.Minute {
			delete(m.qqSessions, id)
		}
	}
	m.qqSessions[qrsig] = session
	m.qqMu.Unlock()
	return &QR{Data: resp.BodyBytes(), Type: QRTypeQQ, Mimetype: "image/png", Identifier: qrsig}, nil
}

// qqStatusRe ptuiCB('0','0','https://...','0','登录成功!', '昵称')
var (
	qqJSVersionRe = regexp.MustCompile(`ptui_version:\s*encodeURIComponent\(["']([0-9]+)["']\)`)
	qqStatusRe    = regexp.MustCompile(`ptuiCB\((.*?)\)`)
	qqArgsRe      = regexp.MustCompile(`'((?:\\.|[^'])*)'`)
)

// CheckQQQR 轮询 QQ 二维码状态。
func (m *LoginModule) CheckQQQR(ctx context.Context, qrsig string) (*QRLoginResult, error) {
	m.qqMu.Lock()
	session := m.qqSessions[qrsig]
	if session != nil && time.Since(session.created) > 10*time.Minute {
		delete(m.qqSessions, qrsig)
		session = nil
	}
	m.qqMu.Unlock()
	if session == nil {
		return &QRLoginResult{Event: EventTimeout, Done: true}, nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.result != nil || session.err != nil {
		return session.result, session.err
	}
	params := qqmusic.P(
		"u1", "https://graph.qq.com/oauth2.0/login_jump",
		"ptqrtoken", strconv.FormatInt(qqmusic.Hash33(qrsig, 0), 10),
		"ptredirect", "0", "h", "1", "t", "1", "g", "1", "from_ui", "1", "ptlang", "2052",
		"action", fmt.Sprintf("0-0-%d", time.Now().UnixMilli()),
		"login_sig", session.cookies("https://xui.ptlogin2.qq.com/")["pt_login_sig"],
		"js_ver", session.jsVersion, "js_type", "1", "pt_uistyle", "40",
		"aid", "716027609", "daid", "383", "pt_3rd_aid", "100497308", "has_onekey", "1")
	resp, err := m.qqRequest(ctx, session, "GET", "https://ssl.ptlogin2.qq.com/ptqrlogin", qqmusic.HTTPOption{
		Params: params, Headers: map[string]string{"Referer": session.referer},
	})
	if err != nil {
		return nil, err
	}
	status := qqStatusRe.FindStringSubmatch(resp.Text)
	if status == nil {
		return nil, qqmusic.NewDataError("获取二维码状态失败: 无法解析响应")
	}
	args := qqArgsRe.FindAllStringSubmatch(status[1], -1)
	if len(args) == 0 {
		return nil, qqmusic.NewDataError("获取二维码状态失败: 无法解析状态参数")
	}
	codeStr := args[0][1]
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return nil, qqmusic.NewDataError("获取二维码状态失败: 无效的状态码")
	}
	event := qqEventFromCode(code)
	if event != EventDone {
		return &QRLoginResult{Event: event}, nil
	}
	if len(args) < 3 {
		return nil, qqmusic.NewDataError("获取登录凭据失败: 缺少必要参数")
	}
	// 回跳参数顺序和位置不是协议保证；旧正则要求 uin 后紧跟 service、
	// ptsigx 后紧跟 s_url，会在参数换序/末参时卡在「确认后没反应」。
	redirectURL := html.UnescapeString(strings.NewReplacer(`\x26`, "&", `\u0026`, "&", `\/`, "/").Replace(args[2][1]))
	redirect, parseErr := url.Parse(redirectURL)
	if parseErr != nil {
		return nil, qqmusic.NewDataError("获取登录凭据失败: 无效回跳地址")
	}
	if !qqLoginURLAllowed(redirect) || redirect.Path != "/check_sig" || redirect.Query().Get("ptsigx") == "" {
		return nil, qqmusic.NewDataError("获取登录凭据失败: 无效授权回跳地址")
	}
	// ptsigx 是一次性凭据。保留上游完整地址（包括 pt_login_type/pt_3rd_aid 等），
	// 不改写参数；确认后的失败也缓存，避免每 2s 重放已消费的签名。
	cred, err := m.authorizeQQQR(ctx, redirect.String(), session)
	if err != nil {
		session.err = err
		return nil, err
	}
	session.result = &QRLoginResult{Event: EventDone, Done: true, Credential: cred}
	return session.result, nil
}

// qqEventFromCode ptuiCB 状态码 → 事件。
func qqEventFromCode(code int) QRLoginEvent {
	switch code {
	case 0:
		return EventDone
	case 66:
		return EventScan
	case 67:
		return EventConf
	case 68:
		return EventRefuse
	default: // 65 及未知
		return EventTimeout
	}
}

func qqLoginURLAllowed(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Hostname() {
	case "graph.qq.com", "ssl.ptlogin2.graph.qq.com", "ssl.ptlogin2.qq.com":
		return true
	default:
		return false
	}
}

// qqOAuthHTTP 手动跟随授权重定向，使用同一张二维码的 Cookie jar。
func (m *LoginModule) qqOAuthHTTP(ctx context.Context, method, rawURL string, opt qqmusic.HTTPOption, session *qqLoginSession, wantCode bool) (*qqmusic.RawResponse, error) {
	for hop := 0; hop < 5; hop++ {
		resp, err := m.qqRequest(ctx, session, method, rawURL, opt)
		if err != nil {
			return nil, err
		}
		if (wantCode && qqOAuthCode(resp.Headers.Get("Location")) != "") || (!wantCode && session.cookies("https://graph.qq.com/oauth2.0/authorize")["p_skey"] != "") {
			return resp, nil
		}
		if resp.StatusCode < 300 || resp.StatusCode > 399 || resp.Headers.Get("Location") == "" {
			return resp, nil
		}
		base, _ := url.Parse(rawURL)
		next, err := base.Parse(resp.Headers.Get("Location"))
		if err != nil || !qqLoginURLAllowed(next) {
			return nil, qqmusic.NewDataError("QQ 授权回跳地址异常，请重新生成二维码")
		}
		rawURL = next.String()
		if resp.StatusCode != 307 && resp.StatusCode != 308 {
			method, opt.FormBody = "GET", nil
		}
		opt.Params = nil
		opt.Headers = map[string]string{"Referer": session.referer}
	}
	return nil, qqmusic.NewDataError("QQ 授权回跳次数过多，请重新生成二维码")
}

func qqOAuthCode(location string) string {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Hostname() != "y.qq.com" && u.Hostname() != "graph.qq.com") {
		return ""
	}
	return u.Query().Get("code")
}

func (m *LoginModule) authorizeQQQR(ctx context.Context, redirectURL string, session *qqLoginSession) (*qqmusic.Credential, error) {
	check, err := m.qqOAuthHTTP(ctx, "GET", redirectURL, qqmusic.HTTPOption{
		Headers: map[string]string{"Referer": session.referer},
	}, session, false)
	if err != nil {
		return nil, err
	}
	pSkey := session.cookies("https://graph.qq.com/oauth2.0/authorize")["p_skey"]
	if pSkey == "" {
		return nil, qqmusic.NewDataError(fmt.Sprintf("QQ 确认回跳未返回 p_skey（HTTP %d），请重新生成二维码", check.StatusCode))
	}
	authResp, err := m.qqOAuthHTTP(ctx, "POST", "https://graph.qq.com/oauth2.0/authorize", qqmusic.HTTPOption{
		FormBody: qqmusic.P(
			"response_type", "code", "client_id", "100497308",
			"redirect_uri", "https://y.qq.com/portal/wx_redirect.html?login_type=1&surl=https://y.qq.com/",
			"scope", "get_user_info,get_app_friends", "state", "state", "switch", "",
			"from_ptlogin", "1", "src", "1", "update_auth", "1", "openapi", "1010_1030",
			"g_tk", strconv.FormatInt(qqmusic.Hash33(pSkey, 5381), 10),
			"auth_time", strconv.FormatInt(time.Now().UnixMilli(), 10), "ui", randomGUID()),
		Headers: map[string]string{"Referer": "https://graph.qq.com/", "Origin": "https://graph.qq.com"},
	}, session, true)
	if err != nil {
		return nil, err
	}
	code := qqOAuthCode(authResp.Headers.Get("Location"))
	if code == "" {
		return nil, qqmusic.NewDataError("获取 QQ 授权码失败，请重新生成二维码")
	}
	// wx_redirect.html 在 qq.com 设置 login_type=1，并按 musicu 域实际可见的
	// Cookie 计算 g_tk。graph.qq.com 的 p_skey 不应跨域塞给 musicu。
	musicCookies := session.cookies("https://u.y.qq.com/cgi-bin/musicu.fcg")
	musicCookies["login_type"] = "1"
	gtk := int64(5381)
	for _, name := range []string{"qqmusic_key", "p_skey", "skey", "p_lskey", "lskey"} {
		if key := musicCookies[name]; key != "" {
			gtk = qqmusic.Hash33(key, 5381)
			break
		}
	}
	item, err := m.cl.CgiCall("QQConnectLogin.LoginServer", "QQLogin", qqmusic.NewJObj().Set("code", code), qqmusic.CGIOption{
		Platform: qqmusic.PlatformWeb, Credential: &qqmusic.Credential{},
		Comm:    qqmusic.NewJObj().Set("tmeLoginType", 2).Set("cv", 0).Set("platform", "yqq").Set("g_tk", gtk).Set("g_tk_new_20200303", gtk),
		Cookies: musicCookies, Headers: map[string]string{"Referer": "https://y.qq.com/", "Origin": "https://y.qq.com"},
		AllowErrorCodes: allowLoginErrorCodes,
	})
	if err != nil {
		return nil, err
	}
	cred, err := validateLoginResponse(item)
	if err != nil {
		return nil, err
	}
	if !cred.HasLogin() {
		return nil, qqmusic.NewDataError("QQ 登录响应缺少有效凭证，请重新生成二维码")
	}
	return cred, nil
}

// ===================== 微信二维码 =====================

var wxUUIDRe = regexp.MustCompile(`uuid=(.+?)"`)

// GetWXQR 获取微信登录二维码（identifier 为 uuid）。
func (m *LoginModule) GetWXQR(ctx context.Context) (*QR, error) {
	page, err := m.cl.DoHTTPPlainContext(ctx, "GET", "https://open.weixin.qq.com/connect/qrconnect", qqmusic.HTTPOption{
		Params: qqmusic.P(
			"appid", "wx48db31d50e334801",
			"redirect_uri", "https://y.qq.com/portal/wx_redirect.html?login_type=2&surl=https://y.qq.com/",
			"response_type", "code", "scope", "snsapi_login", "state", "STATE",
			"href", "https://y.qq.com/mediastyle/music_v17/src/css/popup_wechat.css#wechat_redirect"),
	})
	if err != nil {
		return nil, err
	}
	uuids := wxUUIDRe.FindStringSubmatch(page.Text)
	if uuids == nil {
		return nil, qqmusic.NewDataError("获取 uuid 失败")
	}
	img, err := m.cl.DoHTTPPlainContext(ctx, "GET", "https://open.weixin.qq.com/connect/qrcode/"+uuids[1], qqmusic.HTTPOption{
		Headers: map[string]string{"Referer": "https://open.weixin.qq.com/connect/qrconnect"},
	})
	if err != nil {
		return nil, err
	}
	return &QR{Data: img.BodyBytes(), Type: QRTypeWX, Mimetype: "image/jpeg", Identifier: uuids[1]}, nil
}

var wxStatusRe = regexp.MustCompile(`window\.wx_errcode=(\d+);window\.wx_code='([^']*)'`)

// CheckWXQR 长轮询微信二维码状态（上游 35s 超时视为「扫码中」）。
func (m *LoginModule) CheckWXQR(ctx context.Context, uuid string) (*QRLoginResult, error) {
	resp, err := m.cl.DoHTTPPlainContext(ctx, "GET", "https://lp.open.weixin.qq.com/connect/l/qrconnect", qqmusic.HTTPOption{
		Params: qqmusic.P(
			"uuid", uuid,
			"_", fmt.Sprintf("%d000", time.Now().Unix())),
		Headers:        map[string]string{"Referer": "https://open.weixin.qq.com/"},
		Credential:     &qqmusic.Credential{},
		TimeoutSeconds: 35,
	})
	if err != nil {
		if qqmusic.AsAPIError(err).Kind == qqmusic.ErrKindTimeout {
			return &QRLoginResult{Event: EventScan}, nil
		}
		return nil, err
	}
	status := wxStatusRe.FindStringSubmatch(resp.Text)
	if status == nil {
		return nil, qqmusic.NewDataError("获取二维码状态失败: 无法解析响应")
	}
	errcode, err := strconv.Atoi(status[1])
	if err != nil {
		return nil, qqmusic.NewDataError("获取二维码状态失败: 无效的错误码")
	}
	event := wxEventFromCode(errcode)
	if event != EventDone {
		return &QRLoginResult{Event: event}, nil
	}
	if status[2] == "" {
		return nil, qqmusic.NewDataError("获取 code 失败: 无效的 code")
	}
	item, err := m.loginCgi("music.login.LoginServer", "Login",
		qqmusic.NewJObj().Set("code", status[2]).Set("strAppid", "wx48db31d50e334801"),
		qqmusic.NewJObj().Set("tmeLoginType", 1))
	if err != nil {
		return nil, err
	}
	cred, err := validateLoginResponse(item)
	if err != nil {
		return nil, err
	}
	return &QRLoginResult{Event: EventDone, Done: true, Credential: cred}, nil
}

// wxEventFromCode 微信 errcode → 事件（405 完成 / 408 扫码 / 404 确认 / 402 超时 / 403 拒绝）。
func wxEventFromCode(code int) QRLoginEvent {
	switch code {
	case 405:
		return EventDone
	case 408:
		return EventScan
	case 404:
		return EventConf
	case 403:
		return EventRefuse
	default: // 402 及未知
		return EventTimeout
	}
}

// ===================== 手机客户端二维码（MQTT 推送） =====================

// GetMobileQR 获取手机客户端扫码二维码（identifier 为 qrcodeID）。
func (m *LoginModule) GetMobileQR(ctx context.Context) (*QR, error) {
	param := qqmusic.NewJObj().
		Set("tmeAppID", "qqmusic").
		Set("ct", 11).
		Set("cv", 14090008)
	item, err := m.cl.CgiCall("music.login.LoginServer", "CreateQRCode", param, qqmusic.CGIOption{
		Comm:            qqmusic.NewJObj().Set("ct", 23).Set("cv", 0),
		Platform:        qqmusic.PlatformAndroid,
		AllowErrorCodes: []int{},
	})
	if err != nil {
		return nil, err
	}
	data, err := qqmusic.ParseCGIData(item, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Qrcode   string `json:"qrcode"`
		QrcodeID string `json:"qrcodeID"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, qqmusic.NewDataError("获取二维码失败")
	}
	if raw.Qrcode == "" || raw.QrcodeID == "" {
		return nil, qqmusic.NewDataError("获取二维码失败")
	}
	// qrcode 形如 "data:image/png;base64,xxxx"（取最后一个逗号之后）
	png := raw.Qrcode
	if idx := strings.LastIndex(png, ","); idx >= 0 {
		png = png[idx+1:]
	}
	bin, err := base64.StdEncoding.DecodeString(png)
	if err != nil {
		return nil, qqmusic.NewDataError("二维码数据解码失败")
	}
	return &QR{Data: bin, Type: QRTypeMobile, Mimetype: "image/png", Identifier: raw.QrcodeID}, nil
}

// ConsumeMobileQR 订阅手机扫码事件流（单个 MQTT 连接生命周期）。
//
// 返回的 channel 逐条产出状态；DONE/REFUSE/TIMEOUT 后关闭。ctx 取消即断开。
// cookies 消息会在通道内完成换取凭证的 CGI 登录，凭证随 DONE 事件带出。
func (m *LoginModule) ConsumeMobileQR(ctx context.Context, qrcodeID string) (<-chan *QRLoginResult, error) {
	clientID := fmt.Sprintf("%d%d", time.Now().UnixMilli(), 1000+randIntN(9000))
	out := make(chan *QRLoginResult, 8)

	client := mqtt.NewClient(clientID, "mu.y.qq.com", 443, "/ws/handshake")
	m.mqttMu.Lock()
	node := m.mqttNode
	m.mqttMu.Unlock()
	connCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	err := client.Connect(connCtx, "pass", []mqtt.Property{
		{K: "tmeAppID", V: "qqmusic"},
		{K: "business", V: "management"},
		{K: "hashTag", V: qrcodeID},
		{K: "clientTag", V: "management.user"},
		{K: "userID", V: qrcodeID},
	}, map[string]string{
		"Origin":     "https://y.qq.com",
		"Referer":    "https://y.qq.com/",
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	}, node)
	if err != nil {
		// 缓存节点失效：回退基础路径重试一次
		if node != "" {
			err = client.Connect(connCtx, "pass", []mqtt.Property{
				{K: "tmeAppID", V: "qqmusic"},
				{K: "business", V: "management"},
				{K: "hashTag", V: qrcodeID},
				{K: "clientTag", V: "management.user"},
				{K: "userID", V: qrcodeID},
			}, map[string]string{
				"Origin":     "https://y.qq.com",
				"Referer":    "https://y.qq.com/",
				"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
			}, "")
		}
		if err != nil {
			return nil, err
		}
	}
	m.mqttMu.Lock()
	m.mqttNode = client.GoodPath()
	m.mqttMu.Unlock()
	if err := client.Subscribe("management.qrcode_login/"+qrcodeID, []mqtt.Property{
		{K: "authorization", V: "tmelogin"},
		{K: "pubsub", V: "unicast"},
	}); err != nil {
		client.Close()
		return nil, err
	}

	go func() {
		defer close(out)
		defer client.Close()
		// 订阅成功即产出「等待扫描」
		out <- &QRLoginResult{Event: EventScan}
		for {
			msg, err := client.ReadMessage()
			if err != nil {
				logWarn("mobile 扫码 MQTT 读消息结束: %v", err)
				return
			}
			switch msg.Properties["type"] {
			case "scanned":
				out <- &QRLoginResult{Event: EventConf}
			case "canceled":
				out <- &QRLoginResult{Event: EventRefuse}
				return
			case "timeout":
				out <- &QRLoginResult{Event: EventTimeout}
				return
			case "loginFailed":
				out <- &QRLoginResult{Event: EventTimeout}
				return
			default:
				logWarn("mobile 扫码收到未处理消息 type=%q payload=%.160s", msg.Properties["type"], string(msg.Payload))
			case "cookies":
				var payload struct {
					Cookies map[string]struct {
						Value string `json:"value"`
					} `json:"cookies"`
				}
				if err := json.Unmarshal(msg.Payload, &payload); err != nil {
					logWarn("mobile 扫码 cookies 消息解析失败: %v（payload=%.200s）", err, string(msg.Payload))
					out <- &QRLoginResult{Event: EventTimeout, Error: "登录消息解析失败"}
					return
				}
				uin := payload.Cookies["qqmusic_uin"].Value
				key := payload.Cookies["qqmusic_key"].Value
				if uin == "" || key == "" {
					logWarn("mobile 扫码 cookies 缺少 uin/key（payload=%.200s）", string(msg.Payload))
					out <- &QRLoginResult{Event: EventTimeout, Error: "登录消息缺少凭据字段"}
					return
				}
				musicID, _ := strconv.ParseInt(uin, 10, 64)
				item, err := m.loginCgi("music.login.LoginServer", "Login",
					qqmusic.NewJObj().
						Set("musicid", musicID).
						Set("qrCodeID", qrcodeID).
						Set("token", key),
					qqmusic.NewJObj().Set("tmeLoginType", 6))
				if err != nil {
					logWarn("mobile 扫码 Login CGI 失败: %v", err)
					out <- &QRLoginResult{Event: EventTimeout, Error: "凭证换取失败: " + err.Error()}
					return
				}
				cred, err := validateLoginResponse(item)
				if err != nil {
					logWarn("mobile 扫码 Login 响应校验失败: %v", err)
					out <- &QRLoginResult{Event: EventTimeout, Error: "凭证校验失败: " + err.Error()}
					return
				}
				out <- &QRLoginResult{Event: EventDone, Done: true, Credential: cred}
				return
			}
		}
	}()
	return out, nil
}

// ===================== 刷新 / 登出 =====================

// RefreshCredential 刷新登录凭证（musickey 约 3 天有效；登录域错误归一为 Login 错误）。
func (m *LoginModule) RefreshCredential(cred *qqmusic.Credential) (*qqmusic.Credential, error) {
	if cred == nil {
		cred = m.cl.Credential()
	}
	param := qqmusic.NewJObj()
	switch cred.LoginType {
	case 1: // 微信
		param.Set("openid", cred.OpenID).
			Set("refresh_token", cred.RefreshToken).
			Set("str_musicid", orDefaultStr(cred.StrMusicID, strconv.FormatInt(cred.MusicID, 10))).
			Set("musickey", cred.MusicKey).
			Set("unionid", cred.UnionID).
			Set("refresh_key", cred.RefreshKey).
			Set("loginMode", 2)
	case 2: // QQ
		param.Set("openid", cred.OpenID).
			Set("access_token", cred.AccessToken).
			Set("refresh_token", cred.RefreshToken).
			Set("expired_in", cred.ExpiredAt).
			Set("musicid", cred.MusicID).
			Set("musickey", cred.MusicKey).
			Set("refresh_key", cred.RefreshKey).
			Set("loginMode", 2)
	default:
		param.Set("openid", cred.OpenID).
			Set("access_token", cred.AccessToken).
			Set("refresh_token", cred.RefreshToken).
			Set("expired_in", cred.ExpiredAt).
			Set("str_musicid", orDefaultStr(cred.StrMusicID, strconv.FormatInt(cred.MusicID, 10))).
			Set("musicid", cred.MusicID).
			Set("musickey", cred.MusicKey).
			Set("unionid", cred.UnionID).
			Set("refresh_key", cred.RefreshKey).
			Set("loginMode", 2)
	}
	item, err := m.cl.CgiCall("music.login.LoginServer", "Login", param, qqmusic.CGIOption{
		Comm:            qqmusic.NewJObj().Set("tmeLoginType", cred.LoginType),
		Credential:      cred,
		AllowErrorCodes: allowLoginErrorCodes,
	})
	if err != nil {
		return nil, err
	}
	newCred, err := validateLoginResponse(item)
	if err != nil {
		return nil, err
	}
	return newCred, nil
}

// Logout 登出（服务端注销 + 清凭证由上层处理）。
func (m *LoginModule) Logout() error {
	_, err := m.cl.CgiCall("music.login.LoginServer", "Logout", qqmusic.NewJObj(),
		qqmusic.CGIOption{RequireLogin: true, AllowErrorCodes: allowLoginErrorCodes})
	return err
}

func orDefaultStr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
