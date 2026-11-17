package modules

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// TestToSonglistWriteTypeMapping 读侧→写侧映射逐项锁定
// （上游 QQMusicApi PR 306 的回归表；旧「读侧-1」口径在 2/5/111 等类型上全是错的）。
func TestToSonglistWriteTypeMapping(t *testing.T) {
	cases := []struct{ read, write int64 }{
		{0, 0}, {1, 0}, {2, 1}, {3, 1}, {4, 1}, {5, 5}, {6, 1},
		{110, 1}, {111, 111}, {112, 112}, {113, 113}, {114, 1},
	}
	for _, tc := range cases {
		if got := ToSonglistWriteType(tc.read); got != tc.write {
			t.Errorf("read=%d: got %d want %d", tc.read, got, tc.write)
		}
	}
}

// cgiCapture 桩传输：GetSession 直接放行，业务请求记下参数后按 retCode 应答。
type cgiCapture struct {
	param   map[string]any
	module  string
	method  string
	retCode int
	subCode int
}

func (c *cgiCapture) RoundTrip(r *http.Request) (*http.Response, error) {
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
	c.module, c.method, c.param = payload.Req0.Module, payload.Req0.Method, payload.Req0.Param
	if c.subCode != 0 {
		return reply(`{"code":0,"req_0":{"code":` + itoa(c.subCode) + `,"data":{}}}`), nil
	}
	return reply(`{"code":0,"req_0":{"code":0,"data":{"retCode":` + itoa(c.retCode) + `}}}`), nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newCaptureClient(t *testing.T, cap *cgiCapture) *SonglistModule {
	t.Helper()
	cl, err := qqmusic.NewClient(&qqmusic.Credential{MusicID: 123, MusicKey: "KEY", LoginType: 2},
		"", "", qqmusic.WithHTTPClient(&http.Client{Transport: cap}))
	if err != nil {
		t.Fatal(err)
	}
	return NewSonglistModule(cl)
}

// TestAddSongsConvertsTypeAtParamBuild 加歌走 AddSonglist：SongRef 收读侧原值，
// v_songInfo 里必须已是写侧枚举（普通歌 1→0，特殊类型按两阶段映射），顺序保持。
func TestAddSongsConvertsTypeAtParamBuild(t *testing.T) {
	cap := &cgiCapture{}
	m := newCaptureClient(t, cap)
	ok, err := m.AddSongs(9, 77, []SongRef{{SongID: 100, SongType: 1}, {SongID: 101, SongType: 5}, {SongID: 102, SongType: 2}})
	if err != nil || !ok {
		t.Fatalf("AddSongs ok=%v err=%v", ok, err)
	}
	if cap.module != "music.musicasset.PlaylistDetailWrite" || cap.method != "AddSonglist" {
		t.Fatalf("wrong cgi: %s.%s", cap.module, cap.method)
	}
	if cap.param["dirId"] != float64(9) || cap.param["tid"] != float64(77) || cap.param["bFmtUtf8"] != true {
		t.Fatalf("param = %v", cap.param)
	}
	items, _ := cap.param["v_songInfo"].([]any)
	want := []struct{ id, typ float64 }{{100, 0}, {101, 5}, {102, 1}}
	if len(items) != len(want) {
		t.Fatalf("v_songInfo = %v", items)
	}
	for i, w := range want {
		item := items[i].(map[string]any)
		if item["songId"] != w.id || item["songType"] != w.typ {
			t.Fatalf("item[%d] = %v want id=%v type=%v", i, item, w.id, w.typ)
		}
	}
}

// TestDelSongsConvertsType DelSonglist 与加歌共用同一映射（bFmtUtf8 按上游转 0/1）。
func TestDelSongsConvertsType(t *testing.T) {
	cap := &cgiCapture{}
	m := newCaptureClient(t, cap)
	ok, err := m.DelSongs(9, 77, []SongRef{{SongID: 100, SongType: 111}})
	if err != nil || !ok {
		t.Fatalf("DelSongs ok=%v err=%v", ok, err)
	}
	if cap.method != "DelSonglist" {
		t.Fatalf("method = %s", cap.method)
	}
	items, _ := cap.param["v_songInfo"].([]any)
	item := items[0].(map[string]any)
	if item["songType"] != float64(111) {
		t.Fatalf("special type must pass through: %v", item)
	}
}

// TestReorderSongsParamShape SeqSonglist 的参数形状（PR 307）：
// dirId/tid + v_songPair（不是 v_songInfo），类型走同一写侧映射，输入顺序原样保留。
func TestReorderSongsParamShape(t *testing.T) {
	cap := &cgiCapture{}
	m := newCaptureClient(t, cap)
	ok, err := m.ReorderSongs(9, 77, []SongRef{{SongID: 3, SongType: 1}, {SongID: 1, SongType: 1}, {SongID: 2, SongType: 113}})
	if err != nil || !ok {
		t.Fatalf("ReorderSongs ok=%v err=%v", ok, err)
	}
	if cap.module != "music.musicasset.PlaylistDetailWrite" || cap.method != "SeqSonglist" {
		t.Fatalf("wrong cgi: %s.%s", cap.module, cap.method)
	}
	if cap.param["dirId"] != float64(9) || cap.param["tid"] != float64(77) {
		t.Fatalf("param = %v", cap.param)
	}
	if _, has := cap.param["v_songInfo"]; has {
		t.Fatal("SeqSonglist must use v_songPair, not v_songInfo")
	}
	pairs, _ := cap.param["v_songPair"].([]any)
	wantIDs := []float64{3, 1, 2}
	wantTypes := []float64{0, 0, 113}
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

// TestReorderSongsRequiresLogin 未登录不发请求（RequireLogin 在 CgiCall 前置拦截）。
func TestReorderSongsRequiresLogin(t *testing.T) {
	cap := &cgiCapture{}
	cl, err := qqmusic.NewClient(&qqmusic.Credential{}, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: cap}))
	if err != nil {
		t.Fatal(err)
	}
	m := NewSonglistModule(cl)
	if _, err := m.ReorderSongs(1, 0, []SongRef{{SongID: 100, SongType: 1}}); err == nil {
		t.Fatal("expected credential error")
	} else if qqmusic.AsAPIError(err).Kind != qqmusic.ErrKindCredentialInvalid {
		t.Fatalf("err kind = %v", qqmusic.AsAPIError(err).Kind)
	}
	if cap.param != nil {
		t.Fatal("no business request may go out without login")
	}
}

