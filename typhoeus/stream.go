package typhoeus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team-quaver/typhoeus-go/qmc"
)

// ResumeRetries 断流续传次数（导出给中继层使用）。
const ResumeRetries = resumeRetries

// 回源流中继（对标 typhoeus/stream.py）：
//
//	只做一件事——向 CDN 带 Range 取一段，按需解密（仅加密档），流式转发。
//	不落盘、不整文件缓存；断流可按已发字节重连续传。
const (
	upstreamUA     = "Mozilla/5.0 (X11; Linux x86_64) Quaver/0.1 typhoeus-go"
	upstreamTimout = 15 * time.Second
	chunkSize      = 256 * 1024
	// 断流续传次数与退避
	resumeRetries = 2
	resumeBackoff = 250 * time.Millisecond
)

// upstreamClient 回源专用 HTTP 客户端（长超时以支持慢速播放）。
var upstreamClient = &http.Client{
	Timeout: 0, // 不设总超时；由 ctx 与每段读取控制
	Transport: &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ByteRange 请求区间（闭区间；Start/End 可为 nil = 开放端）。
type ByteRange struct {
	Start *int64
	End   *int64
}

// ParseRange 解析 "bytes=a-b" / "bytes=a-" / "bytes=-b" 请求头。
func ParseRange(header string) *ByteRange {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "bytes=") {
		return nil
	}
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return nil
	}
	rng := &ByteRange{}
	if parts[0] != "" {
		v, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil
		}
		rng.Start = &v
	}
	if parts[1] != "" {
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil
		}
		rng.End = &v
	}
	if rng.Start == nil && rng.End == nil {
		return nil
	}
	return rng
}

// Clamp 把开区间规整到具体闭区间（total<=0 视为未知长度，原样返回）。
func (r *ByteRange) Clamp(total int64) *ByteRange {
	if total <= 0 {
		return r
	}
	out := &ByteRange{}
	switch {
	case r.Start == nil && r.End == nil:
		return nil
	case r.Start == nil: // 后缀 bytes=-N → 最后 N 字节
		n := *r.End
		if n > total {
			n = total
		}
		s := total - n
		e := total - 1
		out.Start, out.End = &s, &e
	default:
		s := *r.Start
		var e int64
		if r.End == nil || *r.End > total-1 {
			e = total - 1
		} else {
			e = *r.End
		}
		out.Start, out.End = &s, &e
	}
	return out
}

// Header 序列化回 Range 头。
func (r *ByteRange) Header() string {
	var sb strings.Builder
	sb.WriteString("bytes=")
	if r.Start != nil {
		sb.WriteString(strconv.FormatInt(*r.Start, 10))
	}
	sb.WriteString("-")
	if r.End != nil {
		sb.WriteString(strconv.FormatInt(*r.End, 10))
	}
	return sb.String()
}

// UpstreamResponse 回源响应摘要。
type UpstreamResponse struct {
	Status       int
	Header       http.Header
	ContentRange string
	Total        int64 // 从 Content-Range 解析的文件总长（无则为 0）
	Body         io.ReadCloser
}

// openRange 打开上游区段流。
func OpenRange(ctx context.Context, url string, rng *ByteRange) (*UpstreamResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, errStream("回源请求构造失败: " + err.Error())
	}
	req.Header.Set("User-Agent", upstreamUA)
	if rng != nil && (rng.Start != nil || rng.End != nil) {
		req.Header.Set("Range", rng.Header())
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, errStream("回源连接失败: " + err.Error())
	}
	cr := resp.Header.Get("Content-Range")
	total := parseTotal(cr)
	return &UpstreamResponse{
		Status:       resp.StatusCode,
		Header:       resp.Header,
		ContentRange: cr,
		Total:        total,
		Body:         resp.Body,
	}, nil
}

// parseTotal 从 "bytes 0-99/5000" 提取总长。
func parseTotal(contentRange string) int64 {
	idx := strings.LastIndex(contentRange, "/")
	if idx < 0 {
		return 0
	}
	v, err := strconv.ParseInt(contentRange[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ProbeTotal 探测文件总长：对 CDN 发 bytes=0-0，从 Content-Range 解析（不下载实体）。
func ProbeTotal(ctx context.Context, url string) (int64, error) {
	_, total, err := fetchRange(ctx, url, 0, 0)
	return total, err
}

// CopyRange 把上游响应流按需解密后写到 w。
//
// resume=0 表示不续传（后缀 Range 且总长未知时无从推导偏移）；
// cipher 非 nil 时按「绝对偏移」逐块解密——块边界与网络分片无关，
// 任意重连位置都能无缝续解。
func CopyRange(ctx context.Context, w io.Writer, url string, first *UpstreamResponse,
	cipher qmc.Cipher, start, end int64, totalKnown bool, resume int) error {

	pos := start
	resp := first
	left := resume
	for {
		err := pump(ctx, w, resp.Body, pos, cipher)
		if err == nil {
			// 正常收尾：定长响应被截断时 net/http 只静默 EOF，必须靠 pos 比对发现
			if !totalKnown || end < 0 || pos > end {
				return nil
			}
			err = errStream(fmt.Sprintf("回源提前 EOF（已发 %d 字节）", pos))
		}
		// 已发完本段就不关后面的事
		if totalKnown && end >= 0 && pos > end {
			return nil
		}
		if left <= 0 {
			return err
		}
		left--
		time.Sleep(resumeBackoff)
		resp.Body.Close()
		next, rerr := reopenAt(ctx, url, pos)
		if rerr != nil {
			return err // 续传失败，报原始错误
		}
		resp = next
	}
}

// pump 从 body 逐块读取，按绝对偏移解密（cipher 非 nil）后写入 w。
func pump(ctx context.Context, w io.Writer, body io.Reader, startPos int64, cipher qmc.Cipher) error {
	buf := make([]byte, chunkSize)
	pos := startPos
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, rerr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if cipher != nil {
				chunk = cipher.Decrypt(chunk, pos)
			}
			if _, werr := w.Write(chunk); werr != nil {
				return werr // 客户端断开：直接返回
			}
			pos += int64(n)
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// reopenAt 从 pos 重开一段（上游不认 Range 时宁可放弃也不重发字节）。
func reopenAt(ctx context.Context, url string, pos int64) (*UpstreamResponse, error) {
	rng := &ByteRange{Start: &pos}
	next, err := OpenRange(ctx, url, rng)
	if err != nil {
		return nil, err
	}
	if next.Status != 200 && next.Status != 206 {
		next.Body.Close()
		return nil, errStream(fmt.Sprintf("续传失败 HTTP %d", next.Status))
	}
	if next.Status == 200 && pos > 0 {
		next.Body.Close()
		return nil, errStream("上游忽略 Range（回 200 全量），放弃续传以免字节重复")
	}
	if got := rangeStart(next.ContentRange); got >= 0 && got != pos {
		next.Body.Close()
		return nil, errStream(fmt.Sprintf("续传起点不符：期望 %d，上游给 %d", pos, got))
	}
	return next, nil
}

// rangeStart 从 Content-Range 提取起始偏移（无则 -1）。
func rangeStart(contentRange string) int64 {
	if contentRange == "" {
		return -1
	}
	parts := strings.SplitN(contentRange, " ", 2)
	if len(parts) < 2 {
		return -1
	}
	segs := strings.SplitN(parts[1], "-", 2)
	v, err := strconv.ParseInt(segs[0], 10, 64)
	if err != nil {
		return -1
	}
	return v
}
