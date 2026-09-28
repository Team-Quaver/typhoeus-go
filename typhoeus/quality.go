// Package typhoeus 是 Quaver 的音质档位协商与播放流中继层（对标 vendor/Typhoeus/typhoeus）。
//
// 与 Python 版的关键差异：本实现**支持会员加密档位的解密播放**（QMCv2，
// 解密全程仅驻内存，不落盘），并把明文高阶档位扩展到杜比全景声/DTS:X 全系。
// 加密边界的落点：明文/加密档位在类型上显式区分（Tier.Encrypted + Pair），
// 加密路径必须持有效 ekey 才能构建流；解密后的字节只经过内存管道直推播放端。
package typhoeus

import (
	"fmt"

	"github.com/team-quaver/typhoeus-go/qmc"
)

// Membership 会员等级（按特权从低到高）。
type Membership int

// 会员等级。
const (
	MembershipNone  Membership = 0 // 未登录 / 普通用户
	MembershipGreen Membership = 1 // 绿钻 / 豪华绿钻
	MembershipSuper Membership = 2 // 超级会员（音乐包/超级会员）

	membershipTTL = 600 // 会员缓存秒数
)

// String 展示名。
func (m Membership) String() string {
	switch m {
	case MembershipSuper:
		return "超级会员"
	case MembershipGreen:
		return "会员"
	default:
		return "普通用户"
	}
}

// Tier 一个可协商的音质档位。
//
// Encrypted=true 的档位是 QMC 密文源（mflac/mgg/mmp4）：本实现会取 ekey 做流式
// 解密播放，解密数据仅驻内存。PlainPrefix/PlainExt 为同档位明文形态的文件名规则
// （加密档位也可能存在明文孪生，如 flac↔F000/F0M0）。
type Tier struct {
	ID        string `json:"id"`        // 档位标识（对 UI/API 暴露的稳定字符串）
	Label     string `json:"label"`     // 中文展示名
	PlainPref string `json:"-"`         // 明文文件名前缀（如 F000/AI00）；空 = 无明文形态
	PlainExt  string `json:"-"`         // 明文扩展名
	EncPref   string `json:"-"`         // 加密文件名前缀（如 F0M0）；空 = 无加密形态
	EncExt    string `json:"-"`         // 加密扩展名
	Mime      string `json:"mime"`      // Content-Type
	Ext       string `json:"ext"`       // 明文容器扩展名（诊断/文件名推断）
	Rank      int    `json:"rank"`      // 音质序（越大越高；回退 = rank 向下）
	Requires  int    `json:"requires"`  // 会员门槛（0/1/2）
	Encrypted bool   `json:"encrypted"` // 是否加密源（需要解密播放）
	HiRes     bool   `json:"hi_res"`    // rank >= 40（UI 徽标）
}

// hasPlain 是否存在明文形态。
func (t *Tier) hasPlain() bool { return t.PlainPref != "" }

// hasEnc 是否存在加密形态。
func (t *Tier) hasEnc() bool { return t.EncPref != "" }

// 档位表（rank 升序）。明文/加密形态按实际可用性标注：
//   - mp3 无加密孪生（官方从不下发 m* 加密 mp3）
//   - vinyl 只有加密形态
//   - NAC（TL01 腾讯自研 codec）浏览器不可播，不进档位表
var tierTable = []*Tier{
	{ID: "128", Label: "标准音质", PlainPref: "M500", PlainExt: ".mp3", EncPref: "", EncExt: "", Mime: "audio/mpeg", Ext: ".mp3", Rank: 10, Requires: 0},
	{ID: "320", Label: "高品质 HQ", PlainPref: "M800", PlainExt: ".mp3", EncPref: "", EncExt: "", Mime: "audio/mpeg", Ext: ".mp3", Rank: 20, Requires: 1},
	{ID: "320ogg", Label: "高品质 HQ (OGG)", PlainPref: "O800", PlainExt: ".ogg", EncPref: "O8M0", EncExt: ".mgg", Mime: "audio/ogg", Ext: ".ogg", Rank: 25, Requires: 1},
	{ID: "640ogg", Label: "无损 SQ (OGG)", PlainPref: "O801", PlainExt: ".ogg", EncPref: "O8M1", EncExt: ".mgg", Mime: "audio/ogg", Ext: ".ogg", Rank: 30, Requires: 1},
	{ID: "flac", Label: "无损 SQ", PlainPref: "F000", PlainExt: ".flac", EncPref: "F0M0", EncExt: ".mflac", Mime: "audio/flac", Ext: ".flac", Rank: 40, Requires: 1},
	{ID: "atmos2", Label: "臻品音质 2.0", PlainPref: "Q000", PlainExt: ".flac", EncPref: "Q0M0", EncExt: ".mflac", Mime: "audio/flac", Ext: ".flac", Rank: 50, Requires: 2},
	{ID: "atmos51", Label: "臻品全景声 5.1", PlainPref: "Q001", PlainExt: ".flac", EncPref: "Q0M1", EncExt: ".mflac", Mime: "audio/flac", Ext: ".flac", Rank: 55, Requires: 2},
	{ID: "atmos71", Label: "臻品全景声 7.1", PlainPref: "Q003", PlainExt: ".ogg", EncPref: "Q0M3", EncExt: ".mgg", Mime: "audio/ogg", Ext: ".ogg", Rank: 57, Requires: 2},
	{ID: "master", Label: "臻品母带", PlainPref: "AI00", PlainExt: ".flac", EncPref: "AIM0", EncExt: ".mflac", Mime: "audio/flac", Ext: ".flac", Rank: 60, Requires: 2},
	{ID: "dts", Label: "DTS:X 环绕声", PlainPref: "DT03", PlainExt: ".mp4", EncPref: "DTM3", EncExt: ".mmp4", Mime: "audio/mp4", Ext: ".mp4", Rank: 65, Requires: 2},
	{ID: "atmosdb", Label: "杜比全景声 (AC-4)", PlainPref: "D004", PlainExt: ".mp4", EncPref: "D0M4", EncExt: ".mmp4", Mime: "audio/mp4", Ext: ".mp4", Rank: 66, Requires: 2},
	{ID: "vinyl", Label: "臻品黑胶", PlainPref: "", PlainExt: "", EncPref: "V0M0", EncExt: ".mflac", Mime: "audio/flac", Ext: ".flac", Rank: 90, Requires: 2, Encrypted: true},
}

