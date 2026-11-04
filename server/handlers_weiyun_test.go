package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/qqmusic"
)

func TestWeiyunCipherDecryptsSeekOffset(t *testing.T) {
	plain := bytes.Repeat([]byte("weiyun-qmc-v1-range-test-"), 9000)
	cipherText := append([]byte(nil), plain...)
	qmc.DecryptV1InPlace(cipherText, 0)
	offset := int64(0x8000 + 17)
	// Copy-style Decrypt must preserve the absolute QMCv1 offset across seeks.
	got := weiyunCipher{}.Decrypt(cipherText[offset:], offset)
	if !bytes.Equal(got, plain[offset:]) {
		t.Fatal("weiyun cipher failed to decrypt at a seek offset")
	}
}

func TestWeiyunMusicFileBlocksBrowserNavigation(t *testing.T) {
	app := &App{weiyunStore: &weiyunStore{}}
	req := httptest.NewRequest(http.MethodGet, "/weiyun/music/file-1", nil)
	req.SetPathValue("fileID", "file-1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()

	app.handleWeiyunMusicFile(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("navigation must be blocked before any Weiyun access, got %d", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Body); len(body) == 0 {
		t.Fatal("error body must be written")
	}
}

func TestWeiyunStatusWithoutToken(t *testing.T) {
	t.Setenv("QUAVER_CONFIG_DIR", t.TempDir())
	app := &App{weiyunStore: &weiyunStore{}, session: &Session{cred: &qqmusic.Credential{}}}
	rec := httptest.NewRecorder()
	app.handleWeiyunStatus(rec, httptest.NewRequest(http.MethodGet, "/weiyun/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Data struct {
			Configured bool `json:"configured"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Configured {
		t.Fatal("empty config dir must not report configured")
	}
}

func TestWeiyunAuthStoredWithPrivateMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", dir)
	s := &weiyunStore{}
	if err := s.save("secret-token", "https://example.invalid/mcp"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "weiyun-auth.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "QQMusic") {
		t.Fatal("weiyun token file must not mix QQ Music credential fields")
	}
	got := s.load()
	if got == nil || got.Token != "secret-token" {
		t.Fatalf("loaded = %+v", got)
	}
	if err := s.clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists: %v", err)
	}
}
