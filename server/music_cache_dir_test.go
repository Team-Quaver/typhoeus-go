package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// 缓存目录与配置目录分家：Linux 走 XDG_CACHE_HOME，Windows/macOS 用带 "Quaver Cache"
// 后缀的名字与 Chromium 的 Cache 区分开。两侧规则必须与 ui/electron/config.ts 一致。
func TestCacheDirSeparatesFromConfigDir(t *testing.T) {
	cfg := t.TempDir()
	cache := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", cfg)
	t.Setenv("QUAVER_CACHE_DIR", cache)

	if got := CacheDir(); got != cache {
		t.Fatalf("QUAVER_CACHE_DIR override ignored: %q", got)
	}
	if got := musicCacheDir(); got != filepath.Join(cache, "music") {
		t.Fatalf("music cache must live under <cache>/music, got %q", got)
	}
	if strings.HasPrefix(musicCacheDir(), cfg) {
		t.Fatal("music cache must not live inside the config dir")
	}
	if got := LegacyMusicCacheDir(); got != filepath.Join(cfg, "cache", "music") {
		t.Fatalf("legacy dir must stay in the config dir, got %q", got)
	}
}

func TestCacheDirPlatformDefaults(t *testing.T) {
	t.Setenv("QUAVER_CACHE_DIR", "")
	t.Setenv("QUAVER_CONFIG_DIR", t.TempDir())

	switch runtime.GOOS {
	case "linux":
		t.Setenv("XDG_CACHE_HOME", "/xdg-cache")
		t.Setenv("XDG_CONFIG_HOME", "/xdg-config")
		if got := CacheDir(); got != filepath.Join("/xdg-cache", "quaver-music") {
			t.Fatalf("Linux must follow XDG_CACHE_HOME, got %q", got)
		}
	case "windows":
		t.Setenv("LOCALAPPDATA", `C:\Users\me\AppData\Local`)
		want := filepath.Join(`C:\Users\me\AppData\Local`, "Quaver Music", "Quaver Cache")
		if got := CacheDir(); got != want {
			t.Fatalf("Windows cache dir = %q, want %q", got, want)
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		if got := CacheDir(); got != filepath.Join(home, "Library", "Caches", "Quaver Music") {
			t.Fatalf("macOS must use ~/Library/Caches, got %q", got)
		}
	}
}

// 升级路径：旧版落在配置目录里的缓存整目录搬到默认缓存目录，旧目录不再压着字节。
func TestCacheAdoptsLegacyConfigDirOnce(t *testing.T) {
	cfg := t.TempDir()
	cache := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", cfg)
	t.Setenv("QUAVER_CACHE_DIR", cache)

	legacy := LegacyMusicCacheDir()
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &musicCache{dir: legacy, readers: map[string]*cacheReader{}}
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("seed legacy cache: %v", err)
	}
	id := randomTag()
	if err := os.WriteFile(filepath.Join(legacy, id+cacheFileExt), []byte("track"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.idx.Items = []cacheEntry{{ID: id, Size: 5}}
	c.idx.Bytes = 5
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	// 旧目录里还放一个不属于本缓存的文件：搬家与清扫都不该碰它。
	foreign := filepath.Join(legacy, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved := newMusicCache()
	if ok, err := moved.openLocked(true); err != nil || !ok {
		t.Fatalf("open migrated cache: ok=%v err=%v", ok, err)
	}
	if len(moved.idx.Items) != 1 || moved.idx.Items[0].ID != id {
		t.Fatalf("index did not survive the move: %+v", moved.idx)
	}
	if _, err := os.Stat(filepath.Join(moved.dir, id+cacheFileExt)); err != nil {
		t.Fatalf("track file not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, cacheIndexName)); !os.IsNotExist(err) {
		t.Fatalf("legacy index must be gone, stat err = %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file must be left alone: %v", err)
	}
}

// 自定义位置是用户显式指定的目录：不该被旧数据塞满，也不该把旧缓存搬走。
func TestCacheCustomDirDoesNotAdoptLegacy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", cfg)
	t.Setenv("QUAVER_CACHE_DIR", t.TempDir())

	legacy := LegacyMusicCacheDir()
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	old := &musicCache{dir: legacy, readers: map[string]*cacheReader{}}
	if _, err := old.openLocked(false); err != nil {
		t.Fatal(err)
	}

	custom := filepath.Join(t.TempDir(), "music")
	c := &musicCache{dir: custom, readers: map[string]*cacheReader{}}
	if _, err := c.openLocked(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, cacheIndexName)); !os.IsNotExist(err) {
		t.Fatalf("custom dir must stay empty until something is cached, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, cacheIndexName)); err != nil {
		t.Fatalf("legacy cache must be untouched: %v", err)
	}
}

// 换位置：已有缓存跟着搬，旧目录收干净，旧播放令牌作废（里面存的是绝对路径）。
func TestCacheSetDirMovesDataAndDropsTokens(t *testing.T) {
	cfg := t.TempDir()
	first := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", cfg)
	t.Setenv("QUAVER_CACHE_DIR", first)

	app := &App{musicCache: newMusicCache(), cacheLimit: &atomic.Int64{}, session: &Session{cred: &qqmusic.Credential{}}}
	c := app.musicCache
	if ok, err := c.openLocked(false); err != nil || !ok {
		t.Fatalf("open: %v", err)
	}
	id := randomTag()
	if err := os.WriteFile(filepath.Join(c.dir, id+cacheFileExt), []byte("track"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.idx.Items = []cacheEntry{{ID: id, Size: 5}}
	c.idx.Bytes = 5
	if err := c.saveLocked(); err != nil {
		t.Fatal(err)
	}
	c.readers["old-token"] = &cacheReader{filePath: filepath.Join(c.dir, id+cacheFileExt)}
	from := c.dir

	next := t.TempDir()
	dir, err := app.cacheSetDir(next)
	if err != nil {
		t.Fatalf("set dir: %v", err)
	}
	if dir != filepath.Join(next, "music") {
		t.Fatalf("dir = %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, id+cacheFileExt)); err != nil {
		t.Fatalf("track not moved to the new location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(from, id+cacheFileExt)); !os.IsNotExist(err) {
		t.Fatalf("old location must not keep the bytes, stat err = %v", err)
	}
	if app.cacheResolvePlayback("old-token") != nil {
		t.Fatal("tokens pointing at the old location must be invalidated")
	}
	if ok, err := c.openLocked(true); err != nil || !ok || len(c.idx.Items) != 1 {
		t.Fatalf("index must reload from the new location: ok=%v err=%v items=%d", ok, err, len(c.idx.Items))
	}

	// 空串 = 回到平台默认（此处由 QUAVER_CACHE_DIR 给出）
	back, err := app.cacheSetDir("")
	if err != nil || back != filepath.Join(first, "music") {
		t.Fatalf("empty dir must restore the platform default: %q err=%v", back, err)
	}
	if _, err := os.Stat(filepath.Join(back, id+cacheFileExt)); err != nil {
		t.Fatalf("track must move back: %v", err)
	}
}

// 切到一个「已经有自己一份缓存」的目录：两份都必须留着。覆盖/清扫会删掉用户另一处的
// 真实缓存 —— 那是数据丢失，不是整理。
func TestCacheSetDirKeepsBothWhenTargetHasCache(t *testing.T) {
	cfg := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv("QUAVER_CONFIG_DIR", cfg)
	t.Setenv("QUAVER_CACHE_DIR", first)

	seedDir := func(dir string) string {
		c := &musicCache{dir: dir, readers: map[string]*cacheReader{}}
		if ok, err := c.openLocked(false); err != nil || !ok {
			t.Fatalf("seed %s: %v", dir, err)
		}
		id := randomTag()
		if err := os.WriteFile(filepath.Join(dir, id+cacheFileExt), []byte("track"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.idx.Items = []cacheEntry{{ID: id, Size: 5}}
		c.idx.Bytes = 5
		if err := c.saveLocked(); err != nil {
			t.Fatal(err)
		}
		return id
	}

	app := &App{musicCache: newMusicCache(), cacheLimit: &atomic.Int64{}, session: &Session{cred: &qqmusic.Credential{}}}
	from := app.cacheDir()
	idA := seedDir(from)
	idB := seedDir(filepath.Join(second, "music"))

	if _, err := app.cacheSetDir(second); err != nil {
		t.Fatalf("set dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "music", idB+cacheFileExt)); err != nil {
		t.Fatalf("target cache must stay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(from, idA+cacheFileExt)); err != nil {
		t.Fatalf("source cache must not be deleted when target already has one: %v", err)
	}
	if _, err := os.Stat(filepath.Join(from, cacheIndexName)); err != nil {
		t.Fatalf("source index must stay: %v", err)
	}
}

func TestCacheSetDirRejectsRelativeAndUnwritable(t *testing.T) {
	t.Setenv("QUAVER_CONFIG_DIR", t.TempDir())
	t.Setenv("QUAVER_CACHE_DIR", t.TempDir())
	app := &App{musicCache: newMusicCache(), cacheLimit: &atomic.Int64{}, session: &Session{cred: &qqmusic.Credential{}}}
	before := app.cacheDir()

	if _, err := app.cacheSetDir("relative/cache"); err == nil {
		t.Fatal("relative path must be rejected")
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.cacheSetDir(file); err == nil {
		t.Fatal("a path under a regular file must be rejected")
	}
	if app.cacheDir() != before {
		t.Fatal("failed relocation must not change the active dir")
	}
}

func TestMusicCacheDirHandlerReportsLocation(t *testing.T) {
	t.Setenv("QUAVER_CONFIG_DIR", t.TempDir())
	t.Setenv("QUAVER_CACHE_DIR", t.TempDir())
	app := &App{musicCache: newMusicCache(), cacheLimit: &atomic.Int64{}, session: &Session{cred: &qqmusic.Credential{}}}

	rec := httptest.NewRecorder()
	app.handleMusicCacheStatus(rec, httptest.NewRequest(http.MethodGet, "/cache/music/status", nil))
	var status struct {
		Data struct {
			Dir string `json:"dir"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Data.Dir != app.cacheDir() {
		t.Fatalf("status must report the live dir: %q vs %q", status.Data.Dir, app.cacheDir())
	}

	target := t.TempDir()
	body := strings.NewReader(`{"dir":` + quoteJSON(target) + `}`)
	rec = httptest.NewRecorder()
	app.handleMusicCacheSetDir(rec, httptest.NewRequest(http.MethodPost, "/cache/music/dir", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("set dir status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Dir string `json:"dir"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Dir != filepath.Join(target, "music") {
		t.Fatalf("handler dir = %q", out.Data.Dir)
	}

	rec = httptest.NewRecorder()
	app.handleMusicCacheSetDir(rec, httptest.NewRequest(http.MethodPost, "/cache/music/dir", strings.NewReader(`{"dir":"relative/x"}`)))
	if rec.Code != 422 {
		t.Fatalf("relative path must be 422, got %d", rec.Code)
	}
}

// 「路径不从渲染层来」红线：中继透传 Sec-Fetch-Dest，带该头 = 页面发起，一律 403。
// 渲染层改的是 quaver.conf 的配置键，由主进程解析后下发。
func TestMusicCacheSetDirBlocksRenderer(t *testing.T) {
	t.Setenv("QUAVER_CONFIG_DIR", t.TempDir())
	t.Setenv("QUAVER_CACHE_DIR", t.TempDir())
	app := &App{musicCache: newMusicCache(), cacheLimit: &atomic.Int64{}, session: &Session{cred: &qqmusic.Credential{}}}
	before := app.cacheDir()
	target := t.TempDir()

	for _, dest := range []string{"empty", "document", "audio"} {
		req := httptest.NewRequest(http.MethodPost, "/cache/music/dir",
			strings.NewReader(`{"dir":`+quoteJSON(target)+`}`))
		req.Header.Set("Sec-Fetch-Dest", dest)
		rec := httptest.NewRecorder()

		app.handleMusicCacheSetDir(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Dest=%s must be 403, got %d", dest, rec.Code)
		}
		if app.cacheDir() != before {
			t.Fatalf("blocked request changed the dir (dest=%s)", dest)
		}
	}
}

func quoteJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