// Tiers 档位表副本（含加密档位；本实现可解密播放）。
func Tiers() []*Tier {
	out := make([]*Tier, len(tierTable))
	copy(out, tierTable)
	return out
}

// TierByID 按档位 id 取档位。
func TierByID(id string) (*Tier, error) {
	for _, t := range tierTable {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, &Error{Status: 422, Message: fmt.Sprintf("未知音质档位: %s", id)}
}

// FallbackChain 目标档位 → 依次降质的候选链（自目标 rank 向下，兜底标准音质）。
//
// deprioritize：把这些档位整体压到链尾（仍按 rank 降序）——自动模式下
// 「全景声」不优先降档到此；但目标本身是它时仍最先尝试（显式选档语义不变）。
func FallbackChain(id string, deprioritize []string) ([]*Tier, error) {
	target, err := TierByID(id)
	if err != nil {
		return nil, err
	}
	// 从高到低收集 rank <= 目标的档位
	var chain []*Tier
	for i := len(tierTable) - 1; i >= 0; i-- {
		t := tierTable[i]
		if t.Rank <= target.Rank {
			chain = append(chain, t)
		}
	}
	if len(deprioritize) > 0 {
		head, rest := chain[:1], chain[1:] // chain[0] = 目标档，永不动
		var demoted, kept []*Tier
		for _, t := range rest {
			if containsStr(deprioritize, t.ID) {
				demoted = append(demoted, t)
			} else {
				kept = append(kept, t)
			}
		}
		chain = append(append(append([]*Tier{}, head...), kept...), demoted...)
	}
	return chain, nil
}

// AvailableFor 按会员等级列出可播档位（升序，含加密档位——本实现支持解密）。
func AvailableFor(m Membership) []*Tier {
	var out []*Tier
	for _, t := range tierTable {
		if Membership(t.Requires) <= m {
			out = append(out, t)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// plainMagics 明文容器 magic 白名单（前缀匹配）。
var plainMagics = [][]byte{
	[]byte("fLaC"), // FLAC
	[]byte("OggS"), // Ogg（QMC 的 mgg 密文不会有合法 OggS 页首）
	[]byte("ID3"),  // MP3 with tag
	{0xFF, 0xFB},   // MP3 frame sync (MPEG1 Layer3)
	{0xFF, 0xF3},
	{0xFF, 0xF2},
	{0x8A, 'X', 0x1A, 0xDF}, // Matroska/EBML 头（DTS/杜比 mp4 若为明文多为 EBML 或 mp4 ftyp）
}

// LooksPlain 判定文件头是否为明文可播容器。
func LooksPlain(head []byte) bool {
	for _, m := range plainMagics {
		if len(head) >= len(m) && string(head[:len(m)]) == string(m) {
			return true
		}
	}
	// mp4 容器：第 4-8 字节为 "ftyp"
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}

// SniffPlain 经解密嗅探首块（加密流专用）：把密文首 16 字节解出后验明文 magic。
func SniffPlain(cipher qmc.Cipher, head []byte) bool {
	if len(head) == 0 {
		return false
	}
	plain := cipher.Decrypt(head, 0)
	return LooksPlain(plain)
}
