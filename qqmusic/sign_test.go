package qqmusic

import (
	"bytes"
	"encoding/json"
	"testing"
)

// 向量由 vendor/QQMusicApi 的 zzc_sign 实现直接生成（逐字相同的算法）。
func TestZzcSign(t *testing.T) {
	body := `{"comm":{"ct":"11","cv":"14090008"},"req_0":{"module":"music.vkey.GetVkey","method":"UrlGetVkey","param":{"uin":"12345","filename":["M500aabbcc.mp3"],"guid":"abc","songmid":["aabbcc"],"songtype":[0],"ctx":0}}}`
	want := "zzcb3490304it1bxvism1bl9qcnb8wocm7ru0a6b066e"
	if got := ZzcSign([]byte(body)); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// 有序 JSON：键序保持插入序、无 HTML 转义、紧凑输出 —— 与 orjson 对齐。
func TestOrderedJSON(t *testing.T) {
	obj := NewJObj().
		Set("b", 1).
		Set("a", "x<y&z").
		Set("nested", NewJObj().Set("z", true).Set("y", nil))
	want := `{"b":1,"a":"x<y&z","nested":{"z":true,"y":null}}`
	if got := string(obj.Marshal()); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	arr := &JArr{"s", int64(2), false}
	if got := string(arr.Marshal()); got != `["s",2,false]` {
		t.Fatalf("got %s", got)
	}
}

func TestCredentialJSONCompat(t *testing.T) {
	// Python Credential.model_dump_json() 输出（snake_case）必须能读回
	pyJSON := `{"openid":"o1","refresh_token":"rt","access_token":"at","expired_at":123,
		"musicid":10000,"musickey":"Q_H_L_XX","unionid":"","str_musicid":"10000",
		"refresh_key":"rk","musickey_create_time":1700000000,"key_expires_in":259200,
		"first_login":0,"bind_account_type":0,"need_refresh_key_in":0,
		"encrypt_uin":"euin","login_type":2}`
	var c Credential
	if err := json.Unmarshal([]byte(pyJSON), &c); err != nil {
		t.Fatal(err)
	}
	if !c.HasLogin() || c.EncryptUin != "euin" || c.LoginType != 2 {
		t.Fatalf("snake_case 解析失败: %+v", c)
	}
	// camelCase 别名也能读（Electron 主进程可能透传 pydantic alias 形态）
	camel := `{"musicid":1,"musickey":"W_X_YY","encryptUin":"e","loginType":1,
		"musickeyCreateTime":1700000000,"keyExpiresIn":259200}`
	var c2 Credential
	if err := json.Unmarshal([]byte(camel), &c2); err != nil {
		t.Fatal(err)
	}
	if c2.EncryptUin != "e" || c2.KeyExpiresIn != 259200 || c2.LoginType != 1 {
		t.Fatalf("camelCase 解析失败: %+v", c2)
	}
	// dump 输出保持 snake_case（QCRED1 交接兼容）
	if !bytes.Contains([]byte(dumpCredential(&c)), []byte(`"str_musicid"`)) {
		t.Fatalf("序列化应保持 snake_case")
	}
}

func TestHash33(t *testing.T) {
	// 手算：h=5381 → *33+'a' → *33+'b' → *33+'c'
	if got := Hash33("abc", 5381); got != 193485963 {
		t.Fatalf("got %d", got)
	}
}
