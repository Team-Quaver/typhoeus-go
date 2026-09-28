// Package modules 是 QQ 音乐各业务域接口的薄封装（对标 vendor/QQMusicApi/modules）。
//
// 每个函数只负责：拼参数 → CgiCall → ParseCGIData → 透传原始 JSON。
// 响应不做强类型建模：服务端是适配层，原始透传字段最全、也省去双重维护；
// 需要结构化字段的场景（如取链的 ekey/result）就地小结构解析。
package modules

import (
	"encoding/json"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// SongModule 歌曲域接口。
type SongModule struct{ cl *qqmusic.Client }

// NewSongModule 构造。
func NewSongModule(cl *qqmusic.Client) *SongModule { return &SongModule{cl: cl} }

// SongFileType 明文文件类型（前缀 + 扩展名，与上游枚举一致）。
type SongFileType struct {
	Name string // 枚举名（诊断用）
	Pref string // 文件名前缀
	Ext  string // 文件名扩展名
}

// EncryptedSongFileType QMC 加密文件类型（需 ekey 解密）。
type EncryptedSongFileType = SongFileType

// 明文档位（SongFileType）。
var (
	TypeDTSX    = SongFileType{"DTS_X", "DT03", ".mp4"}
	TypeMaster  = SongFileType{"MASTER", "AI00", ".flac"}
	TypeAtmos2  = SongFileType{"ATMOS_2", "Q000", ".flac"}
	TypeAtmos51 = SongFileType{"ATMOS_51", "Q001", ".flac"}
	TypeAtmos71 = SongFileType{"ATMOS_71", "Q003", ".ogg"}
	TypeAtmosDB = SongFileType{"ATMOS_DB", "D004", ".mp4"}
	TypeNAC     = SongFileType{"NAC", "TL01", ".nac"}
	TypeFLAC    = SongFileType{"FLAC", "F000", ".flac"}
	TypeOGG640  = SongFileType{"OGG_640", "O801", ".ogg"}
	TypeOGG320  = SongFileType{"OGG_320", "O800", ".ogg"}
	TypeOGG192  = SongFileType{"OGG_192", "O600", ".ogg"}
	TypeOGG96   = SongFileType{"OGG_96", "O400", ".ogg"}
	TypeMP3320  = SongFileType{"MP3_320", "M800", ".mp3"}
	TypeMP3128  = SongFileType{"MP3_128", "M500", ".mp3"}
	TypeACC192  = SongFileType{"ACC_192", "C600", ".m4a"}
	TypeACC96   = SongFileType{"ACC_96", "C400", ".m4a"}
	TypeACC48   = SongFileType{"ACC_48", "C200", ".m4a"}
	EncDTSX     = SongFileType{"DTS_X_ENC", "DTM3", ".mmp4"}
	EncVinyl    = SongFileType{"VINYL", "V0M0", ".mflac"}
	EncMaster   = SongFileType{"MASTER_ENC", "AIM0", ".mflac"}
	EncAtmos2   = SongFileType{"ATMOS_2_ENC", "Q0M0", ".mflac"}
	EncAtmos51  = SongFileType{"ATMOS_51_ENC", "Q0M1", ".mflac"}
	EncAtmos71  = SongFileType{"ATMOS_71_ENC", "Q0M3", ".mgg"}
	EncAtmosDB  = SongFileType{"ATMOS_DB_ENC", "D0M4", ".mmp4"}
	EncNAC      = SongFileType{"NAC_ENC", "TLM1", ".mnac"}
	EncFLAC     = SongFileType{"FLAC_ENC", "F0M0", ".mflac"}
	EncOGG640   = SongFileType{"OGG_640_ENC", "O8M1", ".mgg"}
	EncOGG320   = SongFileType{"OGG_320_ENC", "O8M0", ".mgg"}
	EncOGG192   = SongFileType{"OGG_192_ENC", "O6M0", ".mgg"}
	EncOGG96    = SongFileType{"OGG_96_ENC", "O4M0", ".mgg"}
)

// IsEncrypted 加密类型集合判别。
func IsEncrypted(t SongFileType) bool {
	switch t.Name {
	case "DTS_X_ENC", "VINYL", "MASTER_ENC", "ATMOS_2_ENC", "ATMOS_51_ENC", "ATMOS_71_ENC",
		"ATMOS_DB_ENC", "NAC_ENC", "FLAC_ENC", "OGG_640_ENC", "OGG_320_ENC", "OGG_192_ENC", "OGG_96_ENC":
		return true
	}
	return false
}

// SongFileInfo 单首歌曲的取链描述（mid + 类型 + songtype）。
type SongFileInfo struct {
	Mid      string
	FileType *SongFileType // nil 时用调用级默认类型
	SongType int
	MediaMid string // 空时用 Mid
}

// GetSongURLs 批量获取播放链接。
//
// 返回原始 data（expiration + midurlinfo[]，含 songmid/filename/purl/vkey/ekey/result）。
// 加密类型自动切换到 GetEVkey 通道；ekey 仅在加密通道返回。
func (m *SongModule) GetSongURLs(files []SongFileInfo, fileType SongFileType, opt qqmusic.CGIOption) (json.RawMessage, error) {
	if len(files) > 100 {
		return nil, qqmusic.NewDataError("mid 数量不能超过 100")
	}
	encrypted := IsEncrypted(fileType)
	module, method := "music.vkey.GetVkey", "UrlGetVkey"
	if encrypted {
		module, method = "music.vkey.GetEVkey", "CgiGetEVkey"
	}
	filenames := make([]string, 0, len(files))
	songmids := make([]string, 0, len(files))
	songtypes := make([]int, 0, len(files))
	for _, f := range files {
		ft := fileType
		if f.FileType != nil {
			ft = *f.FileType
		}
		mediaMid := f.MediaMid
		if mediaMid == "" {
			mediaMid = f.Mid
			filenames = append(filenames, ft.Pref+f.Mid+f.Mid+ft.Ext)
		} else {
			filenames = append(filenames, ft.Pref+mediaMid+ft.Ext)
		}
		songmids = append(songmids, f.Mid)
		songtypes = append(songtypes, f.SongType)
	}
	param := qqmusic.NewJObj().
		Set("uin", m.cl.Credential().StrMusicID).
		Set("filename", filenames).
		Set("guid", randomGUID()).
		Set("songmid", songmids).
		Set("songtype", songtypes).
		Set("ctx", 0)
	data, err := m.cl.CgiCall(module, method, param, opt)
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetCdnDispatch 获取音频 CDN 域名列表（sip）。
func (m *SongModule) GetCdnDispatch() (json.RawMessage, error) {
	param := qqmusic.NewJObj().
		Set("guid", randomGUID()).
		Set("uid", "0").
		Set("use_new_domain", 1).
		Set("use_ipv6", 1)
	data, err := m.cl.CgiCall("music.audioCdnDispatch.cdnDispatch", "GetCdnDispatch", param, qqmusic.CGIOption{})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// GetDetail 歌曲详情（固定 Web 平台）。
func (m *SongModule) GetDetail(value string) (json.RawMessage, error) {
	param := qqmusic.NewJObj()
	if isDigits(value) {
		param.Set("song_id", json.Number(value))
	} else {
		param.Set("song_mid", value)
	}
	data, err := m.cl.CgiCall("music.pf_song_detail_svr", "get_song_detail_yqq", param,
		qqmusic.CGIOption{Platform: qqmusic.PlatformWeb})
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// isDigits 全数字判定（等价 Python str.isdigit 的本包用法）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
