package server

// SDK 形状契约层：把上游 CGI 原始响应归一化成 QQMusicApi Python SDK 模型的输出形状。
//
// 前端 ui/src 的 TS 接口（views.ts / api.ts / player.ts 等）全部按 Python SDK 的
// pydantic model_dump 结果编写；vendor/QQMusicApi 退役后，本层是这套形状的唯一契约源。
// 字段别名链以 SDK 模型的 validation_alias / jsonpath 为准（见 models/*.py）。
//
// 注意：歌曲 item 本身上游就是扁平 Song 结构（mid/name/title/subtitle/singer/album/
// file/interval...），SDK 亦原样透传 —— 所以列表只做「解包裹」，不动 item 内部。

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

type (
	obj = map[string]any
	arr = []any
)

// decodeObj 用 json.Number 解（大整数 tid 经 float64 会丢精度）。
func decodeObj(raw json.RawMessage) obj {
	d := obj{}
	if len(raw) == 0 {
		return d
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return obj{}
	}
	return d
}

func asObj(v any) obj {
	o, _ := v.(obj)
	return o
}

func asArr(v any) arr {
	a, _ := v.(arr)
	return a
}

func orArr(v any) arr {
	if a, ok := v.(arr); ok && a != nil {
		return a
	}
	return arr{}
}

