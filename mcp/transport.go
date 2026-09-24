// Package mcp — MCP 传输层。
//
// 定义 MCP 协议的传输接口和实现：
//   - StdioTransport: 子进程 stdin/stdout，每行一条 JSON-RPC 消息
//   - HTTPTransport:  Streamable HTTP（POST JSON-RPC，响应为 JSON 或 SSE 流）
//
// 两种传输都按 id 配对请求与响应：服务端穿插发来的通知被忽略，服务端
// 发起的请求（ping 等）自动回复——不会把别的消息错当成响应。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// Transport 是 MCP 传输接口。
type Transport interface {
	// Send 发送请求并等待对应 id 的响应（id 由传输层分配，覆盖 req.ID）。
	// ctx 取消时立即返回 ctx.Err()。
	Send(ctx context.Context, req *Request) (*Response, error)
	// Notify 发送通知（无 id、不等响应）。
	Notify(ctx context.Context, method string, params any) error
	// Close 关闭传输连接。
	Close() error
}

// ---- stdio ----

// StdioConfig stdio 传输配置。
type StdioConfig struct {
	Command string
	Args    []string
	Env     map[string]string // 在继承的进程环境变量之上追加/覆盖
	Dir     string            // 子进程工作目录（空 = 继承）
}

// StdioTransport 通过子进程 stdin/stdout 实现 MCP 传输。
// 读循环独占 stdout，按 id 把响应分发给等待中的请求。
type StdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64
	stderr  *tailBuffer

	mu      sync.Mutex
	pending map[int64]chan *Response
	closed  bool
	done    chan struct{} // 读循环退出（stdout 关闭 / 进程退出）
	readErr error
}

// NewStdioTransport 创建一个新的 stdio 传输。
// command 是要启动的 MCP 服务器命令，args 是命令参数。
func NewStdioTransport(command string, args ...string) (*StdioTransport, error) {
	return NewStdioTransportConfig(StdioConfig{Command: command, Args: args})
}

// NewStdioTransportConfig 按完整配置（环境变量、工作目录）创建 stdio 传输。
func NewStdioTransportConfig(cfg StdioConfig) (*StdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	t := &StdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  &tailBuffer{max: 4096},
		pending: map[int64]chan *Response{},
		done:    make(chan struct{}),
	}
	cmd.Stderr = t.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}
	go t.readLoop(stdout)
	return t, nil
}

// readLoop 逐行读取 stdout：响应按 id 分发，服务端请求自动回复，通知忽略。
func (t *StdioTransport) readLoop(stdout io.Reader) {
	r := bufio.NewReaderSize(stdout, 64*1024)
	var err error
	for {
		var line []byte
		line, err = r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			t.dispatch(trimmed)
		}
		if err != nil {
			break
		}
	}
	_ = t.cmd.Wait()
	t.mu.Lock()
	if errors.Is(err, io.EOF) {
		err = errors.New("MCP 服务器已退出")
	}
	if tail := t.stderr.String(); tail != "" {
		err = fmt.Errorf("%w；stderr: %s", err, tail)
	}
	t.readErr = err
	t.mu.Unlock()
	close(t.done)
}

func (t *StdioTransport) dispatch(line []byte) {
	var msg wireMessage
	if json.Unmarshal(line, &msg) != nil {
		return // 非 JSON 行（部分服务器把日志打到 stdout）
	}
	switch {
	case msg.isServerRequest():
		_ = t.write(replyTo(&msg))
	case msg.isResponse():
		id, ok := msg.responseID()
		if !ok {
			return
		}
		t.mu.Lock()
		ch := t.pending[id]
		delete(t.pending, id)
		t.mu.Unlock()
		if ch != nil {
			ch <- msg.toResponse(id)
		}
	}
}

func (t *StdioTransport) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入消息失败: %w", err)
	}
	return nil
}

