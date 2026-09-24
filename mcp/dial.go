// Package mcp — 按配置建立连接的便捷入口。
package mcp

import (
	"context"
	"errors"
	"fmt"
)

// ServerConfig 一个 MCP 服务器的连接配置。
// 本地服务器用 Command（stdio），远程服务器用 URL（Streamable HTTP），二选一。
type ServerConfig struct {
	Name string // 配置名（工具注册名前缀，见 ToolName）

	// stdio
	Command string
	Args    []string
	Env     map[string]string // 在继承的进程环境变量之上追加/覆盖
	Dir     string            // 子进程工作目录

	// Streamable HTTP
	URL     string
	Headers map[string]string // 附加请求头（如 Authorization）
}

// Validate 校验配置完整性（name 必填，command 与 url 二选一）。
func (c ServerConfig) Validate() error {
	if c.Name == "" {
		return errors.New("MCP 服务器缺少 name")
	}
	if (c.Command == "") == (c.URL == "") {
		return fmt.Errorf("MCP 服务器 %s: command 与 url 必须二选一", c.Name)
	}
	return nil
}

// Dial 按配置建立传输并完成初始化握手。失败时已启动的子进程会被清理。
func Dial(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var transport Transport
	if cfg.Command != "" {
		t, err := NewStdioTransportConfig(StdioConfig{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, Dir: cfg.Dir})
		if err != nil {
			return nil, err
		}
		transport = t
	} else {
		transport = NewHTTPTransportConfig(HTTPConfig{URL: cfg.URL, Headers: cfg.Headers})
	}
	client := NewClient(transport, WithName(cfg.Name))
	if err := client.Connect(ctx); err != nil {
		_ = client.Disconnect()
		return nil, err
	}
	return client, nil
}

// Connect 一步完成：建立连接、握手、发现工具并转换为框架工具。
// 发现失败时连接会被关闭（不留孤儿子进程）。
func Connect(ctx context.Context, cfg ServerConfig) (*Client, []FrameworkTool, error) {
	client, err := Dial(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	tools, err := DiscoverAndConvert(ctx, client)
	if err != nil {
		_ = client.Disconnect()
		return nil, nil, err
	}
	return client, tools, nil
}
