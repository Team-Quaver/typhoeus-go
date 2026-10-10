package typhoeus

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

// CDNFallback CDN 域名兜底（CDN dispatch 失败时使用）。
const CDNFallback = "https://isure.stream.qqmusic.qq.com/"

// CDNDomain 取当前可用 CDN 域名（随机挑选；供 /song/urls 拼绝对链接）。
func (p *QQMusicProvider) CDNDomain() (string, error) {
	return p.cdnDomain(context.Background())
}

// LinkResult 单首歌的取链结果。
type LinkResult struct {
	Mid        string
	Playable   bool
	URL        string // 绝对 URL（CDN 域名已拼接）
	Filename   string
	ResultCode int64
	Ekey       string // 仅加密链路携带；明文链路恒为空且被显式丢弃
	Encrypted  bool
}

// QQMusicProvider 把 Typhoeus 的取链/会员查询接到 qqmusic 客户端上。
type QQMusicProvider struct {
	cl   *qqmusic.Client
	song *modules.SongModule
	user *modules.UserModule

	mu                sync.Mutex
	membershipInfo    modules.VipInfo
	membershipAt      int64
	membershipRetryAt int64
	membershipErr     error
	membershipAccount int64
	cdnDomains        []string
	cdnDomainsAt      int64
}

// NewQQMusicProvider 构造。
func NewQQMusicProvider(cl *qqmusic.Client) *QQMusicProvider {
	return &QQMusicProvider{
		cl:   cl,
		song: modules.NewSongModule(cl),
		user: modules.NewUserModule(cl),
	}
}

// InvalidateMembership 登录/登出/刷新后调用，强制重查会员等级。
func (p *QQMusicProvider) InvalidateMembership() {
	p.mu.Lock()
	p.membershipAt = 0
	p.membershipRetryAt = 0
	p.membershipErr = nil
	p.membershipAccount = 0
	p.membershipInfo = modules.VipInfo{}
	p.mu.Unlock()
}

// Membership 查询失败不是「无会员」。成功缓存 10 分钟，失败最多 15 秒后重试。
// 刷新失败时只短暂保留同账号已验证的权益，仍逐次校验到期；首次失败明确报错。
func (p *QQMusicProvider) Membership(ctx context.Context) (Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cred := p.cl.Credential()
	if !cred.HasLogin() {
		return MembershipNone, nil
	}
	if cred.MusicID != p.membershipAccount {
		p.membershipInfo, p.membershipAt, p.membershipRetryAt = modules.VipInfo{}, 0, 0
		p.membershipErr, p.membershipAccount = nil, cred.MusicID
	}
	now := time.Now()
	if (p.membershipAt == 0 || now.Unix()-p.membershipAt >= membershipTTL) && now.Unix() >= p.membershipRetryAt {
		info, err := p.fetchMembership(ctx)
		if err != nil {
			p.membershipErr, p.membershipRetryAt = err, now.Unix()+15
		} else {
			p.membershipInfo, p.membershipAt = info, now.Unix()
			p.membershipErr, p.membershipRetryAt = nil, 0
		}
	}
	if p.membershipErr != nil {
		kind := qqmusic.AsAPIError(p.membershipErr).Kind
		// 凭证过期时不使用缓存；无有效缓存也不把一次接口失败写成普通用户。
		if p.membershipAt == 0 || now.Unix()-p.membershipAt >= 2*membershipTTL ||
			kind == qqmusic.ErrKindCredentialInvalid || kind == qqmusic.ErrKindCredentialExpired {
			return MembershipNone, errProvider("会员权益查询暂时失败，请稍后重试（未更改账号会员等级）")
		}
	}
	return membershipFromInfo(p.membershipInfo, now), nil
}

func membershipFromInfo(info modules.VipInfo, now time.Time) Membership {
	info = info.Active(now)
	if info.Svip == 1 {
		return MembershipSuper
	}
	if info.Identity.HugeVip == 1 || info.Identity.Vip == 1 {
		return MembershipGreen
	}
	return MembershipNone
}

// fetchMembership 只有有效响应才表示会员状态，不能把错误改成零值权益。
func (p *QQMusicProvider) fetchMembership(ctx context.Context) (modules.VipInfo, error) {
	raw, err := p.user.GetVipInfo()
	if err != nil {
		return modules.VipInfo{}, err
	}
	var info modules.VipInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return modules.VipInfo{}, qqmusic.NewDataError("会员权益响应格式异常")
	}
	return info, nil
}

