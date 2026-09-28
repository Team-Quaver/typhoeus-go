package modules

import (
	"encoding/json"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// SingerModule 歌手域接口。
type SingerModule struct{ cl *qqmusic.Client }

// NewSingerModule 构造。
func NewSingerModule(cl *qqmusic.Client) *SingerModule { return &SingerModule{cl: cl} }

// GetInfo 歌手主页基本信息（固定 Android 平台）。
func (m *SingerModule) GetInfo(mid string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("SingerMid", mid)
	data, err := m.cl.CgiCall("music.UnifiedHomepage.UnifiedHomepageSrv", "GetHomepageHeader", param,
		qqmusic.CGIOption{Platform: qqmusic.PlatformAndroid})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetSongsList 歌手歌曲列表。
// order：1=按热度（热门）、2=按发行时间倒序（最新发布）——必须显式透传，
// 否则「新歌」标签拿到的与热门相同、去重后恒为空。
func (m *SingerModule) GetSongsList(mid string, page, num, order int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("singerMid", mid).
		Set("order", order).
		Set("number", num).
		Set("begin", num*(page-1))
	data, err := m.cl.CgiCall("musichall.song_list_server", "GetSingerSongList", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetAlbumList 歌手专辑列表。
func (m *SingerModule) GetAlbumList(mid string, page, num, order int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("singerMid", mid).
		Set("order", order).
		Set("number", num).
		Set("begin", num*(page-1))
	data, err := m.cl.CgiCall("music.musichallAlbum.AlbumListServer", "GetAlbumList", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetSimilar 相似歌手。
func (m *SingerModule) GetSimilar(mid string, number int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("singerMid", mid).Set("number", number)
	data, err := m.cl.CgiCall("music.SimilarSingerSvr", "GetSimilarSingerList", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetDesc 歌手描述原始响应（wiki 百科 XML + ex_info 扩展）。
func (m *SingerModule) GetDesc(mids []string) (json.RawMessage, error) {
	arr := &qqmusic.JArr{}
	for _, mid := range mids {
		*arr = append(*arr, mid)
	}
	param := qqmusic.NewJObj().
		Set("singer_mids", arr).
		Set("group_singer", true).
		Set("wiki_singer", true).
		Set("ex_singer", true).
		Set("pic", true).
		Set("photos", true)
	data, err := m.cl.CgiCall("music.musichallSinger.SingerInfoInter", "GetSingerDetail", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// ExtractWikiDesc 从歌手详情原始 JSON 里取出归一化的描述字段。
// 百科简介（wiki <desc>）优先，回退 ex_info.desc；area 枚举转展示文案。
