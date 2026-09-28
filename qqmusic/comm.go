package qqmusic

import (
	"strconv"
	"strings"
)

// 请求平台（对标 core/versioning.py Platform）。默认 ANDROID，与官方客户端行为对齐。
type Platform string

const (
	PlatformAndroid Platform = "android"
	PlatformDesktop Platform = "desktop"
	PlatformWeb     Platform = "web"
)

// 各平台的版本档案（ct=客户端类型 / cv=版本号，逆向自官方客户端）。
type versionProfile struct {
	ct          int64
	cv          int64
	v           int64 // 仅 android
	platform    string
	uaVersion   int64
	qimeiAppVer string
	qimeiSDKVer string
}

var defaultVersionProfiles = map[Platform]versionProfile{
	PlatformAndroid: {ct: 11, cv: 14090008, v: 14090008, uaVersion: 14090008,
		qimeiAppVer: "14.9.0.8", qimeiSDKVer: "1.2.13.6"},
	PlatformDesktop: {ct: 19, cv: 2201},
	PlatformWeb:     {ct: 24, cv: 4747474, platform: "yqq.json"},
}

// Hash33 QQ 音乐通用字符串哈希（g_tk、ptqrtoken 都用它）。
func Hash33(s string, h int64) int64 {
	for _, c := range s {
		h = (h << 5) + h + int64(c)
	}
	return 2147483647 & h
}

// GTK 计算 CSRF token（web/desktop comm 用）。
func GTK(c *Credential) int64 {
	if c.MusicKey != "" {
		return Hash33(c.MusicKey, 5381)
	}
	return 5381
}

// BuildComm 构建统一公共参数 comm（所有值转字符串；0 值也保留，如 notice=0）。
//
// android：凭证走 qq/authst 字段（官方客户端语义），并携带设备指纹与 QIMEI；
// web/desktop：凭证只体现在 uin/g_tk。
func (cl *Client) BuildComm(platform Platform, c *Credential) *JObj {
	profile := defaultVersionProfiles[platform]
	p := NewJObj()
	switch platform {
	case PlatformAndroid:
		p.Set("ct", strconv.FormatInt(profile.ct, 10))
		p.Set("cv", strconv.FormatInt(profile.cv, 10))
		p.Set("v", strconv.FormatInt(profile.v, 10))
		p.Set("chid", "10003505")
		if c.MusicID != 0 {
			p.Set("qq", strconv.FormatInt(c.MusicID, 10))
		}
		if c.MusicKey != "" {
			p.Set("authst", c.MusicKey)
		}
		p.Set("tmeAppID", "qqmusic")
		if c.LoginType != 0 {
			p.Set("tmeLoginType", strconv.FormatInt(c.LoginType, 10))
		}
		p.Set("QIMEI36", cl.qimei36())
		p.Set("OpenUDID", cl.device.OpenUDID)
		p.Set("udid", cl.device.OpenUDID)
		// uid/sid：Android 匿名会话（跨自然日刷新），失败时缺省
		if cl.androidSession != nil {
			p.Set("uid", cl.androidSession.UID)
			p.Set("sid", cl.androidSession.SID)
		}
		p.Set("OpenUDID2", cl.device.OpenUDID2)
		p.Set("aid", cl.device.AndroidID)
		p.Set("os_ver", cl.device.Version.Release)
		p.Set("phonetype", xmlEscapeQuote(cl.device.Model))
	case PlatformDesktop:
		p.Set("ct", strconv.FormatInt(profile.ct, 10))
		p.Set("cv", strconv.FormatInt(profile.cv, 10))
		p.Set("chid", "0")
		if c.MusicID != 0 {
			p.Set("uin", strconv.FormatInt(c.MusicID, 10))
		}
		p.Set("g_tk", strconv.FormatInt(GTK(c), 10))
		p.Set("guid", strings.ToUpper(cl.device.OpenUDID))
	default: // web
		p.Set("ct", strconv.FormatInt(profile.ct, 10))
		p.Set("cv", strconv.FormatInt(profile.cv, 10))
		p.Set("platform", profile.platform)
		p.Set("chid", "0")
		p.Set("uin", strconv.FormatInt(c.MusicID, 10))
		gtk := strconv.FormatInt(GTK(c), 10)
		p.Set("g_tk", gtk)
		p.Set("g_tk_new_20200303", gtk)
		p.Set("format", "json")
		p.Set("inCharset", "utf-8")
		p.Set("outCharset", "utf-8")
		p.Set("notice", "0")
		p.Set("need_new_code", "1")
	}
	return p
}

// UserAgent 按平台取 UA（web/desktop 都是 Chrome 桌面 UA）。
func (cl *Client) UserAgent(platform Platform) string {
	if platform == PlatformAndroid {
		profile := defaultVersionProfiles[platform]
		return "QQMusic " + strconv.FormatInt(profile.uaVersion, 10) +
			"(android " + cl.device.Version.Release + ")"
	}
	return chromeUA
}

const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// xmlEscapeQuote 上游 phonetype 只对引号做 XML 转义（escape(model, {'"': "&quot;"})）。
func xmlEscapeQuote(s string) string {
	return strings.ReplaceAll(s, `"`, "&quot;")
}

// mergeCommOverrides 把请求级 comm 覆盖合并进默认 comm：
// 非空值覆盖为字符串，空值/None 删除同名键（对齐 CgiExecutor._prepare_batch）。
func mergeCommOverrides(base *JObj, overrides *JObj) *JObj {
	if overrides == nil {
		return base
	}
	for _, k := range overrides.keys {
		v := overrides.vals[k]
		if v == nil {
			base.Delete(k)
			continue
		}
		switch x := v.(type) {
		case string:
			if x == "" {
				base.Delete(k)
				continue
			}
			base.Set(k, x)
		default:
			base.Set(k, commStringify(v))
		}
	}
	return base
}

// commStringify 与 Python str() 行为对齐：bool → "True"/"False"，数值去掉类型后缀。
func commStringify(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		panic("comm: 不支持的覆盖值类型")
	}
}
