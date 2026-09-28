package modules

import (
	"encoding/json"
	"strconv"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// LyricModule 歌词域接口。
type LyricModule struct{ cl *qqmusic.Client }

// NewLyricModule 构造。
func NewLyricModule(cl *qqmusic.Client) *LyricModule { return &LyricModule{cl: cl} }

// GetLyric 歌词原始数据（crypt=1 的密文 / 明文按上游返回）。
// qrc=逐字歌词；trans=翻译；roma=罗马音。needSingingAnnotations 保持布尔原样。
func (m *LyricModule) GetLyric(value string, songType int, qrc, trans, roma bool) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("crypt", 1).
		Set("lrc_t", 0).
		Set("qrc", bool2i(qrc)).
		Set("qrc_t", 0).
		Set("roma", bool2i(roma)).
		Set("roma_t", 0).
		Set("trans", bool2i(trans)).
		Set("trans_t", 0).
		Set("needSingingAnnotations", false).
		Set("type", songType)
	if isDigits(value) {
		param.Set("songId", json.Number(value))
	} else {
		param.Set("songMid", value)
	}
	data, err := m.cl.CgiCall("music.musichallSong.PlayLyricInfo", "GetPlayLyricInfo", param,
		qqmusic.CGIOption{PreserveBool: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

func bool2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SearchModule 搜索域接口。
type SearchModule struct{ cl *qqmusic.Client }

// NewSearchModule 构造。
func NewSearchModule(cl *qqmusic.Client) *SearchModule { return &SearchModule{cl: cl} }

// GetHotkey 热搜词。
func (m *SearchModule) GetHotkey() (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("search_id", randomSearchID())
	data, err := m.cl.CgiCall("music.musicsearch.HotkeyService", "GetHotkeyForQQMusicMobile", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// Complete 搜索词补全建议。
func (m *SearchModule) Complete(keyword string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("search_id", randomSearchID()).
		Set("query", keyword).
		Set("num_per_page", 0).
		Set("page_idx", 0)
	data, err := m.cl.CgiCall("music.smartboxCgi.SmartBoxCgi", "GetSmartBoxResult", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// ByType 类型搜索（固定 Android 平台）。
// search_type: 0歌曲 1歌手 2专辑 3歌单 4MV 7歌词 8用户 10彩铃 15节目专辑 18节目。
func (m *SearchModule) ByType(keyword string, searchType, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("searchid", randomSearchID()).
		Set("query", keyword).
		Set("search_type", searchType).
		Set("num_per_page", num).
		Set("page_num", page).
		Set("highlight", 1).
		Set("grp", true).
		Set("selectors", qqmusic.NewJObj()).
		Set("vec_selectors", &qqmusic.JArr{})
	data, err := m.cl.CgiCall("music.search.SearchCgiService", "DoSearchForQQMusicMobile", param,
		qqmusic.CGIOption{Platform: qqmusic.PlatformAndroid})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// General 综合搜索。
func (m *SearchModule) General(keyword string, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("searchid", randomSearchID()).
		Set("search_type", 100).
		Set("page_num", num).
		Set("query", keyword).
		Set("page_id", page).
		Set("highlight", true).
		Set("grp", true)
	data, err := m.cl.CgiCall("music.adaptor.SearchAdaptor", "do_search_v2", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// AlbumModule 专辑域接口。
type AlbumModule struct{ cl *qqmusic.Client }

// NewAlbumModule 构造。
func NewAlbumModule(cl *qqmusic.Client) *AlbumModule { return &AlbumModule{cl: cl} }

// GetDetail 专辑详情（数字 ID 或 MID）。
func (m *AlbumModule) GetDetail(value string) (json.RawMessage, error) {
	param := qqmusic.NewJObj()
	if isDigits(value) {
		param.Set("albumId", json.Number(value))
	} else {
		param.Set("albumMId", value)
	}
	data, err := m.cl.CgiCall("music.musichallAlbum.AlbumInfoServer", "GetAlbumDetail", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetSongs 专辑歌曲列表（分页）。
func (m *AlbumModule) GetSongs(value string, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("begin", num*(page-1)).
		Set("num", num)
	if isDigits(value) {
		param.Set("albumId", json.Number(value))
	} else {
		param.Set("albumMid", value)
	}
	data, err := m.cl.CgiCall("music.musichallAlbum.AlbumSongList", "GetAlbumSongList", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// CommentModule 评论域接口。
type CommentModule struct{ cl *qqmusic.Client }

// NewCommentModule 构造。
func NewCommentModule(cl *qqmusic.Client) *CommentModule { return &CommentModule{cl: cl} }

// GetHotComments 歌曲热评（分页）。
func (m *CommentModule) GetHotComments(bizID int64, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("BizType", 1).
		Set("BizId", intToString(bizID)).
		Set("LastCommentSeqNo", "").
		Set("PageSize", num).
		Set("PageNum", page-1).
		Set("HotType", 1).
		Set("WithAirborne", 0).
		Set("PicEnable", 1).
		Set("BizSubType", 2)
	data, err := m.cl.CgiCall("music.globalComment.CommentRead", "GetHotCommentList", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

func intToString(v int64) string { return strconv.FormatInt(v, 10) }
