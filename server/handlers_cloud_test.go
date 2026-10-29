package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudDecodeName(t *testing.T) {
	padded := base64.StdEncoding.EncodeToString([]byte("测试 歌曲.flac"))
	raw := base64.RawStdEncoding.EncodeToString([]byte("无填充.mp3"))

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"标准 base64", padded, "测试 歌曲.flac"},
		{"无填充 base64", raw, "无填充.mp3"},
		{"明文文件名原样", "plain name.mp3", "plain name.mp3"},
		{"非字符串为空", nil, ""},
	}

	for _, tc := range cases {
		if got := cloudDecodeName(tc.in); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCloudAddTime(t *testing.T) {
	if got := cloudAddTime(json.Number("1700000000")); got != 1700000000000 {
		t.Fatalf("秒应转毫秒: %d", got)
	}
	if got := cloudAddTime(json.Number("1700000000123")); got != 1700000000123 {
		t.Fatalf("毫秒应原样: %d", got)
	}
	if got := cloudAddTime(nil); got != 0 {
		t.Fatalf("空值应为 0: %d", got)
	}
}

func TestCloudMatchedAndPlayable(t *testing.T) {
	if cloudMatched(json.Number("2")) != cloudMatchedOriginalFile {
		t.Fatal("matched=2 应识别为原文件")
	}
	if !cloudPlayable("file-original", cloudMatchedOriginalFile) {
		t.Fatal("matched=2 且有 file_id 时应可播放原文件")
	}
	if cloudPlayable("", cloudMatchedOriginalFile) {
		t.Fatal("缺少 file_id 时不可播放")
	}
}

func TestCloudPlaybackStateRoute(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", dir)
	key := cloudKeyOf("10001", "file-playback")
	app := &App{cloudIndex: &cloudIndex{Version: 1, Items: map[string]*cloudMeta{}, Locations: map[string]cloudLocation{}}}

	body := `{"key":"` + key + `","state":"blocked","error":"曲库拒绝 result=104003"}`
	rec := httptest.NewRecorder()
	app.handleCloudPlayback(rec, httptest.NewRequest(http.MethodPost, "/cloud/playback", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	meta := app.cloudIndex.Items[key]
	if meta == nil || meta.PlaybackState != "blocked" || meta.PlaybackError != "曲库拒绝 result=104003" {
		t.Fatalf("播放状态未写入: %+v", meta)
	}

	bad := httptest.NewRecorder()
	app.handleCloudPlayback(bad, httptest.NewRequest(http.MethodPost, "/cloud/playback",
		strings.NewReader(`{"key":"`+key+`","state":"playing"}`)))
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法状态应拒绝，got %d", bad.Code)
	}
}

func TestCloudMetaSetPreservesExistingFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", dir)
	key := cloudKeyOf("10001", "file-meta")
	app := &App{cloudIndex: &cloudIndex{
		Version: 1, Locations: map[string]cloudLocation{},
		Items: map[string]*cloudMeta{
			key: {Key: key, Title: "Old", Artist: "Old Artist", Album: "Old Album"},
		},
	}}

	body := `{"key":"` + key + `","title":"New","artist":"","album":"New Album"}`
	rec := httptest.NewRecorder()
	app.handleCloudMetaSet(rec, httptest.NewRequest(http.MethodPost, "/cloud/meta/set", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	meta := app.cloudIndex.Items[key]
	if meta.Title != "New" || meta.Artist != "Old Artist" || meta.Album != "New Album" || !meta.EditedMeta {
		t.Fatalf("meta update = %+v", meta)
	}
}

func TestCloudLocationCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", dir)
	app := &App{cloudIndex: &cloudIndex{
		Version: 1, Items: map[string]*cloudMeta{}, Locations: map[string]cloudLocation{},
	}}

	app.rememberCloudLocation("file-location", cloudLocation{PdirKey: "pdir-location", FileName: "song[aq].6f6767"})
	if got, ok := app.cloudLocationOf("file-location"); !ok || got.PdirKey != "pdir-location" || got.FileName != "song[aq].6f6767" {
		t.Fatalf("location cache = %+v, %v", got, ok)
	}

	reloaded := &App{}
	if got, ok := reloaded.cloudLocationOf("file-location"); !ok || got.PdirKey != "pdir-location" || got.FileName != "song[aq].6f6767" {
		t.Fatalf("persisted location cache = %+v, %v", got, ok)
	}
}

func TestCloudDisplayMetaFallsBackToSongAndFileName(t *testing.T) {
	entry := cloudEntry{
		Key:      "a",
		FileName: "fallback.flac",
		Song: json.RawMessage(`{
			"mid":"M1","title":"在线标题",
			"singer":[{"name":"甲","mid":"S1"}],
			"album":{"name":"在线专辑","mid":"A1","pmid":"A1_1"}
		}`),
	}
	meta := cloudDisplayMeta(entry)

	if meta.Title != "在线标题" || meta.Artist != "甲" || meta.Album != "在线专辑" {
		t.Fatalf("歌曲信息回填失败: %+v", meta)
	}
	if meta.SingerMid != "S1" || meta.AlbumMid != "A1" || meta.AlbumPmid != "A1_1" {
		t.Fatalf("mid 回填失败: %+v", meta)
	}

	meta = cloudDisplayMeta(cloudEntry{Key: "b", FileName: "裸文件名.mp3"})
	if meta.Title != "裸文件名" {
		t.Fatalf("缺歌曲信息时应从文件名兜底: %q", meta.Title)
	}

	meta = cloudDisplayMeta(cloudEntry{Key: "c", FileName: "未匹配.mp3"})
	if meta.PlaybackState != "blocked" || meta.PlaybackError == "" {
		t.Fatalf("不可播放条目应有明确 blocked 状态: %+v", meta)
	}

	meta = cloudDisplayMeta(cloudEntry{Key: "d", FileName: "匹配.mp3", Playable: true})
	if meta.PlaybackState != "matched" {
		t.Fatalf("可播放条目默认应为 matched: %+v", meta)
	}
}

func TestCloudImageExtAndBase64(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	if got := cloudImageExt(jpeg); got != "jpg" {
		t.Fatalf("jpeg: %q", got)
	}
	if got := cloudImageExt([]byte("not image")); got != "" {
		t.Fatalf("unknown: %q", got)
	}

	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)
	buf, err := decodeCloudBase64(dataURL)
	if err != nil || len(buf) != len(jpeg) {
		t.Fatalf("data URL 解码失败: %v len=%d", err, len(buf))
	}
}

func TestSniffCloudLyricFormat(t *testing.T) {
	cases := []struct{ text, name, want string }{
		{`<tt xmlns="http://www.w3.org/ns/ttml"></tt>`, "x", "ttml"},
		{`[0,900]<w d="450">词</w>`, "x", "qrc"},
		{`[00:01.00]普通行`, "x.lrc", "lrc"},
		{`任意内容`, "x.qrc", "qrc"},
	}

	for _, tc := range cases {
		if got := sniffCloudLyricFormat(tc.text, tc.name); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.text, got, tc.want)
		}
	}
}

