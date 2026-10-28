package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// 音乐网盘：列表来自 GetMatchAudio；自定义歌词/封面与刮削结果落在配置目录
// cloud-library/ 下（0600/0700）。云盘条目没有可持久化的本地文件路径，因此所有
// 资源端点只认条目 key（sha1(file_id)），请求参数不参与拼路径。

const (
	cloudMaxPages            = 10
	cloudDefaultLimit        = 200
	cloudMaxLimit            = 1000
	cloudMatchedOriginalFile = 2
	cloudLyricMaxBytes       = 512 * 1024
	cloudCoverMaxBytes       = 8 * 1024 * 1024
)

type cloudScrapeState struct {
	Running    bool   `json:"running"`
	Total      int    `json:"total"`
	Processed  int    `json:"processed"`
	Matched    int    `json:"matched"`
	Unmatched  int    `json:"unmatched"`
	Failed     int    `json:"failed"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	LastError  string `json:"lastError"`
}

type cloudMeta struct {
	Key           string `json:"key"`
	FileID        string `json:"-"`
	FileName      string `json:"fileName"`
	AddTime       int64  `json:"addTime"`
	Matched       int    `json:"matched"`
	Playable      bool   `json:"playable"`
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	Album         string `json:"album"`
	SingerMid     string `json:"singerMid"`
	AlbumMid      string `json:"albumMid"`
	AlbumPmid     string `json:"albumPmid"`
	CoverQQ       bool   `json:"coverQq"`
	LyricQQ       bool   `json:"lyricQq"`
	ScrapedAt     int64  `json:"scrapedAt"`
	EditedMeta    bool   `json:"editedMeta"`
	CoverManual   bool   `json:"coverManual"`
	LyricImport   bool   `json:"lyricImport"`
	ImportFormat  string `json:"importFormat"`
	PlaybackState string `json:"playbackState"`
	PlaybackError string `json:"playbackError"`
}

type cloudIndex struct {
	Version int                   `json:"version"`
	Items   map[string]*cloudMeta `json:"items"`
}

type cloudEntry struct {
	Key      string
	FileID   string
	FileName string
	AddTime  int64
	Matched  int
	Playable bool
	Song     json.RawMessage
	Meta     *cloudMeta
}

type cloudLibrarySnapshot struct {
	Items  []cloudItemView  `json:"items"`
	Count  int              `json:"count"`
	Stats  cloudStats       `json:"stats"`
	Scrape cloudScrapeState `json:"scrape"`
}

type cloudStats struct {
	Total     int `json:"total"`
	Playable  int `json:"playable"`
	WithCover int `json:"withCover"`
	WithLyric int `json:"withLyric"`
	Scraped   int `json:"scraped"`
}

type cloudItemView struct {
	Key           string          `json:"key"`
	FileID        string          `json:"fileId"`
	FileName      string          `json:"fileName"`
	AddTime       int64           `json:"addTime"`
	Matched       int             `json:"matched"`
	Playable      bool            `json:"playable"`
	Title         string          `json:"title"`
	Artist        string          `json:"artist"`
	Album         string          `json:"album"`
	SingerMid     string          `json:"singerMid"`
	AlbumMid      string          `json:"albumMid"`
	AlbumPmid     string          `json:"albumPmid"`
	CoverQq       bool            `json:"coverQq"`
	LyricQq       bool            `json:"lyricQq"`
	ScrapedAt     int64           `json:"scrapedAt"`
	EditedMeta    bool            `json:"editedMeta"`
	CoverManual   bool            `json:"coverManual"`
	LyricImport   bool            `json:"lyricImport"`
	ImportFormat  string          `json:"importFormat"`
	HasCover      bool            `json:"hasCover"`
	HasLyric      bool            `json:"hasLyric"`
	PlaybackState string          `json:"playbackState"`
	PlaybackError string          `json:"playbackError"`
	Song          json.RawMessage `json:"song,omitempty"`
}

// key 由上游 file_id 哈希而来，只允许 40 位 hex；不让请求参数进入文件路径。
var cloudKeyRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func cloudRoot() string {
	return filepath.Join(ConfigDir(), "cloud-library")
}

func cloudIndexFile() string {
	return filepath.Join(cloudRoot(), "library.json")
}

func cloudCoversDir() string {
	return filepath.Join(cloudRoot(), "covers")
}

func cloudLyricsDir() string {
	return filepath.Join(cloudRoot(), "lyrics")
}

func ensureCloudDirs() error {
	if err := os.MkdirAll(cloudRoot(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(cloudCoversDir(), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(cloudLyricsDir(), 0o700)
}

func writeCloudFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(path, mode)
	return nil
}

func cloudAccountKey(cred *qqmusic.Credential) string {
	if cred == nil {
		return "anonymous"
	}
	if cred.StrMusicID != "" {
		return cred.StrMusicID
	}
	if cred.MusicID != 0 {
		return strconv.FormatInt(cred.MusicID, 10)
	}
	return "anonymous"
}

func cloudKeyOf(accountID, fileID string) string {
	sum := sha1.Sum([]byte(accountID + "\x00" + fileID))
	return hex.EncodeToString(sum[:])
}

func cloudDecodeName(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	// 文件名是 base64(UTF-8)。只有严格解码出可打印文本才替换，否则原样保留。
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil || len(raw) == 0 || !utf8.Valid(raw) {
		return s
	}
	text := string(raw)
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return s
		}
	}
	return text
}

func cloudAddTime(v any) int64 {
	var n int64
	switch x := v.(type) {
	case json.Number:
		n, _ = x.Int64()
	case float64:
		n = int64(x)
	case int64:
		n = x
	case string:
		n, _ = strconv.ParseInt(x, 10, 64)
	}
	if n <= 0 {
		return 0
	}
	if n < 100000000000 {
		return n * 1000
	}
	return n
}

func cloudMatched(v any) int {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case float64:
		return int(x)
	case int64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

// cloudPlayable: a file_id is enough to locate and stream the original upload.
// matched=2 only means QQ Music has no library match; it does not make the
// Weiyun file itself unplayable.
func cloudPlayable(fileID string, _ int) bool {
	return strings.TrimSpace(fileID) != ""
}

func cloudSongObj(raw json.RawMessage) obj {
	if len(raw) == 0 {
		return nil
	}
	var o obj
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&o) != nil {
		return nil
	}
	return o
}

func cloudDisplayMeta(entry cloudEntry) cloudMeta {
	m := cloudMeta{}
	if entry.Meta != nil {
		m = *entry.Meta
	}
	m.Key = entry.Key
	m.FileID = entry.FileID
	m.FileName = entry.FileName
	m.AddTime = entry.AddTime
	m.Matched = entry.Matched
	m.Playable = entry.Playable
	if m.Title == "" || m.Artist == "" || m.Album == "" {
		s := cloudSongObj(entry.Song)
		if s != nil {
			if m.Title == "" {
				m.Title = strOf(s, "title", "name")
			}
			if m.Artist == "" {
				singers := arrOf(s, "singer")
				names := make([]string, 0, len(singers))
				for _, singer := range singers {
					if name := strOf(asObj(singer), "name"); name != "" {
						names = append(names, name)
					}
				}
				m.Artist = strings.Join(names, " / ")
			}
			if m.Album == "" {
				m.Album = strOf(objOf(s, "album"), "name")
			}
			if m.SingerMid == "" {
				singers := arrOf(s, "singer")
				if len(singers) == 1 {
					m.SingerMid = strOf(asObj(singers[0]), "mid")
				}
			}
			if m.AlbumMid == "" {
				album := objOf(s, "album")
				m.AlbumMid = strOf(album, "mid")
				m.AlbumPmid = strOf(album, "pmid")
			}
		}
	}
	if m.Title == "" {
		m.Title = strings.TrimSuffix(entry.FileName, filepath.Ext(entry.FileName))
	}
	if m.Artist == "" && m.Title != "" {
		title, artist := cloudParseFileName(entry.FileName)

		if artist != "" {
			m.Title = title
			m.Artist = artist
		}
	}
	if m.Title == "" {
		m.Title = entry.FileName
	}
	if m.PlaybackState == "" {
		if entry.Playable {
			m.PlaybackState = "matched"
		} else {
			m.PlaybackState = "blocked"
			m.PlaybackError = "未匹配到曲库，无法取流"
		}
	}
	return m
}

func (a *App) loadCloudIndex() *cloudIndex {
	a.cloudMu.RLock()
	if a.cloudIndex != nil {
		idx := a.cloudIndex
		a.cloudMu.RUnlock()
		return idx
	}
	a.cloudMu.RUnlock()

	a.cloudMu.Lock()
	defer a.cloudMu.Unlock()
	if a.cloudIndex != nil {
		return a.cloudIndex
	}
	idx := &cloudIndex{Version: 1, Items: map[string]*cloudMeta{}}
	if raw, err := os.ReadFile(cloudIndexFile()); err == nil {
		var loaded cloudIndex
		if json.Unmarshal(raw, &loaded) == nil && loaded.Items != nil {
			if loaded.Version == 0 {
				loaded.Version = 1
			}
			idx = &loaded
		}
	}
	a.cloudIndex = idx
	return idx
}

func (a *App) saveCloudIndexLocked() error {
	if err := ensureCloudDirs(); err != nil {
		return err
	}
	raw, err := json.Marshal(a.cloudIndex)
	if err != nil {
		return err
	}
	return writeCloudFile(cloudIndexFile(), raw, 0o600)
}

func (a *App) cloudMetaOf(key string) *cloudMeta {
	idx := a.loadCloudIndex()
	a.cloudMu.RLock()
	defer a.cloudMu.RUnlock()
	if item := idx.Items[key]; item != nil {
		cp := *item
		return &cp
	}
	return nil
}

func (a *App) cloudEntries(pageNo int) ([]cloudEntry, int, bool, error) {
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.cloud.GetMatchAudio(pageNo)
	})
	if err != nil {
		return nil, 0, false, err
	}
	d := decodeObj(raw)
	list, ok := d["info_list"].(arr)
	if !ok && d["info_list"] != nil {
		return nil, 0, false, qqmusic.NewDataError("网盘列表格式异常")
	}
	entries := make([]cloudEntry, 0, len(list))
	for _, item := range list {
		m := asObj(item)
		if m == nil {
			continue
		}
		fileID := strOf(m, "file_id")
		if fileID == "" {
			continue
		}
		songRaw, _ := json.Marshal(m["song_info"])
		matched := cloudMatched(firstOf(m, "matched"))
		playable := cloudPlayable(fileID, matched)
		key := cloudKeyOf(cloudAccountKey(a.session.Credential()), fileID)
		entries = append(entries, cloudEntry{
			Key: key, FileID: fileID, FileName: cloudDecodeName(firstOf(m, "filename")),
			AddTime: cloudAddTime(firstOf(m, "timestamp")), Matched: matched,
			Playable: playable, Song: songRaw, Meta: a.cloudMetaOf(key),
		})
	}
	hasMore := false
	if v := firstOf(d, "has_more"); v != nil {
		hasMore = nval(v) == 1
	}
	total := int(nval(firstOf(d, "total")))
	if total < len(entries) {
		total = len(entries)
	}
	return entries, total, hasMore, nil
}

func cloudPageParams(r *http.Request) (int, int) {
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	limit := queryInt(r, "limit", cloudDefaultLimit)
	if limit < 1 {
		limit = cloudDefaultLimit
	}
	if limit > cloudMaxLimit {
		limit = cloudMaxLimit
	}
	return offset, limit
}

func (a *App) handleUserCloud(w http.ResponseWriter, r *http.Request) {
	// 兼容上游 /user/cloud 路由形状；Quaver 页面使用 /cloud/library 拿合并后的条目。
	offset, limit := cloudPageParams(r)
	all := []cloudEntry{}
	total := 0
	hasMore := false
	for page := 1; page <= cloudMaxPages; page++ {
		entries, count, more, err := a.cloudEntries(page)
		if err != nil {
			writeError(w, err)
			return
		}
		total = count
		all = append(all, entries...)
		hasMore = more
		if !more || len(all) >= offset+limit {
			break
		}
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	data := make([]map[string]any, 0, max(0, end-offset))
	for _, e := range all[offset:end] {
		m := cloudDisplayMeta(e)
		data = append(data, map[string]any{
			"song":     e.Song,
			"fileName": m.FileName,
			"addTime":  m.AddTime,
			"playable": m.Playable,
		})
	}
	writeOK(w, map[string]any{"data": data, "count": total, "hasMore": end < total || hasMore})
}

func (a *App) handleCloudLibrary(w http.ResponseWriter, r *http.Request) {
	all := []cloudEntry{}
	total := 0
	for page := 1; page <= cloudMaxPages; page++ {
		entries, count, more, err := a.cloudEntries(page)
		if err != nil {
			writeError(w, err)
			return
		}
		total = count
		all = append(all, entries...)
		if !more {
			break
		}
	}
	items := make([]cloudItemView, 0, len(all))
	stats := cloudStats{Total: total, Scraped: 0}
	for _, e := range all {
		m := cloudDisplayMeta(e)
		hasCover := cloudCoverFile(e.Key, m) != ""
		hasLyric := m.LyricImport || fileExists(cloudLyricsDir(), e.Key+".json") || fileExists(cloudLyricsDir(), e.Key+".txt")
		playable := e.Playable && m.PlaybackState != "blocked"
		item := cloudItemView{
			Key: e.Key, FileName: m.FileName, AddTime: m.AddTime, Matched: e.Matched,
			Playable: playable, Title: m.Title, Artist: m.Artist, Album: m.Album,
			SingerMid: m.SingerMid, AlbumMid: m.AlbumMid, AlbumPmid: m.AlbumPmid,
			CoverQq: m.CoverQQ, LyricQq: m.LyricQQ, ScrapedAt: m.ScrapedAt,
			EditedMeta: m.EditedMeta, CoverManual: m.CoverManual,
			LyricImport: m.LyricImport, ImportFormat: m.ImportFormat,
			HasCover: hasCover, HasLyric: hasLyric,
			PlaybackState: m.PlaybackState, PlaybackError: m.PlaybackError,
			Song: e.Song, FileID: e.FileID,
		}
		items = append(items, item)
		if item.Playable {
			stats.Playable++
		}
		if item.ScrapedAt > 0 {
			stats.Scraped++
		}
		if item.HasCover {
			stats.WithCover++
		}
		if item.HasLyric {
			stats.WithLyric++
		}
	}
	writeOK(w, cloudLibrarySnapshot{Items: items, Count: total, Stats: stats, Scrape: a.cloudScrapeSnapshot()})
}

func fileExists(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && st.Mode().IsRegular()
}

func cloudCoverFile(key string, m cloudMeta) string {
	if !cloudKeyRe.MatchString(key) {
		return ""
	}
	name := cloudCoverFileName(key)

	if name == "" {
		return ""
	}
	return filepath.Join(cloudCoversDir(), name)
}

func cloudCoverFileName(key string) string {
	for _, ext := range []string{"jpg", "jpeg", "png", "gif", "webp"} {
		name := key + "." + ext

		if fileExists(cloudCoversDir(), name) {
			return name
		}
	}

	return ""
}

func (a *App) cloudScrapeSnapshot() cloudScrapeState {
	a.cloudScrapeMu.Lock()
	defer a.cloudScrapeMu.Unlock()
	return a.cloudScrape
}

func (a *App) handleCloudScrapeStatus(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, a.cloudScrapeSnapshot())
}

func (a *App) handleCloudScrapeStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys  []string `json:"keys"`
		All   bool     `json:"all"`
		Force bool     `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)

	a.cloudScrapeMu.Lock()
	if a.cloudScrapeRunning {
		state := a.cloudScrape
		a.cloudScrapeMu.Unlock()
		writeOK(w, map[string]any{"started": false, "busy": true, "total": state.Total})
		return
	}
	a.cloudScrapeRunning = true
	a.cloudScrape = cloudScrapeState{Running: true, StartedAt: time.Now().UnixMilli()}
	a.cloudScrapeMu.Unlock()

	go a.runCloudScrape(body.Keys, body.All, body.Force)
	writeOK(w, map[string]any{"started": true, "total": 0})
}

