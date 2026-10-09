package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/team-quaver/typhoeus-go/inhibit"
	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// App sidecar 主应用：会话 + 各业务模块 + Typhoeus 协商器 + 中继 token 表。
type App struct {
	session  *Session
	provider *typhoeus.QQMusicProvider
	resolver *typhoeus.StreamResolver

	login    *modules.LoginModule
	song     *modules.SongModule
	user     *modules.UserModule
	songlist *modules.SonglistModule
	recom    *modules.RecommendModule
	top      *modules.TopModule
	lyric    *modules.LyricModule
	search   *modules.SearchModule
	album    *modules.AlbumModule
	singer   *modules.SingerModule
	comment  *modules.CommentModule

	// 电源抑制：播放音频时禁止睡眠，画廊模式时禁止进入空闲（渲染层经 POST /inhibit
	// 驱动；两个句柄独立持有，退出 sidecar 时由 OS 自动回收）。
	inhibitSleep *inhibit.Inhibitor
	inhibitIdle  *inhibit.Inhibitor

	// QR 状态
	qrLocks      sync.Map // identifier → *sync.Mutex（qq/wx 轮询互斥）
	mobileMu     sync.Mutex
	mobileStates map[string]*mobileQRState // identifier → 状态（mobile 型 MQTT 后台消费）

	// 中继 token 表（进程内，不落盘；重启丢链接可接受——UI 重新 resolve 即可）
	streamMu sync.Mutex
	streams  map[string]*streamEntry
}

// NewApp 组装应用。
func NewApp() (*App, error) {
	// typhoeus 协商/中继层的诊断日志统一走 sidecar 日志（membership 判定、
	// 嗅探放行、平台回退等——排查「高阶莫名降档/播不了」的关键线索源）
	typhoeus.Logf = logWarn
	inhibit.Logf = func(level, format string, args ...any) {
		if level == "WARN" {
			logWarn(format, args...)
			return
		}
		logInfo(format, args...)
	}
	sess, err := NewSession()
	if err != nil {
		return nil, err
	}
	provider := typhoeus.NewQQMusicProvider(sess.Client())
	resolver := typhoeus.NewStreamResolver(provider)
	// 登录/登出后会员缓存立即失效
	sess.OnCredentialChange(provider.InvalidateMembership)
	app := &App{
		session:      sess,
		provider:     provider,
		resolver:     resolver,
		login:        modules.NewLoginModule(sess.Client()),
		song:         modules.NewSongModule(sess.Client()),
		user:         modules.NewUserModule(sess.Client()),
		songlist:     modules.NewSonglistModule(sess.Client()),
		recom:        modules.NewRecommendModule(sess.Client()),
		top:          modules.NewTopModule(sess.Client()),
		lyric:        modules.NewLyricModule(sess.Client()),
		search:       modules.NewSearchModule(sess.Client()),
		album:        modules.NewAlbumModule(sess.Client()),
		singer:       modules.NewSingerModule(sess.Client()),
		comment:      modules.NewCommentModule(sess.Client()),
		inhibitSleep: inhibit.New(),
		inhibitIdle:  inhibit.NewWithMode(inhibit.ModeIdle),
		mobileStates: map[string]*mobileQRState{},
		streams:      map[string]*streamEntry{},
	}
	return app, nil
}

