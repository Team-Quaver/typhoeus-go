package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
	"github.com/team-quaver/typhoeus-go/typhoeus"
	"github.com/team-quaver/typhoeus-go/weiyun"
)

type rangeOpener struct {
	body      []byte
	firstCut  int
	calls     int
	lastRange string
}

type cutReader struct {
	data []byte
}

func (r *cutReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *cutReader) Close() error { return nil }

func (o *rangeOpener) OpenRange(_ context.Context, _ string, rng *typhoeus.ByteRange) (*typhoeus.UpstreamResponse, error) {
	start, end := int64(0), int64(len(o.body)-1)
	if rng != nil {
		if rng.Start != nil {
			start = *rng.Start
		}
		if rng.End != nil && *rng.End >= start && *rng.End < end {
			end = *rng.End
		}
	}
	o.calls++
	o.lastRange = fmt.Sprintf("%d-%d", start, end)
	var body io.ReadCloser = io.NopCloser(bytes.NewReader(o.body[start : end+1]))
	if o.calls == 1 && o.firstCut > 0 {
		cutEnd := start + int64(o.firstCut) - 1
		if cutEnd > end {
			cutEnd = end
		}
		body = &cutReader{data: o.body[start : cutEnd+1]}
	}
	cr := fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.body))
	return &typhoeus.UpstreamResponse{
		Status: http.StatusPartialContent, Header: http.Header{"Content-Range": {cr}},
		ContentRange: cr, Total: int64(len(o.body)), Body: body,
	}, nil
}

func newWeiyunStreamApp(body []byte, opener streamOpener) *App {
	return &App{
		streams: map[string]*streamEntry{
			"tok": {
				mid: "weiyun:test", url: "test", mime: "audio/ogg", total: int64(len(body)),
				tier: "weiyun", encrypted: true, cipher: weiyunCipher{}, opener: opener,
				expiresAt: time.Now().Add(time.Hour),
			},
		},
		paceMap: map[string]*byteBucket{},
	}
}

func TestWeiyunStreamRangeUsesAbsoluteQmcOffset(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 4000)
	cipherText := append([]byte(nil), plain...)
	qmc.DecryptV1InPlace(cipherText, 0)
	opener := &rangeOpener{body: cipherText}
	app := newWeiyunStreamApp(cipherText, opener)

	const start = int64(0x8000 + 17)
	req := httptest.NewRequest(http.MethodGet, "/stream/tok", nil)
	req.SetPathValue("token", "tok")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	rec := httptest.NewRecorder()
	app.handleStreamProxy(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, plain[start:]) {
		t.Fatalf("range body mismatch at %d", start)
	}
	if opener.lastRange != fmt.Sprintf("%d-%d", start, len(cipherText)-1) {
		t.Fatalf("upstream range = %s", opener.lastRange)
	}
}

func TestWeiyunStreamResumeKeepsDecryptOffset(t *testing.T) {
	plain := bytes.Repeat([]byte("resume-offset-test-"), 4000)
	cipherText := append([]byte(nil), plain...)
	qmc.DecryptV1InPlace(cipherText, 0)
	opener := &rangeOpener{body: cipherText, firstCut: 4096}
	app := newWeiyunStreamApp(cipherText, opener)

	const start = int64(0x8000 + 3)
	req := httptest.NewRequest(http.MethodGet, "/stream/tok", nil)
	req.SetPathValue("token", "tok")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	rec := httptest.NewRecorder()
	app.handleStreamProxy(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, plain[start:]) {
		t.Fatalf("resumed body mismatch: got %d bytes, want %d", len(got), len(plain)-int(start))
	}
	t.Logf("calls=%d lastRange=%s body=%d", opener.calls, opener.lastRange, rec.Body.Len())
	if opener.calls != 2 {
		t.Fatalf("expected one resume request, got %d calls", opener.calls)
	}
}

