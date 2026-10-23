// Package httprr 录制与回放 HTTP 流量，用于确定性地测试多轮 agent 循环：第一次
// 对真实服务录制（模型提供方、MCP 服务器），之后在没有网络、没有密钥的环境里
// 原样回放。
//
//	rr, err := httprr.Open("testdata/weather.httprr", http.DefaultTransport)
//	if errors.Is(err, fs.ErrNotExist) {
//	    t.Skip("没有录制文件：设置 GOAGENT_HTTPRR=record 并提供密钥后录制")
//	}
//	defer rr.Close()
//	prov := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"), anthropic.WithHTTPClient(rr.Client()))
//
// 模式由环境变量 GOAGENT_HTTPRR 决定：值为 "record"（或 "1"、"true"）时录制全部
// 文件，其他非空值作为正则表达式匹配文件路径、只录制匹配的；未设置时回放。
//
// 回放按"清洗后的请求"精确匹配：方法、URL 与请求体（请求头不参与匹配——SDK 的
// User-Agent、X-Stainless-* 等随版本和机器变化）。同一请求录到多次时按录制顺序
// 依次回放（重试场景：先 429 后成功），用完或找不到时返回错误。
//
// 录制前清洗：删除鉴权与易变请求头（Authorization、X-Api-Key、Cookie、
// User-Agent、X-Stainless-* 等）、URL 中的 key / api_key 参数与响应的
// Set-Cookie；ScrubReq / ScrubResp 追加自定义清洗（同时作用于录制与回放时的
// 匹配键）。录制时不发送 Accept-Encoding，由 Go 透明解压，录制文件保持可读。
//
// 文件格式（与 golang.org/x/oscar 的 httprr 同构）：首行 "httprr trace v1"，之后
// 每条记录是一行 "<请求字节数> <响应字节数>"，紧跟 HTTP/1.1 线格式的请求与响应。
package httprr

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// EnvVar 是选择录制模式的环境变量。
const EnvVar = "GOAGENT_HTTPRR"

const header = "httprr trace v1\n"

// Mode 是录制或回放。
type Mode int

const (
	// Replay 从文件回放，不访问网络。
	Replay Mode = iota
	// Record 访问真实服务并写入文件（覆盖）。
	Record
)

// ModeFor 按环境变量 GOAGENT_HTTPRR 返回 path 的模式。
func ModeFor(path string) Mode {
	v := os.Getenv(EnvVar)
	switch strings.ToLower(v) {
	case "":
		return Replay
	case "record", "1", "true":
		return Record
	}
	if re, err := regexp.Compile(v); err == nil && re.MatchString(filepath.ToSlash(path)) {
		return Record
	}
	return Replay
}

// RecordReplay 是录制或回放的 http.RoundTripper。并发安全。
type RecordReplay struct {
	path string
	mode Mode
	real http.RoundTripper

	mu        sync.Mutex
	reqScrub  []func(*http.Request) error
	respScrub []func(*bytes.Buffer) error
	replay    map[string][]string // 匹配键 → 依次回放的响应
	file      *os.File            // 录制中的文件
	writeErr  error
}

// Open 按 ModeFor(path) 打开：录制时经 rt 访问真实服务（rt 为空时用
// http.DefaultTransport）；回放时文件不存在返回包装 fs.ErrNotExist 的错误。
func Open(path string, rt http.RoundTripper) (*RecordReplay, error) {
	return OpenMode(path, ModeFor(path), rt)
}

// OpenMode 以指定模式打开。
func OpenMode(path string, mode Mode, rt http.RoundTripper) (*RecordReplay, error) {
	if rt == nil {
		rt = http.DefaultTransport
	}
	rr := &RecordReplay{path: path, mode: mode, real: rt}
	if mode == Record {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(header); err != nil {
			f.Close()
			return nil, err
		}
		rr.file = f
		return rr, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("httprr: %w", err)
	}
	if err := rr.load(data); err != nil {
		return nil, fmt.Errorf("httprr: %s: %w", path, err)
	}
	return rr, nil
}

