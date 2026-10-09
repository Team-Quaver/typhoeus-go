package server

import (
	"encoding/json"
	"testing"
)

// vipFixture 与 ui/scripts/verify-user-vip.mjs 的抓包 FIXTURE 同源
// （2026-09-19 本机 sidecar :3200 /user/vip，snake_case 形态）。
const vipFixture = `{"auto_down":0,"can_renew":1,"max_dir_num":2000,"max_song_num":1000000,"song_limit_msg":"","svip":1,"star":0,"star_start":"","star_end":"","ystar":0,"ystar_start":"","ystar_end":"","identity":{"vip":1,"huge_vip":1,"huge_vip_start":"2026-07-25 18:40:17","huge_vip_end":"2026-09-25 18:40:17","year_flag":0,"huge_year_flag":0,"twelve":0,"twelve_start":"","twelve_end":"","child_vip":0,"exp_vip":0,"group_vip_flag":0,"group_vip_start":"","group_vip_end":"","cp_lover_flag":0,"cp_lover_start":"","cp_lover_end":"","ad_vip_flag":0,"eight":1,"eight_start":"2026-05-25","eight_end":"2026-09-26","level":6,"next_level":7,"icon":"http://y.gtimg.cn/mediastyle/global/vip_icon/lv_6.png","purchase_url":"http://y.qq.com/m/client/mall/myvip.html?"},"userinfo":{"buy_url":"https://y.qq.com/n2/m/myservice/index.html","my_vip_url":"https://y.qq.com/n2/m/myservice/index.html","score":20426,"expire":0,"music_level":10}}`

// TestNormalizeVipSnakeCase 抓包原形态（snake_case）必须逐字段保真：
// 归一化是「别名兜底」，不是「重新建模」——上游给什么形状都不能丢字段。
func TestNormalizeVipSnakeCase(t *testing.T) {
	var in json.RawMessage = []byte(vipFixture)
	out := normalizeVip(in)
	b, _ := json.Marshal(out)
	var src, dst map[string]any
	_ = json.Unmarshal([]byte(vipFixture), &src)
	_ = json.Unmarshal(b, &dst)

	// 顶层：SDK 模型字段一个不能少，值必须原样
	for _, k := range []string{"auto_down", "can_renew", "max_dir_num", "max_song_num",
		"song_limit_msg", "svip", "star", "star_start", "star_end", "ystar", "ystar_start", "ystar_end"} {
		if dst[k] != src[k] {
			t.Fatalf("顶层 %s: got %v want %v", k, dst[k], src[k])
		}
	}
	id := dst["identity"].(map[string]any)
	sid := src["identity"].(map[string]any)
	for _, k := range []string{"vip", "huge_vip", "huge_vip_start", "huge_vip_end", "year_flag",
		"huge_year_flag", "eight", "eight_start", "eight_end", "level", "next_level", "icon", "purchase_url"} {
		if id[k] != sid[k] {
			t.Fatalf("identity %s: got %v want %v", k, id[k], sid[k])
		}
	}
	ui := dst["userinfo"].(map[string]any)
	sui := src["userinfo"].(map[string]any)
	for _, k := range []string{"buy_url", "my_vip_url", "score", "expire", "music_level"} {
		if ui[k] != sui[k] {
			t.Fatalf("userinfo %s: got %v want %v", k, ui[k], sui[k])
		}
	}
}

// TestNormalizeVipAliases 上游换风格（驼峰/小写连写，QQMusicApi SDK alias 链所见）
// 时归一化成 SDK dump 形状 —— 前端按 snake_case 挑字段，别名不通就是静默空卡片。
func TestNormalizeVipAliases(t *testing.T) {
	raw := `{"canRenew":1,"maxDirNum":2000,"starstart":"","starend":"2026-12-31",` +
		`"svip":1,"identity":{"HugeVip":1,"HugeVipStart":"2026-07-25 18:40:17","HugeVipEnd":"2026-09-25 18:40:17",` +
		`"yearflag":0,"HugeYearFlag":1,"nextlevel":7,"purchaseUrl":"http://x"},` +
		`"userinfo":{"buyurl":"http://b","myvipurl":"http://m","music_level":10}}`
	out := normalizeVip(json.RawMessage(raw))
	b, _ := json.Marshal(out)
	var dst map[string]any
	_ = json.Unmarshal(b, &dst)

	want := map[string]any{
		"can_renew":   float64(1),
		"max_dir_num": float64(2000),
		"star_end":    "2026-12-31",
		"svip":        float64(1),
	}
	for k, v := range want {
		if dst[k] != v {
			t.Fatalf("顶层 %s: got %v want %v", k, dst[k], v)
		}
	}
	id := dst["identity"].(map[string]any)
	wantID := map[string]any{
		"huge_vip":       float64(1),
		"huge_vip_start": "2026-07-25 18:40:17",
		"huge_vip_end":   "2026-09-25 18:40:17",
		"year_flag":      float64(0),
		"huge_year_flag": float64(1),
		"next_level":     float64(7),
		"purchase_url":   "http://x",
	}
	for k, v := range wantID {
		if id[k] != v {
			t.Fatalf("identity %s: got %v want %v", k, id[k], v)
		}
	}
	ui := dst["userinfo"].(map[string]any)
	if ui["buy_url"] != "http://b" || ui["my_vip_url"] != "http://m" || ui["music_level"] != float64(10) {
		t.Fatalf("userinfo 别名未归一化: %v", ui)
	}
}