func TestWeiyunStreamEntryUsesSongMid(t *testing.T) {
	body := []byte("plain audio")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	app := &App{
		streams:     map[string]*streamEntry{},
		weiyunStore: &weiyunStore{},
		weiyunTickets: map[string]*weiyunTicket{
			weiyunTicketID("file-1", "pdir-1"): {
				URL: srv.URL, Size: int64(len(body)),
				Name: "plain.ogg", PdirKey: "pdir-1", FileID: "file-1", Expires: time.Now().Add(time.Hour),
			},
		},
		weiyunIndex: &weiyunIndexCache{
			Files: map[string]weiyun.IndexedFile{
				"file-1": {FileID: "file-1", FileName: "plain.ogg", FileSize: int64(len(body)), PdirKey: "pdir-1"},
			},
			Expires: time.Now().Add(time.Hour),
		},
		cloudIndex: &cloudIndex{
			Version: 1, Items: map[string]*cloudMeta{},
			Locations: map[string]cloudLocation{"file-1": {PdirKey: "pdir-1", FileName: "plain.ogg"}},
		},
		paceMap: map[string]*byteBucket{},
	}

	rec := httptest.NewRecorder()
	bodyJSON := `{"mid":"song-mid","tier":"weiyun","duration_ms":180000,"cloud_file_id":"file-1"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/resolve", strings.NewReader(bodyJSON))
	req.Header.Set("content-type", "application/json")
	app.handleWeiyunStreamResolve(rec, req, streamResolveBody{
		Mid: "song-mid", Tier: "weiyun", DurationMs: 180000, CloudFileID: "file-1",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Resolve must not open the upstream just to discover the length: that
	// used to issue a full-file GET and block the response until it completed.
	if requests != 0 {
		t.Fatalf("resolve opened upstream %d times; expected 0", requests)
	}
	var out struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(out.Data.Path, "/stream/")
	if token == "" || token == out.Data.Path {
		t.Fatalf("unexpected path %q", out.Data.Path)
	}
	entry := app.streams[token]
	if entry == nil || entry.mid != "song-mid" {
		t.Fatalf("entry mid = %+v", entry)
	}
	if entry.cacheStarted {
		t.Fatal("cache must not start before playback delivers bytes")
	}
}

func TestWeiyunNamedTicketUpdatesCachedMetadata(t *testing.T) {
	fileID, pdirKey, name := "file-name", "pdir-name", "大石碎胸口-万能青年旅店[aq].776176"
	key := weiyunTicketID(fileID, pdirKey)
	app := &App{
		weiyunStore: &weiyunStore{},
		weiyunTickets: map[string]*weiyunTicket{
			key: {
				URL: "https://example.invalid/audio", Size: 1,
				PdirKey: pdirKey, FileID: fileID, Expires: time.Now().Add(time.Hour),
			},
		},
	}

	ticket, err := app.weiyunDownloadTicketNamed(context.Background(), fileID, "", name)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Name != name {
		t.Fatalf("returned name = %q", ticket.Name)
	}
	if cached := app.weiyunTickets[key]; cached == nil || cached.Name != name {
		t.Fatalf("cached ticket metadata was not updated: %+v", cached)
	}
	if weiyunAudioMIME(app.weiyunTickets[key].Name) != "audio/wav" {
		t.Fatal("cached file name must still drive Weiyun QMC MIME detection")
	}
}

func TestWeiyunTicketInvalidateForcesFreshDownload(t *testing.T) {
	fileID, pdirKey := "file-stale", "pdir-stale"
	key := weiyunTicketID(fileID, pdirKey)
	app := &App{
		weiyunTickets: map[string]*weiyunTicket{
			key: {FileID: fileID, PdirKey: pdirKey, Expires: time.Now().Add(time.Hour)},
		},
	}
	app.invalidateWeiyunTicket(fileID, pdirKey)
	if _, ok := app.weiyunTickets[key]; ok {
		t.Fatal("stale CDN ticket must be removed before a forced refresh")
	}
}

func TestWeiyunLocationsOfPersistsWholeScan(t *testing.T) {
	locations := weiyunLocationsOf(map[string]weiyun.IndexedFile{
		"file-1": {FileID: "file-1", FileName: "one[aq].776176", PdirKey: "pdir-1"},
		"file-2": {FileID: "file-2", FileName: "two[aq].666c6163", PdirKey: "pdir-2"},
	})
	if len(locations) != 2 || locations["file-1"].PdirKey != "pdir-1" || locations["file-2"].FileName != "two[aq].666c6163" {
		t.Fatalf("locations = %+v", locations)
	}
}

func TestWeiyunStreamSourceRetriesExpiredCDNLink(t *testing.T) {
	firstCalls, secondCalls := 0, 0
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls++
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer expired.Close()
	body := []byte("RIFF-fresh-audio")
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		if got := r.Header.Get("Range"); got != "bytes=0-15" {
			t.Fatalf("range = %q", got)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer fresh.Close()

	source := &weiyunStreamSource{
		ticket: &weiyunTicket{URL: expired.URL, Name: "song[aq].776176"},
		refreshTest: func(_ context.Context, _ string, _ string, name string) (*weiyunTicket, error) {
			if name != "song[aq].776176" {
				t.Fatalf("refresh lost file name: %q", name)
			}
			return &weiyunTicket{URL: fresh.URL, Name: "song[aq].776176"}, nil
		},
	}
	start, end := int64(0), int64(15)
	resp, err := source.OpenRange(context.Background(), "ignored", &typhoeus.ByteRange{Start: &start, End: &end})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 1 || resp.Status != http.StatusPartialContent || !bytes.Equal(got, body) {
		t.Fatalf("retry = first:%d second:%d status:%d body:%q", firstCalls, secondCalls, resp.Status, got)
	}
}

func TestWeiyunResolveQueryNameRestoresEncryptionMetadata(t *testing.T) {
	fileID, pdirKey, name := "file-encrypted", "pdir-encrypted", "song[aq].776176"
	app := &App{
		streams:     map[string]*streamEntry{},
		weiyunStore: &weiyunStore{},
		weiyunTickets: map[string]*weiyunTicket{
			weiyunTicketID(fileID, pdirKey): {
				URL: "https://example.invalid/audio", Size: 1024,
				PdirKey: pdirKey, FileID: fileID, Expires: time.Now().Add(time.Hour),
			},
		},
		paceMap: map[string]*byteBucket{},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/stream/resolve?name="+name, nil)
	app.handleWeiyunStreamResolve(rec, req, streamResolveBody{
		Mid: "song-mid", Tier: "weiyun", DurationMs: 180000, CloudFileID: fileID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Path      string `json:"path"`
			Mime      string `json:"mime"`
			Encrypted bool   `json:"encrypted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Mime != "audio/wav" || !out.Data.Encrypted {
		t.Fatalf("resolve metadata = mime %q encrypted %v", out.Data.Mime, out.Data.Encrypted)
	}
	token := strings.TrimPrefix(out.Data.Path, "/stream/")
	if entry := app.streams[token]; entry == nil || !entry.encrypted || entry.mime != "audio/wav" {
		t.Fatalf("stream entry = %+v", entry)
	}
}