// Routes 注册全部路由（Go 1.22+ 方法+路径模式；字面量优先于通配符）。
func (a *App) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /{$}", a.handleRoot)

	// 登录
	mux.HandleFunc("GET /login/qrcode/{login_type}", a.handleLoginQrcode)
	mux.HandleFunc("GET /login/qrcode/{login_type}/status", a.handleLoginQrcodeStatus)
	mux.HandleFunc("GET /login/status", a.handleLoginStatus)
	mux.HandleFunc("GET /login/refresh", a.handleLoginRefresh)
	mux.HandleFunc("POST /login/logout", a.handleLoginLogout)

	// 用户
	mux.HandleFunc("GET /user/me", a.handleUserMe)
	mux.HandleFunc("GET /user/vip", a.handleUserVip)
	mux.HandleFunc("GET /user/liked", a.handleUserLiked)
	mux.HandleFunc("GET /user/created-songlists", a.handleUserCreatedSonglists)
	mux.HandleFunc("GET /user/fav-songlists", a.handleUserFavSonglists)

	// 播放流（Typhoeus：高档位协商 + 会员门控 + Range 中继 + 加密流解密）
	mux.HandleFunc("GET /stream/tiers", a.handleStreamTiers)
	mux.HandleFunc("POST /stream/resolve", a.handleStreamResolve)
	mux.HandleFunc("GET /stream/{token}", a.handleStreamProxy)

	// 歌曲
	mux.HandleFunc("POST /song/urls", a.handleSongUrls)
	mux.HandleFunc("GET /song/{value}/detail", a.handleSongDetail)
	mux.HandleFunc("GET /song/{value}/url", a.handleSongURL)
	mux.HandleFunc("GET /song/{value}/lyric", a.handleSongLyric)
	mux.HandleFunc("GET /song/{song_id}/comments", a.handleSongComments)
	mux.HandleFunc("POST /song/like", a.handleSongLike)
	mux.HandleFunc("POST /song/unlike", a.handleSongUnlike)

	// 歌单
	mux.HandleFunc("GET /songlist/{songlist_id}/detail", a.handleSonglistDetail)
	mux.HandleFunc("POST /songlist/{songlist_id}/like", a.handleSonglistLike)
	mux.HandleFunc("DELETE /songlist/{songlist_id}/like", a.handleSonglistUnlike)
	mux.HandleFunc("GET /songlist/fav/check", a.handleSonglistFavCheck)
	mux.HandleFunc("POST /songlist/{dirid}/songs", a.handleSonglistAddSong)
	mux.HandleFunc("DELETE /songlist/{dirid}/songs", a.handleSonglistDelSong)
	mux.HandleFunc("DELETE /songlist/{dirid}", a.handleSonglistDelete)

	// 搜索
	mux.HandleFunc("GET /search/hotkey", a.handleSearchHotkey)
	mux.HandleFunc("GET /search/complete", a.handleSearchComplete)
	mux.HandleFunc("GET /search", a.handleSearch)
	mux.HandleFunc("GET /search/general", a.handleSearchGeneral)

	// 推荐 / 榜单
	mux.HandleFunc("GET /recommend/guess", a.handleRecommendGuess)
	mux.HandleFunc("GET /recommend/songlist", a.handleRecommendSonglist)
	mux.HandleFunc("GET /recommend/newsong", a.handleRecommendNewsong)
	mux.HandleFunc("GET /recommend/daily", a.handleRecommendDaily)
	mux.HandleFunc("GET /top/category", a.handleTopCategory)
	mux.HandleFunc("GET /top/{top_id}/detail", a.handleTopDetail)

	// 专辑 / 歌手
	mux.HandleFunc("GET /album/{value}/detail", a.handleAlbumDetail)
	mux.HandleFunc("GET /album/{value}/songs", a.handleAlbumSongs)
	mux.HandleFunc("GET /singer/{mid}/info", a.handleSingerInfo)
	mux.HandleFunc("GET /singer/{mid}/songs", a.handleSingerSongs)
	mux.HandleFunc("GET /singer/{mid}/albums", a.handleSingerAlbums)
	mux.HandleFunc("GET /singer/{mid}/similar", a.handleSingerSimilar)
	mux.HandleFunc("GET /singer/{mid}/desc", a.handleSingerDesc)

	// 睡眠禁止（播放态由渲染层驱动；本进程持有，进程退出 OS 自动回收）
	mux.HandleFunc("POST /inhibit", a.handleInhibit)

	return mux
}

func (a *App) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, map[string]any{"backend": "typhoeus-go/QQMusicApi", "version": "0.2.0"})
}

// ---- 统一调用守卫 ----

// call 统一执行 SDK 调用：登录守卫 + 凭证过期自动刷新重试一次。
//
// 注意凭证有效期：musickey 只有 3 天。过期后上游只读接口照常可用、写接口一律
// 报「凭证已过期」——既提前换新，也把过期/无效两类异常都接住重试。
func (a *App) call(needLogin bool, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	if needLogin {
		if _, err := a.session.Require(); err != nil {
			return nil, err
		}
		if a.session.Credential().IsExpired() {
			a.tryRefresh()
		}
	}
	raw, err := fn()
	if err == nil {
		return raw, nil
	}
	e := qqmusic.AsAPIError(err)
	if (e.Kind == qqmusic.ErrKindCredentialExpired || e.Kind == qqmusic.ErrKindCredentialInvalid) &&
		a.session.LoggedIn() {
		if a.tryRefresh() {
			return fn()
		}
	}
	return nil, err
}