// TestReorderSongsForeignSongErrors 歌单里不存在的歌（80095）必须如实报错，
// 不能像 add/del 的 80092 那样宽容吞掉 —— 重排失败装成功会让前端把假顺序写进缓存。
func TestReorderSongsForeignSongErrors(t *testing.T) {
	cap := &cgiCapture{subCode: 80095}
	m := newCaptureClient(t, cap)
	ok, err := m.ReorderSongs(9, 77, []SongRef{{SongID: 999, SongType: 1}})
	if err == nil || ok {
		t.Fatalf("expected error, got ok=%v err=%v", ok, err)
	}
	if e := qqmusic.AsAPIError(err); e.Code != 80095 {
		t.Fatalf("err = %+v", e)
	}
}

// TestReorderSongsRetCodeNonZero retCode≠0 压成 false（HTTP 层成功 ≠ 业务接受）。
func TestReorderSongsRetCodeNonZero(t *testing.T) {
	cap := &cgiCapture{retCode: 1}
	m := newCaptureClient(t, cap)
	ok, err := m.ReorderSongs(9, 77, []SongRef{{SongID: 1, SongType: 1}})
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// TestWriteSongs80092IsFalse 上游 80092（子响应 code）压成 false 而不是 true：
// UI 的 assertAccepted/toggleLove 把 false 当确凿失败 —— 吞成 true 就是
// 「歌其实没进歌单」却显示「已加入」（与上游 SDK 的 80092→False 同口径）。
func TestWriteSongs80092IsFalse(t *testing.T) {
	for _, run := range []struct {
		name string
		call func(*SonglistModule) (bool, error)
	}{
		{"add", func(m *SonglistModule) (bool, error) { return m.AddSongs(9, 77, []SongRef{{SongID: 1, SongType: 1}}) }},
		{"del", func(m *SonglistModule) (bool, error) { return m.DelSongs(9, 77, []SongRef{{SongID: 1, SongType: 1}}) }},
	} {
		t.Run(run.name, func(t *testing.T) {
			cap := &cgiCapture{subCode: 80092}
			m := newCaptureClient(t, cap)
			ok, err := run.call(m)
			if err != nil || ok {
				t.Fatalf("ok=%v err=%v (want false, nil)", ok, err)
			}
		})
	}
}
