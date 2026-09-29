package modules

import (
	"regexp"
	"testing"
)

// qqCodeRe 提取 oauth authorize 302 Location 里的 code。
// 回归背景：曾从 Python 版照抄 lookbehind 正则 (?<=code=)(.+?)(?=&)——
// Go regexp 是 RE2 引擎，不支持 lookbehind，MustCompile 直接 panic。
// panic 被 net/http 的 per-connection recover 吃掉 → 前端只看到「手机确认了没反应」
// （每次轮询都在同一行炸，永远拿不到凭证）。本测试兜住「正则必须可编译 + 可提取」。
func TestQQCodeRe(t *testing.T) {
	cases := []struct {
		name string
		loc  string
		want string
	}{
		{"标准 QQ 授权回跳", "https://y.qq.com/portal/wx_redirect.html?login_type=1&surl=https://y.qq.com/&code=4F2AB9C1&state=state", "4F2AB9C1"},
		{"code 为首参", "https://graph.qq.com/oauth2.0/login_jump?code=ABCDEF123&state=state", "ABCDEF123"},
		{"code 为末参", "https://y.qq.com/portal/wx_redirect.html?state=state&code=XYZ7890", "XYZ7890"},
		{"无 code", "https://y.qq.com/portal/wx_redirect.html?state=state", ""},
	}
	for _, c := range cases {
		m := qqCodeRe.FindStringSubmatch(c.loc)
		got := ""
		if len(m) > 1 {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// 编译期哨兵：任何以 lookbehind/lookahead 写法回归都会在 MustCompile panic
	_ = regexp.MustCompile(`[?&]code=([^&]+)`)
}