func (a *App) runCloudScrape(keys []string, all bool, force bool) {
	defer func() {
		a.cloudScrapeMu.Lock()
		a.cloudScrape.Running = false
		a.cloudScrape.FinishedAt = time.Now().UnixMilli()
		a.cloudScrapeRunning = false
		a.cloudScrapeMu.Unlock()
	}()
	entries := make([]cloudEntry, 0)
	for page := 1; page <= cloudMaxPages; page++ {
		batch, _, more, err := a.cloudEntries(page)
		if err != nil {
			a.cloudScrapeMu.Lock()
			a.cloudScrape.LastError = err.Error()
			a.cloudScrapeMu.Unlock()
			return
		}
		entries = append(entries, batch...)
		if !more {
			break
		}
	}
	want := map[string]bool{}
	for _, key := range keys {
		want[key] = true
	}
	targets := make([]cloudEntry, 0, len(entries))
	for _, e := range entries {
		if len(want) > 0 && !want[e.Key] {
			continue
		}
		m := cloudDisplayMeta(e)
		if len(want) == 0 && !force && m.ScrapedAt > 0 && m.CoverQQ && m.LyricQQ {
			continue
		}
		if len(want) == 0 && !all && m.ScrapedAt > 0 && (m.CoverQQ || m.LyricQQ) {
			continue
		}
		targets = append(targets, e)
	}
	a.cloudScrapeMu.Lock()
	a.cloudScrape.Total = len(targets)
	a.cloudScrapeMu.Unlock()
	for _, entry := range targets {
		outcome, scrapeErr := a.scrapeCloudEntry(entry, force)
		a.cloudScrapeMu.Lock()
		a.cloudScrape.Processed++
		switch outcome {
		case "matched":
			a.cloudScrape.Matched++
		case "unmatched":
			a.cloudScrape.Unmatched++
		case "error":
			a.cloudScrape.Failed++
		}
		if scrapeErr != nil {
			a.cloudScrape.LastError = scrapeErr.Error()
		}
		a.cloudScrapeMu.Unlock()
		time.Sleep(180 * time.Millisecond)
	}
}

