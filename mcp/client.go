// Package mcp 实现 Model Context Protocol (MCP) 客户端。
//
// MCP 是一种标准化协议，允许 LLM 应用与外部工具服务器通信。
// 此包实现客户端侧，支持通过 stdio 和 HTTP 传输发现和调用远程工具。
//
// 对齐 Claude Code 的 MCP 客户端架构。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Client 是 MCP 协议客户端。
type Client struct {
	mu         sync.RWMutex
	transport  Transport
	tools      []ToolInfo
	connected  bool
	serverInfo *ServerInfo
}

// ServerInfo 包含 MCP 服务器信息。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// NewClient 创建一个新的 MCP 客户端。
func NewClient(transport Transport) *Client {
	return &Client{
		transport: transport,
	}
}

// Connect 连接到 MCP 服务器并完成初始化握手。
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	// 发送 initialize 请求。
	resp, err := c.transport.Send(ctx, &Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
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

	// 解析服务器信息。
	if resp.Result != nil {
		var initResult struct {
			ServerInfo ServerInfo `json:"serverInfo"`
		}
		if err := json.Unmarshal(resp.Result, &initResult); err == nil {
			c.serverInfo = &initResult.ServerInfo
		}
	}

	// 发送 initialized 通知。
	_, _ = c.transport.Send(ctx, &Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})

	c.connected = true
	return nil
}

// ListTools 获取服务器提供的所有工具列表。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("MCP 客户端未连接")
	}
	c.mu.RUnlock()

	resp, err := c.transport.Send(ctx, &Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	})
	if err != nil {
		return nil, fmt.Errorf("获取工具列表失败: %w", err)
	}

	var result struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("解析工具列表失败: %w", err)
	}

	c.mu.Lock()
	c.tools = result.Tools
	c.mu.Unlock()

	return result.Tools, nil
}

// CallTool 调用指定的 MCP 工具。
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolCallResult, error) {
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("MCP 客户端未连接")
	}
	c.mu.RUnlock()

	resp, err := c.transport.Send(ctx, &Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("调用工具 %q 失败: %w", name, err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("工具 %q 返回错误: %s (code: %d)", name, resp.Error.Message, resp.Error.Code)
	}

	var result ToolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("解析工具结果失败: %w", err)
	}

	return &result, nil
}

// Disconnect 断开与 MCP 服务器的连接。
func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil
	}

	err := c.transport.Close()
	c.connected = false
	return err
}

// IsConnected 返回客户端是否已连接。
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// ServerName 返回连接的服务器名称。
func (c *Client) ServerName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.serverInfo != nil {
		return c.serverInfo.Name
	}
	return ""
}
