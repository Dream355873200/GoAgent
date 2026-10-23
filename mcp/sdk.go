// Package mcp — 官方 Go SDK（github.com/modelcontextprotocol/go-sdk）承载的连接。
//
// Dial / Connect 建立的客户端走这里：握手、分页列工具、调用、取消（ctx 取消时
// SDK 向服务端发 notifications/cancelled）、进度通知（notifications/progress）、
// 工具列表变更（notifications/tools/list_changed）都由 SDK 处理。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultProtocolVersion 是 Dial 握手时请求的协议版本（ServerConfig.ProtocolVersion
// 为空时）。固定在走 initialize 握手的最新版本：更新的版本会先探测
// server/discover，不认识它的老服务器可能一直不回应，直到连接超时。服务端可
// 协商为自己支持的旧版本。
const DefaultProtocolVersion = "2025-11-25"

// clientVersion 是握手时自报的客户端版本。
const clientVersion = "0.14.0"

// dialSDK 按配置建立 SDK 会话并完成握手。
func dialSDK(ctx context.Context, cfg ServerConfig) (*Client, error) {
	c := &Client{name: cfg.Name}
	opts := &sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			// 不能在 SDK 的接收协程里同步发请求：另起协程重新列工具。
			go c.refreshTools()
		},
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			if req == nil || req.Params == nil {
				return
			}
			p := req.Params
			c.deliverProgress(fmt.Sprint(p.ProgressToken), Progress{Progress: p.Progress, Total: p.Total, Message: p.Message})
		},
	}
	if handler := cfg.Elicit; handler != nil {
		// 设置处理函数即向服务器声明 elicitation 能力（form 与 url 两种模式）。
		opts.ElicitationHandler = func(ctx context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return c.handleElicit(ctx, handler, req)
		}
		opts.Capabilities = &sdk.ClientCapabilities{
			RootsV2:     &sdk.RootCapabilities{ListChanged: true},
			Elicitation: &sdk.ElicitationCapabilities{Form: &sdk.FormElicitationCapabilities{}, URL: &sdk.URLElicitationCapabilities{}},
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "goagent", Version: clientVersion}, opts)

	var transport sdk.Transport
	if cfg.Command != "" {
		c.stderr = &tailBuffer{max: 4096}
		transport = &stdioTransport{cfg: StdioConfig{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, Dir: cfg.Dir}, stderr: c.stderr}
	} else {
		transport = &sdk.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{headers: cfg.Headers, base: http.DefaultTransport}},
		}
	}
	version := cfg.ProtocolVersion
	if version == "" {
		version = DefaultProtocolVersion
	}
	session, err := client.Connect(ctx, transport, &sdk.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		if c.stderr != nil {
			if tail := c.stderr.String(); tail != "" {
				err = fmt.Errorf("%w；stderr: %s", err, tail)
			}
		}
		return nil, fmt.Errorf("MCP 初始化失败: %w", err)
	}
	c.session = session
	c.connected = true
	if init := session.InitializeResult(); init != nil {
		c.protocolVersion = init.ProtocolVersion
		if init.ServerInfo != nil {
			c.serverInfo = &ServerInfo{Name: init.ServerInfo.Name, Version: init.ServerInfo.Version}
		}
	}
	return c, nil
}

// sdkListTools 分页列出全部工具。
func (c *Client) sdkListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for {
		res, err := c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("获取工具列表失败: %w", err)
		}
		for _, t := range res.Tools {
			if t == nil {
				continue
			}
			all = append(all, ToolInfo{Name: t.Name, Description: t.Description, InputSchema: schemaMap(t.InputSchema)})
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// sdkCallTool 调用工具；onProgress 非空时附带进度令牌，服务端的进度通知转给它。
func (c *Client) sdkCallTool(ctx context.Context, name string, args map[string]any, onProgress func(Progress)) (*ToolCallResult, error) {
	params := &sdk.CallToolParams{Name: name, Arguments: args}
	if onProgress != nil {
		token := fmt.Sprintf("goagent-%d", c.progressSeq.Add(1))
		c.progress.Store(token, onProgress)
		defer c.progress.Delete(token)
		params.SetProgressToken(token)
	}
	done := c.calls.add(ctx)
	res, err := c.session.CallTool(ctx, params)
	done()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("调用工具 %q 已取消: %w", name, ctxErr)
		}
		return nil, fmt.Errorf("调用工具 %q 失败: %w", name, err)
	}
	return convertResult(res), nil
}

func (c *Client) deliverProgress(token string, p Progress) {
	if fn, ok := c.progress.Load(token); ok {
		fn.(func(Progress))(p)
	}
}

// refreshTools 收到 tools/list_changed 后重新列工具，并通知 OnToolsChanged 的订阅者。
func (c *Client) refreshTools() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx)
	if err != nil {
		return
	}
	c.listenersMu.Lock()
	listeners := append([]func([]ToolInfo){}, c.listeners...)
	c.listenersMu.Unlock()
	for _, fn := range listeners {
		fn(tools)
	}
}

