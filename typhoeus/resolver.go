package typhoeus

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// ResolvedStream 协商结果：一条可中继的播放流。
//
// 加密档位（Encrypted=true）时 Cipher 必为非 nil——中继层会用它在内存里
// 对每个 Range 分片做按偏移解密；明文档位 Cipher 恒为 nil。
type ResolvedStream struct {
	Mid           string
	Tier          *Tier // 实际命中档位（可能低于请求档位 = 降级）
	RequestedTier *Tier // 客户端请求档位
	URL           string
	Filename      string
	Encrypted     bool
	Cipher        qmc.Cipher // 加密流的解密器（仅内存；不序列化、不落盘）
	Degraded      bool       // 命中档位低于请求档位
}

// StreamResolver 档位协商器：会员门控 + 明文/加密回退链 + 首块嗅探。
type StreamResolver struct {
	provider *QQMusicProvider
	// probePlain=false 关闭首块嗅探（省一次 RTT；单测/可信环境用）
	probePlain bool
}

// NewStreamResolver 构造。
func NewStreamResolver(provider *QQMusicProvider) *StreamResolver {
	return &StreamResolver{provider: provider, probePlain: true}
}

// Resolve 协商一条可播放流。
//
// autoDowngrade=false（用户显式选档）：会员不足 → 403 MembershipRequired。
// autoDowngrade=true（自动音质）：把链裁剪到会员可及的最高档，不抛门控错误。
//
// 每个候选档位先试明文形态，再试加密形态（需会员且有 ekey）——这是本实现
// 相对 Python 版的核心增强：FLAC/臻品系大量歌曲只有加密源可得。
func (r *StreamResolver) Resolve(ctx context.Context, mid, mediaMid, tierID string,
	autoDowngrade bool, deprioritize []string, platform string, songType int) (*ResolvedStream, error) {

	target, err := TierByID(tierID)
	if err != nil {
		return nil, err
	}
	if mediaMid == "" {
		mediaMid = mid
	}
	membership := r.provider.Membership(ctx)
	if Membership(target.Requires) > membership && !autoDowngrade {
		return nil, errMembershipRequired(gateReason(membership, target))
	}

	chain, err := FallbackChain(tierID, deprioritize)
	if err != nil {
		return nil, err
	}
	lastCode := int64(-1)
	for _, tier := range chain {
		if Membership(tier.Requires) > membership {
			continue // auto 模式：越过会员不可及的高档
		}
		// 1) 明文形态
		if tier.hasPlain() {
			links, err := r.fetchLinksForTier(ctx, mid, mediaMid, plainType(tier), platform, songType)
			if err != nil {
				return nil, err
			}
			if hit := pickLink(links, mid); hit != nil {
				if !r.probePlain || r.sniffDirectOK(ctx, hit.URL) {
					return &ResolvedStream{
						Mid: mid, Tier: tier, RequestedTier: target,
						URL: hit.URL, Filename: hit.Filename,
						Degraded: tier != target,
					}, nil
				}
				lastCode = -2 // 明文档位却返回密文内容（上游偶发），降档继续
			} else if len(links) > 0 {
				lastCode = links[0].ResultCode
			}
		}
		// 2) 加密形态（需 ekey；解密器仅驻内存）
		if tier.hasEnc() {
			links, err := r.fetchLinksForTier(ctx, mid, mediaMid, encType(tier), platform, songType)
			if err != nil {
				return nil, err
			}
			hit := pickLink(links, mid)
			if hit == nil || hit.Ekey == "" {
				if hit != nil {
					lastCode = hit.ResultCode
				}
				continue
			}
			cipher, err := qmc.NewCipherFromEkey(hit.Ekey)
			if err != nil {
				lastCode = -3 // ekey 不可用（过期/格式变化）
				continue
			}
			if r.probePlain && !r.sniffDecryptedOK(ctx, hit.URL, cipher) {
				lastCode = -4 // 解密后仍非明文容器（ekey 不匹配），降档继续
				continue
			}
			return &ResolvedStream{
				Mid: mid, Tier: tier, RequestedTier: target,
				URL: hit.URL, Filename: hit.Filename,
				Encrypted: true, Cipher: cipher,
				Degraded: tier != target,
			}, nil
		}
	}
	// 兜底（lyrune 策略）：整条链都失败时，把链上所有加密变体合并进一次
	// CgiGetEVkey 批量请求。上游按文件逐个判定——单变体请求被拒的歌，
	// 批量请求里可能恰有变体放行（不同加密形态的授权判定相互独立）。
	for _, tier := range chain {
		if !tier.hasEnc() || Membership(tier.Requires) > membership {
			continue
		}
		hit, err := r.resolveEncryptedBatch(ctx, mid, mediaMid, tier)
		if err != nil {
			continue
		}
		return hit, nil
	}
	return nil, errProvider(fmt.Sprintf("无可播档位（最后上游 result=%d）", lastCode))
}