func TestCloudKeyOf(t *testing.T) {
	key := cloudKeyOf("10001", "file-1")
	if len(key) != 40 {
		t.Fatalf("key 应为 40 位 hex: %q", key)
	}
	if key != cloudKeyOf("10001", "file-1") || key == cloudKeyOf("10001", "file-2") {
		t.Fatalf("key 应稳定且区分文件")
	}
	if key == cloudKeyOf("10002", "file-1") {
		t.Fatalf("key 应区分账号")
	}
	if !cloudKeyRe.MatchString(key) {
		t.Fatalf("key 未通过白名单: %q", key)
	}
}

func TestCloudParseFileName(t *testing.T) {
	cases := []struct{ name, title, artist string }{
		{"01. 周杰伦 - 稻香.flac", "稻香", "周杰伦"},
		{"Aimer – 残響散歌.mp3", "残響散歌", "Aimer"},
		{"无分隔符.flac", "无分隔符", ""},
	}

	for _, tc := range cases {
		title, artist := cloudParseFileName(tc.name)

		if title != tc.title || artist != tc.artist {
			t.Fatalf("%s: got (%q,%q) want (%q,%q)", tc.name, title, artist, tc.title, tc.artist)
		}
	}
}

func TestCloudResourceRoutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", dir)

	key := cloudKeyOf("10001", "file-route")
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3, 4}
	lyric := "[00:01.00]导入歌词\n"
	app := &App{
		cloudIndex: &cloudIndex{
			Version: 1, Locations: map[string]cloudLocation{},
			Items: map[string]*cloudMeta{
				key: {Key: key, LyricImport: true, ImportFormat: "lrc", CoverManual: true},
			},
		},
	}
	if err := ensureCloudDirs(); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudFile(filepath.Join(cloudLyricsDir(), key+".import"), []byte(lyric), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudFile(filepath.Join(cloudCoversDir(), key+".jpg"), jpeg, 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/cloud/lyric/"+key, nil)
	req.SetPathValue("key", key)
	rec := httptest.NewRecorder()
	app.handleCloudLyric(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lyric status = %d body=%s", rec.Code, rec.Body.String())
	}
	var lyricOut struct {
		Data struct {
			Lyric  string `json:"lyric"`
			Source string `json:"source"`
			Format string `json:"format"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lyricOut); err != nil {
		t.Fatal(err)
	}
	if lyricOut.Data.Lyric != lyric || lyricOut.Data.Source != "import" || lyricOut.Data.Format != "lrc" {
		t.Fatalf("unexpected lyric: %+v", lyricOut.Data)
	}

	req = httptest.NewRequest(http.MethodGet, "/cloud/cover/"+key, nil)
	req.SetPathValue("key", key)
	rec = httptest.NewRecorder()
	app.handleCloudCover(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != len(jpeg) || rec.Header().Get("Content-Type") == "" {
		t.Fatalf("cover status=%d len=%d ct=%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}

	req = httptest.NewRequest(http.MethodGet, "/cloud/lyric/not-a-key", nil)
	req.SetPathValue("key", "not-a-key")
	rec = httptest.NewRecorder()
	app.handleCloudLyric(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非法 key 应 404: %d", rec.Code)
	}
}

func TestCloudEntriesFromUpstreamShape(t *testing.T) {
	// 直接构造上游 data 的等价 decode 路径，验证 base64 文件名/matched/播放判定。
	fileID := "private-file-1"
	filename := base64.StdEncoding.EncodeToString([]byte("测试文件.flac"))
	raw := json.RawMessage(`{
		"info_list":[{
			"file_id":"` + fileID + `",
			"filename":"` + filename + `",
			"matched":1,
			"timestamp":1700000000,
			"song_info":{"mid":"M1","name":"云盘曲"}
		}],
		"has_more":0,
		"total":1
	}`)
	// cloudEntries 需要真实 client；这里把 decode 后的字段检查拆到同一组纯函数上。
	m := asObj(arrOf(decodeObj(raw), "info_list")[0])
	if got := cloudDecodeName(m["filename"]); got != "测试文件.flac" {
		t.Fatalf("filename = %q", got)
	}
	if got := cloudAddTime(m["timestamp"]); got != 1700000000000 {
		t.Fatalf("addTime = %d", got)
	}
	songRaw, _ := json.Marshal(m["song_info"])
	if mid := strOf(cloudSongObj(songRaw), "mid"); mid != "M1" {
		t.Fatalf("mid = %q", mid)
	}
	if mid := strOf(cloudSongObj(songRaw), "mid"); mid == "" || !cloudPlayable(fileID, cloudMatched(m["matched"])) {
		t.Fatal("有 file_id 的条目应判可播放")
	}
}
