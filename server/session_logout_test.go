package server

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

type blockingTransport struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(b.started)
	<-b.release

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"code":0,"req_0":{"code":0,"data":{}}}`)),
		Header:     make(http.Header),
	}, nil
}

func testSession(mode CredentialMode, transport http.RoundTripper) (*Session, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cred := &qqmusic.Credential{}
	cl, err := qqmusic.NewClient(cred, "", "", qqmusic.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		panic(err)
	}

	return &Session{
		mode: mode,
		cred: cred,
		cl:   cl,
		out:  bufio.NewWriter(out),
	}, out
}

func TestLogoutClearsCredentialBeforeUpstream(t *testing.T) {
	transport := &blockingTransport{started: make(chan struct{}), release: make(chan struct{})}
	s, out := testSession(ModeExternal, transport)
	s.setCred(&qqmusic.Credential{MusicID: 1001, MusicKey: "KEY", LoginType: 2})

	loggedOut := make(chan error, 1)
	go func() { loggedOut <- s.Logout() }()

	select {
	case <-transport.started:
	case <-time.After(2 * time.Second):
		t.Fatal("上游注销没有启动")
	}

	select {
	case err := <-loggedOut:
		if err != nil {
			t.Fatalf("logout: %v", err)
		}
	default:
		t.Fatal("上游请求仍被挡住时，Logout 必须先返回")
	}

	if s.LoggedIn() {
		t.Fatal("logout 返回前本地凭证必须已清空")
	}
	if got := out.String(); got != HandoffPrefix+"null\n" {
		t.Fatalf("external 模式必须交接 null: %q", got)
	}
	close(transport.release)
}

func TestLateRefreshCannotRestoreLoggedOutCredential(t *testing.T) {
	s, _ := testSession(ModeMemory, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}))
	old := &qqmusic.Credential{MusicID: 1001, MusicKey: "OLD_KEY", LoginType: 2}
	s.setCred(old)
	_, gen := s.CredentialWithGeneration()

	if err := s.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if s.AdoptIfGeneration(gen, &qqmusic.Credential{MusicID: 1001, MusicKey: "REFRESHED", LoginType: 2}) {
		t.Fatal("登出后晚到的刷新结果不得写回会话")
	}
	if s.LoggedIn() {
		t.Fatal("旧账号被刷新结果复活")
	}
}

func TestRefreshAfterAccountSwitchCannotOverwriteNewAccount(t *testing.T) {
	s, _ := testSession(ModeMemory, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}))
	s.setCred(&qqmusic.Credential{MusicID: 1001, MusicKey: "OLD_KEY", LoginType: 2})
	_, gen := s.CredentialWithGeneration()

	s.Adopt(&qqmusic.Credential{MusicID: 2002, MusicKey: "NEW_KEY", LoginType: 2})
	if s.AdoptIfGeneration(gen, &qqmusic.Credential{MusicID: 1001, MusicKey: "REFRESHED", LoginType: 2}) {
		t.Fatal("换号后晚到的旧刷新结果不得覆盖新账号")
	}
	if got := s.Credential().MusicID; got != 2002 {
		t.Fatalf("新账号被覆盖: %d", got)
	}
}

func TestLateRefreshFailureCannotRejectNewAccount(t *testing.T) {
	s, _ := testSession(ModeMemory, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}))
	s.setCred(&qqmusic.Credential{MusicID: 1001, MusicKey: "OLD_KEY", LoginType: 2})
	_, gen := s.CredentialWithGeneration()

	s.Adopt(&qqmusic.Credential{MusicID: 2002, MusicKey: "NEW_KEY", LoginType: 2})
	if s.RejectIfGeneration(gen, "旧账号凭证失效") {
		t.Fatal("旧账号的刷新失败不得踢掉新账号")
	}
	if got := s.Credential().MusicID; got != 2002 {
		t.Fatalf("新账号被旧失败结果清除: %d", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
