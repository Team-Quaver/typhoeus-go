package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

func TestCacheIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("open: %v", err)
	}
	item := cacheEntry{ID: randomTag(), MidMAC: cacheMidMAC(c.seed, "m1"), Size: 100}
	c.idx.Items = append(c.idx.Items, item)
	c.idx.Bytes = 100
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}

	reloaded := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	if ok, err := reloaded.openLocked(true); err != nil || !ok {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.idx.Items) != 1 || reloaded.idx.Items[0].ID != item.ID {
		t.Fatalf("index mismatch: %+v", reloaded.idx)
	}
}

func TestCacheIndexObfuscatedAndEncrypted(t *testing.T) {
	dir := t.TempDir()
	c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	_, _ = c.openLocked(false)
	c.idx.Items = append(c.idx.Items, cacheEntry{ID: "cached-id", MidMAC: []byte("mac")})
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, cacheIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("cached-id")) || bytes.Contains(raw, []byte(`"items"`)) {
		t.Fatal("index leaks plaintext")
	}
	if raw[16] == raw[17] && raw[17] == raw[18] && raw[18] == raw[19] {
		t.Fatal("header looks non-random")
	}
}

func TestCacheMidMatchAndLimit(t *testing.T) {
	seed := "0123456789abcdef0123456789abcdef"
	if !cacheSameMAC(cacheMidMAC(seed, "a"), cacheMidMAC(seed, "a")) {
		t.Fatal("same mid must match")
	}
	if cacheSameMAC(cacheMidMAC(seed, "a"), cacheMidMAC(seed, "b")) {
		t.Fatal("different mids must not match")
	}
	if cacheKey(seed, seed, "x") == nil {
		t.Fatal("valid seeds must derive a key")
	}
	if cacheKey("short", seed, "x") != nil {
		t.Fatal("short disk seed must be rejected")
	}
}

func TestCacheOnlyHitsExactTier(t *testing.T) {
	dir := t.TempDir()
	c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("open: %v", err)
	}
	c.idx.Items = append(c.idx.Items, cacheEntry{
		ID: randomTag(), UserID: -1, MidMAC: cacheMidMAC(c.seed, "m1"), Tier: "320", Size: 100,
	})
	app := &App{musicCache: c, session: &Session{cred: &qqmusic.Credential{}}}
	seed := "0123456789abcdef0123456789abcdef"

	tok, item, err := app.cacheTryResolve("m1", seed, "320")
	if err != nil || tok == "" || item == nil || item.Tier != "320" {
		t.Fatalf("same tier must hit: tok=%q item=%+v err=%v", tok, item, err)
	}
	if tok, item, err := app.cacheTryResolve("m1", seed, "flac"); err != nil || tok != "" || item != nil {
		t.Fatalf("different tier must miss: tok=%q item=%+v err=%v", tok, item, err)
	}
}

func TestStreamResolveCacheHitKeepsExactTier(t *testing.T) {
	dir := t.TempDir()
	c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("open: %v", err)
	}
	app := &App{musicCache: c, session: &Session{cred: &qqmusic.Credential{}}}
	seed := "0123456789abcdef0123456789abcdef"
	if err := app.cacheSetSeed(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.idx.Items = append(c.idx.Items, cacheEntry{
		ID: randomTag(), UserID: -1, MidMAC: cacheMidMAC(c.seed, "m1"), Tier: "320", Mime: "audio/mpeg", Size: 100,
	})
	body := `{"mid":"m1","media_mid":"m1","tier":"320","auto":true,"cache_seed":"` + seed + `"}`
	rec := httptest.NewRecorder()

	app.handleStreamResolve(rec, httptest.NewRequest(http.MethodPost, "/stream/resolve", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Tier      string `json:"tier"`
			TierLabel string `json:"tier_label"`
			Degraded  bool   `json:"degraded"`
			Cached    bool   `json:"cached"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Tier != "320" || out.Data.TierLabel != "高品质 HQ" || !out.Data.Cached || out.Data.Degraded {
		t.Fatalf("unexpected cache response: %+v", out.Data)
	}
}

func TestCacheStoresTierVariantsSeparately(t *testing.T) {
	dir := t.TempDir()
	c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("open: %v", err)
	}
	mac := cacheMidMAC(c.seed, "m1")
	c.idx.Items = append(c.idx.Items,
		cacheEntry{ID: randomTag(), UserID: -1, MidMAC: mac, Tier: "320"},
		cacheEntry{ID: randomTag(), UserID: -1, MidMAC: mac, Tier: "flac"},
	)
	app := &App{musicCache: c, session: &Session{cred: &qqmusic.Credential{}}}
	seed := "0123456789abcdef0123456789abcdef"

	for _, tier := range []string{"320", "flac"} {
		if tok, item, err := app.cacheTryResolve("m1", seed, tier); err != nil || tok == "" || item == nil || item.Tier != tier {
			t.Fatalf("%s must hit its own cache: tok=%q item=%+v err=%v", tier, tok, item, err)
		}
	}
}

func TestMusicCacheCleanupRemovesTempAndOrphans(t *testing.T) {
	c := &musicCache{dir: t.TempDir(), readers: map[string]*cacheReader{}}
	keep := "0123456789abcdef0123456789abcdef"
	keepName := keep + cacheFileExt
	if err := os.WriteFile(filepath.Join(c.dir, keepName), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, ".interrupted.tmp"), []byte("tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "orphan"+cacheFileExt), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.idx.Items = []cacheEntry{{ID: keep}}

	c.cleanupLocked()

	if _, err := os.Stat(filepath.Join(c.dir, keepName)); err != nil {
		t.Fatalf("referenced cache file was removed: %v", err)
	}
	for _, name := range []string{".interrupted.tmp", "orphan" + cacheFileExt} {
		if _, err := os.Stat(filepath.Join(c.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed, stat err = %v", name, err)
		}
	}
}
