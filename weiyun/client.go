// Package weiyun implements the Weiyun MCP file API used by Quaver's cloud page.
//
// The QQ Music credential is intentionally not used here: Weiyun's web/MCP API
// is authorized by a separate mcp_token. QQ Music's musickey remains the
// credential for the existing music.WeiyunMusic.* read APIs.
package weiyun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultMCPURL = "https://www.weiyun.com/api/v3/mcpserver"
	maxListLimit  = 50
)

// Client is a small JSON-RPC client for https://www.weiyun.com/api/v3/mcpserver.
type Client struct {
	token   string
	mcpURL  string
	http    *http.Client
	request atomic.Uint64
}

// NewClient constructs a Weiyun MCP client. An empty mcpURL uses the public endpoint.
func NewClient(token, mcpURL string) *Client {
	if strings.TrimSpace(mcpURL) == "" {
		mcpURL = DefaultMCPURL
	}
	return &Client{
		token:  strings.TrimSpace(token),
		mcpURL: mcpURL,
		http: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// WithHTTPClient replaces the HTTP client (used by tests and embedders).
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	if h != nil {
		c.http = h
	}
	return c
}

// Call invokes one MCP tool and returns the JSON object embedded in its text content.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
	if c.token == "" {
		return nil, errors.New("微云令牌未配置")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		out, retry, err := c.callOnce(ctx, tool, args)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) callOnce(ctx context.Context, tool string, args map[string]any) (map[string]any, bool, error) {
	id := c.request.Add(1)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mcpURL, bytes.NewReader(raw))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Quaver/1.7 WeiyunMCP")
	req.Header.Set("WyHeader", "mcp_token="+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("微云请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, true, fmt.Errorf("读取微云响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
			fmt.Errorf("微云 HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var envelope struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("微云响应不是 JSON-RPC: %w", err)
	}
	if envelope.Error != nil {
		return nil, false, fmt.Errorf("微云 MCP 错误 %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	for _, item := range envelope.Result.Content {
		if item.Type != "text" || strings.TrimSpace(item.Text) == "" {
			continue
		}
		var out map[string]any
		dec := json.NewDecoder(strings.NewReader(item.Text))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			return nil, false, fmt.Errorf("微云工具响应格式异常: %w", err)
		}
		retry := strings.Contains(strings.ToLower(strValue(out["error"])), "retcode=50000") ||
			int64Value(out["retcode"]) == 50000
		return out, retry, nil
	}
	return nil, false, errors.New("微云 MCP 响应没有 text 内容")
}

// ListOptions controls a directory listing.
type ListOptions struct {
	DirKey  string
	PdirKey string
	Offset  int
	Limit   int
	GetType int
	OrderBy int
	Asc     bool
}

// File is a Weiyun file entry.
type File struct {
	FileID    string `json:"file_id"`
	FileName  string `json:"filename"`
	FileSize  int64  `json:"file_size"`
	FileCTime int64  `json:"file_ctime"`
	FileMTime int64  `json:"file_mtime"`
	PdirKey   string `json:"pdir_key"`
}

// Dir is a Weiyun directory entry.
type Dir struct {
	DirKey   string `json:"dir_key"`
	DirName  string `json:"dir_name"`
	DirCTime int64  `json:"dir_ctime"`
	DirMTime int64  `json:"dir_mtime"`
}

// ListResult is a normalized weiyun.list response.
type ListResult struct {
	PdirKey        string `json:"pdir_key"`
	TotalDirCount  int64  `json:"total_dir_count"`
	TotalFileCount int64  `json:"total_file_count"`
	FinishFlag     bool   `json:"finish_flag"`
	DirList        []Dir  `json:"dir_list"`
	FileList       []File `json:"file_list"`
	Error          string `json:"error"`
}

// List returns a directory page. The MCP endpoint caps limit at 50.
func (c *Client) List(ctx context.Context, opt ListOptions) (*ListResult, error) {
	if opt.Limit <= 0 {
		opt.Limit = maxListLimit
	}
	if opt.Limit > maxListLimit {
		opt.Limit = maxListLimit
	}
	args := map[string]any{
		"offset":   opt.Offset,
		"limit":    opt.Limit,
		"get_type": opt.GetType,
		"order_by": opt.OrderBy,
		"asc":      opt.Asc,
	}
	if opt.DirKey != "" {
		args["dir_key"] = opt.DirKey
	}
	if opt.PdirKey != "" {
		args["pdir_key"] = opt.PdirKey
	}
	out, err := c.Call(ctx, "weiyun.list", args)
	if err != nil {
		return nil, err
	}
	res := &ListResult{
		PdirKey:        strValue(out["pdir_key"]),
		TotalDirCount:  int64Value(out["total_dir_count"]),
		TotalFileCount: int64Value(out["total_file_count"]),
		FinishFlag:     boolValue(out["finish_flag"]),
		Error:          strValue(out["error"]),
	}
	res.DirList = decodeSlice[Dir](out["dir_list"])
	res.FileList = decodeSlice[File](out["file_list"])
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// DownloadItem identifies a file for a link request.
type DownloadItem struct {
	FileID  string `json:"file_id"`
	PdirKey string `json:"pdir_key"`
}

// IndexedFile is the minimal file location needed to request a download link.
// PdirKey is the top-level pdir_key of the listing that contained the file,
// not the usually-empty pdir_key on the file entry itself.
type IndexedFile struct {
	FileID   string
	FileName string
	FileSize int64
	PdirKey  string
}

// IndexOptions bounds a recursive directory scan. The bounds protect the local
// sidecar from pathological directory trees while still allowing large drives.
type IndexOptions struct {
	MaxDirs  int
	MaxFiles int
}

// IndexResult is a file_id index built from recursive weiyun.list calls.
type IndexResult struct {
	Files     map[string]IndexedFile
	DirCount  int
	FileCount int
	Truncated bool
}

type pendingDir struct {
	DirKey  string
	PdirKey string
}

// IndexFiles recursively scans the Weiyun directory tree. QQ Music cloud rows
// only expose file_id, so this index supplies the pdir_key required by
// weiyun.download. The category API is intentionally not used: it can require
// a separate web cookie and may not contain files uploaded through QQ Music.
func (c *Client) IndexFiles(ctx context.Context, opt IndexOptions) (*IndexResult, error) {
	if opt.MaxDirs <= 0 {
		opt.MaxDirs = 2000
	}
	if opt.MaxFiles <= 0 {
		opt.MaxFiles = 50000
	}
	res := &IndexResult{Files: make(map[string]IndexedFile)}
	queue := []pendingDir{{}}
	seenDirs := map[string]bool{}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := queue[0]
		queue = queue[1:]
		if cur.DirKey != "" {
			if seenDirs[cur.DirKey] {
				continue
			}
			seenDirs[cur.DirKey] = true
		}
		if res.DirCount >= opt.MaxDirs {
			res.Truncated = true
			break
		}
		res.DirCount++

		offset := 0
		var pdirKey string
		for page := 0; page < 200; page++ {
			list, err := c.List(ctx, ListOptions{
				DirKey: cur.DirKey, PdirKey: cur.PdirKey,
				Offset: offset, Limit: maxListLimit,
			})
			if err != nil {
				return nil, err
			}
			if list.PdirKey != "" {
				pdirKey = list.PdirKey
			}
			for _, f := range list.FileList {
				if f.FileID == "" || pdirKey == "" {
					continue
				}
				if len(res.Files) >= opt.MaxFiles {
					res.Truncated = true
					return res, nil
				}
				if _, exists := res.Files[f.FileID]; !exists {
					res.Files[f.FileID] = IndexedFile{
						FileID: f.FileID, FileName: f.FileName,
						FileSize: f.FileSize, PdirKey: pdirKey,
					}
					res.FileCount++
				}
			}
			for _, d := range list.DirList {
				if d.DirKey != "" {
					queue = append(queue, pendingDir{DirKey: d.DirKey, PdirKey: pdirKey})
				}
			}
			offset += len(list.DirList) + len(list.FileList)
			if list.FinishFlag || len(list.DirList)+len(list.FileList) == 0 {
				break
			}
		}
	}
	return res, nil
}

// FindFile builds a directory index and returns one file location.
func (c *Client) FindFile(ctx context.Context, fileID string) (*IndexedFile, error) {
	if strings.TrimSpace(fileID) == "" {
		return nil, errors.New("缺少 file_id")
	}
	idx, err := c.IndexFiles(ctx, IndexOptions{})
	if err != nil {
		return nil, err
	}
	f, ok := idx.Files[fileID]
	if !ok {
		if idx.Truncated {
			return nil, errors.New("微云目录过大，索引达到上限后仍未找到该文件")
		}
		return nil, errors.New("微云目录中找不到该文件")
	}
	return &f, nil
}

// DownloadResult is one entry returned by weiyun.download.
type DownloadResult struct {
	FileID           string `json:"file_id"`
	HTTPSDownloadURL string `json:"https_download_url"`
	FileSize         int64  `json:"file_size"`
	Cookie           string `json:"cookie"`
}

// Download asks Weiyun for short-lived HTTPS links and their download cookies.
func (c *Client) Download(ctx context.Context, items []DownloadItem) ([]DownloadResult, error) {
	out, err := c.Call(ctx, "weiyun.download", map[string]any{"items": items})
	if err != nil {
		return nil, err
	}
	if msg := strValue(out["error"]); msg != "" {
		return nil, errors.New(msg)
	}
	return decodeSlice[DownloadResult](out["items"]), nil
}

func decodeSlice[T any](v any) []T {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return nil
	}
	return out
}

func strValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func int64Value(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

func boolValue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		n, _ := x.Int64()
		return n != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	default:
		return false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