// Recording 报告是否在录制。
func (rr *RecordReplay) Recording() bool { return rr.mode == Record }

// Client 返回经由本传输的 HTTP 客户端。
func (rr *RecordReplay) Client() *http.Client { return &http.Client{Transport: rr} }

// ScrubReq 追加请求清洗函数：在录制写入前与计算匹配键前调用，可删改请求头、
// URL 与请求体（替换 req.Body）。
func (rr *RecordReplay) ScrubReq(fns ...func(*http.Request) error) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.reqScrub = append(rr.reqScrub, fns...)
}

// ScrubResp 追加响应清洗函数：在录制写入前对线格式的整个响应调用。
func (rr *RecordReplay) ScrubResp(fns ...func(*bytes.Buffer) error) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.respScrub = append(rr.respScrub, fns...)
}

// Close 结束录制（回放时无操作），返回录制过程中的写入错误。
func (rr *RecordReplay) Close() error {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.file == nil {
		return nil
	}
	err := rr.file.Close()
	rr.file = nil
	if rr.writeErr != nil {
		return rr.writeErr
	}
	return err
}

// load 解析录制文件。
func (rr *RecordReplay) load(data []byte) error {
	rest, ok := bytes.CutPrefix(data, []byte(header))
	if !ok {
		return errors.New("不是 httprr 录制文件（缺少首行）")
	}
	rr.replay = map[string][]string{}
	for len(rest) > 0 {
		line, after, ok := bytes.Cut(rest, []byte("\n"))
		if !ok {
			return errors.New("记录头不完整")
		}
		var n1, n2 int
		if _, err := fmt.Sscanf(string(line), "%d %d", &n1, &n2); err != nil || n1 < 0 || n2 < 0 || n1+n2 > len(after) {
			return fmt.Errorf("记录头无效: %q", line)
		}
		reqWire, respWire := after[:n1], after[n1:n1+n2]
		rest = after[n1+n2:]
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqWire)))
		if err != nil {
			return fmt.Errorf("解析录制的请求: %w", err)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("读取录制的请求体: %w", err)
		}
		key := matchKey(req.Method, req.URL, body)
		rr.replay[key] = append(rr.replay[key], string(respWire))
	}
	return nil
}

// matchKey 是回放的匹配键：方法、URL 与请求体。URL 取 WriteProxy 写出的形态
// （不含 userinfo 与 fragment），录制文件解析出的请求与实时请求算出同一个键。
func matchKey(method string, u *url.URL, body []byte) string {
	return method + " " + u.Scheme + "://" + u.Host + u.RequestURI() + "\n" + string(body)
}

// sensitiveHeaders 录制前删除的请求头（鉴权与随版本、机器变化的头）。
var sensitiveHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "Api-Key",
	"X-Goog-Api-Key", "Openai-Organization", "Openai-Project", "Idempotency-Key",
}

// scrubbed 返回请求的清洗副本（请求体读入内存）与清洗后的请求体。
func (rr *RecordReplay) scrubbed(req *http.Request, body []byte) (*http.Request, []byte, error) {
	c := req.Clone(req.Context())
	c.Header = req.Header.Clone()
	for _, h := range sensitiveHeaders {
		c.Header.Del(h)
	}
	for k := range c.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-stainless-") {
			delete(c.Header, k)
		}
	}
	c.Header.Del("Accept-Encoding")
	c.Header["User-Agent"] = []string{""} // Request.Write 不再补默认 UA
	if q := c.URL.Query(); q.Has("key") || q.Has("api_key") {
		q.Del("key")
		q.Del("api_key")
		u := *c.URL
		u.RawQuery = q.Encode()
		c.URL = &u
	}
	c.Body = io.NopCloser(bytes.NewReader(body))
	c.ContentLength = int64(len(body))
	rr.mu.Lock()
	scrub := append([]func(*http.Request) error(nil), rr.reqScrub...)
	rr.mu.Unlock()
	for _, fn := range scrub {
		if err := fn(c); err != nil {
			return nil, nil, err
		}
	}
	if c.Body != nil {
		b, err := io.ReadAll(c.Body)
		if err != nil {
			return nil, nil, err
		}
		body = b
	} else {
		body = nil
	}
	c.Body = io.NopCloser(bytes.NewReader(body))
	c.ContentLength = int64(len(body))
	return c, body, nil
}