// topKeys 响应顶层键名（诊断用；解析失败返回 "?"）。
func topKeys(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "?"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// resolveLinks 取链（明文/加密通道按 fileType 前缀自动路由）。
func (p *QQMusicProvider) resolveLinks(ctx context.Context, mid, mediaMid string, ft modules.SongFileType) ([]LinkResult, error) {
	return p.resolveLinksPlatform(ctx, mid, mediaMid, ft, "", 0)
}

// resolveLinksPlatform 同上，但允许指定请求平台身份（android/web/desktop）。
// 上游对不同平台 + 凭证家族（wx/QQ 登录）的版权判定不同——严格曲库（日本 VOCALOID、
// 周杰伦等）在 android 身份下会被 104003/101404 拒，web/desktop 身份可能放行。
func (p *QQMusicProvider) resolveLinksPlatform(ctx context.Context, mid, mediaMid string, ft modules.SongFileType, platform string, songType int) ([]LinkResult, error) {
	// songtype 语义（对齐 amtoaer/lyrune）：上游搜索项 type==1（普通歌曲）映射为 0，
	// 其余类型原样透传；EVkey 加密通道则固定 1（见 modules.GetEVkeyBatch / GetSongURLs）。
	// 搞反会导致严格曲库高档位判定异常。
	if songType == 1 {
		songType = 0
	}
	files := []modules.SongFileInfo{{Mid: mid, MediaMid: mediaMid, FileType: &ft, SongType: songType}}
	opt := qqmusic.CGIOption{Platform: modules.NormalizePlatform(platform)}
	raw, err := p.song.GetSongURLs(files, ft, opt)
	if err != nil {
		return nil, errProvider(fmt.Sprintf("取链失败: %v", err))
	}
	var resp struct {
		Expiration int64 `json:"expiration"`
		Items      []struct {
			Songmid  string `json:"songmid"`
			Filename string `json:"filename"`
			Purl     string `json:"purl"`
			Vkey     string `json:"vkey"`
			Ekey     string `json:"ekey"`
			Result   int64  `json:"result"`
		} `json:"midurlinfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errProvider("取链响应解析失败")
	}
	domain, _ := p.cdnDomain(ctx)
	out := make([]LinkResult, 0, len(resp.Items))
	enc := modules.IsEncrypted(ft)
	for _, it := range resp.Items {
		playable := it.Result == 0 && it.Purl != ""
		url := ""
		if playable {
			url = domain + it.Purl
		}
		out = append(out, LinkResult{
			// 单文件取链：结果恒对应请求的那首歌，直接用请求 mid 归档。
			// 不能用回包里的 songmid——EVkey 加密通道按 lyrune 语义以 media_mid 作 songmid 发请求，
			// 上游会原样回 media_mid；media_mid != mid 的歌若按回包归档，pickLink(links, mid)
			// 会全部落空，加密档就只剩批量兜底那一档能播（历史症状「只有 HD OGG 能解」）。
			Mid:        mid,
			Playable:   playable,
			URL:        url,
			Filename:   it.Filename,
			ResultCode: it.Result,
			// 加密链路保留 ekey 供解密器使用；明文链路即便上游误回也显式丢弃
			Ekey:      pickEkey(enc, it.Ekey),
			Encrypted: enc,
		})
	}
	return out, nil
}

func pickEkey(encrypted bool, ekey string) string {
	if !encrypted {
		return ""
	}
	return ekey
}

// cdnDomain 取 CDN 域名（缓存 1 小时，随机挑选，失败回退默认）。
func (p *QQMusicProvider) cdnDomain(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	if p.cdnDomainsAt > 0 && now-p.cdnDomainsAt < 3600 && len(p.cdnDomains) > 0 {
		return p.cdnDomains[rand.Intn(len(p.cdnDomains))], nil
	}
	raw, err := p.song.GetCdnDispatch()
	if err != nil {
		p.cdnDomainsAt = now // 失败也记账，避免每次请求都打一次失败的 dispatch
		return CDNFallback, nil
	}
	var dispatch struct {
		Sip []string `json:"sip"`
	}
	if err := json.Unmarshal(raw, &dispatch); err != nil || len(dispatch.Sip) == 0 {
		p.cdnDomainsAt = now
		return CDNFallback, nil
	}
	p.cdnDomains = dispatch.Sip
	p.cdnDomainsAt = now
	return p.cdnDomains[rand.Intn(len(p.cdnDomains))], nil
}

// normalizeDomain 补全 scheme。
func normalizeDomain(domain string) string {
	if domain == "" {
		return CDNFallback
	}
	if !strings.HasPrefix(domain, "http") {
		domain = "https://" + domain
	}
	if !strings.HasSuffix(domain, "/") {
		domain += "/"
	}
	return domain
}

// streamURL 拼接 CDN 域名与 purl（dispatch 失败时用兜底域名）。
func (p *QQMusicProvider) streamURL(ctx context.Context, purl string) string {
	domain, _ := p.cdnDomain(ctx)
	return domain + purl
}