// tryRefresh 尝试刷新凭证并交接；成功返回 true。
func (a *App) tryRefresh() bool {
	cred, err := a.login.RefreshCredential(a.session.Credential())
	if err != nil {
		logWarn("凭证刷新失败: %v", err)
		return false
	}
	a.session.Adopt(cred)
	logInfo("凭证已自动刷新 musicid=%d", cred.MusicID)
	return true
}

// requireEuin 登录守卫 + 加密 UIN。
func (a *App) requireEuin() (string, error) {
	cred, err := a.session.Require()
	if err != nil {
		return "", err
	}
	return cred.SelfEuin(), nil
}

// jsonableCredential 给 UI 的凭证摘要——绝不下发 musickey/refresh_token 等敏感字段。
func jsonableCredential(c *qqmusic.Credential) map[string]any {
	if c == nil {
		return nil
	}
	return map[string]any{
		"musicid":        c.MusicID,
		"str_musicid":    c.StrMusicID,
		"encrypt_uin":    c.EncryptUin,
		"login_type":     c.LoginType,
		"expired_at":     c.ExpiredAt,
		"key_expires_in": c.KeyExpiresIn,
	}
}

// ===================== 登录 =====================

// qrTypeNames 支持的登录类型。
var qrTypeNames = map[string]modules.QRLoginType{
	"qq":     modules.QRTypeQQ,
	"wx":     modules.QRTypeWX,
	"mobile": modules.QRTypeMobile,
}

// qrEventNumbers QR 事件 → 数字（与上游 web 层一致；UI 按数字渲染）。
var qrEventNumbers = map[modules.QRLoginEvent]int{
	modules.EventDone:    0,
	modules.EventScan:    1,
	modules.EventConf:    2,
	modules.EventTimeout: 3,
	modules.EventRefuse:  4,
}

func qrEventNum(ev modules.QRLoginEvent) int {
	if n, ok := qrEventNumbers[ev]; ok {
		return n
	}
	return -1
}

func serializeQR(qr *modules.QR) map[string]any {
	data := ""
	if len(qr.Data) > 0 {
		data = base64.StdEncoding.EncodeToString(qr.Data)
	}
	return map[string]any{
		"qr_type":    string(qr.Type),
		"identifier": qr.Identifier,
		"mimetype":   qr.Mimetype,
		"data":       data,
		"img":        "data:" + qr.Mimetype + ";base64," + data,
	}
}