// schemaMap 把 SDK 工具的 inputSchema（任意 JSON Schema 值）转为 map。
func schemaMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// convertResult 把 SDK 的调用结果转为本包的 ToolCallResult。只有结构化结果、
// 没有内容项时，把结构化结果的 JSON 作为一条文本。
func convertResult(res *sdk.CallToolResult) *ToolCallResult {
	out := &ToolCallResult{IsError: res.IsError}
	for _, item := range res.Content {
		switch v := item.(type) {
		case *sdk.TextContent:
			out.Content = append(out.Content, ContentItem{Type: "text", Text: v.Text})
		case *sdk.ImageContent:
			out.Content = append(out.Content, ContentItem{Type: "image", MimeType: v.MIMEType, Data: base64.StdEncoding.EncodeToString(v.Data)})
		case *sdk.AudioContent:
			out.Content = append(out.Content, ContentItem{Type: "audio", MimeType: v.MIMEType, Data: base64.StdEncoding.EncodeToString(v.Data)})
		case *sdk.ResourceLink:
			out.Content = append(out.Content, ContentItem{Type: "resource_link", URI: v.URI, MimeType: v.MIMEType})
		case *sdk.EmbeddedResource:
			if r := v.Resource; r != nil {
				rc := &ResourceContent{URI: r.URI, MimeType: r.MIMEType, Text: r.Text}
				if len(r.Blob) > 0 {
					rc.Blob = base64.StdEncoding.EncodeToString(r.Blob)
				}
				out.Content = append(out.Content, ContentItem{Type: "resource", Resource: rc})
			}
		}
	}
	if len(out.Content) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			out.Content = append(out.Content, ContentItem{Type: "text", Text: string(b)})
		}
	}
	return out
}

// headerTransport 给每个请求附加配置的请求头（Authorization 等）。
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(req)
}

// stdioTransport 启动子进程，经 SDK 的 IOTransport 收发换行分隔的 JSON-RPC。
// stdout 上不是 JSON 的行（服务器误打到 stdout 的日志）被丢弃——否则 SDK
// 会因解析失败断开整个会话。关闭时先关 stdin，等进程退出，超时再 SIGTERM、
// 最后 SIGKILL（与 SDK 的 CommandTransport 一致）。
type stdioTransport struct {
	cfg    StdioConfig
	stderr *tailBuffer
}

const stdioTerminateWait = 5 * time.Second

func (t *stdioTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	cmd := exec.Command(t.cfg.Command, t.cfg.Args...)
	cmd.Dir = t.cfg.Dir
	if len(t.cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range t.cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	cmd.Stderr = t.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}
	conn := &sdk.IOTransport{
		Reader: &jsonLines{r: bufio.NewReaderSize(stdout, 64*1024)},
		Writer: &processStdin{WriteCloser: stdin, cmd: cmd},
	}
	return conn.Connect(ctx)
}

// jsonLines 只放行以 { 或 [ 开头的行（JSON-RPC 消息或批），其余行丢弃。
// Close 不关闭 stdout：会话由关闭 stdin 结束，进程退出后 stdout 自然 EOF。
type jsonLines struct {
	r   *bufio.Reader
	buf []byte
}

func (j *jsonLines) Read(p []byte) (int, error) {
	for len(j.buf) == 0 {
		line, err := j.r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			j.buf = append(trimmed, '\n')
		}
		if err != nil {
			if len(j.buf) > 0 {
				break
			}
			return 0, err
		}
	}
	n := copy(p, j.buf)
	j.buf = j.buf[n:]
	return n, nil
}

func (j *jsonLines) Close() error { return nil }

// processStdin 关闭时按 MCP 规范结束子进程：关 stdin → 等待 → SIGTERM → SIGKILL。
type processStdin struct {
	io.WriteCloser
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func (s *processStdin) Close() error {
	s.once.Do(func() {
		_ = s.WriteCloser.Close()
		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()
		wait := func() bool {
			select {
			case <-done:
				return true
			case <-time.After(stdioTerminateWait):
				return false
			}
		}
		if wait() {
			return
		}
		if err := s.cmd.Process.Signal(syscall.SIGTERM); err == nil && wait() {
			return
		}
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			s.err = err
			return
		}
		if !wait() {
			s.err = errors.New("MCP 服务器进程无响应")
		}
	})
	return s.err
}

// ModelText 把工具结果转为交给模型的文本：第一张图片以内联图片标记
// （message.SplitImageMarker 的格式）放在开头，provider 会把它转成多模态
// 输入；其余内容同 ExtractText。
func (r *ToolCallResult) ModelText() string {
	for i, item := range r.Content {
		if item.Type != "image" || item.Data == "" {
			continue
		}
		rest := &ToolCallResult{Content: append(append([]ContentItem{}, r.Content[:i]...), r.Content[i+1:]...)}
		media := strings.TrimPrefix(strings.ToLower(item.MimeType), "image/")
		if media == "" {
			media = "png"
		}
		return "[IMAGE " + media + " " + item.Data + "]\n" + rest.ExtractText()
	}
	return r.ExtractText()
}