func (a *App) scrapeCloudEntry(entry cloudEntry, force bool) (string, error) {
	m := cloudDisplayMeta(entry)
	if m.Title == "" || m.Artist == "" {
		return "error", errors.New("缺少用于刮削的曲名/歌手")
	}
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.search.ByType(strings.TrimSpace(m.Title+" "+m.Artist), 0, 1, 10)
	})
	if err != nil {
		return "error", err
	}
	best, score := bestCloudSearchMatch(m, raw)
	if best == nil || score < 3 {
		return "unmatched", nil
	}
	mid := strOf(best, "mid")
	if mid == "" {
		return "unmatched", nil
	}
	title := strOf(best, "title", "name")
	artist := ""
	for _, singer := range arrOf(best, "singer") {
		if name := strOf(asObj(singer), "name"); name != "" {
			if artist != "" {
				artist += " / "
			}
			artist += name
		}
	}
	album := strOf(objOf(best, "album"), "name")
	albumObj := objOf(best, "album")
	coverQQ := m.CoverQQ
	lyricQQ := m.LyricQQ
	if force || !lyricQQ {
		lraw, lerr := a.call(false, func() (json.RawMessage, error) {
			return a.lyric.GetLyric(mid, 1, false, true, true)
		})
		if lerr == nil {
			lyric, _ := decodeLyricField(decodeObj(lraw)["lyric"]).(string)
			if strings.TrimSpace(lyric) != "" {
				if err := ensureCloudDirs(); err == nil {
					trans, _ := decodeLyricField(decodeObj(lraw)["trans"]).(string)
					roma, _ := decodeLyricField(decodeObj(lraw)["roma"]).(string)
					payload, _ := json.Marshal(map[string]string{
						"lyric": lyric,
						"trans": trans,
						"roma":  roma,
					})
					if writeCloudFile(filepath.Join(cloudLyricsDir(), entry.Key+".json"), payload, 0o600) == nil {
						lyricQQ = true
					}
				}
			}
		}
	}
	if !m.CoverManual && (force || !coverQQ) {
		base := strings.Split(strOf(albumObj, "pmid"), "_")[0]
		if base == "" {
			base = strOf(albumObj, "mid")
		}
		if base != "" {
			if buf, ferr := fetchCloudCover(base); ferr == nil {
				if ensureCloudDirs() == nil && writeCloudFile(filepath.Join(cloudCoversDir(), entry.Key+".jpg"), buf, 0o600) == nil {
					coverQQ = true
				}
			}
		}
	}
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	meta := idx.Items[entry.Key]
	if meta == nil {
		meta = &cloudMeta{Key: entry.Key}
		idx.Items[entry.Key] = meta
	}
	meta.FileName = entry.FileName
	meta.AddTime = entry.AddTime
	meta.Matched = entry.Matched
	meta.Playable = entry.Playable
	if !meta.EditedMeta {
		meta.Title = title
		meta.Artist = artist
		meta.Album = album
	}
	singers := arrOf(best, "singer")
	meta.SingerMid = ""
	if len(singers) > 0 {
		meta.SingerMid = strOf(asObj(singers[0]), "mid")
	}
	meta.AlbumMid = strOf(albumObj, "mid")
	meta.AlbumPmid = strOf(albumObj, "pmid")
	meta.CoverQQ = coverQQ
	meta.LyricQQ = lyricQQ
	meta.ScrapedAt = time.Now().UnixMilli()
	err = a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		return "error", err
	}
	return "matched", nil
}

