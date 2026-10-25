package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
