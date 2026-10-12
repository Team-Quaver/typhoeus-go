package qqmusic

import (
	"encoding/json"
	"strconv"
	"time"
)

// Credential 登录凭证（对标 models/request.py Credential）。
//
// JSON 序列化采用 snake_case 字段名 —— 与 Python 侧 Credential.model_dump_json()
// 的输出逐字一致，保证与 Electron 主进程的 QCRED1 交接管道双向兼容；
// 反序列化同时接受 snake_case 与 camelCase（musickeyCreateTime 等）别名。
type Credential struct {
	OpenID           string `json:"openid"`
	RefreshToken     string `json:"refresh_token"`
	AccessToken      string `json:"access_token"`
	ExpiredAt        int64  `json:"expired_at"`
	MusicID          int64  `json:"musicid"`
	MusicKey         string `json:"musickey"`
	UnionID          string `json:"unionid"`
	StrMusicID       string `json:"str_musicid"`
	RefreshKey       string `json:"refresh_key"`
	MusicKeyCreateAt int64  `json:"musickey_create_time"`
	KeyExpiresIn     int64  `json:"key_expires_in"`
	FirstLogin       int64  `json:"first_login"`
	BindAccountType  int64  `json:"bind_account_type"`
	NeedRefreshKeyIn int64  `json:"need_refresh_key_in"`
	EncryptUin       string `json:"encrypt_uin"`
	LoginType        int64  `json:"login_type"`
}

// credentialAliasMap camelCase → snake_case 的输入别名（Python pydantic alias）。
var credentialAliasMap = map[string]string{
	"openid":               "openid",
	"refresh_token":        "refresh_token",
	"refreshToken":         "refresh_token",
	"access_token":         "access_token",
	"accessToken":          "access_token",
	"expired_at":           "expired_at",
	"expiredAt":            "expired_at",
	"musicid":              "musicid",
	"musickey":             "musickey",
	"unionid":              "unionid",
	"str_musicid":          "str_musicid",
	"strMusicid":           "str_musicid",
	"refresh_key":          "refresh_key",
	"refreshKey":           "refresh_key",
	"musickeyCreateTime":   "musickey_create_time",
	"musickey_create_time": "musickey_create_time",
	"keyExpiresIn":         "key_expires_in",
	"key_expires_in":       "key_expires_in",
	"first_login":          "first_login",
	"firstLogin":           "first_login",
	"bindAccountType":      "bind_account_type",
	"bind_account_type":    "bind_account_type",
	"needRefreshKeyIn":     "need_refresh_key_in",
	"need_refresh_key_in":  "need_refresh_key_in",
	"encryptUin":           "encrypt_uin",
	"encrypt_uin":          "encrypt_uin",
	"loginType":            "login_type",
	"login_type":           "login_type",
}

// stringCredentialFields JSON 里以字符串形态出现的凭证字段。
var stringCredentialFields = map[string]bool{
	"openid": true, "refresh_token": true, "access_token": true, "musickey": true,
	"unionid": true, "str_musicid": true, "refresh_key": true, "encrypt_uin": true,
}

// UnmarshalJSON 兼容 snake/camel 两种字段名。
//
// 数字字段严禁经 float64 中转：微信 uin 是长整数，超过 2^53 后
// any(float64) → int64 会静默丢精度，导致后续所有带 uin 的请求
// （comm.uin/qq、Cookie、uin_list）拿着错误的 uin 鉴权。
// 这里按 RawMessage 原样取值，数字字面量走 json.Number → ParseInt 精确转换。
func (c *Credential) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		name, ok := credentialAliasMap[k]
		if !ok {
			continue
		}
		if stringCredentialFields[name] {
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				setStringField(c, name, s)
			}
			continue
		}
		if num, ok := parseCredentialInt(v); ok {
			setIntField(c, name, num)
		}
	}
	return nil
}

// parseCredentialInt 数值字段宽容解析：字符串/整数/浮点/布尔都收，
// 整数字面量（含大数）走 json.Number 保持精确。
func parseCredentialInt(v json.RawMessage) (int64, bool) {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	}
	var b bool
	if err := json.Unmarshal(v, &b); err == nil && b {
		return 1, true
	}
	return 0, false
}

func setStringField(c *Credential, name, v string) {
	switch name {
	case "openid":
		c.OpenID = v
	case "refresh_token":
		c.RefreshToken = v
	case "access_token":
		c.AccessToken = v
	case "musickey":
		c.MusicKey = v
	case "unionid":
		c.UnionID = v
	case "str_musicid":
		c.StrMusicID = v
	case "refresh_key":
		c.RefreshKey = v
	case "encrypt_uin":
		c.EncryptUin = v
	}
}

func setIntField(c *Credential, name string, v int64) {
	switch name {
	case "expired_at":
		c.ExpiredAt = v
	case "musicid":
		c.MusicID = v
	case "musickey_create_time":
		c.MusicKeyCreateAt = v
	case "key_expires_in":
		c.KeyExpiresIn = v
	case "first_login":
		c.FirstLogin = v
	case "bind_account_type":
		c.BindAccountType = v
	case "need_refresh_key_in":
		c.NeedRefreshKeyIn = v
	case "login_type":
		c.LoginType = v
	}
}

// HasLogin 凭证是否含可用登录信息（同上游 web 层判定）。
func (c *Credential) HasLogin() bool {
	return c.MusicID > 0 && c.MusicKey != ""
}

// IsExpired musickey 只有约 3 天有效期（key_expires_in，秒）；过期后写接口会被
// 服务端拦截。与上游一致按「本地时间到点」判定。
func (c *Credential) IsExpired() bool {
	return time.Now().Unix() >= c.MusicKeyCreateAt+c.KeyExpiresIn
}

// inferLoginType 在缺省时根据 musickey 前缀推断登录类型（1=微信 W_X 前缀，2=QQ）。
func inferLoginType(musicKey string) int64 {
	if musicKey == "" {
		return 0
	}
	if len(musicKey) >= 3 && musicKey[:3] == "W_X" {
		return 1
	}
	return 2
}

// SelfEuin 当前账号的「加密 UIN / 字符串 UIN」（收藏歌单等接口的主键）。
func (c *Credential) SelfEuin() string {
	if c.EncryptUin != "" {
		return c.EncryptUin
	}
	if c.StrMusicID != "" {
		return c.StrMusicID
	}
	return strconv.FormatInt(c.MusicID, 10)
}

// dumpCredential 序列化凭证（snake_case，QCRED1 交接管道格式）。
func dumpCredential(c *Credential) string {
	raw, _ := json.Marshal(c)
	return string(raw)
}
