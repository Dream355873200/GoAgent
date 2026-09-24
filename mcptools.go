package goagent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/mcp"
)

// mcpConnectTimeout 单个 MCP 服务器建连 + 握手 + 工具发现的超时。
const mcpConnectTimeout = 30 * time.Second

// MCPToolDef 把 MCP 服务器发现的工具转成可注册的 ToolDef：入参 schema
// 取服务器声明的 inputSchema（不经结构体反射），调用时原样透传 JSON 参数。
// 权限为 Normal——远程工具的副作用框架无从判断，交给审批流程。
//
// 宿主自行管理 MCP 连接时的典型用法：
//
//	client, tools, err := mcp.Connect(ctx, mcp.ServerConfig{Name: "fs", Command: "npx", Args: ...})
//	for _, t := range tools {
//	    app.Tool(t.Name, goagent.MCPToolDef(t))
//	}
//	defer client.Disconnect()
func MCPToolDef(t mcp.FrameworkTool) ToolDef {
	exec := t.Execute
	return ToolDef{
		Description: t.Description,
		Schema:      t.InputSchema,
		Permission:  Normal,
		Execute: func(ctx Context, in json.RawMessage) (string, error) {
			return exec(ctx, in)
		},
	}
}

// mcpServerConfig 把 WithMCP 的配置映射为 mcp.ServerConfig。
// Transport 为空时按 Command/URL 推断；Auth 作为 Authorization 请求头。
func mcpServerConfig(srv agent.MCPServerConfig) mcp.ServerConfig {
	cfg := mcp.ServerConfig{Name: srv.Name}
	useHTTP := srv.Transport == "http" || (srv.Transport == "" && srv.Command == "" && srv.URL != "")
	if useHTTP {
		cfg.URL = srv.URL
		if len(srv.Headers) > 0 || srv.Auth != "" {
			cfg.Headers = make(map[string]string, len(srv.Headers)+1)
			for k, v := range srv.Headers {
				cfg.Headers[k] = v
			}
			if srv.Auth != "" {
				cfg.Headers["Authorization"] = srv.Auth
			}
		}
		return cfg
	}
	cfg.Command = srv.Command
	cfg.Args = srv.Args
	cfg.Env = srv.Env
	cfg.Dir = srv.Dir
	return cfg
}

// initMCP 连接 WithMCP 配置的服务器并注册其工具。
// 单个服务器失败只记告警、不影响其他服务器与 App 构造；与已注册工具
// 重名的 MCP 工具跳过（不覆盖宿主工具）。
func (a *App) initMCP() {
	for _, srv := range a.config.mcpServers {
		cfg := mcpServerConfig(srv)
		ctx, cancel := context.WithTimeout(context.Background(), mcpConnectTimeout)
		client, tools, err := mcp.Connect(ctx, cfg)
		cancel()
		if err != nil {
			a.logger.Warn("goagent: MCP 服务器连接失败", "server", srv.Name, "error", err)
			continue
		}

		a.mu.Lock()
		a.mcpClients = append(a.mcpClients, client)
		for _, t := range tools {
			if _, exists := a.tools[t.Name]; exists {
				a.logger.Warn("goagent: MCP 工具与已注册工具重名，跳过", "server", srv.Name, "tool", t.Name)
				continue
			}
			a.registerToolLocked(t.Name, MCPToolDef(t))
		}
		a.mu.Unlock()
	}
}

// CloseMCP 断开 WithMCP 建立的全部 MCP 连接（stdio 服务器进程随之终止）。
// 已注册的 MCP 工具保留在注册表中，之后调用会返回「未连接」错误。
func (a *App) CloseMCP() {
	a.mu.Lock()
	clients := a.mcpClients
	a.mcpClients = nil
	a.mu.Unlock()
	for _, c := range clients {
		_ = c.Disconnect()
	}
}