func bestCloudSearchMatch(meta cloudMeta, raw json.RawMessage) (obj, int) {
	d := decodeObj(raw)
	body := objOf(d, "body")
	list := arrOf(body, "item_song")
	if len(list) == 0 {
		list = arrOf(body, "song")
	}
	bestScore := 0
	var best obj
	for _, item := range list {
		song := asObj(item)
		if song == nil || strOf(song, "mid") == "" {
			continue
		}
		title := normalizeCloudText(strOf(song, "title", "name"))
		want := normalizeCloudText(meta.Title)
		if title == "" || want == "" {
			continue
		}
		score := 0
		if title == want {
			score = 3
		} else if strings.Contains(title, want) || strings.Contains(want, title) {
			score = 2
		} else {
			continue
		}
		artist := normalizeCloudText(meta.Artist)
		for _, singer := range arrOf(song, "singer") {
			name := normalizeCloudText(strOf(asObj(singer), "name"))
			if artist != "" && name == artist {
				score += 2
				break
			}
			if artist != "" && name != "" && (strings.Contains(name, artist) || strings.Contains(artist, name)) {
				score++
				break
			}
		}
		if score > bestScore {
			bestScore = score
			best = song
		}
	}
	return best, bestScore
}

func normalizeCloudText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '·', '、', ',', '，', '.', '。', '-', '_', '—', '–', '~', '!', '！', '?', '？', '(', ')', '（', '）', '[', ']', '【', '】', '{', '}', '《', '》', '\'', '"', '“', '”', '/', '\\', '|', ':', '&', ';':
			return -1
		}
		return r
	}, s)
}

