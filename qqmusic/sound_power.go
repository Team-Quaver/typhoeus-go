package qqmusic

// 听歌时长（音响力物理流水）上报：协议对标 QQMusicApi examples/sound_power.py。
// imusic_tj 吃 gzip XML，响应是 5 字节哨兵 + zlib(JSON)，与普通 CGI 信封不同。

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	imusicTJURL      = "https://stat6.y.qq.com/android/fcgi-bin/imusic_tj"
	imusicTJTimeSalt = "gk2$Lh-&l4#!4iow"
	imusicTJMaxBody  = 1 << 20
)

// PlayDurationReport 一次「听歌物理流水」上报。
//
// 这是音响力的原始播放流水，不是普通播放计数：上游会把 time 计入当日听歌时长。
// 一次只报一段真实播放时间，调用方按累计时长分片即可。
type PlayDurationReport struct {
	SongID             int64
	AudioTimeMS        int64
	PlayDurationSecond int
	VKey               string
	CoSinger           string
	SingerID           int64
}

// ReportPlayDuration 向 imusic_tj 网关提交听歌物理流水。
//
// 返回 ok=false 表示上游明确拒绝（HTTP 请求本身成功）；网络/协议错误返回 error。
// vkey 由播放链路在服务端取链时保留，绝不会下发到渲染层。
func (cl *Client) ReportPlayDuration(ctx context.Context, report PlayDurationReport) (bool, error) {
	cred := cl.Credential()
	if !cred.HasLogin() {
		return false, errCredentialInvalid("上报听歌时长需要登录")
	}
	if report.SongID <= 0 || report.AudioTimeMS <= 0 || report.PlayDurationSecond <= 0 {
		return false, errData("听歌时长上报参数不完整")
	}
	if report.PlayDurationSecond > 3600 {
		report.PlayDurationSecond = 3600
	}
	if strings.TrimSpace(report.VKey) == "" {
		return false, errData("听歌时长上报缺少 vkey")
	}

	session, err := cl.ensureAndroidSession()
	if err != nil {
		return false, err
	}
	// QIMEI 失败不该阻止上报；BuildComm 会按空值继续。
	_, _ = cl.qimeiCached()

	optime := time.Now().Unix()
	comm := cl.BuildComm(PlatformAndroid, cred)
	userQQ, ok := credentialUin(cred)
	if !ok {
		userQQ = "0"
	}
	traceUID := userQQ
	if traceUID == "0" {
		traceUID = cl.device.OpenUDID
	}
	comm.Set("traceid", fmt.Sprintf("10002_%s_%d", traceUID, optime))

	body, err := buildPlayDurationXML(comm, playDurationItem{
		Optime:       optime,
		UserQQ:       userQQ,
		UID:          session.UID,
		OSVersion:    cl.device.Version.Release,
		Model:        cl.device.Model,
		AudioTimeMS:  report.AudioTimeMS,
		Duration:     report.PlayDurationSecond,
		VKey:         report.VKey,
		CoSinger:     report.CoSinger,
		SongID:       report.SongID,
		SingerID:     report.SingerID,
		TimeKey:      playDurationTimeKey(optime, report.PlayDurationSecond, userQQ),
		String27JSON: playDurationString27(report.PlayDurationSecond),
	})
	if err != nil {
		return false, err
	}

	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write([]byte(body)); err != nil {
		return false, errNetwork("压缩听歌时长上报失败: " + err.Error())
	}
	if err := gw.Close(); err != nil {
		return false, errNetwork("压缩听歌时长上报失败: " + err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imusicTJURL, bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return false, errNetwork(err.Error())
	}
	req.Header.Set("User-Agent", cl.UserAgent(PlatformAndroid))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "*/*")

	resp, err := cl.httpClient().Do(req)
	if err != nil {
		return false, errNetwork("听歌时长上报请求失败: " + err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, imusicTJMaxBody))
	if err != nil {
		return false, errNetwork("读取听歌时长上报响应失败: " + err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return false, errHTTP(fmt.Sprintf("听歌时长上报上游返回 %s", resp.Status), resp.StatusCode)
	}
	if len(raw) <= 5 || !bytes.Equal(raw[:5], []byte{0, 0, 0, 0, 0}) {
		return false, errData("听歌时长上报响应格式异常")
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[5:]))
	if err != nil {
		return false, errData("听歌时长上报响应解压失败: " + err.Error())
	}
	defer zr.Close()
	decoded, err := io.ReadAll(io.LimitReader(zr, imusicTJMaxBody))
	if err != nil {
		return false, errData("听歌时长上报响应读取失败: " + err.Error())
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(decoded, &result); err != nil {
		return false, errData("听歌时长上报响应 JSON 异常: " + err.Error())
	}
	return result.Code == 0, nil
}

// playDurationTimeKey 上游物理流水防重签名，算法见 QQMusicApi 示例。
func playDurationTimeKey(optime int64, seconds int, userQQ string) string {
	sum := md5.Sum([]byte(strconv.FormatInt(optime, 10) + strconv.Itoa(seconds) + userQQ + imusicTJTimeSalt))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// playDurationString27 页面停留时长的 base64 声明。
func playDurationString27(seconds int) string {
	payload := struct {
		ScreenOn      int `json:"screen_on"`
		AppIn         int `json:"app_in"`
		AppTime       int `json:"app_time"`
		PlaypageTime  int `json:"playpage_time"`
		TagID         int `json:"tag_id"`
		TagType       int `json:"tag_type"`
		StartPlayType int `json:"start_playtype"`
		StartPlayTime int `json:"start_playtime"`
	}{
		ScreenOn: 1, AppIn: 1, AppTime: seconds, PlaypageTime: int(float64(seconds) * 0.4),
		TagID: 10001, TagType: 15,
	}
	raw, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(raw)
}

type playDurationItem struct {
	Optime       int64
	UserQQ       string
	UID          string
	OSVersion    string
	Model        string
	AudioTimeMS  int64
	Duration     int
	VKey         string
	CoSinger     string
	SongID       int64
	SingerID     int64
	TimeKey      string
	String27JSON string
}

func buildPlayDurationXML(comm *JObj, item playDurationItem) (string, error) {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\r\n<root>\r\n")
	for _, key := range comm.Keys() {
		value, _ := comm.Get(key)
		text := fmt.Sprint(value)
		if key == "phonetype" {
			// BuildComm 为 CGI JSON 预先转义了引号；这里还原后统一按 XML 文本转义。
			text = strings.ReplaceAll(text, "&quot;", "\"")
		}
		b.WriteString("    <")
		b.WriteString(key)
		b.WriteString(">")
		writeXMLEscaped(&b, text, false)
		b.WriteString("</")
		b.WriteString(key)
		b.WriteString(">\r\n")
	}
	b.WriteString("    <cid>228</cid>\r\n    <item cmd=\"1\"")
	writeXMLAttr(&b, "optime", strconv.FormatInt(item.Optime, 10))
	writeXMLAttr(&b, "nettype", "1030")
	writeXMLAttr(&b, "QQ", item.UserQQ)
	writeXMLAttr(&b, "uid", item.UID)
	writeXMLAttr(&b, "os", item.OSVersion)
	writeXMLAttr(&b, "model", item.Model)
	for _, kv := range [][2]string{
		{"version", "20.8.0.8"}, {"songtype", "1"}, {"playtype", "5"}, {"from", "8,"},
		{"openstore", "0"}, {"crytype", "2"}, {"paytype", "1"}, {"hijackflag", "1001"},
		{"desktoplyric", "0"}, {"playdevice", "0"}, {"playlist_mode", "0"}, {"outdev", "0"},
		{"url", "16"}, {"playmode", "1"}, {"repeat_times", "-1"}, {"string25", "free"},
		{"string26", "n"}, {"string29", "0"}, {"supersound", "5"}, {"cdn", ""}, {"cdnip", ""},
		{"hasFirstBuffer", "3"}, {"filetype", "4"}, {"err", "0"}, {"time2", "161"},
		{"issoftdecode", "1"}, {"component_type", "-1"}, {"wait_time", "325"},
		{"player_retry", "0"}, {"audiotime", strconv.FormatInt(item.AudioTimeMS, 10)},
		{"timekey", item.TimeKey}, {"co_singer", item.CoSinger}, {"vkey", item.VKey},
		{"time", strconv.Itoa(item.Duration)}, {"play_duration_mi", strconv.Itoa(item.Duration * 1000)},
		{"errcode", ""}, {"play_speed", "1.0"}, {"vip_level", "69632"}, {"audio_effect", "0:0"},
		{"string27", item.String27JSON}, {"songid", strconv.FormatInt(item.SongID, 10)},
		{"singerid", strconv.FormatInt(item.SingerID, 10)}, {"fversion", "0"}, {"buildver", "1"},
	} {
		writeXMLAttr(&b, kv[0], kv[1])
	}
	b.WriteString("/>\r\n</root>")
	return b.String(), nil
}

func writeXMLAttr(b *strings.Builder, key, value string) {
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteString("=\"")
	writeXMLEscaped(b, value, true)
	b.WriteByte('"')
}

func writeXMLEscaped(b *strings.Builder, value string, attr bool) {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	s := escaped.String()
	if attr {
		// xml.EscapeText 已覆盖 & < >，补掉属性边界字符。
		s = strings.ReplaceAll(s, "\"", "&quot;")
		s = strings.ReplaceAll(s, "'", "&apos;")
	}
	b.WriteString(s)
}