func (a *App) handleLoginQrcode(w http.ResponseWriter, r *http.Request) {
	typ, ok := qrTypeNames[r.PathValue("login_type")]
	if !ok {
		writeErr(w, 422, -1, fmt.Sprintf("不支持的登录类型: %s（可选 qq/wx/mobile）", r.PathValue("login_type")))
		return
	}
	ctx := r.Context()
	var qr *modules.QR
	var err error
	switch typ {
	case modules.QRTypeWX:
		qr, err = a.login.GetWXQR(ctx)
	case modules.QRTypeMobile:
		qr, err = a.login.GetMobileQR(ctx)
	default:
		qr, err = a.login.GetQQQR(ctx)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if typ == modules.QRTypeMobile {
		// mobile 型走 MQTT 推送：GET 二维码时启动后台消费任务，状态从这里读
		a.startMobileQRConsumer(qr.Identifier)
	}
	writeOK(w, serializeQR(qr))
}

// mobileQRState 手机扫码的后台消费状态。
type mobileQRState struct {
	mu         sync.Mutex
	event      int
	done       bool
	credential map[string]any
	error      string
}

// startMobileQRConsumer 后台消费手机客户端扫码事件流（MQTT 生命周期内持续推送）。
func (a *App) startMobileQRConsumer(identifier string) {
	state := &mobileQRState{event: 1}
	a.mobileMu.Lock()
	a.mobileStates[identifier] = state
	a.mobileMu.Unlock()

	ctx := context.Background()
	ch, err := a.login.ConsumeMobileQR(ctx, identifier)
	if err != nil {
		logWarn("mobile 二维码 MQTT 建连失败: %v", err)
		state.mu.Lock()
		state.event = -1
		state.done = true
		state.error = err.Error()
		state.mu.Unlock()
		return
	}
	go func() {
		for result := range ch {
			state.mu.Lock()
			state.event = qrEventNum(result.Event)
			state.done = result.Done
			if result.Error != "" {
				state.error = result.Error
			}
			if result.Done && result.Credential != nil {
				a.session.Adopt(result.Credential)
				state.credential = jsonableCredential(result.Credential)
			}
			state.mu.Unlock()
			if result.Event == modules.EventDone || result.Event == modules.EventRefuse ||
				result.Event == modules.EventTimeout {
				return
			}
		}
	}()
}

func (a *App) handleLoginQrcodeStatus(w http.ResponseWriter, r *http.Request) {
	typ, ok := qrTypeNames[r.PathValue("login_type")]
	if !ok {
		writeErr(w, 422, -1, fmt.Sprintf("不支持的登录类型: %s", r.PathValue("login_type")))
		return
	}
	identifier := r.URL.Query().Get("identifier")
	if identifier == "" {
		writeErr(w, 422, -1, "缺少 identifier")
		return
	}
	ctx := r.Context()

	if typ == modules.QRTypeMobile {
		a.mobileMu.Lock()
		state := a.mobileStates[identifier]
		a.mobileMu.Unlock()
		if state == nil {
			writeOK(w, map[string]any{"event": 3, "done": true, "credential": nil,
				"error": "二维码不存在或已过期"})
			return
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		payload := map[string]any{
			"event":      state.event,
			"done":       state.done,
			"identifier": identifier,
			"credential": state.credential,
			"login_type": "mobile",
		}
		if state.error != "" {
			payload["error"] = state.error
		}
		writeOK(w, payload)
		return
	}

	// qq/wx：每标识符互斥轮询（防止 UI 多 tab 重复打上游）
	lockAny, _ := a.qrLocks.LoadOrStore(identifier, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	var result *modules.QRLoginResult
	var err error
	if typ == modules.QRTypeWX {
		result, err = a.login.CheckWXQR(ctx, identifier)
	} else {
		result, err = a.login.CheckQQQR(ctx, identifier)
	}
	if err != nil {
		// 上游把 104401(过期)/20450(封号) 等包成 LoginError；对 UI 来说过期/拒绝
		// 仍应可继续流程（事件化为 TIMEOUT/REFUSE 并带错误文案）
		e := qqmusic.AsAPIError(err)
		if e.Kind == qqmusic.ErrKindLogin {
			event := 3 // TIMEOUT
			if e.Code == 20450 || e.Code == 20277 || e.Code == 20278 {
				event = 4 // REFUSE
			}
			writeOK(w, map[string]any{"event": event, "done": true, "credential": nil,
				"identifier": identifier, "login_type": string(typ), "error": e.Message})
			return
		}
		writeError(w, err)
		return
	}
	payload := map[string]any{
		"event":      qrEventNum(result.Event),
		"done":       result.Done,
		"identifier": identifier,
		"login_type": string(typ),
	}
	if result.Done && result.Credential != nil {
		a.session.Adopt(result.Credential)
		payload["credential"] = jsonableCredential(result.Credential)
	}
	writeOK(w, payload)
}

func (a *App) handleLoginStatus(w http.ResponseWriter, _ *http.Request) {
	cred := a.session.Credential()
	loggedIn := cred.HasLogin()
	expired := loggedIn && cred.IsExpired()
	// credential_mode：external=凭证交给 Electron 主进程（系统密钥管理器加密）；
	// memory=只驻内存。只报模式，便于手工 curl 确认。
	writeOK(w, map[string]any{
		"logged_in":       loggedIn,
		"expired":         expired,
		"credential_mode": string(a.session.Mode()),
		"credential":      jsonableCredential(cred),
	})
}

func (a *App) handleLoginRefresh(w http.ResponseWriter, _ *http.Request) {
	if _, err := a.session.Require(); err != nil {
		writeError(w, err)
		return
	}
	cred, err := a.login.RefreshCredential(a.session.Credential())
	if err != nil {
		writeError(w, err)
		return
	}
	a.session.Adopt(cred)
	writeOK(w, jsonableCredential(cred))
}

func (a *App) handleLoginLogout(w http.ResponseWriter, _ *http.Request) {
	if err := a.session.Logout(); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"logged_out": true})
}
