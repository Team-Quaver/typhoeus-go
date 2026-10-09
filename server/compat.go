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
	"sort"
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
	type albumSong struct {
		song     obj
		index    int64
		hasIndex bool
	}
	items := []albumSong{}
	for _, it := range arrOf(d, "songList") {
		if s := objOf(asObj(it), "songInfo"); s != nil {
			indexValue := firstOf(s, "index_album", "indexAlbum")
			items = append(items, albumSong{
				song:     s,
				index:    nval(indexValue),
				hasIndex: indexValue != nil,
			})
		}
	}
	// AlbumSongList 的返回数组不是曲序（例如会把第 2 首放在第 1 首前），
	// 但每个 songInfo 都带有 index_album。按这个上游曲序字段恢复专辑原序；
	// 没有曲序字段的旧响应则保持接口原顺序。
	if len(items) > 0 {
		hasIndexes := false
		for _, item := range items {
			if item.hasIndex {
				hasIndexes = true
				break
			}
		}
		if hasIndexes {
			sort.SliceStable(items, func(i, j int) bool {
				if items[i].hasIndex != items[j].hasIndex {
					return items[i].hasIndex
				}
				return items[i].index < items[j].index
			})
		}
	}
	songs := arr{}
	for _, item := range items {
		songs = append(songs, item.song)
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

// —— 歌手专辑（GetAlbumListResponse：album_list←albumList[*] 平铺驼峰）——
// 上游每项直接是 albumMid/albumName/pmid/albumType/publishDate（驼峰平铺，
// 不是专辑详情那种 basicInfo 包一层）。此前原样透传，前端读 snake_case 全空：
// 专辑名恒为占位符「专辑」、点进详情缺 mid。逐项归一化成前端 AlbumBrief 形状。
func normalizeSingerAlbums(raw json.RawMessage) obj {
	d := decodeObj(raw)
	list := arr{}
	for _, it := range arrOf(d, "albumList") {
		al := asObj(it)
		list = append(list, obj{
			"mid":         strOf(al, "albumMid", "mid"),
			"pmid":        strOf(al, "pmid"),
			"name":        strOf(al, "albumName", "name"),
			"tran_name":   strOf(al, "albumTranName"),
			"album_type":  strOf(al, "albumType", "album_type"),
			"time_public": strOf(al, "publishDate", "time_public"),
			"singer_name": strOf(al, "singerName"),
			"total_num":   numOf(al, "totalNum"),
		})
	}
	return obj{
		"singer_mid": strOf(d, "singerMid", "singer_mid"),
		"total":      numOf(d, "total"),
		"album_list": list,
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

// —— VIP 信息（UserVipInfoResponse：identity / userinfo 两层嵌套）——
//
// 上游这条 CGI 的字段命名风格不稳定：同一份语义在不同账号/版本下既有 snake_case
// （huge_vip_end，2026-09-19 抓包实测）、也有驼峰/小写连写（HugeVipEnd、starend、
// canRenew —— QQMusicApi Python SDK 的 validation_alias 链即为此而设）。
// Py 后端当年靠 pydantic 模型 dump 成稳定形状；这里沿用 SDK 的别名链做归一化，
// 前端 ui/src/lib/vip.ts 按 snake_case 挑字段，名字错了不会报错、只会静默少一行。
//
// 与其他 normalize 一样按 SDK dump 形状重建输出：别名链按序取第一个**存在**的键，
// 值原样搬运（时间串/数字/URL 都不做类型改写）；嵌套块缺失时补空对象（对应
// pydantic 的 default_factory），未知键按 SDK 形状丢弃。
func normalizeVip(raw json.RawMessage) obj {
	d := decodeObj(raw)
	out := obj{}

	// 顶层（UserVipInfoResponse）：svip/star 等无别名的字段直接保留
	for _, k := range []string{"svip", "star", "ystar", "identity", "userinfo"} {
		if v, ok := d[k]; ok && v != nil {
			out[k] = v
		}
	}
	pick(&out, d,
		[]string{"auto_down", "auto_down", "autoDown", "autodown"},
		[]string{"can_renew", "can_renew", "canRenew"},
		[]string{"max_dir_num", "max_dir_num", "maxDirNum", "maxdirnum"},
		[]string{"max_song_num", "max_song_num", "maxSongNum", "maxsongnum"},
		[]string{"song_limit_msg", "song_limit_msg", "songLimitMsg"},
		[]string{"star_start", "star_start", "starstart"},
		[]string{"star_end", "star_end", "starend"},
		[]string{"ystar_start", "ystar_start", "ystarstart"},
		[]string{"ystar_end", "ystar_end", "ystarend"},
	)

	// identity（VipIdentity）：会员身份明细（徽章档位 + 等级 + 协议档位）。
	// huge_vip 系/year_flag 没有别名歧义，走下方别名链（小写优先）统一搬运。
	id := objOf(d, "identity")
	nid := obj{}
	for _, k := range []string{"vip", "twelve", "eight", "level", "icon"} {
		if v, ok := id[k]; ok && v != nil {
			nid[k] = v
		}
	}
	pick(&nid, id,
		[]string{"huge_vip", "huge_vip", "HugeVip"},
		[]string{"huge_vip_start", "huge_vip_start", "HugeVipStart"},
		[]string{"huge_vip_end", "huge_vip_end", "HugeVipEnd"},
		[]string{"huge_year_flag", "huge_year_flag", "HugeYearFlag"},
		[]string{"year_flag", "year_flag", "yearflag"},
		[]string{"twelve_start", "twelve_start", "twelveStart"},
		[]string{"twelve_end", "twelve_end", "twelveEnd"},
		[]string{"child_vip", "child_vip", "ChildVip"},
		[]string{"exp_vip", "exp_vip", "ExpVip"},
		[]string{"group_vip_flag", "group_vip_flag", "GroupVipFlag"},
		[]string{"group_vip_start", "group_vip_start", "GroupVipStart"},
		[]string{"group_vip_end", "group_vip_end", "GroupVipEnd"},
		[]string{"cp_lover_flag", "cp_lover_flag", "CPLoverFlag"},
		[]string{"cp_lover_start", "cp_lover_start", "CPLoverStart"},
		[]string{"cp_lover_end", "cp_lover_end", "CPLoverEnd"},
		[]string{"ad_vip_flag", "ad_vip_flag", "AdVipFlag"},
		[]string{"eight_start", "eight_start", "eightStart"},
		[]string{"eight_end", "eight_end", "eightEnd"},
		[]string{"next_level", "next_level", "nextlevel"},
		[]string{"purchase_url", "purchase_url", "purchaseUrl"},
	)
	out["identity"] = nid

	// userinfo（VipUserInfo）：权益摘要（expire 是前端「会员有效至」的兜底字段）
	ui := objOf(d, "userinfo")
	nui := obj{}
	for _, k := range []string{"score", "expire"} {
		if v, ok := ui[k]; ok && v != nil {
			nui[k] = v
		}
	}
	pick(&nui, ui,
		[]string{"buy_url", "buy_url", "buyurl"},
		[]string{"my_vip_url", "my_vip_url", "myvipurl"},
		[]string{"music_level", "music_level"},
	)
	out["userinfo"] = nui

	return out
}

// pick 按 (输出名, 别名链…) 组从 src 搬运第一个存在的键值到 dst。
func pick(dst *obj, src obj, groups ...[]string) {
	for _, g := range groups {
		if v := firstOf(src, g[1:]...); v != nil {
			(*dst)[g[0]] = v
		}
	}
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
