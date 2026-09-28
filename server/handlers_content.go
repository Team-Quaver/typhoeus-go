package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
	"github.com/team-quaver/typhoeus-go/typhoeus"
)

// ===================== 用户 =====================

func (a *App) handleUserMe(w http.ResponseWriter, _ *http.Request) {
	// 当前账号主页信息（昵称/头像/加密 UIN/音乐人标志）。
	// 透传原始 GetHomepageHeader 响应并挑出所需字段，不改 SDK 层。
	euin, err := a.requireEuin()
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.user.GetHomepageHeader(euin)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	var payload struct {
		Info struct {
			BaseInfo struct {
				Name         string `json:"Name"`
				Avatar       string `json:"Avatar"`
				EncryptedUin string `json:"EncryptedUin"`
				UserType     int    `json:"UserType"`
				IsSinger     int    `json:"IsSinger"`
			} `json:"BaseInfo"`
		} `json:"Info"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, qqmusic.NewDataError("主页信息解析失败"))
		return
	}
	b := payload.Info.BaseInfo
	writeOK(w, map[string]any{
		"base_info": map[string]any{
			"name":          b.Name,
			"avatar":        b.Avatar,
			"encrypted_uin": b.EncryptedUin,
			"user_type":     b.UserType,
			"is_singer":     b.IsSinger != 0,
		},
	})
}

func (a *App) handleUserVip(w http.ResponseWriter, _ *http.Request) {
	raw, err := a.call(true, a.user.GetVipInfo)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleUserLiked(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 30)
	euin, err := a.requireEuin()
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := a.call(true, func() (json.RawMessage, error) {
		return a.user.GetFavSong(euin, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, normalizeSonglistDetail(raw))
}

func (a *App) handleUserCreatedSonglists(w http.ResponseWriter, _ *http.Request) {
	cred, err := a.session.Require()
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.user.GetCreatedSonglist(fmt.Sprintf("%d", cred.MusicID))
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, normalizeCreatedSonglists(raw))
}

func (a *App) handleUserFavSonglists(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 20)
	euin, err := a.requireEuin()
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.user.GetFavSonglist(euin, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, normalizeFavSonglists(raw))
}

// ===================== 歌曲 =====================

// songURLItem /song/urls 请求体里的单个文件描述。
type songURLItem struct {
	Mid      string `json:"mid"`
	FileType *int   `json:"file_type"` // 未显式给出必须传 nil——否则批量档位会被静默覆盖
	MediaMid string `json:"media_mid"`
	SongType *int   `json:"song_type"`
}

type songUrlsBody struct {
	FileInfo []songURLItem `json:"file_info"`
	FileType int           `json:"file_type"` // 128mp3（上游 web 层 SONG_FILE_TYPE_MAPPING 顺序值）
}

// songFileTypes file_type 整数 → SDK 枚举（与上游 web/src/modules/song.py 的
// SONG_FILE_TYPES 顺序一致：先明文系，再加密系；默认 13 = MP3_128）。
var songFileTypes = []modules.SongFileType{
	modules.TypeDTSX, modules.TypeMaster, modules.TypeAtmos2, modules.TypeAtmos51,
	modules.TypeAtmos71, modules.TypeAtmosDB, modules.TypeNAC, modules.TypeFLAC,
	modules.TypeOGG640, modules.TypeOGG320, modules.TypeOGG192, modules.TypeOGG96,
	modules.TypeMP3320, modules.TypeMP3128, modules.TypeACC192, modules.TypeACC96,
	modules.TypeACC48,
	modules.EncDTSX, modules.EncVinyl, modules.EncMaster, modules.EncAtmos2,
	modules.EncAtmos51, modules.EncAtmos71, modules.EncAtmosDB, modules.EncNAC,
	modules.EncFLAC, modules.EncOGG640, modules.EncOGG320, modules.EncOGG192,
	modules.EncOGG96,
}

func songFileTypeOf(code *int) (modules.SongFileType, error) {
	if code == nil {
		return modules.TypeMP3128, nil
	}
	if *code < 0 || *code >= len(songFileTypes) {
		return modules.SongFileType{}, &APIErrorLocal{Status: 422,
			Message: fmt.Sprintf("未知 file_type: %d", *code)}
	}
	return songFileTypes[*code], nil
}

// APIErrorLocal 本地参数错误（直接映射 HTTP 状态）。
type APIErrorLocal struct {
	Status  int
	Message string
}

func (e *APIErrorLocal) Error() string { return e.Message }

// serializeSongUrls 统一拼 CDN 域名并输出 items。
func (a *App) serializeSongUrls(raw json.RawMessage) (map[string]any, error) {
	var resp struct {
		Expiration int64 `json:"expiration"`
		Data       []struct {
			Songmid  string `json:"songmid"`
			Filename string `json:"filename"`
			Purl     string `json:"purl"`
			Vkey     string `json:"vkey"`
			Ekey     string `json:"ekey"`
			Result   int64  `json:"result"`
		} `json:"midurlinfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, qqmusic.NewDataError("取链响应解析失败")
	}
	domain, err := a.provider.CDNDomain()
	if err != nil {
		domain = typhoeus.CDNFallback
	}
	items := make([]map[string]any, 0, len(resp.Data))
	for _, it := range resp.Data {
		url := ""
		if it.Result == 0 && it.Purl != "" {
			url = domain + it.Purl
		}
		items = append(items, map[string]any{
			"mid":      it.Songmid,
			"filename": it.Filename,
			"purl":     it.Purl,
			"vkey":     it.Vkey,
			// ekey 只在加密链路有意义；明文链路不透传（ Typhoeus 语义：解密钥匙不出后端）
			"ekey":   ekeyForClient(it.Ekey, it.Result),
			"result": it.Result,
			"url":    url,
		})
	}
	return map[string]any{"expiration": resp.Expiration, "items": items}, nil
}

