// Package mcp 实现 Model Context Protocol (MCP) 客户端。
//
// MCP 是一种标准化协议，允许 LLM 应用与外部工具服务器通信。
// 此包实现客户端侧，支持通过 stdio 和 Streamable HTTP 传输发现和调用远程工具。
//
// 最常用的入口是 Connect：按 ServerConfig 建立连接、握手、发现工具并转换为
// 框架工具（见 dial.go）。Dial / Connect 建立的连接由官方 Go SDK 承载（见
// sdk.go）：调用取消、进度通知、工具列表变更、图片结果都可用。用 NewClient
// 直接给传输构造的客户端走本包早期的实现，不支持这些能力。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProtocolVersion 是 NewClient 构造的客户端握手时声明的协议版本（服务端可
// 协商为自己支持的版本）。Dial 使用 DefaultProtocolVersion。
const ProtocolVersion = "2025-03-26"

// Client 是 MCP 协议客户端。
type Client struct {
	mu              sync.RWMutex
	name            string             // 配置名（工具名前缀；空则回落服务端自报名）
	transport       Transport          // NewClient 给的传输（早期实现）
	session         *sdk.ClientSession // Dial 建立的 SDK 会话
	tools           []ToolInfo
	connected       bool
	serverInfo      *ServerInfo
	protocolVersion string

	// SDK 会话专用：进度令牌 → 回调；工具列表变更的订阅者；子进程 stderr 尾部。
	progressSeq atomic.Int64
	progress    sync.Map
	listenersMu sync.Mutex
	listeners   []func([]ToolInfo)
	stderr      *tailBuffer
	calls       inflight // 进行中的工具调用（elicitation 据此找到触发它的调用）
}

// ServerInfo 包含 MCP 服务器信息。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Progress 是工具调用期间服务端发来的进度通知（notifications/progress）。
type Progress struct {
	Progress float64
	Total    float64 // 0 表示总量未知
	Message  string
}

// String 返回一行进度描述。
func (p Progress) String() string {
	switch {
	case p.Total > 0 && p.Message != "":
		return fmt.Sprintf("[%g/%g] %s", p.Progress, p.Total, p.Message)
	case p.Total > 0:
		return fmt.Sprintf("进度 %g/%g", p.Progress, p.Total)
	case p.Message != "":
		return p.Message
	default:
		return fmt.Sprintf("进度 %g", p.Progress)
	}
}

// ClientOption 客户端选项。
type ClientOption func(*Client)

// WithName 设置服务器配置名：工具注册名的前缀取它，而不是服务端自报的
// serverInfo.name（后者不可控，可能重复或含非法字符）。
func WithName(name string) ClientOption {
	return func(c *Client) { c.name = name }
}

// NewClient 用给定的传输创建客户端（需再调用 Connect 握手）。
//
// Deprecated: 用 Dial / Connect 按 ServerConfig 建立连接——它们由官方 Go SDK
// 承载，支持调用取消、进度通知、工具列表变更与图片结果；直接给传输构造的
// 客户端不支持这些能力。
func NewClient(transport Transport, opts ...ClientOption) *Client {
	c := &Client{transport: transport}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect 完成初始化握手（重复调用无副作用；Dial 建立的客户端已经握手）。
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}
	if c.transport == nil {
		return fmt.Errorf("MCP 客户端没有传输")
	}

	resp, err := c.transport.Send(ctx, &Request{
		Method: "initialize",
		Params: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "goagent",
				"version": clientVersion,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("MCP 初始化失败: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("MCP 初始化被拒绝: %w", resp.Error)
	}

	var initResult struct {
		ProtocolVersion string     `json:"protocolVersion"`
		ServerInfo      ServerInfo `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &initResult); err == nil {
		c.serverInfo = &initResult.ServerInfo
		c.protocolVersion = initResult.ProtocolVersion
	}

	if err := c.transport.Notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("MCP 初始化通知失败: %w", err)
	}

	c.connected = true
	return nil
}

// ListTools 获取服务器提供的所有工具列表（自动翻页）。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if !c.IsConnected() {
		return nil, fmt.Errorf("MCP 客户端未连接")
	}
	var (
		all []ToolInfo
		err error
	)
	if c.session != nil {
		all, err = c.sdkListTools(ctx)
	} else {
		all, err = c.legacyListTools(ctx)
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tools = all
	c.mu.Unlock()
	return all, nil
}

func (c *Client) legacyListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		resp, err := c.transport.Send(ctx, &Request{Method: "tools/list", Params: params})
		if err != nil {
			return nil, fmt.Errorf("获取工具列表失败: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("获取工具列表失败: %w", resp.Error)
		}
		var page struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Result, &page); err != nil {
			return nil, fmt.Errorf("解析工具列表失败: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// CallTool 调用指定的 MCP 工具。ctx 取消时调用立即返回，Dial 建立的连接同时
// 通知服务端取消（notifications/cancelled）。
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolCallResult, error) {
	return c.CallToolWithProgress(ctx, name, arguments, nil)
}

// CallToolWithProgress 同 CallTool，onProgress 非空时接收服务端的进度通知
// （只有 Dial 建立的连接支持；其他情况下忽略）。
func (c *Client) CallToolWithProgress(ctx context.Context, name string, arguments map[string]any, onProgress func(Progress)) (*ToolCallResult, error) {
	if !c.IsConnected() {
		return nil, fmt.Errorf("MCP 客户端未连接")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if c.session != nil {
		return c.sdkCallTool(ctx, name, arguments, onProgress)
	}

	resp, err := c.transport.Send(ctx, &Request{
		Method: "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("调用工具 %q 失败: %w", name, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("工具 %q 返回错误: %w", name, resp.Error)
	}

	var result ToolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("解析工具结果失败: %w", err)
	}
	return &result, nil
}

// OnToolsChanged 订阅工具列表变更：服务端发出 notifications/tools/list_changed
// 后客户端重新列出工具，以新列表调用 fn（在后台协程中）。只有 Dial 建立的
// 连接会收到变更通知。
func (c *Client) OnToolsChanged(fn func(tools []ToolInfo)) {
	c.listenersMu.Lock()
	defer c.listenersMu.Unlock()
	c.listeners = append(c.listeners, fn)
}

// Disconnect 断开与 MCP 服务器的连接（关闭会话；stdio 服务器进程随之终止）。
func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	if c.session != nil {
		return c.session.Close()
	}
	if c.transport != nil {
		return c.transport.Close()
	}
	return nil
}

// IsConnected 返回客户端是否已连接。
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// ServerName 返回服务器名称：配置名优先，未配置时取服务端自报名。
func (c *Client) ServerName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.name != "" {
		return c.name
	}
	if c.serverInfo != nil {
		return c.serverInfo.Name
	}
	return ""
}

// ServerInfo 返回服务端握手时自报的信息（未连接返回 nil）。
func (c *Client) ServerInfo() *ServerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo
}

// NegotiatedVersion 返回握手协商出的协议版本（未连接时为空）。
func (c *Client) NegotiatedVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.protocolVersion
}