func TestValidateRangeResponseRejectsWrongStart(t *testing.T) {
	up := &typhoeus.UpstreamResponse{
		Status: http.StatusPartialContent, ContentRange: "bytes 100-199/1000",
	}
	if err := validateRangeResponse(up, 50); err == nil {
		t.Fatal("shifted Content-Range must be rejected")
	}
	up.Status = http.StatusOK
	up.ContentRange = ""
	if err := validateRangeResponse(up, 50); err == nil {
		t.Fatal("non-206 range response must be rejected")
	}
}

func TestWeiyunDecryptReaderMatchesWholeFile(t *testing.T) {
	plain := bytes.Repeat([]byte("OggS-weiyun-qmc-v1-test-"), 100)
	encrypted := append([]byte(nil), plain...)
	qmc.DecryptV1InPlace(encrypted, 0)

	var out bytes.Buffer
	reader := &weiyunDecryptReader{r: bytes.NewReader(encrypted)}
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatal("streamed decrypt differs from source")
	}
}

func TestWeiyunDecryptReaderRangeOffset(t *testing.T) {
	plain := bytes.Repeat([]byte("range-test-"), 9000)
	encrypted := append([]byte(nil), plain...)
	qmc.DecryptV1InPlace(encrypted, 0)
	const offset = int64(0x8000 + 17)
	var out bytes.Buffer
	reader := &weiyunDecryptReader{r: bytes.NewReader(encrypted[offset:]), offset: offset}
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain[offset:]) {
		t.Fatal("offset decrypt differs from source")
	}
}

func TestWeiyunRangeStart(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"", 0, true},
		{"bytes=0-1023", 0, true},
		{"bytes=4096-8191", 4096, true},
		{"bytes=100-", 100, true},
		{"bytes=-500", 0, false},
		{"bytes=0-1,4-5", 0, false},
	}
	for _, tc := range cases {
		got, err := weiyunRangeStart(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("%q: got %d, %v", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q: should reject", tc.in)
		}
	}
}

func TestWeiyunContentRangeStart(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes 0-65535/9224608", 0, true},
		{"bytes 4096-8191/9224608", 4096, true},
		{"bytes 9216000-9224607/*", 9216000, true},
		{"", 0, false},
		{"bytes */9224608", 0, false},
		{"bytes 0-1,4-5/100", 0, false},
	}
	for _, tc := range cases {
		got, err := weiyunContentRangeStart(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("%q: got %d, %v", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q: should reject", tc.in)
		}
	}
}

func TestWeiyunShouldDecryptHexOggSuffix(t *testing.T) {
	if !weiyunShouldDecrypt("Come+Alive+Stripped.-三Z-STUDIOHOYO-MiX[aq].6f6767") {
		t.Fatal("Weiyun [aq].6f6767 download should be treated as encrypted Ogg")
	}
	if weiyunShouldDecrypt("notes.txt") {
		t.Fatal("non-audio files must pass through untouched")
	}
	if weiyunShouldDecrypt("plain-song.ogg") {
		t.Fatal("plain Ogg uploads must pass through untouched")
	}
	if !weiyunShouldDecrypt("song[aq].776176") {
		t.Fatal("Weiyun [aq].776176 should be treated as encrypted WAV")
	}
}

func TestWeiyunAudioMIMEHexSuffixes(t *testing.T) {
	cases := map[string]string{
		"song[aq].6f6767":   "audio/ogg",
		"song[aq].776176":   "audio/wav",
		"song[aq].6d7033":   "audio/mpeg",
		"song[aq].666c6163": "audio/flac",
	}
	for name, want := range cases {
		if got := weiyunAudioMIME(name); got != want {
			t.Fatalf("%s: got %q want %q", name, got, want)
		}
	}
}