// resolveEncryptedBatch 把单个档位的加密形态交给批量 EVkey 请求处理：
// 同一请求里带上全档位加密变体（rank ≤ 该档位），命中哪个回哪个。
// 命中即按文件名前缀映射回档位并构建解密流。
func (r *StreamResolver) resolveEncryptedBatch(ctx context.Context, mid, mediaMid string, target *Tier) (*ResolvedStream, error) {
	variants := make([]modules.EVkeyVariant, 0, len(tierTable))
	byPrefix := map[string]*Tier{}
	for _, t := range tierTable {
		if t.Rank > target.Rank || t.EncPref == "" {
			continue
		}
		variants = append(variants, modules.EVkeyVariant{Pref: t.EncPref, Ext: t.EncExt})
		byPrefix[t.EncPref] = t
	}
	if len(variants) == 0 {
		return nil, errProvider("无可用的加密变体")
	}
	entries, err := r.provider.song.GetEVkeyBatch(mid, mediaMid, variants)
	if err != nil {
		return nil, errProvider(fmt.Sprintf("EVkey 批量取链失败: %v", err))
	}
	for _, e := range entries {
		if e.Result != 0 || e.Purl == "" || e.Ekey == "" {
			continue
		}
		tier := byPrefix[prefixOf(e.Filename, byPrefix)]
		if tier == nil {
			continue
		}
		cipher, cerr := qmc.NewCipherFromEkey(e.Ekey)
		if cerr != nil {
			continue // ekey 不可用（过期/格式变化）
		}
		if r.probePlain && !r.sniffDecryptedOK(ctx, r.provider.streamURL(ctx, e.Purl), cipher) {
			continue // 解密后仍非明文容器（ekey 不匹配）
		}
		return &ResolvedStream{
			Mid: mid, Tier: tier, RequestedTier: target,
			URL:      r.provider.streamURL(ctx, e.Purl),
			Filename: e.Filename,
			Encrypted: true, Cipher: cipher,
			Degraded: tier != target,
		}, nil
	}
	return nil, errProvider("EVkey 批量无可播条目")
}

// prefixOf 从文件名提取加密前缀（文件名形如 F0M0<media_mid>.mflac，前缀定长 4）。
func prefixOf(filename string, byPrefix map[string]*Tier) string {
	if len(filename) >= 4 {
		if _, ok := byPrefix[filename[:4]]; ok {
			return filename[:4]
		}
	}
	return filename
}

func plainType(t *Tier) *modules.SongFileType {
	return &modules.SongFileType{Name: t.ID, Pref: t.PlainPref, Ext: t.PlainExt}
}

func encType(t *Tier) *modules.SongFileType {
	return &modules.SongFileType{Name: t.ID + "_ENC", Pref: t.EncPref, Ext: t.EncExt}
}

// pickLink 从取链结果里挑目标 mid 的可播项。
func pickLink(links []LinkResult, mid string) *LinkResult {
	for i := range links {
		if links[i].Mid == mid && links[i].Playable && links[i].URL != "" {
			return &links[i]
		}
	}
	return nil
}

// sniffTimeout 单次嗅探/探测请求的预算。resolve 全链路必须快于前端 12s 超时；
// 此前无超时，CDN 挂起会把整次 resolve 拖到前端兜底标准音质（高阶「莫名消失」的
// 真实回归源之一）。
const sniffTimeout = 5 * time.Second

