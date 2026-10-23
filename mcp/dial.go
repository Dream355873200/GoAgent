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

	// ProtocolVersion 握手时请求的协议版本（空 = DefaultProtocolVersion）。
	ProtocolVersion string

	// Elicit 处理服务器在工具调用中向用户索取的输入（elicitation/create，见
	// ElicitHandler）。为空时不向服务器声明 elicitation 能力。WithMCP 连接的
	// 服务器默认使用 goagent.ElicitViaAskUser（转成所在会话的一次提问）。
	Elicit ElicitHandler
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

// Dial 按配置建立连接并完成初始化握手（官方 Go SDK 承载：调用取消、进度
// 通知、工具列表变更、图片结果都可用）。失败时已启动的子进程会被清理。
func Dial(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return dialSDK(ctx, cfg)
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
