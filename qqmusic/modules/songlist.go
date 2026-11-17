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
// SongType 一律填**读侧** Song.type 原值（get_detail 等读取接口的返回值）；
// 写侧转换由 ToSonglistWriteType 在参数构造处统一完成，调用方不做偏移。
type SongRef struct {
	SongID   int64
	SongType int64
}

// ToSonglistWriteType 把读侧 Song.type 映射成歌单写接口的 songType。
// 依据 QQ 音乐 Android 20.9.0.8 parser.d.d / newfolderprotocol.d.c 的两阶段映射
// （上游 QQMusicApi PR 306）：0/1 → 0；5/111/112/113 保持原值；其余 → 1。
// 旧口径「读侧 - 1」只是普通歌（1→0）碰巧对：特殊类型按偏移发出去，
// 上游 retCode=0 但歌单毫无变化（静默空操作）。
func ToSonglistWriteType(songType int64) int64 {
	switch songType {
	case 0, 1:
		return 0
	case 5, 111, 112, 113:
		return songType
	default:
		return 1
	}
}

// songlistOperParam 歌单写操作最小参数。
func songlistOperParam(dirid, tid int64, songs []SongRef) *qqmusic.JObj {
	items := &qqmusic.JArr{}
	for _, s := range songs {
		*items = append(*items, qqmusic.NewJObj().
			Set("songId", s.SongID).
			Set("songType", ToSonglistWriteType(s.SongType)))
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

// AddSongs 加歌到自建歌单。已存在也返回 true；上游 80092 压成 false（确凿失败，
// 与上游 SDK 同口径 —— 调用方必须把 false 当失败，别显示「已加入」）。
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
	item, err := m.cl.CgiCall("music.musicasset.PlaylistDetailWrite", method,
		songlistOperParam(dirid, tid, songs),
		qqmusic.CGIOption{RequireLogin: true, PreserveBool: preserveBool})
	if err != nil {
		return false, err
	}
	// 子响应业务码必须校验：80092 之类走 code 而不是 data.retCode，
	// 不解析就会把「无变化/被拒」当成 retCode=0 的假成功。
	data, err := qqmusic.ParseCGIData(item, nil)
	if err != nil {
		if e := qqmusic.AsAPIError(err); e.Kind == qqmusic.ErrKindCGI && e.Code == 80092 {
			return false, nil
		}
		return false, err
	}
	return songlistWriteResult(data)
}

// ReorderSongs 按给定顺序重排歌单歌曲（SeqSonglist，上游 QQMusicApi PR 307）。
//
// songs 必须是歌单内**全部**歌曲的新顺序：只提交部分歌曲时上游同样返回成功，
// 但未提交歌曲的顺序会被打乱；包含歌单中不存在的歌曲时上游报 80095。
// SongType 用读侧 Song.type 原值；dirid 是歌单目录 ID（「我喜欢」= 201）。
func (m *SonglistModule) ReorderSongs(dirid, tid int64, songs []SongRef) (bool, error) {
	items := &qqmusic.JArr{}
	for _, s := range songs {
		*items = append(*items, qqmusic.NewJObj().
			Set("songId", s.SongID).
			Set("songType", ToSonglistWriteType(s.SongType)))
	}
	param := qqmusic.NewJObj().
		Set("dirId", dirid).
		Set("tid", tid).
		Set("v_songPair", items)
	item, err := m.cl.CgiCall("music.musicasset.PlaylistDetailWrite", "SeqSonglist", param,
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return false, err
	}
	data, err := qqmusic.ParseCGIData(item, nil)
	if err != nil {
		return false, err
	}
	return songlistWriteResult(data)
}

// songlistWriteResult 歌单写操作的 retCode 判定（0 = 接受）。
func songlistWriteResult(data json.RawMessage) (bool, error) {
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