// cloudParseFileName 从「歌手 - 曲名.flac」推断刮削用的展示信息；没有明确分隔符时只给曲名。
func cloudParseFileName(name string) (string, string) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.TrimSpace(base)
	i := 0
	for i < len(base) && i < 3 && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	if i > 0 {
		j := i
		for j < len(base) && base[j] == ' ' {
			j++
		}
		if j < len(base) && (base[j] == '.' || base[j] == '_' || base[j] == '-') {
			j++
			for j < len(base) && base[j] == ' ' {
				j++
			}
			base = base[j:]
		}
	}

	for _, sep := range []string{" - ", " – ", " — ", "－"} {
		if i := strings.Index(base, sep); i > 0 {
			artist := strings.TrimSpace(base[:i])
			title := strings.TrimSpace(base[i+len(sep):])

			if artist != "" && title != "" {
				return title, artist
			}
		}
	}

	return base, ""
}

func fetchCloudCover(base string) ([]byte, error) {
	url := "https://y.gtimg.cn/music/photo_new/T002R500x500M000" + base + ".jpg"
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("封面 HTTP %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, cloudCoverMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(buf) < 1024 || len(buf) > cloudCoverMaxBytes || buf[0] != 0xff || buf[1] != 0xd8 {
		return nil, errors.New("封面不是有效 JPEG")
	}
	return buf, nil
}

