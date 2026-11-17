package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// orderCapture 与 modules 侧的 cgiCapture 同构：GetSession 放行，SeqSonglist 记下参数。
type orderCapture struct {
	param  map[string]any
	method string
	calls  int
}

func (c *orderCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Req0 struct {
			Module string         `json:"module"`
			Method string         `json:"method"`
			Param  map[string]any `json:"param"`
		} `json:"req_0"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	reply := func(s string) *http.Response {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(s))}
	}
	if payload.Req0.Module == "music.getSession.session" {
		return reply(`{"code":0,"req_0":{"code":0,"data":{"session":{"uid":123,"sid":"stub"}}}}`), nil
	}
	c.calls++
	c.method, c.param = payload.Req0.Method, payload.Req0.Param
	return reply(`{"code":0,"req_0":{"code":0,"data":{"retCode":0}}}`), nil
}

func newOrderApp(t *testing.T, loggedIn bool) (*App, *orderCapture) {
	t.Helper()
	cred := &qqmusic.Credential{}
	if loggedIn {
		cred = &qqmusic.Credential{MusicID: 123, MusicKey: "KEY", LoginType: 2,
			MusicKeyCreateAt: time.Now().Unix(), KeyExpiresIn: 3600}
	}
	cap := &orderCapture{}
	cl, err := qqmusic.NewClient(cred, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: cap}))
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{mode: ModeMemory, cred: cred, cl: cl}
	return &App{session: sess, songlist: modules.NewSonglistModule(cl)}, cap
}

func postOrder(t *testing.T, app *App, dirid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/songlist/"+dirid+"/order", strings.NewReader(body))
	req.SetPathValue("dirid", dirid)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	return rec
}

// TestSonglistOrderRoute 全量重排走 POST /songlist/{dirid}/order：
// 读侧 song_type 原样进 SDK，SeqSonglist 收到写侧枚举且顺序保持。
func TestSonglistOrderRoute(t *testing.T) {
	app, cap := newOrderApp(t, true)
	rec := postOrder(t, app, "9", `{"tid":77,"songs":[{"song_id":3,"song_type":1},{"song_id":1,"song_type":1},{"song_id":2,"song_type":5}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Code != 0 || env.Data["ok"] != true {
		t.Fatalf("envelope = %s", rec.Body.String())
	}
	if cap.method != "SeqSonglist" || cap.param["dirId"] != float64(9) || cap.param["tid"] != float64(77) {
		t.Fatalf("cgi = %s param = %v", cap.method, cap.param)
	}
	pairs, _ := cap.param["v_songPair"].([]any)
	wantIDs := []float64{3, 1, 2}
	wantTypes := []float64{0, 0, 5}
	if len(pairs) != 3 {
		t.Fatalf("v_songPair = %v", pairs)
	}
	for i, p := range pairs {
		item := p.(map[string]any)
		if item["songId"] != wantIDs[i] || item["songType"] != wantTypes[i] {
			t.Fatalf("pair[%d] = %v", i, item)
		}
	}
}

// TestSonglistOrderRejectsEmpty songs 空列表 = 422，且一个字节都不发给上游
// （部分/空列表提交会打乱未提交歌曲的顺序，必须在门口拦住）。
func TestSonglistOrderRejectsEmpty(t *testing.T) {
	app, cap := newOrderApp(t, true)
	for _, body := range []string{`{"tid":77,"songs":[]}`, `{"tid":77}`} {
		rec := postOrder(t, app, "9", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s status=%d", body, rec.Code)
		}
	}
	if cap.calls != 0 {
		t.Fatalf("upstream got %d calls", cap.calls)
	}
}

// TestSonglistOrderRejectsBadSongID 非法 song_id（0/负数）= 422，不上行。
func TestSonglistOrderRejectsBadSongID(t *testing.T) {
	app, cap := newOrderApp(t, true)
	rec := postOrder(t, app, "9", `{"tid":77,"songs":[{"song_id":1,"song_type":1},{"song_id":0,"song_type":1}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.calls != 0 {
		t.Fatalf("upstream got %d calls", cap.calls)
	}
}

// TestSonglistOrderRequiresLogin 未登录 = 401（call2 登录守卫），不上行。
func TestSonglistOrderRequiresLogin(t *testing.T) {
	app, cap := newOrderApp(t, false)
	rec := postOrder(t, app, "9", `{"tid":77,"songs":[{"song_id":1,"song_type":1}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.calls != 0 {
		t.Fatalf("upstream got %d calls", cap.calls)
	}
}

// TestSongLikeBodyPassesReadType /song/like 收读侧原值，写侧映射发生在 SDK
// （PR 306 口径：调用方不再做 -1 偏移）。
func TestSongLikeBodyPassesReadType(t *testing.T) {
	app, cap := newOrderApp(t, true)
	req := httptest.NewRequest("POST", "/song/like", strings.NewReader(`{"song_id":100,"song_type":1}`))
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.method != "AddSonglist" {
		t.Fatalf("method = %s", cap.method)
	}
	items, _ := cap.param["v_songInfo"].([]any)
	item := items[0].(map[string]any)
	if item["songId"] != float64(100) || item["songType"] != float64(0) {
		t.Fatalf("item = %v（读侧 1 必须映射成写侧 0）", item)
	}
}