func sval(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func nval(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		// 上游部分 id 以字符串下发（如搜索歌单的 dissid）
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0
		}
		return n
	case bool:
		if x {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func bval(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		return nval(x) != 0
	default:
		return false
	}
}

// firstOf 返回第一个存在且非 nil 的键值。
func firstOf(o obj, keys ...string) any {
	if o == nil {
		return nil
	}
	for _, k := range keys {
		if v, ok := o[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func strOf(o obj, keys ...string) string  { return sval(firstOf(o, keys...)) }
func numOf(o obj, keys ...string) int64   { return nval(firstOf(o, keys...)) }
func boolOf(o obj, keys ...string) bool   { return bval(firstOf(o, keys...)) }
func objOf(o obj, key string) obj         { return asObj(o[key]) }
func arrOf(o obj, keys ...string) arr {
	if o == nil {
		return arr{}
	}
	for _, k := range keys {
		if a, ok := o[k].(arr); ok && a != nil {
			return a
		}
	}
	return arr{}
}

// —— 歌单摘要（models/base.py SongList 的别名链）——
func songlistItem(m obj) obj {
	out := obj{
		"id":        numOf(m, "id", "tid", "dissid"),
		"dirid":     numOf(m, "dirid", "dirId"),
		"title":     strOf(m, "title", "dissname", "name", "dirName"),
		"picurl":    strOf(m, "picurl", "cover", "logo", "imgurl", "picUrl"),
		"desc":      strOf(m, "desc", "description"),
		"songnum":   numOf(m, "songnum", "songNum", "song_cnt"),
		"listennum": numOf(m, "listennum", "playCnt", "play_cnt"),
	}
	if v := firstOf(m, "nickname", "creator_nick"); v != nil {
		out["nickname"] = sval(v)
	}
	return out
}

// —— 歌单详情（GetSonglistDetailResponse：info←dirinfo，songs←songlist）——
func normalizeSonglistDetail(raw json.RawMessage) obj {
	d := decodeObj(raw)
	dirinfo := objOf(d, "dirinfo")
	info := songlistItem(dirinfo)
	if c := objOf(dirinfo, "creator"); c != nil {
		info["creator"] = obj{
			"musicid":     numOf(c, "musicid", "uIN", "uin"),
			"nick":        strOf(c, "nick", "name"),
			"headurl":     strOf(c, "headurl", "avatar"),
			"encrypt_uin": strOf(c, "encrypt_uin", "encryptUin"),
		}
	}
	return obj{
		"info":    info,
		"songs":   orArr(d["songlist"]),
		"hasmore": numOf(d, "hasmore"),
		"total":   numOf(d, "total_song_num", "total"),
	}
}

// —— 搜索（SearchByTypeResponse：song←item_song、singer←singer、album←item_album、
// songlist←item_songlist；total_num←meta.sum、nextpage←meta.nextpage）——
func normalizeSearch(raw json.RawMessage) obj {
	d := decodeObj(raw)
	body := objOf(d, "body")
	meta := objOf(d, "meta")

	singer := arr{}
	for _, it := range arrOf(body, "singer") {
		m := asObj(it)
		singer = append(singer, obj{
			"mid":      strOf(m, "singerMID", "singer_mid", "mid"),
			"name":     strOf(m, "singerName", "singer_name", "name"),
			"pic":      strOf(m, "singerPic", "singer_pic", "pic"),
			"song_num": numOf(m, "songNum", "song_num"),
		})
	}

	album := arr{}
	for _, it := range arrOf(body, "item_album") {
		m := asObj(it)
		album = append(album, obj{
			"mid":         strOf(m, "albummid", "albumMid", "mid"),
			"name":        strOf(m, "name", "albumName"),
			"pic":         strOf(m, "pic", "picUrl", "img"),
			"singer":      sval(firstOf(m, "singer", "singerName")),
			"time_public": strOf(m, "publish_date", "time_public", "publishDate"),
			"song_num":    numOf(m, "song_num", "songNum"),
		})
	}

	songlists := arr{}
	for _, it := range arrOf(body, "item_songlist") {
		m := asObj(it)
		item := songlistItem(m)
		item["id"] = numOf(m, "dissid", "id", "tid")
		item["listennum"] = numOf(m, "listennum")
		item["nickname"] = strOf(m, "nickname")
		songlists = append(songlists, item)
	}

	return obj{
		"song":      orArr(body["item_song"]),
		"singer":    singer,
		"album":     album,
		"songlist":  songlists,
		"total_num": numOf(meta, "sum", "total_num"),
		"nextpage":  numOf(meta, "nextpage"),
	}
}

// —— 专辑详情（GetAlbumDetailResponse：album←basicInfo，singers←singer.singerList）——
func normalizeAlbumDetail(raw json.RawMessage) obj {
	d := decodeObj(raw)
	basic := objOf(d, "basicInfo")
	album := obj{
		"id":          numOf(basic, "albumId", "id"),
		"mid":         strOf(basic, "albumMid", "mid"),
		"pmid":        strOf(basic, "pmid"),
		"name":        strOf(basic, "albumName", "name"),
		"subtitle":    strOf(basic, "subtitle"),
		"desc":        strOf(basic, "desc", "description"),
		"album_type":  strOf(basic, "albumType", "album_type"),
		"time_public": strOf(basic, "publishDate", "time_public"),
		"genre":       strOf(basic, "genre"),
		"language":    strOf(basic, "language"),
	}
	if s := firstOf(basic, "singer"); s != nil {
		album["singer"] = s
	}
	singers := arr{}
	if sl := objOf(d, "singer"); sl != nil {
		singers = orArr(sl["singerList"])
	}
	return obj{"album": album, "company": d["company"], "singers": singers}
}

// —— 专辑歌曲（GetAlbumSongResponse：song_list←songList[*].songInfo）——
func normalizeAlbumSongs(raw json.RawMessage) obj {
	d := decodeObj(raw)
	songs := arr{}
	for _, it := range arrOf(d, "songList") {
		if s := objOf(asObj(it), "songInfo"); s != nil {
			songs = append(songs, s)
		}
	}
	return obj{
		"album_mid": strOf(d, "albumMid", "album_mid"),
		"total_num": numOf(d, "totalNum", "total_num"),
		"song_list": songs,
	}
}

// —— 歌手主页（HomepageHeaderResponse：base_info←Info.BaseInfo）——
func normalizeHomepageBaseInfo(raw json.RawMessage) obj {
	d := decodeObj(raw)
	base := objOf(objOf(d, "Info"), "BaseInfo")
	return obj{"base_info": obj{
		"name":          strOf(base, "Name", "name"),
		"avatar":        strOf(base, "Avatar", "avatar"),
		"encrypted_uin": strOf(base, "EncryptedUin", "encrypted_uin"),
		"user_type":     numOf(base, "UserType", "user_type"),
		"is_singer":     numOf(base, "IsSinger", "is_singer") != 0,
	}}
}

// —— 歌手歌曲（SingerSongListResponse：song_list←songList[*].songInfo）——
func normalizeSingerSongs(raw json.RawMessage) obj {
	d := decodeObj(raw)
	songs := arr{}
	for _, it := range arrOf(d, "songList") {
		if s := objOf(asObj(it), "songInfo"); s != nil {
			songs = append(songs, s)
		}
	}
	return obj{
		"singer_mid": strOf(d, "singerMid", "singer_mid"),
		"total_num":  numOf(d, "totalNum", "total_num"),
		"song_list":  songs,
	}
}

// —— 歌手专辑（SingerAlbumListResponse：album_list←albumList）——
func normalizeSingerAlbums(raw json.RawMessage) obj {
	d := decodeObj(raw)
	return obj{
		"singer_mid": strOf(d, "singerMid", "singer_mid"),
		"total":      numOf(d, "total"),
		"album_list": orArr(d["albumList"]),
	}
}

// —— 推荐歌单（RecommendSonglistResponse：songlists←List[*].Playlist.basic，
// picurl←cover.default_url，creator_nick←creator.nick）——
func normalizeRecommendSonglists(raw json.RawMessage) obj {
	d := decodeObj(raw)
	out := arr{}
	for _, it := range arrOf(d, "List") {
		item := asObj(it)
		pl := objOf(item, "Playlist")
		basic := objOf(pl, "basic")
		cover := objOf(basic, "cover")
		creator := objOf(basic, "creator")
		out = append(out, obj{
			"id":           numOf(basic, "tid", "id"),
			"dirid":        numOf(basic, "dirid"),
			"title":        strOf(basic, "title", "dissname", "name"),
			"picurl":       strOf(cover, "default_url", "small_url", "large_url"),
			"desc":         strOf(basic, "desc", "description"),
			"songnum":      numOf(basic, "song_cnt", "songnum", "songNum"),
			"listennum":    numOf(basic, "play_cnt", "listennum"),
			"creator_nick": strOf(creator, "nick"),
		})
	}
	return obj{"songlists": out}
}

// —— 推荐新歌（RecommendNewSongResponse：songs←songlist）——
func normalizeNewsong(raw json.RawMessage) obj {
	d := decodeObj(raw)
	return obj{
		"lanlist": d["lanlist"],
		"lan":     d["lan"],
		"songs":   orArr(d["songlist"]),
		"ret_msg": d["ret_msg"],
		"type":    d["type"],
	}
}

// —— 用户歌单（UserCreatedSonglistResponse / UserFavSonglistResponse）——
func normalizeCreatedSonglists(raw json.RawMessage) obj {
	d := decodeObj(raw)
	playlists := arr{}
	for _, it := range arrOf(d, "v_playlist") {
		playlists = append(playlists, songlistItem(asObj(it)))
	}
	return obj{
		"total":      numOf(d, "total"),
		"playlists":  playlists,
		"hasmore":    boolOf(d, "hasmore"),
		"finished":   d["finished"],
		"deleted_ids": d["deleted_ids"],
	}
}

func normalizeFavSonglists(raw json.RawMessage) obj {
	d := decodeObj(raw)
	playlists := arr{}
	for _, it := range arrOf(d, "v_list") {
		playlists = append(playlists, songlistItem(asObj(it)))
	}
	return obj{
		"number":    numOf(d, "number"),
		"total":     numOf(d, "total"),
		"hasmore":   boolOf(d, "hasmore"),
		"hide":      d["hide"],
		"playlists": playlists,
	}
}

// —— 歌词（GetLyricResponse：songid←songID；lyric/trans/roma 解密）——
func normalizeLyric(raw json.RawMessage) obj {
	d := decodeObj(raw)
	out := obj{
		"songid":          numOf(d, "songID", "songid"),
		"lyric":           decodeLyricField(d["lyric"]),
		"trans":           decodeLyricField(d["trans"]),
		"roma":            decodeLyricField(d["roma"]),
		"singing_annotations_lyric": decodeLyricField(firstOf(d, "singingAnnotationsLyric")),
		"lrc_t":           numOf(d, "lrc_t"),
		"qrc_t":           numOf(d, "qrc_t"),
		"trans_t":         numOf(d, "trans_t"),
		"roma_t":          numOf(d, "roma_t"),
		"has_contributor": boolOf(d, "hasContributor"),
		"qrc":             d["qrc"],
	}
	return out
}

// decodeLyricField 解密歌词字段；解不开保留原值（与 SDK 的 suppress 语义一致）。
func decodeLyricField(v any) any {
	if s, ok := v.(string); ok && s != "" {
		if out := qqmusic.DecryptQRC(s); out != "" {
			return out
		}
	}
	return v
}