// ekeyForClient /song/urls 是通用取链端点：加密档的 ekey 需要透传给解密播放链路。
// 这里保留原始值（仅本服务的调用方可见，不出进程边界）。
func ekeyForClient(ekey string, result int64) string {
	if result != 0 {
		return ""
	}
	return ekey
}

func (a *App) handleSongUrls(w http.ResponseWriter, r *http.Request) {
	var body songUrlsBody
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.FileInfo) == 0 {
		writeErr(w, 422, -1, "file_info 不能为空")
		return
	}
	ft, err := songFileTypeOf(&body.FileType)
	if err != nil {
		writeError(w, err)
		return
	}
	files := make([]modules.SongFileInfo, 0, len(body.FileInfo))
	for _, item := range body.FileInfo {
		itemFT := ft
		if item.FileType != nil {
			itemFT, err = songFileTypeOf(item.FileType)
			if err != nil {
				writeError(w, err)
				return
			}
		}
		files = append(files, modules.SongFileInfo{
			Mid:      item.Mid,
			FileType: &itemFT,
			MediaMid: item.MediaMid,
		})
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.song.GetSongURLs(files, ft, qqmusic.CGIOption{})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := a.serializeSongUrls(raw)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, payload)
}

func (a *App) handleSongDetail(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("value")
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.song.GetDetail(value)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSongURL(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("value")
	ftCode := queryInt(r, "file_type", 13)
	ft, err := songFileTypeOf(&ftCode)
	if err != nil {
		writeError(w, err)
		return
	}
	files := []modules.SongFileInfo{{Mid: value, MediaMid: r.URL.Query().Get("media_mid"), FileType: &ft}}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.song.GetSongURLs(files, ft, qqmusic.CGIOption{})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := a.serializeSongUrls(raw)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, payload)
}

func (a *App) handleSongLyric(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("value")
	trans := queryBool(r, "trans", false)
	roma := queryBool(r, "roma", false)
	qrc := queryBool(r, "qrc", false)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.lyric.GetLyric(value, 1, qrc, trans, roma)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, normalizeLyric(raw))
}

