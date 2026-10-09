package typhoeus

import (
	"testing"

	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// TestTierTableInvariants 档位表必须按 rank 严格升序、ID 唯一 —— FallbackChain /
// AvailableFor / max 档位都依赖这个顺序，插入新档时顺序错了会静默降级错乱。
func TestTierTableInvariants(t *testing.T) {
	seen := map[string]bool{}
	lastRank := -1 << 30
	for i, tr := range tierTable {
		if tr.ID == "" {
			t.Fatalf("第 %d 项档位 id 为空", i)
		}
		if seen[tr.ID] {
			t.Errorf("档位 id 重复: %s", tr.ID)
		}
		seen[tr.ID] = true
		if tr.Rank <= lastRank {
			t.Errorf("档位表未按 rank 升序：%s(rank=%d) 不在前一项(rank=%d)之后", tr.ID, tr.Rank, lastRank)
		}
		lastRank = tr.Rank
	}
}

// TestOGG192And96Tiers 新增的 OGG 低阶档（192ogg=HQ192、96ogg=流畅）必须齐备
// 明文/加密形态且加密形态能被识别为加密（否则走不到 EVkey 通道、取不到 ekey）。
func TestOGG192And96Tiers(t *testing.T) {
	cases := []struct {
		id, label, plainPref, encPref, ext, encExt string
		rank, requires                             int
	}{
		{"96ogg", "流畅音质 (OGG)", "O400", "O4M0", ".ogg", ".mgg", 5, 0},
		{"192ogg", "高品质 HQ (OGG 192)", "O600", "O6M0", ".ogg", ".mgg", 15, 1},
	}
	for _, c := range cases {
		tr, err := TierByID(c.id)
		if err != nil {
			t.Fatalf("档位 %s 不存在: %v", c.id, err)
		}
		if tr.Label != c.label || tr.PlainPref != c.plainPref || tr.EncPref != c.encPref ||
			tr.PlainExt != c.ext || tr.EncExt != c.encExt || tr.Rank != c.rank || tr.Requires != c.requires {
			t.Errorf("档位 %s 字段不符: %+v", c.id, tr)
		}
		if !tr.hasPlain() || !tr.hasEnc() {
			t.Errorf("档位 %s 必须同时有明文与加密形态", c.id)
		}
		if modules.IsEncrypted(modules.SongFileType{Name: tr.ID + "_ENC", Pref: tr.EncPref, Ext: tr.EncExt}) != true {
			t.Errorf("档位 %s 的加密形态未被识别为加密", c.id)
		}
	}

	// 回退链：请求 320 应把 192ogg（rank 15）纳入候选、96ogg（rank 5）在最底。
	chain, err := FallbackChain("320", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(chain))
	for i, c := range chain {
		ids[i] = c.ID
	}
	has := func(x string) bool {
		for _, s := range ids {
			if s == x {
				return true
			}
		}
		return false
	}
	if !has("192ogg") || !has("96ogg") {
		t.Errorf("请求 320 的回退链应含 192ogg 与 96ogg，实得 %v", ids)
	}
}
