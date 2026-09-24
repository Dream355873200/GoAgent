// Package mcp — MCP 工具转换。
//
// 将 MCP 服务器的工具定义转换为框架可用的工具格式。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// FrameworkTool 是转换后可直接注册到框架的工具。
type FrameworkTool struct {
	Name        string // 框架内注册名（见 ToolName）
	RemoteName  string // 服务器上的原始工具名
	Description string
	InputSchema map[string]any
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
	ServerName  string // 来源 MCP 服务器名称
}

// ToolNamePrefix MCP 工具注册名前缀（mcp__<server>__<tool>）。
const ToolNamePrefix = "mcp__"

// ToolName MCP 工具在框架内的注册名：mcp__<server>__<tool>。
// 非 [A-Za-z0-9_-] 字符替换为 _，总长截断到 64（OpenAI 兼容端点对函数名
// 的约束）。宿主可按 ServerPrefix 前缀判断工具来源。
func ToolName(server, tool string) string {
	name := ServerPrefix(server) + sanitize(tool)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ServerPrefix 某服务器全部工具注册名的公共前缀：mcp__<server>__。
func ServerPrefix(server string) string {
	return ToolNamePrefix + sanitize(server) + "__"
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

// ToFrameworkTools 将 MCP 工具列表转换为框架工具列表。
// 每个工具的 Execute 函数会通过 MCP 客户端远程调用。
func ToFrameworkTools(client *Client, tools []ToolInfo) []FrameworkTool {
	server := client.ServerName()
	result := make([]FrameworkTool, len(tools))
	for i, tool := range tools {
		remote := tool.Name // 闭包捕获
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result[i] = FrameworkTool{
			Name:        ToolName(server, remote),
			RemoteName:  remote,
			Description: tool.Description,
			InputSchema: schema,
			ServerName:  server,
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				var args map[string]any
				if len(input) > 0 && string(input) != "null" {
					if err := json.Unmarshal(input, &args); err != nil {
						return "", fmt.Errorf("解析 MCP 工具输入失败: %w", err)
					}
				}

				result, err := client.CallTool(ctx, remote, args)
				if err != nil {
					return "", err
				}

				text := result.ExtractText()
				if result.IsError {
					if text == "" {
						text = "MCP 工具返回错误"
					}
					return "", errors.New(text)
				}
				return text, nil
			},
		}
	}
	return result
}

// DiscoverAndConvert 连接到 MCP 服务器（已连接则跳过握手），发现工具，并转换为框架工具。
func DiscoverAndConvert(ctx context.Context, client *Client) ([]FrameworkTool, error) {
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("连接 MCP 服务器失败: %w", err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取 MCP 工具列表失败: %w", err)
	}

	return ToFrameworkTools(client, tools), nil
}
