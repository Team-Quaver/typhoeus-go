package modules

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

type loginTransport func(*http.Request) (*http.Response, error)

func (f loginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(body string, status int, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}
func requireCookie(t *testing.T, r *http.Request, name, want string) {
	t.Helper()
	cookie, _ := r.Cookie(name)
	if cookie == nil || cookie.Value != want {
		t.Fatalf("missing/wrong cookie %s", name)
	}
}
func TestQQOAuthCode(t *testing.T) {
	for _, tc := range []struct{ location, want string }{
		{"https://y.qq.com/portal/wx_redirect.html?code=ABC&state=state", "ABC"},
		{"https://graph.qq.com/oauth2.0/login_jump?state=state&code=A%2BB", "A+B"},
		{"https://y.qq.com/?state=state", ""},
		{"https://invalid.example/?code=secret", ""},
		{"http://graph.qq.com/?code=secret", ""},
	} {
		if got := qqOAuthCode(tc.location); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

// 从登录页开始测试整个流程。check_sig 地址包含重建会丢失的签名参数；
// p_skey 与删除同名 Cookie 同时出现，按名称折叠响应会再次复现用户报错。
func TestQQQRLoginChain(t *testing.T) {
	redirect := "https://ssl.ptlogin2.graph.qq.com/check_sig?service=ptqrlogin&ptsigx=SIG%2BVALUE&uin=123&pt_login_type=4&pt_3rd_aid=100497308&pt_extra=opaque%2Fvalue&s_url=https%3A%2F%2Fgraph.qq.com%2Foauth2.0%2Flogin_jump"
	for _, final := range []struct {
		name, body string
		ok         bool
	}{
		{"success", `{"code":0,"req_0":{"code":0,"data":{"musicid":123,"musickey":"Q_KEY","loginType":2}}}`, true},
		{"empty credential", `{"code":0,"req_0":{"code":0,"data":{}}}`, false},
		{"login error", `{"code":0,"req_0":{"code":104401,"data":{}}}`, false},
	} {
		t.Run(final.name, func(t *testing.T) {
			step := 0
			transport := loginTransport(func(r *http.Request) (*http.Response, error) {
				step++
				if strings.Contains(r.Header.Get("Cookie"), "OLD_KEY") {
					t.Fatal("previous account in QR flow")
				}
				switch step {
				case 1:
					if r.URL.Path != "/cgi-bin/xlogin" {
						t.Fatal("must bootstrap xlogin")
					}
					return reply(`ptui_version:encodeURIComponent("26092315")`, 200, http.Header{"Set-Cookie": {"pt_login_sig=LOGIN_SIG; Domain=.ptlogin2.qq.com; Path=/"}}), nil
				case 2:
					requireCookie(t, r, "pt_login_sig", "LOGIN_SIG")
					if r.URL.Query().Get("u1") != qqLoginJump {
						t.Fatal("missing QR return target")
					}
					return reply("PNG", 200, http.Header{"Set-Cookie": {"qrsig=QR_SIG; Domain=.ptlogin2.qq.com; Path=/"}}), nil
				case 3:
					requireCookie(t, r, "qrsig", "QR_SIG")
					requireCookie(t, r, "pt_login_sig", "LOGIN_SIG")
					if r.URL.Query().Get("login_sig") != "LOGIN_SIG" {
						t.Fatal("missing login signature")
					}
					return reply(`ptuiCB('66','0','','0','','')`, 200, http.Header{"Set-Cookie": {"scan_nonce=SCAN; Domain=.qq.com; Path=/"}}), nil
				case 4:
					requireCookie(t, r, "scan_nonce", "SCAN")
					return reply("ptuiCB('0','0','"+redirect+"','0','OK','nick')", 200, nil), nil
				case 5:
					if r.URL.String() != redirect {
						t.Fatal("signed callback was reconstructed")
					}
					requireCookie(t, r, "scan_nonce", "SCAN")
					if c, _ := r.Cookie("qrsig"); c != nil {
						t.Fatal("domain-scoped qrsig leaked to graph")
					}
					return reply("", 302, http.Header{"Location": {"https://graph.qq.com/oauth2.0/login_jump"}, "Set-Cookie": {
						"p_skey=NEW_SKEY; Domain=.graph.qq.com; Path=/",
						"p_skey=; Domain=ssl.ptlogin2.graph.qq.com; Path=/; Max-Age=0",
						"skey=QQ_SKEY; Domain=.qq.com; Path=/",
					}}), nil
				case 6:
					requireCookie(t, r, "p_skey", "NEW_SKEY")
					_ = r.ParseForm()
					if r.Method != "POST" || r.Form.Get("g_tk") == "" {
						t.Fatal("bad OAuth request")
					}
					return reply("", 302, http.Header{"Location": {"https://graph.qq.com/oauth2.0/approved"}}), nil
				case 7:
					return reply("", 302, http.Header{"Location": {"https://y.qq.com/portal/wx_redirect.html?state=state&code=CODE%2BVALUE"}}), nil
				case 8:
					var body struct {
						Comm map[string]any `json:"comm"`
						Req  struct {
							Method string         `json:"method"`
							Param  map[string]any `json:"param"`
						} `json:"req_0"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Comm["ct"] != "24" || body.Req.Param["code"] != "CODE+VALUE" || body.Req.Method != "QQLogin" {
						t.Fatalf("bad Web login: %+v", body)
					}
					requireCookie(t, r, "skey", "QQ_SKEY")
					requireCookie(t, r, "login_type", "1")
					wantGTK := strconv.FormatInt(qqmusic.Hash33("QQ_SKEY", 5381), 10)
					if body.Comm["g_tk"] != wantGTK || body.Comm["g_tk_new_20200303"] != wantGTK {
						t.Fatal("music CGI must hash its own domain cookies")
					}
					if c, _ := r.Cookie("p_skey"); c != nil {
						t.Fatal("graph Cookie leaked to music host")
					}
					return reply(final.body, 200, nil), nil
				default:
					t.Fatalf("unexpected request %d (%s)", step, r.URL.Path)
					return nil, nil
				}
			})
			cl, err := qqmusic.NewClient(&qqmusic.Credential{MusicID: 999, MusicKey: "OLD_KEY"}, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			m := NewLoginModule(cl)
			qr, err := m.GetQQQR(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			pending, err := m.CheckQQQR(context.Background(), qr.Identifier)
			if err != nil || pending.Event != EventScan {
				t.Fatalf("pending: %+v %v", pending, err)
			}
			result, err := m.CheckQQQR(context.Background(), qr.Identifier)
			if final.ok {
				if err != nil || result == nil || !result.Done || result.Credential.MusicID != 123 {
					t.Fatalf("login failed: %+v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("invalid login must fail")
			}
			_, _ = m.CheckQQQR(context.Background(), qr.Identifier)
			if step != 8 {
				t.Fatalf("authorization should be consumed once, steps=%d", step)
			}
		})
	}
}

func TestQQQRUnknownSession(t *testing.T) {
	m := NewLoginModule(nil)
	result, err := m.CheckQQQR(context.Background(), "unknown")
	if err != nil || result.Event != EventTimeout || !result.Done {
		t.Fatalf("lost session must expire: %+v %v", result, err)
	}
}

func TestQQOAuthRedirectAndCookies(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "intermediate cookies", true: "foreign redirect"}[foreign], func(t *testing.T) {
			calls := 0
			cl, _ := qqmusic.NewClient(nil, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: loginTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					loc := "https://graph.qq.com/next"
					if foreign {
						loc = "https://other.example/check"
					}
					return reply("", 302, http.Header{"Location": {loc}, "Set-Cookie": {"p_uin=123; Domain=.graph.qq.com; Path=/"}}), nil
				}
				requireCookie(t, r, "p_uin", "123")
				return reply("", 200, http.Header{"Set-Cookie": {"p_skey=FINAL; Domain=.graph.qq.com; Path=/"}}), nil
			})}))
			jar, _ := cookiejar.New(nil)
			session := &qqLoginSession{jar: jar, created: time.Now()}
			_, err := NewLoginModule(cl).qqOAuthHTTP(context.Background(), "GET", "https://ssl.ptlogin2.graph.qq.com/check_sig", qqmusic.HTTPOption{}, session, false)
			if foreign {
				if err == nil || calls != 1 {
					t.Fatal("foreign redirect must be rejected")
				}
			} else if err != nil || session.cookies("https://graph.qq.com/")["p_skey"] != "FINAL" {
				t.Fatalf("intermediate cookie lost: %v", err)
			}
		})
	}
}
