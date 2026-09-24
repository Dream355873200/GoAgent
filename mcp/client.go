// Package mcp 实现 Model Context Protocol (MCP) 客户端。
//
// MCP 是一种标准化协议，允许 LLM 应用与外部工具服务器通信。
// 此包实现客户端侧，支持通过 stdio 和 Streamable HTTP 传输发现和调用远程工具。
//
// 最常用的入口是 Connect：按 ServerConfig 建立连接、握手、发现工具并转换为
// 框架工具（见 dial.go）。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ProtocolVersion 客户端发起握手时声明的协议版本（服务端可协商为自己支持的版本）。
const ProtocolVersion = "2025-03-26"

// Client 是 MCP 协议客户端。
type Client struct {
	mu              sync.RWMutex
	transport       Transport
	name            string // 配置名（工具名前缀；空则回落服务端自报名）
	tools           []ToolInfo
	connected       bool
	serverInfo      *ServerInfo
	protocolVersion string
}

// ServerInfo 包含 MCP 服务器信息。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientOption 客户端选项。
type ClientOption func(*Client)

// WithName 设置服务器配置名：工具注册名的前缀取它，而不是服务端自报的
// serverInfo.name（后者不可控，可能重复或含非法字符）。
func WithName(name string) ClientOption {
	return func(c *Client) { c.name = name }
}

// NewClient 创建一个新的 MCP 客户端。
func NewClient(transport Transport, opts ...ClientOption) *Client {
	c := &Client{transport: transport}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect 连接到 MCP 服务器并完成初始化握手（重复调用无副作用）。
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	resp, err := c.transport.Send(ctx, &Request{
		Method: "initialize",
		Params: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "goagent",
				"version": "0.1.0",
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
			break
		}
		cursor = page.NextCursor
	}

	c.mu.Lock()
	c.tools = all
	c.mu.Unlock()
	return all, nil
}

// CallTool 调用指定的 MCP 工具。
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolCallResult, error) {
	if !c.IsConnected() {
		return nil, fmt.Errorf("MCP 客户端未连接")
	}
	if arguments == nil {
		arguments = map[string]any{}
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

// Disconnect 断开与 MCP 服务器的连接（关闭传输；stdio 服务器进程随之终止）。
func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	return c.transport.Close()
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
