package typhoeus

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
	"github.com/team-quaver/typhoeus-go/qqmusic/modules"
)

type membershipTransport func(*http.Request) (*http.Response, error)

func (f membershipTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testMembershipProvider(t *testing.T) *QQMusicProvider {
	t.Helper()
	cl, err := qqmusic.NewClient(&qqmusic.Credential{MusicID: 123, MusicKey: "TEST", LoginType: 2}, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: membershipTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"req_0":{"code":10006}}`))}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	return NewQQMusicProvider(cl)
}
func TestMembershipExpiryDuringCache(t *testing.T) {
	p := testMembershipProvider(t)
	p.membershipAt = time.Now().Unix()
	p.membershipAccount = 123
	p.membershipInfo = modules.VipInfo{Svip: 1, SvipEnd: "2025-01-01", Identity: modules.VipIdentity{HugeVip: 1, HugeVipEnd: "2099-01-01"}}
	if got, err := p.Membership(context.Background()); err != nil || got != MembershipGreen {
		t.Fatalf("expired super must fall back to valid green: %v %v", got, err)
	}
	p.membershipInfo.Identity.HugeVipEnd = "2025-01-01"
	if got, err := p.Membership(context.Background()); err != nil || got != MembershipNone {
		t.Fatalf("cached flag must expire: %v %v", got, err)
	}
	p.membershipInfo.SvipEnd = "2099-01-01"
	if got, err := p.Membership(context.Background()); err != nil || got != MembershipSuper {
		t.Fatalf("super: %v %v", got, err)
	}
}
func TestMembershipFailureDoesNotDemote(t *testing.T) {
	p := testMembershipProvider(t)
	p.membershipAt = time.Now().Unix() - membershipTTL - 1
	p.membershipAccount = 123
	p.membershipInfo = modules.VipInfo{Svip: 1, SvipEnd: "2099-01-01"}
	for i := 0; i < 2; i++ {
		got, err := p.Membership(context.Background())
		if err != nil || got != MembershipSuper {
			t.Fatalf("query failure demoted cached super: %v %v", got, err)
		}
	}
	if p.membershipErr == nil || p.membershipRetryAt == 0 {
		t.Fatal("failure must be tracked and retried, not cached as success")
	}
	p.membershipInfo.SvipEnd = "2025-01-01"
	if got, err := p.Membership(context.Background()); err != nil || got != MembershipNone {
		t.Fatalf("stale cache cannot keep expired super: %v %v", got, err)
	}
	p.membershipInfo.SvipEnd = "2099-01-01"
	p.membershipAt = time.Now().Unix() - 2*membershipTTL - 1
	if _, err := p.Membership(context.Background()); err == nil {
		t.Fatal("stale cache must have a finite lifetime")
	}
}
func TestMembershipFailureWithoutCacheIsError(t *testing.T) {
	p := testMembershipProvider(t)
	if _, err := p.Membership(context.Background()); err == nil {
		t.Fatal("unknown membership is not a successful nonmember query")
	}
	if p.membershipAt != 0 {
		t.Fatal("must not cache zero membership on failure")
	}
	// 播放解析必须立即返回查询失败，不能进入低档回退链。
	_, err := NewStreamResolver(p).Resolve(context.Background(), "mid", "mid", "flac", true, nil, "", 0)
	if err == nil || !strings.Contains(err.Error(), "会员权益查询") {
		t.Fatalf("resolver must propagate membership failure: %v", err)
	}
}
func TestMembershipCacheCannotCrossAccounts(t *testing.T) {
	p := testMembershipProvider(t)
	p.membershipAt = time.Now().Unix()
	p.membershipAccount = 123
	p.membershipInfo = modules.VipInfo{Svip: 1, SvipEnd: "2099-01-01"}
	p.cl.SetCredential(&qqmusic.Credential{MusicID: 456, MusicKey: "OTHER", LoginType: 2})
	if _, err := p.Membership(context.Background()); err == nil {
		t.Fatal("another account must not inherit cached super")
	}
	p.InvalidateMembership()
	if p.membershipInfo.Svip != 0 || p.membershipAt != 0 || p.membershipErr != nil {
		t.Fatal("invalidation must clear all entitlement state")
	}
	p.cl.SetCredential(nil)
	if got, err := p.Membership(context.Background()); got != MembershipNone || err != nil {
		t.Fatal("logout must be nonmember")
	}
}
