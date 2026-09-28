package modules

import (
	"encoding/json"
	"strconv"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// UserModule 用户域接口。
type UserModule struct{ cl *qqmusic.Client }

// NewUserModule 构造。
func NewUserModule(cl *qqmusic.Client) *UserModule { return &UserModule{cl: cl} }

// placeholderCredential 未登录时的占位凭证（主页头等匿名可读接口需要）。
func (m *UserModule) placeholderCredential() *qqmusic.Credential {
	if m.cl.Credential().HasLogin() {
		return m.cl.Credential()
	}
	return &qqmusic.Credential{
		MusicID:    1,
		StrMusicID: "1",
		MusicKey:   "placeholder-musickey",
		EncryptUin: "00000000000000000000000000000000",
		LoginType:  1,
	}
}

// GetHomepageHeader 用户/歌手主页头部原始响应（/user/me 用它挑字段）。
func (m *UserModule) GetHomepageHeader(euin string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("uin", euin).Set("IsQueryTabDetail", 1)
	data, err := m.cl.CgiCall("music.UnifiedHomepage.UnifiedHomepageSrv", "GetHomepageHeader", param,
		qqmusic.CGIOption{Credential: m.placeholderCredential()})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetVipInfo 当前账号 VIP 信息（require_login）。
func (m *UserModule) GetVipInfo() (json.RawMessage, error) {
	data, err := m.cl.CgiCall("VipLogin.VipLoginInter", "vip_login_base", qqmusic.NewJObj(),
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetFavSong 「我喜欢」歌曲列表（dirid=201，分页）。
func (m *UserModule) GetFavSong(euin string, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("disstid", 0).
		Set("dirid", 201).
		Set("tag", true).
		Set("song_begin", num*(page-1)).
		Set("song_num", num).
		Set("userinfo", true).
		Set("orderlist", true).
		Set("enc_host_uin", euin)
	data, err := m.cl.CgiCall("music.srfDissInfo.DissInfo", "CgiGetDiss", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetCreatedSonglist 自建歌单列表（uin 为数字 UIN 字符串）。
func (m *UserModule) GetCreatedSonglist(uin string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("uin", uin)
	data, err := m.cl.CgiCall("music.musicasset.PlaylistBaseRead", "GetPlaylistByUin", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetFavSonglist 收藏的他人歌单列表（分页）。
func (m *UserModule) GetFavSonglist(euin string, page, num int) (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("uin", euin).
		Set("offset", (page-1)*num).
		Set("size", num)
	data, err := m.cl.CgiCall("music.musicasset.PlaylistFavRead", "CgiGetPlaylistFavInfo", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// FavSonglist 收藏歌单。已在收藏中也返回 true。
func (m *UserModule) FavSonglist(songlistID int64) (bool, error) {
	return m.favPlaylist("FavPlaylist", songlistID)
}

// UnfavSonglist 取消收藏歌单。本就不在收藏中也返回 true。
func (m *UserModule) UnfavSonglist(songlistID int64) (bool, error) {
	return m.favPlaylist("CancelFavPlaylist", songlistID)
}

func (m *UserModule) favPlaylist(method string, songlistID int64) (bool, error) {
	param := qqmusic.NewJObj().
		Set("uin", m.cl.Credential().EncryptUin).
		Set("v_playlistId", &qqmusic.JArr{songlistID})
	data, err := m.cl.CgiCall("music.musicasset.PlaylistFavWrite", method, param,
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return false, err
	}
	var raw struct {
		Result   int64   `json:"result"`
		FailedID []int64 `json:"v_failedPlaylistId"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, qqmusic.NewDataError("收藏歌单响应解析失败")
	}
	if raw.Result != 0 {
		return false, nil
	}
	for _, id := range raw.FailedID {
		if id == songlistID {
			return false, nil
		}
	}
	return true, nil
}

// GetMusicGene 用户音乐基因（音质偏好等画像）。
func (m *UserModule) GetMusicGene(euin string) (json.RawMessage, error) {
	param := qqmusic.NewJObj().Set("VisitAccount", euin)
	data, err := m.cl.CgiCall("music.recommend.UserProfileSettingSvr", "GetProfileReport", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// uinString 数字 UIN → 字符串。
func uinString(uin int64) string { return strconv.FormatInt(uin, 10) }
