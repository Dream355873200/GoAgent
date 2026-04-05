// Package mcp — MCP 工具转换。
//
// 将 MCP 服务器的工具定义转换为框架可用的工具格式。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// FrameworkTool 是转换后可直接注册到框架的工具。
type FrameworkTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
	ServerName  string // 来源 MCP 服务器名称
}

// ToFrameworkTools 将 MCP 工具列表转换为框架工具列表。
// 每个工具的 Execute 函数会通过 MCP 客户端远程调用。
func ToFrameworkTools(client *Client, tools []ToolInfo) []FrameworkTool {
	result := make([]FrameworkTool, len(tools))
	for i, tool := range tools {
		toolName := tool.Name // 闭包捕获
		result[i] = FrameworkTool{
			Name:        fmt.Sprintf("mcp_%s_%s", client.ServerName(), tool.Name),
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			ServerName:  client.ServerName(),
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				// 将 JSON 输入解析为参数 map。
				var args map[string]any
				if len(input) > 0 {
					if err := json.Unmarshal(input, &args); err != nil {
						return "", fmt.Errorf("解析 MCP 工具输入失败: %w", err)
					}
				}

				// 通过 MCP 客户端调用远程工具。
				result, err := client.CallTool(ctx, toolName, args)
				if err != nil {
					return "", err
				}

				if result.IsError {
					return result.ExtractText(), fmt.Errorf("MCP 工具返回错误")
				}

				return result.ExtractText(), nil
			},
		}
	}
	return result
}

// DiscoverAndConvert 连接到 MCP 服务器，发现工具，并转换为框架工具。
// 这是最常用的便捷方法。
func DiscoverAndConvert(ctx context.Context, client *Client) ([]FrameworkTool, error) {
	// 连接。
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("连接 MCP 服务器失败: %w", err)
	}

	// 发现工具。
	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取 MCP 工具列表失败: %w", err)
	}

	// 转换。
	return ToFrameworkTools(client, tools), nil
}
