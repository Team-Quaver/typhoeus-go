package typhoeus

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
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

	mu           sync.Mutex
	membership   Membership
	membershipAt int64
	cdnDomains   []string
	cdnDomainsAt int64
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
	p.mu.Unlock()
}

// Membership 当前会员等级（带 10 分钟缓存；未登录/查询失败按最低门槛处理）。
func (p *QQMusicProvider) Membership(ctx context.Context) Membership {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	if p.membershipAt > 0 && now-p.membershipAt < membershipTTL {
		return p.membership
	}
	level := p.fetchMembership(ctx)
	p.membership = level
	p.membershipAt = now
	return level
}

// fetchMembership 实时查询。
// 区分「未登录」与「登录但非会员」：未登录直接 0，省一次必然失败的请求。
func (p *QQMusicProvider) fetchMembership(ctx context.Context) Membership {
	if !p.cl.Credential().HasLogin() {
		return MembershipNone
	}
	raw, err := p.user.GetVipInfo()
	if err != nil {
		// 拿不到会员信息按最低门槛处理：档位请求仍可能成功（免费曲可播低档）
		return MembershipNone
	}
	var vip struct {
		Svip     int64 `json:"svip"`
		HugeVip  int64 `json:"huge_vip"`
		Identity struct {
			Vip     int64 `json:"vip"`
			HugeVip int64 `json:"huge_vip"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(raw, &vip); err != nil {
		return MembershipNone
	}
	if vip.Svip != 0 {
		return MembershipSuper
	}
	if vip.HugeVip != 0 || vip.Identity.HugeVip != 0 || vip.Identity.Vip != 0 {
		return MembershipGreen
	}
	return MembershipNone
}

// resolveLinks 取链（明文/加密通道按 fileType 前缀自动路由）。
func (p *QQMusicProvider) resolveLinks(ctx context.Context, mid, mediaMid string, ft modules.SongFileType) ([]LinkResult, error) {
	files := []modules.SongFileInfo{{Mid: mid, MediaMid: mediaMid, FileType: &ft}}
	raw, err := p.song.GetSongURLs(files, ft, qqmusic.CGIOption{})
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
			Mid:        it.Songmid,
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
