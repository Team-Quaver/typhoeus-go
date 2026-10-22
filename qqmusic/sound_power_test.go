package qqmusic

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReportPlayDurationBuildsPhysicalStream(t *testing.T) {
	var payload string
	cl, err := NewClient(&Credential{MusicID: 123456, MusicKey: "W_X_test", LoginType: 2}, "", "",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.String() != imusicTJURL {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
			}
			if got := r.Header.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("content-encoding = %q", got)
			}
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Fatalf("gzip body: %v", err)
			}
			raw, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			_ = zr.Close()
			payload = string(raw)

			var compressed bytes.Buffer
			zw := zlib.NewWriter(&compressed)
			_, _ = zw.Write([]byte(`{"code":0}`))
			_ = zw.Close()
			body := append([]byte{0, 0, 0, 0, 0}, compressed.Bytes()...)
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
		})}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	cl.androidSession = &AndroidSession{UID: "session-uid", SID: "sid", SavedAt: now}
	cl.qimei = &qimeiResult{Q16: "q16", Q36: "q36"}
	cl.qimeiAt = now

	ok, err := cl.ReportPlayDuration(context.Background(), PlayDurationReport{
		SongID:             9988,
		AudioTimeMS:        245000,
		PlayDurationSecond: 60,
		VKey:               "vkey&value",
		CoSinger:           `A&B <singer>`,
		SingerID:           777,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected upstream success")
	}
	for _, want := range []string{
		"<cid>228</cid>", `songid="9988"`, `singerid="777"`, `audiotime="245000"`,
		`time="60"`, `play_duration_mi="60000"`, "vkey&amp;value", "A&amp;B &lt;singer&gt;",
		"<uid>session-uid</uid>", "<sid>sid</sid>", "<traceid>10002_123456_",
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload missing %q:\n%s", want, payload)
		}
	}
	if !strings.Contains(payload, `timekey="`) || !strings.Contains(payload, `string27="`) {
		t.Fatalf("payload missing signatures:\n%s", payload)
	}
}

func TestReportPlayDurationDecodesZlibResponse(t *testing.T) {
	cl, err := NewClient(&Credential{MusicID: 1, MusicKey: "key", LoginType: 2}, "", "",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			var compressed bytes.Buffer
			zw := zlib.NewWriter(&compressed)
			_, _ = zw.Write([]byte(`{"code":17}`))
			_ = zw.Close()
			body := append([]byte{0, 0, 0, 0, 0}, compressed.Bytes()...)
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
		})}))
	if err != nil {
		t.Fatal(err)
	}
	cl.androidSession = &AndroidSession{UID: "uid", SID: "sid", SavedAt: time.Now().Unix()}
	cl.qimei = &qimeiResult{}
	cl.qimeiAt = time.Now().Unix()
	ok, err := cl.ReportPlayDuration(context.Background(), PlayDurationReport{
		SongID: 1, AudioTimeMS: 1000, PlayDurationSecond: 1, VKey: "vkey",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("non-zero upstream code must return ok=false")
	}
}

func TestPlayDurationSignaturesMatchReference(t *testing.T) {
	if got, want := playDurationTimeKey(1700000000, 60, "123"), "863A8CA2B701AD1EE03664DFB892F07C"; got != want {
		t.Fatalf("timekey = %s, want %s", got, want)
	}
	if got, want := playDurationString27(60), "eyJzY3JlZW5fb24iOjEsImFwcF9pbiI6MSwiYXBwX3RpbWUiOjYwLCJwbGF5cGFnZV90aW1lIjoyNCwidGFnX2lkIjoxMDAwMSwidGFnX3R5cGUiOjE1LCJzdGFydF9wbGF5dHlwZSI6MCwic3RhcnRfcGxheXRpbWUiOjB9"; got != want {
		t.Fatalf("string27 = %s, want %s", got, want)
	}
}
