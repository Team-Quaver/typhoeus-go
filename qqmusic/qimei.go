package qqmusic

import (
	"bytes"
	"io"

	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QIMEI 设备标识获取（对标 utils/qimei.py）。
//
// QIMEI 是腾讯统一设备标识：向 api.tencentmusic.com 提交一份随机化的设备画像
// （RSA 包裹 AES 会话密钥 + AES-CBC 加密负载 + MD5 签名），换取 q16/q36 两个
// 标识。Android 平台 comm 的 QIMEI36 字段依赖它；获取失败时该字段为空串，
// 业务请求仍可发出。

const qimeiPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDEIxgwoutfwoJxcGQeedgP7FG9qaIuS0qzfR8gWkrkTZKM2iWHn2ajQpBRZjMSoSf6+KJGvar2ORhBfpDXyVtZCKpqLQ+FLkpncClKVIrBwv6PHyUvuCb0rIarmgDnzkfQAqVufEtR64iazGDKatvJ9y6B9NMbHddGSAUmRTCrHQIDAQAB
-----END PUBLIC KEY-----`

const (
	qimeiSecret      = "ZdJqM15EeO2zWc08"
	qimeiAppKey      = "0AND0HD6FE4HY80F"
	qimeiChannelID   = "10003505"
	qimeiPackageID   = "com.tencent.qqmusic"
	qimeiSignKey     = "qimei_qq_androidpzAuCmaFAaFaHrdakPjLIEqKrGnSOOvH"
	qimeiDeviceToken = "lvcwmSYVr2Axv1gn" // AES-128 key（device oz/oo 字段加密）
	qimeiDeviceIV    = "Zs0ntDqG2jyhKN0c"
	qimeiURL         = "https://api.tencentmusic.com/tme/trpc/proxy"
)

var qimeiRSAPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(qimeiPublicKeyPEM))
	if block != nil {
		if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
			qimeiRSAPubKey, _ = pub.(*rsa.PublicKey)
		}
	}
}

// qimeiResult q16/q36 标识。
type qimeiResult struct {
	Q16 string
	Q36 string
}

// aesCBCEncrypt AES-CBC，IV 缺省取 key 本身（上游语义）；PKCS#7 padding。
func aesCBCEncrypt(key, content, iv []byte) []byte {
	if iv == nil {
		iv = key
	}
	block, _ := aes.NewCipher(key)
	padSize := 16 - len(content)%16
	padded := append(append([]byte{}, content...), bytes.Repeat([]byte{byte(padSize)}, padSize)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

// deviceOz 由 Android ID 派生的设备安全字段（base64(AES(android_id))）。
func deviceOz(androidID string) string {
	return base64.StdEncoding.EncodeToString(aesCBCEncrypt([]byte(qimeiDeviceToken), []byte(androidID), []byte(qimeiDeviceIV)))
}

// deviceOo 由机型派生的设备安全字段。
func deviceOo(model string) string {
	return base64.StdEncoding.EncodeToString(aesCBCEncrypt([]byte(qimeiDeviceToken), []byte(model), []byte(qimeiDeviceIV)))
}

// randomBeaconID 随机灯塔 ID（k1..k39 串）。
func randomBeaconID() string {
	month := time.Now().UTC().Format("2006-01") + "-01"
	r1 := 100000 + randomIntN(900000)
	r2 := 100000000 + randomIntN(900000000)
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		switch {
		case i == 1 || i == 2 || i == 13 || i == 14 || i == 17 || i == 18 ||
			i == 21 || i == 22 || i == 25 || i == 26 || i == 29 || i == 30 ||
			i == 33 || i == 34 || i == 37 || i == 38:
			fmt.Fprintf(&b, "k%d:%s%d.%d;", i, month, r1, r2)
		case i == 3:
			b.WriteString("k3:0000000000000000;")
		case i == 4:
			b.WriteString("k4:" + randomString("123456789abcdef", 16) + ";")
		default:
			fmt.Fprintf(&b, "k%d:%d;", i, randomIntN(10000))
		}
	}
	return b.String()
}

// privateIPForDevice 由 Android ID 派生的稳定私网地址（画像里的本机 IP 位）。
func privateIPForDevice(androidID string) string {
	sum := md5.Sum([]byte(androidID))
	return fmt.Sprintf("192.168.%d.%d", sum[0], int(sum[1])%253+2)
}

// buildQimeiRequest 构造 QIMEI 请求（headers + body）。
func buildQimeiRequest(dev *Device, appVersion, sdkVersion string) (map[string]string, *JObj) {
	beaconID := randomBeaconID()
	harmony := "0"
	if strings.HasPrefix(strings.ToLower(dev.VendorOSName), "harmonyos") {
		harmony = "1"
	}
	reserved := NewJObj().
		Set("harmony", harmony).
		Set("clone", "0").
		Set("containe", "").
		Set("oz", deviceOz(dev.AndroidID)).
		Set("oo", deviceOo(dev.Model)).
		Set("kelong", "0").
		Set("ip", privateIPForDevice(dev.AndroidID)).
		Set("uptimes", time.Now().UTC().Add(-time.Duration(randomIntN(14400))*time.Second).
			Format("2006-01-02 15:04:05")).
		Set("multiUser", "0").
		Set("bod", dev.Board).
		Set("brd", dev.Brand).
		Set("dv", dev.DeviceName).
		Set("firstLevel", strconv.Itoa(dev.FirstAPILevel)).
		Set("manufact", orDefault(dev.Manufacturer, dev.Brand)).
		Set("name", dev.Product).
		Set("host", dev.Host).
		Set("kernel", dev.ProcVersion).
		Set("pre", "0").
		Set("av", appVersion).
		Set("ch", "")
	payload := NewJObj().
		Set("androidId", dev.AndroidID).
		Set("platformId", 1).
		Set("appKey", qimeiAppKey).
		Set("appVersion", appVersion).
		Set("beaconIdSrc", beaconID).
		Set("brand", dev.Brand).
		Set("channelId", qimeiChannelID).
		Set("cid", "").
		Set("imei", dev.IMEI).
		Set("imsi", "").
		Set("mac", "").
		Set("model", dev.Model).
		Set("networkType", "wifi").
		Set("oaid", "").
		Set("osVersion", fmt.Sprintf("Android %s,level %d", dev.Version.Release, dev.Version.SDK)).
		Set("qimei", "").
		Set("qimei36", "").
		Set("sdkVersion", sdkVersion).
		Set("targetSdkVersion", "30").
		Set("audit", "").
		Set("userId", "{}").
		Set("packageId", qimeiPackageID).
		Set("deviceType", "Phone").
		Set("sdkName", "").
		Set("reserved", string(reserved.Marshal()))

	// 随机会话密钥/nonce（16 位 hex），RSA 包裹密钥、AES-CBC 加密负载
	const hexChars = "0123456789abcdef"
	cryptKey := randomString(hexChars, 16)
	nonce := randomString(hexChars, 16)
	ts := time.Now().Unix()

	keyBytes, _ := rsa.EncryptPKCS1v15(randReader(), qimeiRSAPubKey, []byte(cryptKey))
	key := base64.StdEncoding.EncodeToString(keyBytes)
	params := base64.StdEncoding.EncodeToString(aesCBCEncrypt([]byte(cryptKey), payload.Marshal(), nil))
	extra := fmt.Sprintf(`{"appKey":"%s"}`, qimeiAppKey)
	reqSign := md5Hex(key + params + strconv.FormatInt(ts*1000, 10) + nonce + qimeiSecret + extra)

	headers := map[string]string{
		"method":     "GetQimei",
		"service":    "trpc.tme_datasvr.qimeiproxy.QimeiProxy",
		"appid":      "qimei_qq_android",
		"sign":       md5Hex(qimeiSignKey + strconv.FormatInt(ts, 10)),
		"user-agent": "QQMusic",
		"timestamp":  strconv.FormatInt(ts, 10),
	}
	body := NewJObj().
		Set("app", 0).
		Set("os", 1).
		Set("qimeiParams", NewJObj().
			Set("key", key).
			Set("params", params).
			Set("time", strconv.FormatInt(ts, 10)).
			Set("nonce", nonce).
			Set("sign", reqSign).
			Set("extra", extra))
	return headers, body
}

// fetchQimei 请求并解析 QIMEI（响应 data 字段是「二次 JSON」的字符串）。
func fetchQimei(cl *Client, dev *Device, profile versionProfile) (*qimeiResult, error) {
	if qimeiRSAPubKey == nil {
		return nil, errData("QIMEI 公钥初始化失败")
	}
	headers, body := buildQimeiRequest(dev, profile.qimeiAppVer, profile.qimeiSDKVer)
	req, err := newRequest("POST", qimeiURL, body.Marshal())
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, respBody, err := cl.doRequest(req, 15*time.Second, false)
	if err != nil {
		return nil, err
	}
	_ = resp
	if resp.StatusCode != 200 {
		return nil, errHTTP(fmt.Sprintf("QIMEI 上游返回 %s", resp.Status), resp.StatusCode)
	}
	var outer struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(respBody, &outer); err != nil {
		return nil, errData("QIMEI 响应非 JSON: " + err.Error())
	}
	var inner struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(outer.Data), &inner); err != nil {
		return nil, errData("QIMEI data 二次解析失败: " + err.Error())
	}
	if inner.Data["q16"] == "" || inner.Data["q36"] == "" {
		return nil, errData("QIMEI 响应缺少 q16/q36")
	}
	return &qimeiResult{Q16: inner.Data["q16"], Q36: inner.Data["q36"]}, nil
}

func orDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randReader RSA 加密随机源。
func randReader() io.Reader { return crand.Reader }