// TestNormalizeVipEmptyBlocks identity/userinfo 缺失时补空对象
// （pydantic default_factory 语义；前端 vip.ts 对空块降级，不能拿到 undefined 连环炸）。
func TestNormalizeVipEmptyBlocks(t *testing.T) {
	out := normalizeVip(json.RawMessage(`{"svip":0}`))
	b, _ := json.Marshal(out)
	if string(b) == `{"svip":0}` {
		t.Fatalf("嵌套块缺失时必须补 {}：got %s", b)
	}
	var dst map[string]any
	_ = json.Unmarshal(b, &dst)
	id, idOK := dst["identity"].(map[string]any)
	ui, uiOK := dst["userinfo"].(map[string]any)
	if !idOK || !uiOK || len(id) != 0 || len(ui) != 0 {
		t.Fatalf("identity/userinfo 应为空对象: %s", b)
	}
	if dst["svip"] != float64(0) {
		t.Fatalf("svip 丢失: %s", b)
	}
}

// TestNormalizeSingerAlbums 歌手专辑列表归一化：上游 GetAlbumList 每项是平铺驼峰
// （albumMid/albumName/pmid/albumType/publishDate，2026-09-29 实抓周杰伦 0025NhlN2yWrP4），
// 前端 AlbumBrief 读 snake_case——原样透传时专辑名恒为占位符「专辑」、卡片 href 缺 mid。
func TestNormalizeSingerAlbums(t *testing.T) {
	raw := json.RawMessage(`{
		"singerMid": "0025NhlN2yWrP4", "total": 42,
		"albumList": [
			{"albumID": 1458791, "albumMid": "003RMaRI1iFoYd", "albumName": "周杰伦的床边故事",
			 "albumTranName": "Jay Chou's Bedtime Stories", "albumType": "录音室专辑",
			 "pmid": "003RMaRI1iFoYd_1", "publishDate": "2016-06-24", "singerName": "周杰伦", "totalNum": 0}
		]}`)
	out := normalizeSingerAlbums(raw)
	list, ok := out["album_list"].(arr)
	if !ok || len(list) != 1 {
		t.Fatalf("album_list 应为长度 1 的数组: %v", out["album_list"])
	}
	al := list[0].(map[string]any)
	if al["mid"] != "003RMaRI1iFoYd" || al["name"] != "周杰伦的床边故事" || al["pmid"] != "003RMaRI1iFoYd_1" {
		t.Fatalf("专辑标识/名字未归一化: %v", al)
	}
	if al["album_type"] != "录音室专辑" || al["time_public"] != "2016-06-24" {
		t.Fatalf("副标题字段未归一化: %v", al)
	}
	if al["singer_name"] != "周杰伦" || al["total_num"] != int64(0) {
		t.Fatalf("附加字段未归一化: %v", al)
	}
	if out["total"] != int64(42) || out["singer_mid"] != "0025NhlN2yWrP4" {
		t.Fatalf("外层字段丢失: %v", out)
	}
}

// TestNormalizeAlbumSongsPreservesSourceOrder 专辑曲序由上游 songList 给出；
// 归一化只能解包 songInfo，不能按歌曲字段重新排序。
func TestNormalizeAlbumSongsPreservesSourceOrder(t *testing.T) {
	raw := json.RawMessage(`{
		"albumMid": "album-mid", "totalNum": 3,
		"songList": [
			{"songInfo": {"mid": "track-3", "name": "第三首"}},
			{"songInfo": {"mid": "track-1", "name": "第一首"}},
			{"songInfo": {"mid": "track-2", "name": "第二首"}}
		]}`)
	out := normalizeAlbumSongs(raw)
	songs, ok := out["song_list"].(arr)
	if !ok || len(songs) != 3 {
		t.Fatalf("song_list 应保留 3 首歌曲: %v", out["song_list"])
	}
	want := []string{"track-3", "track-1", "track-2"}
	for i, v := range songs {
		got := strOf(asObj(v), "mid")
		if got != want[i] {
			t.Fatalf("曲序被改写: index=%d got=%q want=%q", i, got, want[i])
		}
	}
}
