package modules

import (
	"encoding/json"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// CloudModule 音乐网盘读取接口。
//
// 上游 GetMatchAudio 一页 2000 首，返回的 matched 表示文件与曲库的匹配状态：
// 0/1 可按 song_info.mid 播放，2 表示只拿到了上传文件资料、不能保证取到播放链接。
type CloudModule struct{ cl *qqmusic.Client }

// NewCloudModule 构造。
func NewCloudModule(cl *qqmusic.Client) *CloudModule { return &CloudModule{cl: cl} }

// GetMatchAudio 读取一页网盘歌曲。
func (m *CloudModule) GetMatchAudio(pageNo int) (json.RawMessage, error) {
	if pageNo < 1 {
		pageNo = 1
	}
	cred := m.cl.Credential()
	param := qqmusic.NewJObj().
		Set("uin", credentialNumericUin(cred)).
		Set("file_id", "").
		Set("page_no", pageNo).
		Set("page_size", 2000)
	data, err := m.cl.CgiCall("music.WeiyunMusic.WeiyunMusicRead", "GetMatchAudio", param,
		qqmusic.CGIOption{RequireLogin: true})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}
