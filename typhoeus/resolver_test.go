package typhoeus

import (
	"testing"

	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// TestEncTypeRoutedToEVkey 回归：resolver 按档位现造的加密 SongFileType（encType）必须被
// modules.IsEncrypted 认作加密。早期 IsEncrypted 只按枚举名匹配，而 encType 造出的名字是
// 「<tierID>_ENC」（小写档位 id，如 flac_ENC/320ogg_ENC），一个都不在名单里 → 这些加密档
// 被错走 UrlGetVkey 明文通道、拿不到 ekey → 除批量兜底外所有加密档全哑（实测只出 OGG_320）。
func TestEncTypeRoutedToEVkey(t *testing.T) {
	covered := 0
	for _, tier := range tierTable {
		if !tier.hasEnc() {
			continue
		}
		covered++
		if !modules.IsEncrypted(*encType(tier)) {
			t.Errorf("档位 %s 的加密形态未路由到 EVkey 通道（encName=%s ext=%s）",
				tier.ID, encType(tier).Name, encType(tier).Ext)
		}
		// 明文档位不能误判成加密（否则明文档会走 EVkey、拿不到 ekey）。
		if modules.IsEncrypted(*plainType(tier)) {
			t.Errorf("档位 %s 的明文形态被误判为加密（ext=%s）", tier.ID, plainType(tier).Ext)
		}
	}
	if covered == 0 {
		t.Fatal("档位表里没有任何加密档位，测试失去意义")
	}
}

// TestBestEVkeyCandidatesOrdersBestFirst 回归：批量兜底必须「取最接近请求档位的那一档」。
// 早期按上游回包顺序（tierTable 升序）取首个放行条目，会把请求母带的歌降到链上最低档。
func TestBestEVkeyCandidatesOrdersBestFirst(t *testing.T) {
	byPrefix := map[string]*Tier{}
	for _, tr := range tierTable {
		if tr.EncPref != "" {
			byPrefix[tr.EncPref] = tr
		}
	}
	// 上游按 tierTable 升序回报：OGG_320 放行、然后是母带放行。
	entries := []modules.EVkeyEntry{
		{Filename: "O8M0" + "media" + ".mgg", Purl: "/o8m0", Ekey: "k", Result: 0},
		{Filename: "AIM0" + "media" + ".mflac", Purl: "/aim0", Ekey: "k", Result: 0},
		{Filename: "F0M0" + "media" + ".mflac", Purl: "/f0m0", Ekey: "k", Result: 104003}, // 被拒
	}
	got := bestEVkeyCandidates(entries, byPrefix)
	if len(got) != 2 {
		t.Fatalf("可播候选数 = %d，期望 2（被拒条目应剔除）", len(got))
	}
	if got[0].tier.ID != "master" {
		t.Errorf("首选档 = %s，期望 master（应按 rank 降序、不被低档抢先）", got[0].tier.ID)
	}
	if got[1].tier.ID != "320ogg" {
		t.Errorf("次选档 = %s，期望 320ogg", got[1].tier.ID)
	}
}

// TestBestEVkeyCandidatesFiltersIncomplete 缺 purl/ekey 或文件名前缀不认识的条目一律剔除。
func TestBestEVkeyCandidatesFiltersIncomplete(t *testing.T) {
	byPrefix := map[string]*Tier{}
	for _, tr := range tierTable {
		if tr.EncPref != "" {
			byPrefix[tr.EncPref] = tr
		}
	}
	entries := []modules.EVkeyEntry{
		{Filename: "AIM0x.mflac", Purl: "", Ekey: "k", Result: 0},   // 无 purl
		{Filename: "AIM0x.mflac", Purl: "/a", Ekey: "", Result: 0},  // 无 ekey
		{Filename: "ZZZZx.mflac", Purl: "/a", Ekey: "k", Result: 0}, // 前缀不认识
		{Filename: "F0M0x.mflac", Purl: "/f", Ekey: "k", Result: 0}, // 合法
	}
	got := bestEVkeyCandidates(entries, byPrefix)
	if len(got) != 1 || got[0].tier.ID != "flac" {
		t.Fatalf("合法候选应只剩 flac，实得 %d 条", len(got))
	}
}