// Send 通过 stdio 发送 JSON-RPC 请求并等待响应。
func (t *StdioTransport) Send(ctx context.Context, req *Request) (*Response, error) {
	req.JSONRPC = "2.0"
	req.ID = t.nextID.Add(1)
	ch := make(chan *Response, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("传输已关闭")
	}
	t.pending[req.ID] = ch
	t.mu.Unlock()

	forget := func() {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
	}
	if err := t.write(req); err != nil {
		forget()
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-t.done:
		forget()
		t.mu.Lock()
		defer t.mu.Unlock()
		return nil, t.readErr
	}
}

// Notify 通过 stdio 发送通知。
func (t *StdioTransport) Notify(_ context.Context, method string, params any) error {
	return t.write(notification(method, params))
}

// Close 关闭 stdio 传输并终止子进程。
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	<-t.done
	return nil
}

// tailBuffer 只保留最后 max 字节的写入缓冲（收集子进程 stderr 供报错）。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// ---- Streamable HTTP ----

// HTTPConfig HTTP 传输配置。
type HTTPConfig struct {
	URL     string
	Headers map[string]string // 附加请求头（如 Authorization）
	Client  *http.Client      // 空 = http.DefaultClient
}

// HTTPTransport 通过 Streamable HTTP 实现 MCP 传输：每条消息一次 POST，
// 响应体为 application/json（单条响应）或 text/event-stream（SSE 流，
// 在其中找到对应 id 的响应）。服务端下发的 Mcp-Session-Id 会在后续请求中带回。
type HTTPTransport struct {
	cfg    HTTPConfig
	client *http.Client
	nextID atomic.Int64

	mu        sync.Mutex
	sessionID string
	closed    bool
}

// NewHTTPTransport 创建一个新的 HTTP 传输。
func NewHTTPTransport(baseURL string) *HTTPTransport {
	return NewHTTPTransportConfig(HTTPConfig{URL: baseURL})
}

// NewHTTPTransportConfig 按完整配置（附加请求头、自定义 http.Client）创建 HTTP 传输。
func NewHTTPTransportConfig(cfg HTTPConfig) *HTTPTransport {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPTransport{cfg: cfg, client: client}
}

// post 发送一条消息，返回 HTTP 响应（调用方负责关闭 Body）。
func (t *HTTPTransport) post(ctx context.Context, v any) (*http.Response, error) {
	t.mu.Lock()
	closed, sid := t.closed, t.sessionID
	t.mu.Unlock()
	if closed {
		return nil, errors.New("传输已关闭")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化消息失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.sessionID = s
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// Send 通过 HTTP 发送 JSON-RPC 请求并等待响应。
func (t *HTTPTransport) Send(ctx context.Context, req *Request) (*Response, error) {
	req.JSONRPC = "2.0"
	req.ID = t.nextID.Add(1)
	resp, err := t.post(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return t.readSSE(ctx, resp.Body, req.ID)
	}
	var msg wireMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	id, _ := msg.responseID()
	return msg.toResponse(id), nil
}

// readSSE 读取 SSE 流直到拿到 id 对应的响应（期间的通知忽略、服务端请求回复）。
func (t *HTTPTransport) readSSE(ctx context.Context, body io.Reader, id int64) (*Response, error) {
	r := bufio.NewReaderSize(body, 64*1024)
	var data []string
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && len(data) > 0:
			var msg wireMessage
			if json.Unmarshal([]byte(strings.Join(data, "\n")), &msg) == nil {
				if msg.isServerRequest() {
					go t.reply(&msg)
				} else if got, ok := msg.responseID(); ok && msg.isResponse() && got == id {
					return msg.toResponse(id), nil
				}
			}
			data = data[:0]
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("SSE 流在收到响应前结束: %w", err)
		}
	}
}

func (t *HTTPTransport) reply(msg *wireMessage) {
	if resp, err := t.post(context.Background(), replyTo(msg)); err == nil {
		resp.Body.Close()
	}
}

// Notify 通过 HTTP 发送通知（服务端应答 202，无响应体）。
func (t *HTTPTransport) Notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, notification(method, params))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Close 关闭 HTTP 传输（有会话 id 时尽力通知服务端结束会话）。
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		if req, err := http.NewRequest(http.MethodDelete, t.cfg.URL, nil); err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			for k, v := range t.cfg.Headers {
				req.Header.Set(k, v)
			}
			if resp, err := t.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	return nil
}
