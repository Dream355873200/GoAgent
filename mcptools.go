package goagent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/mcp"
)

// mcpConnectTimeout 单个 MCP 服务器建连 + 握手 + 工具发现的超时。
const mcpConnectTimeout = 30 * time.Second

// MCPToolDef 把 MCP 服务器发现的工具转成可注册的 ToolDef：入参 schema
// 取服务器声明的 inputSchema（不经结构体反射），调用时原样透传 JSON 参数。
// 权限为 Normal——远程工具的副作用框架无从判断，交给审批流程。服务端的进度
// 通知显示为工具状态行（Context.Progress），不进入模型上下文；服务端在调用中
// 发起的 elicitation 以会话为范围定位到这次调用（见 mcp.WithElicitScope、
// ElicitViaAskUser）。
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
			return exec(mcp.WithElicitScope(mcp.WithProgressReporter(ctx, ctx.Progress), ctx.SessionID), in)
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
// 重名的 MCP 工具跳过（不覆盖宿主工具）。服务器发起的 elicitation 转成
// 所在会话的提问（ElicitViaAskUser）。
func (a *App) initMCP() {
	for _, srv := range a.config.mcpServers {
		cfg := mcpServerConfig(srv)
		cfg.Elicit = ElicitViaAskUser
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

		// 服务端的工具列表变更（tools/list_changed）同步进注册表：新增的注册、
		// 已有的更新、消失的移除。只动带本服务器前缀的工具。
		c := client
		c.OnToolsChanged(func(infos []mcp.ToolInfo) { a.syncMCPTools(c, infos) })
	}
}

// syncMCPTools 把某个 MCP 服务器的最新工具列表同步进注册表（对之后开始的运行
// 生效）。与其他来源的工具重名时跳过。
func (a *App) syncMCPTools(client *mcp.Client, infos []mcp.ToolInfo) {
	prefix := mcp.ServerPrefix(client.ServerName())
	fresh := mcp.ToFrameworkTools(client, infos)
	keep := make(map[string]bool, len(fresh))
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range fresh {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}
		keep[t.Name] = true
		def := MCPToolDef(t)
		if _, exists := a.tools[t.Name]; exists {
			a.tools[t.Name] = &registeredTool{def: def, inputSchema: inputSchemaOf(def)}
			continue
		}
		a.registerToolLocked(t.Name, def)
	}
	for _, name := range append([]string(nil), a.toolOrder...) {
		if strings.HasPrefix(name, prefix) && !keep[name] {
			a.removeToolLocked(name)
		}
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
