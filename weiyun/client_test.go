package weiyun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientListUsesWyHeaderAndParsesNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("WyHeader"); got != "mcp_token=test-token" {
			t.Fatalf("WyHeader = %q", got)
		}
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Params.Name != "weiyun.list" || req.Params.Arguments["limit"] != float64(7) {
			t.Fatalf("unexpected request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []any{map[string]any{
				"type": "text",
				"text": `{"pdir_key":"root","total_file_count":1,"finish_flag":true,
					"file_list":[{"file_id":"f1","filename":"a.flac","file_size":123,"pdir_key":"root"}]}`,
			}}},
		})
	}))
	defer srv.Close()

	c := NewClient("test-token", srv.URL)
	res, err := c.List(context.Background(), ListOptions{Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if res.PdirKey != "root" || len(res.FileList) != 1 || res.FileList[0].FileSize != 123 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestClientToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"content":[{"type":"text","text":"{\"error\":\"目录不存在\"}"}]}}`))
	}))
	defer srv.Close()
	c := NewClient("t", srv.URL)
	if _, err := c.List(context.Background(), ListOptions{Limit: 1}); err == nil || !strings.Contains(err.Error(), "目录不存在") {
		t.Fatalf("err = %v", err)
	}
}

func TestIndexFilesWalksDirectoriesAndPaginates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Params.Name != "weiyun.list" {
			t.Fatalf("unexpected request: %+v", req)
		}
		calls++
		args := req.Params.Arguments
		dirKey := strValue(args["dir_key"])
		offset := int64Value(args["offset"])
		var text string
		switch {
		case dirKey == "":
			text = `{"pdir_key":"root","finish_flag":true,
				"dir_list":[{"dir_key":"music","dir_name":"QQ音乐"}],
				"file_list":[{"file_id":"root-file","filename":"root.ogg"}]}`
		case dirKey == "music" && offset == 0:
			text = `{"pdir_key":"music","finish_flag":false,
				"file_list":[{"file_id":"f1","filename":"a.ogg","file_size":12}]}`
		case dirKey == "music" && offset == 1:
			text = `{"pdir_key":"music","finish_flag":true,
				"file_list":[{"file_id":"f2","filename":"b.flac","file_size":34}]}`
		default:
			t.Fatalf("unexpected list call: %+v", args)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []any{map[string]any{
				"type": "text", "text": text,
			}}},
		})
	}))
	defer srv.Close()

	c := NewClient("t", srv.URL)
	idx, err := c.IndexFiles(context.Background(), IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Files) != 3 || calls != 3 || idx.DirCount != 2 || idx.Truncated {
		t.Fatalf("unexpected index: %+v calls=%d", idx, calls)
	}
	if idx.Files["f1"].PdirKey != "music" || idx.Files["f2"].PdirKey != "music" {
		t.Fatalf("child files should use child listing pdir_key: %+v", idx.Files)
	}
	if idx.Files["root-file"].PdirKey != "root" {
		t.Fatalf("root file pdir_key = %q", idx.Files["root-file"].PdirKey)
	}
}
