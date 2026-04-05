// Package mcp — MCP 传输层。
//
// 定义 MCP 协议的传输接口和实现：
//   - StdioTransport: 通过子进程 stdin/stdout 通信
//   - HTTPTransport:  通过 HTTP+SSE 通信
//
// 对齐 Claude Code 的 MCP 传输层。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Transport 是 MCP 传输接口。
type Transport interface {
	// Send 发送一个 JSON-RPC 请求并返回响应。
	Send(ctx context.Context, req *Request) (*Response, error)
	// Close 关闭传输连接。
	Close() error
}

// StdioTransport 通过子进程 stdin/stdout 实现 MCP 传输。
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	closed bool
	nextID atomic.Int64
}

// NewStdioTransport 创建一个新的 stdio 传输。
// command 是要启动的 MCP 服务器命令，args 是命令参数。
func NewStdioTransport(command string, args ...string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}

	return &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}, nil
}

// Send 通过 stdio 发送 JSON-RPC 请求。
func (t *StdioTransport) Send(_ context.Context, req *Request) (*Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, fmt.Errorf("传输已关闭")
	}

	// 自动分配 ID（如果未设置）。
	if req.ID == 0 && req.Method != "" {
		req.ID = t.nextID.Add(1)
	}

	// 序列化并发送。
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("写入请求失败: %w", err)
	}

	// 如果是通知（无 ID），不等待响应。
	if req.ID == 0 {
		return &Response{}, nil
	}

	// 读取响应。
	line, err := t.stdout.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &resp, nil
}

// Close 关闭 stdio 传输并终止子进程。
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	t.stdin.Close()
	return t.cmd.Process.Kill()
}

// HTTPTransport 通过 HTTP 实现 MCP 传输。
// 使用 HTTP POST 发送请求，通过 SSE 接收流式响应。
type HTTPTransport struct {
	baseURL string
	mu      sync.Mutex
	closed  bool
	nextID  atomic.Int64
}

// NewHTTPTransport 创建一个新的 HTTP 传输。
func NewHTTPTransport(baseURL string) *HTTPTransport {
	return &HTTPTransport{baseURL: baseURL}
}

// Send 通过 HTTP 发送 JSON-RPC 请求。
// 骨架实现 — 实际需要 HTTP 客户端。
func (t *HTTPTransport) Send(_ context.Context, req *Request) (*Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, fmt.Errorf("传输已关闭")
	}

	// 自动分配 ID。
	if req.ID == 0 && req.Method != "" {
		req.ID = t.nextID.Add(1)
	}

	// TODO: 实际的 HTTP 请求实现。
	// 需要：
	// 1. 将请求 JSON 序列化
	// 2. POST 到 baseURL
	// 3. 解析响应
	return nil, fmt.Errorf("HTTP 传输尚未完整实现")
}

// Close 关闭 HTTP 传输。
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}
