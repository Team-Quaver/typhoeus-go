package modules

import (
	"encoding/json"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// RecommendModule 推荐域接口。
type RecommendModule struct{ cl *qqmusic.Client }

// NewRecommendModule 构造。
func NewRecommendModule(cl *qqmusic.Client) *RecommendModule { return &RecommendModule{cl: cl} }

// GuessRecommend 猜你喜欢（无限电台的数据源）。
//
// 上游一次只给 5 首（num 调大被忽略、song_ids 回灌会报 22006），但每次内容随机
// ——上层「多拿些」就多调几轮再按 mid 去重。必须串行：并发同请求会触发 700000 节流。
func (m *RecommendModule) GuessRecommend() (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("id", 99).
		Set("num", 5).
		Set("from", 0).
		Set("scene", 0).
		Set("song_ids", &qqmusic.JArr{})
	data, err := m.cl.CgiCall("music.radioProxy.MbTrackRadioSvr", "get_radio_track", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetRecommendSonglist 推荐歌单（分页）。
func (m *RecommendModule) GetRecommendSonglist(page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("From", num*(page-1)).Set("Size", num)
	data, err := m.cl.CgiCall("music.playlist.PlaylistSquare", "GetRecommendFeed", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetRecommendNewsong 推荐新歌（type: 1内地 2欧美 3日本 4韩国 5最新 6港台）。
func (m *RecommendModule) GetRecommendNewsong(typ int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("type", typ)
	data, err := m.cl.CgiCall("newsong.NewSongServer", "get_new_song_info", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// TopModule 排行榜域接口。
type TopModule struct{ cl *qqmusic.Client }

// NewTopModule 构造。
func NewTopModule(cl *qqmusic.Client) *TopModule { return &TopModule{cl: cl} }

// GetCategory 所有榜单分类。
func (m *TopModule) GetCategory() (json.RawMessage, error) {
	data, err := m.cl.CgiCall("music.musicToplist.Toplist", "GetAll", qqmusic.NewJObj(), qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetDetail 榜单详情 + 歌曲列表（withTags 保持布尔原样）。
func (m *TopModule) GetDetail(topID, page, num int64) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("topId", topID).
		Set("offset", num*(page-1)).
		Set("num", num).
		Set("withTags", true)
	data, err := m.cl.CgiCall("music.musicToplist.Toplist", "GetDetail", param,
		qqmusic.CGIOption{PreserveBool: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}