func (a *App) handleSongComments(w http.ResponseWriter, r *http.Request) {
	songID := queryInt64(r, "song_id", 0)
	if v := r.PathValue("song_id"); v != "" {
		var n int64
		fmt.Sscanf(v, "%d", &n)
		songID = n
	}
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 20)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.comment.GetHotComments(songID, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

// ---- 歌曲 / 歌单收藏（红心）----

type songLikeBody struct {
	SongID   int64 `json:"song_id"`
	SongType int64 `json:"song_type"` // 写侧枚举（读侧 Song.type - 1）
}

func (a *App) handleSongLike(w http.ResponseWriter, r *http.Request) {
	var body songLikeBody
	if !decodeBody(w, r, &body) {
		return
	}
	ok, err := a.call2(func() (bool, error) { return a.likeUnlike("like", body) })
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}

func (a *App) handleSongUnlike(w http.ResponseWriter, r *http.Request) {
	var body songLikeBody
	if !decodeBody(w, r, &body) {
		return
	}
	ok, err := a.likeUnlike("unlike", body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}

func (a *App) likeUnlike(action string, body songLikeBody) (bool, error) {
	refs := []modules.SongRef{{SongID: body.SongID, SongType: body.SongType}}
	if action == "like" {
		return a.songlist.LikeSong(refs)
	}
	return a.songlist.UnlikeSong(refs)
}

// ===================== 歌单 =====================

func (a *App) handleSonglistDetail(w http.ResponseWriter, r *http.Request) {
	var id int64
	fmt.Sscanf(r.PathValue("songlist_id"), "%d", &id)
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 100)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.songlist.GetDetail(id, 0, int64(page), int64(num), false)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, normalizeSonglistDetail(raw))
}

func (a *App) handleSonglistLike(w http.ResponseWriter, r *http.Request) {
	var id int64
	fmt.Sscanf(r.PathValue("songlist_id"), "%d", &id)
	ok, err := a.call2(func() (bool, error) { return a.user.FavSonglist(id) })
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}

func (a *App) handleSonglistUnlike(w http.ResponseWriter, r *http.Request) {
	var id int64
	fmt.Sscanf(r.PathValue("songlist_id"), "%d", &id)
	ok, err := a.call2(func() (bool, error) { return a.user.UnfavSonglist(id) })
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}

// call2 带登录守卫/刷新重试的 bool 版调用。
func (a *App) call2(fn func() (bool, error)) (bool, error) {
	if _, err := a.session.Require(); err != nil {
		return false, err
	}
	if a.session.Credential().IsExpired() {
		a.tryRefresh()
	}
	ok, err := fn()
	if err != nil {
		e := qqmusic.AsAPIError(err)
		if (e.Kind == qqmusic.ErrKindCredentialExpired || e.Kind == qqmusic.ErrKindCredentialInvalid) &&
			a.session.LoggedIn() && a.tryRefresh() {
			return fn()
		}
	}
	return ok, err
}

func (a *App) handleSonglistFavCheck(w http.ResponseWriter, _ *http.Request) {
	// 已收藏的他人公开歌单 ID 集合（收藏态徽章数据源）
	euin, err := a.requireEuin()
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.user.GetFavSonglist(euin, 1, 100)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	var resp struct {
		Playlists []struct {
			ID int64 `json:"id"`
		} `json:"playlists"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeError(w, qqmusic.NewDataError("收藏歌单解析失败"))
		return
	}
	ids := make([]int64, 0, len(resp.Playlists))
	for _, p := range resp.Playlists {
		ids = append(ids, p.ID)
	}
	writeOK(w, map[string]any{"ids": ids})
}

type songlistSongBody struct {
	SongID   int64 `json:"song_id"`
	SongType int64 `json:"song_type"` // 写侧枚举；发读侧原值会静默空操作
	Tid      int64 `json:"tid"`       // 歌单 tid（自建歌单知道就传）
}

func (a *App) handleSonglistAddSong(w http.ResponseWriter, r *http.Request) {
	var body songlistSongBody
	if !decodeBody(w, r, &body) {
		return
	}
	var dirid int64
	fmt.Sscanf(r.PathValue("dirid"), "%d", &dirid)
	ok, err := a.call2(func() (bool, error) {
		return a.songlist.AddSongs(dirid, body.Tid,
			[]modules.SongRef{{SongID: body.SongID, SongType: body.SongType}})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}

func (a *App) handleSonglistDelSong(w http.ResponseWriter, r *http.Request) {
	var body songlistSongBody
	if !decodeBody(w, r, &body) {
		return
	}
	var dirid int64
	fmt.Sscanf(r.PathValue("dirid"), "%d", &dirid)
	ok, err := a.call2(func() (bool, error) {
		return a.songlist.DelSongs(dirid, body.Tid,
			[]modules.SongRef{{SongID: body.SongID, SongType: body.SongType}})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"ok": ok})
}