func (a *App) handleCloudMetaSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key    string `json:"key"`
		Title  string `json:"title"`
		Artist string `json:"artist"`
		Album  string `json:"album"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !cloudKeyRe.MatchString(body.Key) {
		writeErr(w, 422, -1, "缺少合法网盘条目 key")
		return
	}
	if strings.TrimSpace(body.Title) == "" && strings.TrimSpace(body.Artist) == "" && strings.TrimSpace(body.Album) == "" {
		writeErr(w, 422, -1, "至少填一项")
		return
	}
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	meta := idx.Items[body.Key]
	if meta == nil {
		meta = &cloudMeta{Key: body.Key}
		idx.Items[body.Key] = meta
	}
	if strings.TrimSpace(body.Title) != "" {
		meta.Title = strings.TrimSpace(body.Title)
	}
	if strings.TrimSpace(body.Artist) != "" {
		meta.Artist = strings.TrimSpace(body.Artist)
	}
	if strings.TrimSpace(body.Album) != "" {
		meta.Album = strings.TrimSpace(body.Album)
	}
	meta.EditedMeta = true
	err := a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"key": body.Key})
}

func (a *App) handleCloudPlayback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !cloudKeyRe.MatchString(body.Key) {
		writeErr(w, 422, -1, "缺少合法网盘条目 key")
		return
	}
	if body.State != "matched" && body.State != "resolved" && body.State != "blocked" {
		writeErr(w, 422, -1, "播放状态必须是 matched、resolved 或 blocked")
		return
	}
	if len(body.Error) > 512 {
		body.Error = body.Error[:512]
	}
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	meta := idx.Items[body.Key]
	if meta == nil {
		meta = &cloudMeta{Key: body.Key}
		idx.Items[body.Key] = meta
	}
	meta.PlaybackState = body.State
	meta.PlaybackError = body.Error
	err := a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"key": body.Key, "state": body.State})
}

func (a *App) handleCloudLyricImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !cloudKeyRe.MatchString(body.Key) {
		writeErr(w, 422, -1, "缺少合法网盘条目 key")
		return
	}
	raw, err := decodeCloudBase64(body.Data)
	if err != nil || len(raw) == 0 || len(raw) > cloudLyricMaxBytes {
		writeErr(w, 413, -1, "歌词文件过大或不是 base64")
		return
	}
	// 导入件是用户上传的明文歌词，不做 QRC 解密；否则普通 LRC 会被误当成密文。
	text := string(raw)
	format := sniffCloudLyricFormat(text, body.Name)
	if ensureCloudDirs() != nil || writeCloudFile(filepath.Join(cloudLyricsDir(), body.Key+".import"), []byte(text), 0o600) != nil {
		writeErr(w, 500, -1, "写入歌词失败")
		return
	}
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	meta := idx.Items[body.Key]
	if meta == nil {
		meta = &cloudMeta{Key: body.Key}
		idx.Items[body.Key] = meta
	}
	meta.LyricImport = true
	meta.ImportFormat = format
	err = a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"format": format, "chars": len([]rune(text))})
}

func (a *App) handleCloudLyricRevert(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !cloudKeyRe.MatchString(key) {
		writeErr(w, 422, -1, "缺少合法网盘条目 key")
		return
	}
	_ = os.Remove(filepath.Join(cloudLyricsDir(), key+".import"))
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	if meta := idx.Items[key]; meta != nil {
		meta.LyricImport = false
		meta.ImportFormat = ""
	}
	err := a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"key": key})
}

func (a *App) handleCloudCoverImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !cloudKeyRe.MatchString(body.Key) {
		writeErr(w, 422, -1, "缺少合法网盘条目 key")
		return
	}
	raw, err := decodeCloudBase64(body.Data)
	if err != nil || len(raw) == 0 || len(raw) > cloudCoverMaxBytes {
		writeErr(w, 413, -1, "封面过大或不是 base64")
		return
	}
	ext := cloudImageExt(raw)
	if ext == "" {
		writeErr(w, 422, -1, "只认 jpg/png/gif/webp")
		return
	}
	if ensureCloudDirs() != nil {
		writeErr(w, 500, -1, "创建封面目录失败")
		return
	}
	for _, old := range []string{"jpg", "jpeg", "png", "gif", "webp"} {
		_ = os.Remove(filepath.Join(cloudCoversDir(), body.Key+"."+old))
	}
	if err := writeCloudFile(filepath.Join(cloudCoversDir(), body.Key+"."+ext), raw, 0o600); err != nil {
		writeErr(w, 500, -1, "写入封面失败")
		return
	}
	idx := a.loadCloudIndex()
	a.cloudMu.Lock()
	meta := idx.Items[body.Key]
	if meta == nil {
		meta = &cloudMeta{Key: body.Key}
		idx.Items[body.Key] = meta
	}
	meta.CoverManual = true
	err = a.saveCloudIndexLocked()
	a.cloudMu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, map[string]any{"key": body.Key})
}

func (a *App) handleCloudLyric(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !cloudKeyRe.MatchString(key) {
		writeErr(w, 404, -1, "网盘条目不存在")
		return
	}
	meta := a.cloudMetaOf(key)
	if meta == nil {
		meta = &cloudMeta{Key: key}
	}
	result := map[string]string{"lyric": "", "trans": "", "roma": "", "source": "", "format": "lrc"}
	if meta.LyricImport {
		if raw, err := os.ReadFile(filepath.Join(cloudLyricsDir(), key+".import")); err == nil {
			result["lyric"] = string(raw)
			result["source"] = "import"
			result["format"] = meta.ImportFormat
		}
	}
	if result["lyric"] == "" {
		if raw, err := os.ReadFile(filepath.Join(cloudLyricsDir(), key+".json")); err == nil {
			var lyric struct {
				Lyric string `json:"lyric"`
				Trans string `json:"trans"`
				Roma  string `json:"roma"`
			}
			if json.Unmarshal(raw, &lyric) == nil {
				result["lyric"] = lyric.Lyric
				result["trans"] = lyric.Trans
				result["roma"] = lyric.Roma
				result["source"] = "qq"
			}
		}
	}
	writeOK(w, result)
}

func (a *App) handleCloudCover(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !cloudKeyRe.MatchString(key) {
		writeErr(w, 404, -1, "网盘条目不存在")
		return
	}
	meta := a.cloudMetaOf(key)
	if meta == nil {
		meta = &cloudMeta{Key: key}
	}
	file := cloudCoverFile(key, *meta)
	if file == "" {
		writeErr(w, 404, -1, "无封面")
		return
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		writeErr(w, 404, -1, "封面不存在")
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(file))
	if ct == "" {
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func sniffCloudLyricFormat(text, name string) string {
	head := text
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := strings.ToLower(head)
	if strings.Contains(lower, "<tt ") || strings.Contains(lower, "<ttml") || strings.Contains(lower, "ttml") {
		return "ttml"
	}
	if strings.Contains(head, "<QrcInfos") || strings.Contains(head, "<LyricContent") || strings.Contains(head, "<w d=") {
		return "qrc"
	}
	if strings.HasSuffix(strings.ToLower(name), ".ttml") {
		return "ttml"
	}
	if strings.HasSuffix(strings.ToLower(name), ".qrc") {
		return "qrc"
	}
	return "lrc"
}

func cloudImageExt(buf []byte) string {
	if len(buf) >= 2 && buf[0] == 0xff && buf[1] == 0xd8 {
		return "jpg"
	}
	if len(buf) >= 4 && buf[0] == 0x89 && string(buf[1:4]) == "PNG" {
		return "png"
	}
	if len(buf) >= 6 && (string(buf[:6]) == "GIF87a" || string(buf[:6]) == "GIF89a") {
		return "gif"
	}
	if len(buf) >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP" {
		return "webp"
	}
	return ""
}

func decodeCloudBase64(data string) ([]byte, error) {
	raw := strings.TrimSpace(data)

	if i := strings.Index(raw, ","); i >= 0 && strings.HasPrefix(raw, "data:") {
		raw = raw[i+1:]
	}
	raw = strings.ReplaceAll(raw, "\n", "")
	raw = strings.ReplaceAll(raw, "\r", "")
	if buf, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return buf, nil
	}
	return base64.RawStdEncoding.DecodeString(raw)
}
