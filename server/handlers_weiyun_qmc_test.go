package server

import (
	"bytes"
	"io"
	"testing"

	"github.com/team-quaver/typhoeus-go/qmc"
)

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
	if got := weiyunAudioExt("song[aq].776176"); got != ".wav" {
		t.Fatalf("wav extension = %q", got)
	}
}
