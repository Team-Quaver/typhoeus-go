package qqmusic

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCookieSnapshotKeepsEmptyValues Cookie 快照必须保留空值项、同名取最后值
// （上游 7413b56 的回归口径：niquests 的 jar 按名取值遇空串会抛异常，Go 的
// resp.Cookies() 逐条解析没有这个问题 —— 本测试把这份等价性钉住）。
func TestCookieSnapshotKeepsEmptyValues(t *testing.T) {
	cl, err := NewClient(&Credential{}, "", "",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			h := make(http.Header)
			h.Add("Set-Cookie", "ETK=; Domain=.y.qq.com; Path=/")
			h.Add("Set-Cookie", "sid=first; Domain=.y.qq.com; Path=/")
			h.Add("Set-Cookie", "sid=last; Domain=.y.qq.com; Path=/login")
			return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cl.DoHTTPPlain("GET", "https://y.qq.com/", HTTPOption{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := resp.Cookies["ETK"]; !ok || v != "" {
		t.Fatalf("empty cookie must be kept: %v (present=%v)", v, ok)
	}
	if resp.Cookies["sid"] != "last" {
		t.Fatalf("duplicate name must fold to last value: %q", resp.Cookies["sid"])
	}
}
