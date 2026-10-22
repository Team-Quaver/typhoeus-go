package qqmusic

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestRefreshAndroidSessionUnwrapsCGIData(t *testing.T) {
	cl, err := NewClient(&Credential{}, "", "",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte(`"caller":2`)) {
				t.Fatalf("first session request must use caller=2: %s", body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body: io.NopCloser(bytes.NewBufferString(
					`{"code":0,"req_0":{"code":0,"data":{"session":{"uid":6774031798,"sid":"20261022161021","vkey":"x"}}}}`,
				)),
			}, nil
		})}))
	if err != nil {
		t.Fatal(err)
	}
	cl.qimei = &qimeiResult{Q16: "q16", Q36: "q36"}
	cl.qimeiAt = time.Now().Unix()
	session, err := cl.refreshAndroidSession("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if session.UID != "6774031798" || session.SID != "20261022161021" {
		t.Fatalf("session = %+v", session)
	}
}

func TestRefreshAndroidSessionRejectsMissingSession(t *testing.T) {
	cl, err := NewClient(&Credential{}, "", "",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{"code":0,"req_0":{"code":0,"data":{}}}`)),
			}, nil
		})}))
	if err != nil {
		t.Fatal(err)
	}
	cl.qimei = &qimeiResult{}
	cl.qimeiAt = time.Now().Unix()
	if _, err := cl.refreshAndroidSession("", 2); err == nil {
		t.Fatal("expected malformed session to fail")
	}
}
