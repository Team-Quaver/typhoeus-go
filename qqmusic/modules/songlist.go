package modules

import (
	"encoding/json"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// SonglistModule 歌单域接口。
type SonglistModule struct{ cl *qqmusic.Client }

// NewSonglistModule 构造。
func NewSonglistModule(cl *qqmusic.Client) *SonglistModule { return &SonglistModule{cl: cl} }

// SongRef 写操作用的歌曲引用 (songId, songType)。
type SongRef struct {
	SongID   int64
	SongType int64
}

// songlistOperParam 歌单写操作最小参数。
// 注意 songType 必须用「写侧」枚举（读侧 Song.type - 1），否则上游静默空操作。
func songlistOperParam(dirid, tid int64, songs []SongRef) *qqmusic.JObj {
	items := &qqmusic.JArr{}
	for _, s := range songs {
		*items = append(*items, qqmusic.NewJObj().
			Set("songId", s.SongID).
			Set("songType", s.SongType))
	}
	return qqmusic.NewJObj().
		Set("dirId", dirid).
		Set("tid", tid).
		Set("bFmtUtf8", true).
		Set("v_songInfo", items)
}

// GetDetail 歌单详情（含歌曲列表；每日三十首也走这里，disstid=dirid=202）。
func (m *SonglistModule) GetDetail(songlistID, dirid, page, num int64, onlysong bool) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("disstid", songlistID).
		Set("dirid", dirid).
		Set("tag", true).
		Set("song_begin", num*(page-1)).
		Set("song_num", num).
		Set("userinfo", true).
		Set("orderlist", true).
		Set("onlysonglist", onlysong)
	data, err := m.cl.CgiCall("music.srfDissInfo.DissInfo", "CgiGetDiss", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// AddSongs 加歌到自建歌单。已存在也返回 true；上游 80092（无变化）按 true 处理。
//
// 注意：AddSonglist 上游带 preserve_bool（bFmtUtf8 以 JSON true 发送）；
// DelSonglist 不带（true → 1），与 vendor 实现逐项对齐。
func (m *SonglistModule) AddSongs(dirid, tid int64, songs []SongRef) (bool, error) {
	return m.writeSongs("AddSonglist", dirid, tid, songs, true)
}

// DelSongs 从歌单移除歌曲。不存在也返回 true。
func (m *SonglistModule) DelSongs(dirid, tid int64, songs []SongRef) (bool, error) {
	return m.writeSongs("DelSonglist", dirid, tid, songs, false)
}

func (m *SonglistModule) writeSongs(method string, dirid, tid int64, songs []SongRef, preserveBool bool) (bool, error) {
	data, err := m.cl.CgiCall("music.musicasset.PlaylistDetailWrite", method,
		songlistOperParam(dirid, tid, songs),
		qqmusic.CGIOption{RequireLogin: true, PreserveBool: preserveBool})
	if err != nil {
		if e := qqmusic.AsAPIError(err); e.Kind == qqmusic.ErrKindCGI && e.Code == 80092 {
			return true, nil
		}
		return false, err
	}
	var raw struct {
		RetCode int64 `json:"retCode"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, qqmusic.NewDataError("歌单写操作响应解析失败")
	}
	return raw.RetCode == 0, nil
}

// LikeSong 红心收藏（加入 dirid=201 的「我喜欢」）。
func (m *SonglistModule) LikeSong(songs []SongRef) (bool, error) {
	return m.AddSongs(201, 0, songs)
}

// UnlikeSong 取消红心。
func (m *SonglistModule) UnlikeSong(songs []SongRef) (bool, error) {
	return m.DelSongs(201, 0, songs)
}

// Create 新建歌单（重名不会失败，服务端自动加时间戳）。
func (m *SonglistModule) Create(dirname string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("dirName", dirname)
	data, err := m.cl.CgiCall("music.musicasset.PlaylistBaseWrite", "AddPlaylist", param,
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// Delete 删除歌单（dirid）。
func (m *SonglistModule) Delete(dirid int64) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("dirId", dirid)
	data, err := m.cl.CgiCall("music.musicasset.PlaylistBaseWrite", "DelPlaylist", param,
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}