// strictRejection 严格曲库的平台性拒绝：android 身份下日本 VOCALOID、周杰伦等
// 会被拒，web/desktop 身份可能放行（版权判定按平台独立）。
func strictRejection(code int64) bool {
	return code == 104003 || code == 101404
}

// fetchLinksForTier 取链（带平台回退）：缺省 android 身份被严格曲库拒绝时，
// 换 web 身份重试一次。只在调用方未显式指定平台时回退（显式指定则尊重意图）。
func (r *StreamResolver) fetchLinksForTier(ctx context.Context, mid, mediaMid string,
	ft *modules.SongFileType, platform string, songType int) ([]LinkResult, error) {

	links, err := r.provider.resolveLinksPlatform(ctx, mid, mediaMid, *ft, platform, songType)
	if err != nil {
		return nil, err
	}
	if platform != "" {
		return links, nil
	}
	if hit := pickLink(links, mid); hit != nil {
		return links, nil
	}
	if len(links) > 0 && strictRejection(links[0].ResultCode) {
		retry, rerr := r.provider.resolveLinksPlatform(ctx, mid, mediaMid, *ft, "web", songType)
		if rerr == nil && pickLink(retry, mid) != nil {
			logf("档位 %s：android 被拒(result=%d)，web 身份放行", ft.Name, links[0].ResultCode)
			return retry, nil
		}
	}
	return links, nil
}

// sniffDirectOK 明文直连嗅探：取首 16 字节验容器 magic。
//
// 网络失败 ≠ 内容不符：CDN 抖动一次就静默降档是真实回归源（高阶莫名变标准），
// 因此重试一次后仍连不上就放行该档——上游给的本就是该档资源的授权 URL，
// magic 白名单防的是「上游偶发回密文」这种确定性问题，不是网络抖动。
func (r *StreamResolver) sniffDirectOK(ctx context.Context, url string) bool {
	ok, netFail := sniffDirect(ctx, url)
	if netFail {
		ok, netFail = sniffDirect(ctx, url)
	}
	if netFail {
		logf("明文嗅探网络失败（重试后仍失败），放行该档")
		return true
	}
	return ok
}

func sniffDirect(ctx context.Context, url string) (ok, netFail bool) {
	head, _, err := fetchRange(ctx, url, 0, 15)
	if err != nil {
		return false, true
	}
	return LooksPlain(head), false
}

// sniffDecryptedOK 加密流嗅探：解密首 16 字节后验 magic（验证 ekey 正确性）。
// 与明文嗅探同理：网络失败重试一次后放行；magic 不符（ekey 真不匹配）才降档。
func (r *StreamResolver) sniffDecryptedOK(ctx context.Context, url string, cipher qmc.Cipher) bool {
	ok, netFail := sniffDecrypted(ctx, url, cipher)
	if netFail {
		ok, netFail = sniffDecrypted(ctx, url, cipher)
	}
	if netFail {
		logf("加密嗅探网络失败（重试后仍失败），放行该档")
		return true
	}
	return ok
}

func sniffDecrypted(ctx context.Context, url string, cipher qmc.Cipher) (ok, netFail bool) {
	head, _, err := fetchRange(ctx, url, 0, 15)
	if err != nil {
		return false, true
	}
	return SniffPlain(cipher, head), false
}

// gateReason 门控拒绝文案（与上游一致）。
func gateReason(m Membership, tier *Tier) string {
	if m == MembershipNone {
		return fmt.Sprintf("「%s」需登录后开通会员", tier.Label)
	}
	return fmt.Sprintf("「%s」需 %s 及以上会员（当前 %s）", tier.Label, Membership(tier.Requires), m)
}

// fetchRange 拉取 [start,end] 闭区间的字节（嗅探/取总长用，小数据量）。
// 带单次超时：resolve 是同步链路，单点挂起不能拖垮整次协商。
func fetchRange(ctx context.Context, url string, start, end int64) ([]byte, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, sniffTimeout)
	defer cancel()
	resp, err := OpenRange(ctx, url, &ByteRange{Start: &start, End: &end})
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, end-start+1))
	if err != nil {
		return nil, 0, errStream("嗅探读取失败: " + err.Error())
	}
	return data, resp.Total, nil
}