// RoundTrip 实现 http.RoundTripper。
func (rr *RecordReplay) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("httprr: 读取请求体: %w", err)
		}
		body = b
	}
	clean, cleanBody, err := rr.scrubbed(req, body)
	if err != nil {
		return nil, fmt.Errorf("httprr: 清洗请求: %w", err)
	}
	if rr.mode == Replay {
		return rr.replayOne(req, clean, cleanBody)
	}
	return rr.recordOne(req, body, clean)
}

func (rr *RecordReplay) replayOne(req, clean *http.Request, body []byte) (*http.Response, error) {
	key := matchKey(clean.Method, clean.URL, body)
	rr.mu.Lock()
	queue, ok := rr.replay[key]
	var wire string
	if len(queue) > 0 {
		wire = queue[0]
		rr.replay[key] = queue[1:]
	}
	rr.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("httprr: 录制中没有这个请求: %s %s（%s；请求体或 URL 与录制时不同，重新录制设置 %s=record）",
			clean.Method, clean.URL, rr.sameURLHint(clean), EnvVar)
	}
	if wire == "" {
		return nil, fmt.Errorf("httprr: 请求 %s %s 的录制响应已用完（比录制时多发了一次）", clean.Method, clean.URL)
	}
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), req)
	if err != nil {
		return nil, fmt.Errorf("httprr: 解析录制的响应: %w", err)
	}
	return resp, nil
}

// sameURLHint 统计方法与 URL 相同的录制请求数，帮助判断是请求体不同还是 URL 不同。
func (rr *RecordReplay) sameURLHint(clean *http.Request) string {
	prefix := matchKey(clean.Method, clean.URL, nil)
	rr.mu.Lock()
	defer rr.mu.Unlock()
	n := 0
	for k := range rr.replay {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return strconv.Itoa(n) + " 个录制请求的方法与 URL 相同"
}

func (rr *RecordReplay) recordOne(req *http.Request, body []byte, clean *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header = req.Header.Clone()
	out.Header.Del("Accept-Encoding")
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	if req.Body == nil {
		out.Body = nil
	}
	resp, err := rr.real.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("httprr: 读取响应: %w", err)
	}

	var reqWire bytes.Buffer
	if err := clean.WriteProxy(&reqWire); err != nil {
		return nil, fmt.Errorf("httprr: 序列化请求: %w", err)
	}
	saved := *resp
	saved.Header = resp.Header.Clone()
	saved.Header.Del("Set-Cookie")
	saved.TransferEncoding = nil
	saved.ContentLength = int64(len(respBody))
	saved.Body = io.NopCloser(bytes.NewReader(respBody))
	var respWire bytes.Buffer
	if err := saved.Write(&respWire); err != nil {
		return nil, fmt.Errorf("httprr: 序列化响应: %w", err)
	}
	rr.mu.Lock()
	scrub := append([]func(*bytes.Buffer) error(nil), rr.respScrub...)
	rr.mu.Unlock()
	for _, fn := range scrub {
		if err := fn(&respWire); err != nil {
			return nil, fmt.Errorf("httprr: 清洗响应: %w", err)
		}
	}

	rr.mu.Lock()
	if rr.file == nil {
		rr.mu.Unlock()
		return nil, errors.New("httprr: 录制已关闭")
	}
	if rr.writeErr == nil {
		_, rr.writeErr = fmt.Fprintf(rr.file, "%d %d\n%s%s", reqWire.Len(), respWire.Len(), reqWire.Bytes(), respWire.Bytes())
	}
	rr.mu.Unlock()

	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	resp.ContentLength = int64(len(respBody))
	resp.TransferEncoding = nil
	return resp, nil
}

var _ http.RoundTripper = (*RecordReplay)(nil)
