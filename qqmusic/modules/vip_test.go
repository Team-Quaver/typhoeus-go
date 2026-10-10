package modules

import (
	"encoding/json"
	"fmt"
	"github.com/team-quaver/typhoeus-go/qqmusic"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestVipQueryIndependentEntitlements(t *testing.T) {
	raw := json.RawMessage(`{"infoMap":{"123":{"HugeVip":0,"HugeVipStart":"2025-01-01","HugeVipEnd":"2025-02-01","iSuperVip":"1","superStartTime":"2026-01-01","superEndTime":"2026-11-01","iVipFlag":1,"sStartDateTime":"2026-01-01","sOverDateTime":"2026-12-01","iCurLevel":6}}}`)
	v, err := parseVipQuery(raw, 123)
	if err != nil {
		t.Fatal(err)
	}
	if v.Svip != 0 || v.Identity.HugeVip != 1 || v.Identity.Vip != 1 {
		t.Fatalf("wrong identities: %+v", v)
	}
	if v.SvipEnd != "2025-02-01" || v.Identity.HugeVipEnd != "2026-11-01" || v.Identity.VipEnd != "2026-12-01" {
		t.Fatalf("mixed expiration dates: %+v", v)
	}
	if _, err := parseVipQuery(raw, 456); err == nil {
		t.Fatal("must not use another account's info")
	}
}

func TestVipActiveBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, vipZone)
	for _, tc := range []struct {
		name       string
		flag       int64
		start, end string
		want       bool
	}{
		{"active", 1, "2026-01-01", "2026-10-11", true},
		{"exact expiry", 1, "", "2026-10-10 12:00:00", false},
		{"expired flag", 1, "", "2026-10-09", false},
		{"future start", 1, "2026-10-11", "2026-12-01", false},
		{"date end includes day", 1, "", "2026-10-10", true},
		{"unknown dates", 1, "", "", true},
		{"no membership", 0, "2026-01-01", "2026-12-01", false},
		{"invalid date", 1, "", "2026-02-31", false},
		{"invalid flag", 2, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VipActive(tc.flag, tc.start, tc.end, now); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	v := VipInfo{Svip: 1, SvipEnd: "2026-09-01", Identity: VipIdentity{HugeVip: 1, HugeVipEnd: "2026-11-01", Vip: 1, VipEnd: "2026-09-01"}}
	v = v.Active(now)
	if v.Svip != 0 || v.Identity.HugeVip != 1 || v.Identity.Vip != 0 || v.SvipEnd != "2026-09-01" {
		t.Fatalf("expiry must be per entitlement: %+v", v)
	}
}

func TestLegacyVipMapping(t *testing.T) {
	for _, raw := range []string{
		`{"svip":1,"identity":{"vip":1,"HugeVip":0,"HugeVipEnd":"2025-01-01"}}`,
		`{"svip":"1","identity":{"vip":"1","huge_vip":"0","huge_vip_end":"2025-01-01"}}`,
	} {
		v, err := parseVipLogin(json.RawMessage(raw))
		if err != nil || v.Svip != 0 || v.Identity.HugeVip != 1 || v.SvipEnd != "2025-01-01" || v.Identity.HugeVipEnd != "" {
			t.Fatalf("legacy svip is green; HugeVipEnd must stay with super: %+v %v", v, err)
		}
	}
	v, err := parseVipLogin(json.RawMessage(`{"svip":1,"identity":{"HugeVip":1,"HugeVipEnd":"2099-01-01","HugeYearFlag":1}}`))
	if err != nil || v.Svip != 1 || v.SvipEnd != "2099-01-01" || v.SvipYearFlag != 1 {
		t.Fatalf("legacy super: %+v %v", v, err)
	}
	for _, raw := range []string{`{}`, `null`, `{"identity":{}}`} {
		if _, err := parseVipLogin(json.RawMessage(raw)); err == nil {
			t.Fatal("malformed result must not become nonmember")
		}
	}
}

// 真实请求形状测试，而不只是 parseVipQuery：缺少 Web 平台/Cookie 会被网关拒绝。
func TestVipQueryWebAnd10006Fallback(t *testing.T) {
	for _, code := range []int{0, 10006, 1000} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			webCalls, legacyCalls := 0, 0
			cl, err := qqmusic.NewClient(&qqmusic.Credential{MusicID: 123, MusicKey: "WEB_KEY", LoginType: 2}, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: loginTransport(func(r *http.Request) (*http.Response, error) {
				var body struct {
					Comm map[string]any `json:"comm"`
					Req  struct {
						Module string         `json:"module"`
						Param  map[string]any `json:"param"`
					} `json:"req_0"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch body.Req.Module {
				case "userInfo.VipQueryServer":
					webCalls++
					if body.Comm["ct"] != "24" || body.Comm["tmeLoginType"] != "2" || body.Comm["uin"] != "123" {
						t.Fatalf("wrong VIP platform: %+v", body.Comm)
					}
					// SRFVipQuery_V2 的账号列表是 string[]，数字元素会触发 10006。
					ids, ok := body.Req.Param["uin_list"].([]any)
					if !ok || len(ids) != 1 || ids[0] != "123" {
						t.Fatalf("uin_list must contain JSON strings, got %#v", body.Req.Param["uin_list"])
					}
					requireCookie(t, r, "qqmusic_key", "WEB_KEY")
					requireCookie(t, r, "qm_keyst", "WEB_KEY")
					requireCookie(t, r, "qqmusic_uin", "123")
					if code != 0 {
						return reply(fmt.Sprintf(`{"code":0,"req_0":{"code":%d}}`, code), 200, nil), nil
					}
					return reply(`{"code":0,"req_0":{"code":0,"data":{"infoMap":{"123":{"HugeVip":1,"HugeVipEnd":"2099-01-01","iSuperVip":1,"superEndTime":"2099-02-01"}}}}}`, 200, nil), nil
				case "VipLogin.VipLoginInter":
					legacyCalls++
					return reply(`{"code":0,"req_0":{"code":0,"data":{"svip":1,"identity":{"HugeVip":1,"HugeVipEnd":"2099-01-01"}}}}`, 200, nil), nil
				default: // Android 回退所需的匿名会话/设备请求，失败应不阻塞登录权益 CGI。
					return reply(`{}`, 200, nil), nil
				}
			})}))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := NewUserModule(cl).GetVipInfo()
			if code == 1000 {
				if err == nil || legacyCalls != 0 {
					t.Fatal("expired credential should refresh, not fallback")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var v VipInfo
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			if webCalls != 1 || (code == 10006 && legacyCalls != 1) || (code == 0 && legacyCalls != 0) || v.Svip != 1 || v.SvipEnd != "2099-01-01" || v.Identity.HugeVip != 1 {
				t.Fatalf("VIP query fallback demoted super: calls=%d/%d value=%+v", webCalls, legacyCalls, v)
			}
		})
	}
}
