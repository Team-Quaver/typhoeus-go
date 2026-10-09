// Package modules 是 QQ 音乐各业务域接口的薄封装（对标 vendor/QQMusicApi/modules）。
//
// 每个函数只负责：拼参数 → CgiCall → ParseCGIData → 透传原始 JSON。
// 响应不做强类型建模：服务端是适配层，原始透传字段最全、也省去双重维护；
// 需要结构化字段的场景（如取链的 ekey/result）就地小结构解析。
package modules

import (
	"encoding/json"
	"strconv"

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

// encryptedExts 加密容器的扩展名集合（QMC 密文形态）。
// 明文孪生各有别的扩展名（.flac/.ogg/.m4a/.mp4/.nac），不会与之混淆，所以扩展名
// 是比枚举名更权威、也更抗「调用方自造类型名」的判别依据。
var encryptedExts = map[string]struct{}{
	".mflac": {},
	".mgg":   {},
	".mmp4":  {},
	".mnac":  {},
}

// IsEncrypted 加密类型判别（需走 CgiGetEVkey 通道并取 ekey 解密）。
//
// 主判据是扩展名：调用方（如 typhoeus resolver 的 encType）按档位现造 SongFileType 时
// 枚举名并不一定等于下面的常量名，早期按名字匹配导致这些类型被误判成明文、错走 UrlGetVkey
// 通道拿不到 ekey —— 这正是「只有批量兜底档能解、其余加密档全哑」的根因。
// 名字集合仅作兼容保留（上游枚举直用时）。
func IsEncrypted(t SongFileType) bool {
	if _, ok := encryptedExts[t.Ext]; ok {
		return true
	}
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
	// 非 android 平台身份的 vkey 请求必须带凭证配套字段：authst + tmeLoginType，
	// 否则上游回 result=22（参数缺失）。web 平台走 web comm（ct=24）原样即可。
	if plat := opt.Platform; plat == qqmusic.PlatformDesktop || plat == qqmusic.PlatformWeb {
		cred := m.cl.Credential()
		comm := qqmusic.NewJObj()
		if plat == qqmusic.PlatformDesktop {
			comm.Set("cv", "1859")
		}
		if cred.MusicKey != "" {
			comm.Set("authst", cred.MusicKey)
		}
		if cred.LoginType != 0 {
			comm.Set("tmeLoginType", strconv.FormatInt(cred.LoginType, 10))
		}
		opt.Comm = comm
	}
	if encrypted {
		// 加密通道（CgiGetEVkey）必须对齐官方桌面客户端的请求形态，
		// 否则上游一律按无权限拒绝（result=104003/101404）：
		//   param: guid 固定 "10000"、ctx=1、loginflag=1、platform="27"、songtype 非 0；
		//   comm:  desktop 形态（ct=19/cv=1859）+ authst + tmeLoginType。
		// 参照 qmc-decoder ekey_fetch.rs 与 amtoaer/lyrune 的可用实现。
		// 注意：songmid 用 media_mid（lyrune 语义），歌曲 mid 与 media_mid 不同源时
		// 传 mid 会被上游按无该文件拒绝。
		param.Set("guid", "10000")
		for i, st := range songtypes {
			if st == 0 {
				songtypes[i] = 1
			}
		}
		param.Set("songtype", songtypes)
		param.Set("ctx", 1)
		param.Set("loginflag", 1)
		param.Set("platform", "27")
		for i := range songmids {
			if mm := files[i].MediaMid; mm != "" {
				songmids[i] = mm
			}
		}
		opt.Platform = qqmusic.PlatformDesktop
		cred := m.cl.Credential()
		comm := qqmusic.NewJObj().Set("cv", "1859")
		if cred.MusicKey != "" {
			comm.Set("authst", cred.MusicKey)
		}
		if cred.LoginType != 0 {
			comm.Set("tmeLoginType", strconv.FormatInt(cred.LoginType, 10))
		}
		opt.Comm = comm
	}
	data, err := m.cl.CgiCall(module, method, param, opt)
	if err != nil {
		return nil, err
	}
	return qqmusic.ParseCGIData(data, nil)
}

// EVkeyVariant 一个加密文件变体（文件名前缀 + 扩展名，如 F0M0/.mflac）。
type EVkeyVariant struct {
	Pref string
	Ext  string
}

// EVkeyEntry 批量请求里单个文件的取链结果。
type EVkeyEntry struct {
	Filename string
	Purl     string
	Vkey     string
	Ekey     string
	Result   int64
}

// GetEVkeyBatch 一次 CgiGetEVkey 同时问多个加密文件变体（lyrune 策略）：
// 明文链拿不到的歌，把 master/atmos/flac/ogg 的加密形态一口气全问一遍，
// 上游按文件逐个回 result/ekey/purl —— 谁可播用谁，而不是赌单一变体。
// songmid 用 media_mid（与 lyrune 一致）。返回与 variants 等长的结果切片。
func (m *SongModule) GetEVkeyBatch(mid, mediaMid string, variants []EVkeyVariant) ([]EVkeyEntry, error) {
	if mediaMid == "" {
		mediaMid = mid
	}
	filenames := make([]string, 0, len(variants))
	songmids := make([]string, 0, len(variants))
	for _, v := range variants {
		filenames = append(filenames, v.Pref+mediaMid+v.Ext)
		songmids = append(songmids, mediaMid) // lyrune：songmid = media_mid 逐条重复
	}
	count := len(filenames)
	param := qqmusic.NewJObj().
		Set("uin", m.cl.Credential().StrMusicID).
		Set("filename", filenames).
		Set("guid", "10000").
		Set("songmid", songmids).
		Set("songtype", ones(count)).
		Set("uin", m.cl.Credential().StrMusicID).
		Set("loginflag", 1).
		Set("platform", "27").
		Set("ctx", 1)
	cred := m.cl.Credential()
	comm := qqmusic.NewJObj().Set("cv", "1859")
	if cred.MusicKey != "" {
		comm.Set("authst", cred.MusicKey)
	}
	if cred.LoginType != 0 {
		comm.Set("tmeLoginType", strconv.FormatInt(cred.LoginType, 10))
	}
	data, err := m.cl.CgiCall("music.vkey.GetEVkey", "CgiGetEVkey", param, qqmusic.CGIOption{
		Platform: qqmusic.PlatformDesktop,
		Comm:     comm,
	})
	if err != nil {
		return nil, err
	}
	raw, err := qqmusic.ParseCGIData(data, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []struct {
			Filename string `json:"filename"`
			Purl     string `json:"purl"`
			Vkey     string `json:"vkey"`
			Ekey     string `json:"ekey"`
			Result   int64  `json:"result"`
		} `json:"midurlinfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, qqmusic.NewDataError("EVkey 批量响应解析失败")
	}
	entries := make([]EVkeyEntry, count)
	for i := range entries {
		entries[i].Result = -1 // 上游少回条目时按失败处理
	}
	for i, it := range resp.Items {
		if i >= count {
			break
		}
		entries[i] = EVkeyEntry{Filename: it.Filename, Purl: it.Purl, Vkey: it.Vkey, Ekey: it.Ekey, Result: it.Result}
	}
	return entries, nil
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

// ones 生成 n 个 1 的切片（EVkey 的 songtype 逐条目全 1）。
func ones(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = 1
	}
	return out
}
