package goagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/internal/safename"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServerOptions 配置 App.MCPServer：把哪些已注册工具、以及是否把 agent 本身
// 作为 MCP 工具暴露给 MCP 客户端（Claude Desktop、Cursor、其他 agent 框架）。
type MCPServerOptions struct {
	// Name 与 Version 是服务器自报的实现信息（默认 "goagent" 与 "0.14"）。
	Name, Version string
	// Instructions 是 initialize 结果里给客户端的服务器说明。
	Instructions string
	// Tools 是要暴露的已注册工具名，只暴露列出的。客户端直接调用这些工具：
	// 不经模型，也不经审批者——MCP 客户端负责向它的用户确认（工具带
	// readOnlyHint / destructiveHint 注解）；deny 规则、Guard 与权限模式
	// （规划模式、不询问模式、auto 模式的分类器）照常生效。
	Tools []string
	// Agent 非空时把 App 的 agent 暴露为一个 MCP 工具。
	Agent *MCPAgentTool
}

// MCPAgentTool 描述作为 MCP 工具暴露的 agent：客户端交给它一个任务，它像
// RunSession 一样跑完（可以调用工具、子代理），返回最终回复。运行中的
// 审批（App 的审批者是 PermissionHandler 时）与提问经 MCP elicitation 交给
// 客户端的用户；客户端不支持 elicitation 时审批被拒绝、提问得到"无法回答"。
// 客户端取消调用即中断运行；客户端带进度令牌时，工具调用与子代理进展以
// 进度通知下发。
type MCPAgentTool struct {
	// Name 是工具名（默认 "run_agent"）。
	Name string
	// Description 是工具描述（默认说明它会把任务交给一个能使用工具的代理）。
	Description string
	// KeepSessions 为 true 时（需要 WithSessionManager）工具带 session_id 参数：
	// 省略时新建会话，结果里返回 session_id；带上时延续该会话的对话。
	// 为 false 时每次调用是一次独立运行，不保留历史。
	KeepSessions bool
}

const mcpServerVersion = "0.14"

// MCPServer 按 opts 构造一个 MCP 服务器（官方 Go SDK 的 *mcp.Server），宿主可以
// 用任意 SDK 传输提供服务，或继续向它添加提示词、资源。常用的两种见
// MCPHandler（Streamable HTTP）与 ServeMCPStdio（stdio）。
func (a *App) MCPServer(opts MCPServerOptions) (*sdk.Server, error) {
	if opts.Name == "" {
		opts.Name = "goagent"
	}
	if opts.Version == "" {
		opts.Version = mcpServerVersion
	}
	type exposed struct {
		tool *sdk.Tool
		h    sdk.ToolHandler
	}
	var tools []exposed
	seen := map[string]bool{}
	a.mu.RLock()
	for _, name := range opts.Tools {
		rt := a.tools[name]
		if rt == nil {
			a.mu.RUnlock()
			return nil, fmt.Errorf("goagent: MCPServer: 工具 %q 未注册", name)
		}
		if seen[name] {
			a.mu.RUnlock()
			return nil, fmt.Errorf("goagent: MCPServer: 工具 %q 重复", name)
		}
		seen[name] = true
		in, err := mcpObjectSchema(rt.inputSchema)
		if err != nil {
			a.mu.RUnlock()
			return nil, fmt.Errorf("goagent: MCPServer: 工具 %q: %w", name, err)
		}
		tools = append(tools, exposed{
			tool: &sdk.Tool{Name: name, Description: rt.def.Description, InputSchema: in, Annotations: mcpAnnotations(rt.def.Permission)},
			h:    a.mcpToolHandler(name),
		})
	}
	hasProvider := a.provider != nil
	hasSessions := a.config.sessionManager != nil
	approver := a.config.approver
	a.mu.RUnlock()

	if ag := opts.Agent; ag != nil {
		if !hasProvider {
			return nil, errors.New("goagent: MCPServer: 暴露 agent 需要 provider（WithProvider）")
		}
		if ag.KeepSessions && !hasSessions {
			return nil, errors.New("goagent: MCPServer: KeepSessions 需要 WithSessionManager")
		}
		name := ag.Name
		if name == "" {
			name = "run_agent"
		}
		if seen[name] {
			return nil, fmt.Errorf("goagent: MCPServer: agent 工具名 %q 与暴露的工具重名", name)
		}
		desc := ag.Description
		if desc == "" {
			desc = "把任务交给一个能使用工具的 AI 代理：它会自行规划、调用工具并返回最终结果。task 写清楚要做什么、有哪些约束。"
		}
		props := map[string]any{"task": map[string]any{"type": "string", "description": "要完成的任务"}}
		if ag.KeepSessions {
			props["session_id"] = map[string]any{"type": "string", "description": "延续之前的会话（省略则新建，结果里返回它）"}
		}
		tools = append(tools, exposed{
			tool: &sdk.Tool{
				Name:        name,
				Description: desc,
				InputSchema: map[string]any{"type": "object", "properties": props, "required": []any{"task"}},
				OutputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"text":       map[string]any{"type": "string"},
					"session_id": map[string]any{"type": "string"},
				}, "required": []any{"text"}},
			},
			h: a.mcpAgentHandler(*ag),
		})
		// 内置 AskUser 工具没有提问处理器时会读进程的 stdin——stdio 模式下那是
		// MCP 的协议流。
		a.mu.Lock()
		if a.askUserHandler == nil {
			a.mu.Unlock()
			a.SetAskUserHandler(NewAskUserHandler())
		} else {
			a.mu.Unlock()
		}
		if approver == nil {
			a.logger.Warn("goagent: MCP 服务器暴露了 agent 但未配置审批者，需要审批的工具调用将被拒绝；" +
				"交互审批请用 WithApprover(goagent.NewPermissionHandler())（审批经 elicitation 交给 MCP 客户端）")
		}
	}

	server := sdk.NewServer(&sdk.Implementation{Name: opts.Name, Version: opts.Version}, &sdk.ServerOptions{
		Instructions: opts.Instructions,
		// 直接调用工具时以 MCP 会话为 GoAgent 会话（键值存储等）：MCP 会话结束时释放。
		InitializedHandler: func(_ context.Context, req *sdk.InitializedRequest) {
			ss := req.Session
			go func() {
				_ = ss.Wait()
				a.CloseSession(mcpToolSession(ss))
			}()
		},
	})
	for _, t := range tools {
		server.AddTool(t.tool, t.h)
	}
	return server, nil
}

// MCPHandler 返回以 Streamable HTTP 提供 MCPServer(opts) 的 http.Handler（挂到
// 宿主自己的路由与鉴权之后）。
func (a *App) MCPHandler(opts MCPServerOptions) (http.Handler, error) {
	server, err := a.MCPServer(opts)
	if err != nil {
		return nil, err
	}
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil), nil
}

// ServeMCPStdio 在进程的 stdin/stdout 上提供 MCPServer(opts)，直到客户端断开或
// ctx 取消（MCP 客户端以子进程方式启动宿主时使用）。stdout 是协议流：日志要写到
// stderr（WithLogger）。
func (a *App) ServeMCPStdio(ctx context.Context, opts MCPServerOptions) error {
	server, err := a.MCPServer(opts)
	if err != nil {
		return err
	}
	return server.Run(ctx, &sdk.StdioTransport{})
}

// mcpObjectSchema 返回工具的入参 schema（MCP 要求顶层为 object）。
func mcpObjectSchema(s any) (map[string]any, error) {
	var m map[string]any
	switch v := s.(type) {
	case map[string]any:
		m = v
	case nil:
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	default:
		b, err := json.Marshal(v)
		if err != nil || json.Unmarshal(b, &m) != nil {
			return nil, errors.New("入参 schema 无法序列化为 JSON 对象")
		}
	}
	if m["type"] != "object" {
		return nil, fmt.Errorf("MCP 要求入参 schema 的顶层 type 为 object（实际 %v）", m["type"])
	}
	return m, nil
}

// mcpAnnotations 按权限级别给出 MCP 工具注解（客户端据此决定是否向用户确认）。
func mcpAnnotations(p Permission) *sdk.ToolAnnotations {
	destructive := p >= Dangerous
	return &sdk.ToolAnnotations{ReadOnlyHint: p == ReadOnly, DestructiveHint: &destructive}
}

// mcpToolSession 是直接调用工具时使用的 GoAgent 会话 ID（每个 MCP 会话一个）。
func mcpToolSession(ss *sdk.ServerSession) string {
	if ss == nil || ss.ID() == "" {
		return "mcp-stdio"
	}
	return "mcp-" + ss.ID()
}

func mcpError(format string, args ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

// mcpContent 把工具结果转为 MCP 内容：开头的内联图片标记转为图片。
func mcpContent(out string) []sdk.Content {
	if img, ok := message.SplitImageMarker(out); ok && img.Valid {
		data, _ := base64.StdEncoding.DecodeString(img.Data)
		content := []sdk.Content{&sdk.ImageContent{MIMEType: img.MIME(), Data: data}}
		if img.Body != "" {
			content = append(content, &sdk.TextContent{Text: img.Body})
		}
		return content
	}
	return []sdk.Content{&sdk.TextContent{Text: out}}
}

// mcpToolHandler 让 MCP 客户端直接调用已注册工具 name。
func (a *App) mcpToolHandler(name string) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		a.mu.RLock()
		rt := a.tools[name]
		cfg := a.config
		a.mu.RUnlock()
		if rt == nil {
			return mcpError("工具 %s 已注销", name), nil
		}
		input := json.RawMessage(req.Params.Arguments)
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage(`{}`)
		}
		if s, ok := rt.inputSchema.(map[string]any); ok {
			if err := schema.Validate(s, input); err != nil {
				return mcpError("参数不符合工具 %s 的入参 schema：%v", name, err), nil
			}
		}

		sessionID := mcpToolSession(req.Session)
		srt := a.sessionRuntime(sessionID)
		gate, release := a.acquireGate(srt)
		verdict := gate.CheckResult(name, string(input), permission.Level(rt.def.Permission))
		release()
		if verdict.Behavior == permission.BehaviorDeny {
			return mcpError("权限被拒绝: %s", verdict.Reason), nil
		}

		workDir := ""
		if cfg.sessionWorkDirFn != nil {
			workDir = cfg.sessionWorkDirFn(sessionID)
		}
		if cfg.sandbox != nil {
			sb, err := cfg.sandbox.Enter(ctx, sessionID, cfg.sandboxPolicy)
			if err != nil {
				return mcpError("沙箱初始化失败: %v", err), nil
			}
			defer func() { _ = sb.Close() }()
			ctx = WithSandboxSession(ctx, sb)
			if r := sb.Root(); r != "" {
				workDir = r
			}
		}
		ctx = withSessionRuntime(WithSessionContext(ctx, sessionID, workDir), srt)
		report := mcpProgressReporter(ctx, req)
		ctx = loop.WithEmitFunc(ctx, func(ev loop.Event) {
			if ev.Type == loop.EvtProgress && ev.Text != "" {
				report(ev.Text)
			}
		})
		out, err := rt.def.call(ctx, input)
		if err != nil {
			return mcpError("%v", err), nil
		}
		return &sdk.CallToolResult{Content: mcpContent(out)}, nil
	}
}

// mcpAgentHandler 把一次 MCP 调用转成一次 agent 运行。
func (a *App) mcpAgentHandler(opt MCPAgentTool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var in struct {
			Task      string `json:"task"`
			SessionID string `json:"session_id"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return mcpError("参数无效: %v", err), nil
			}
		}
		if strings.TrimSpace(in.Task) == "" {
			return mcpError("task 不能为空"), nil
		}
		clientID := ""
		if opt.KeepSessions {
			clientID = in.SessionID
			if clientID == "" {
				clientID = newRequestID("s")
			} else if err := safename.Check("session_id", clientID); err != nil {
				return mcpError("%v", err), nil
			}
		}
		sessionID := "mcp-agent-" + clientID
		if clientID == "" {
			sessionID = newRequestID("mcp-agent")
		}
		// 同一会话不能同时运行；先占住再订阅交互，免得后来的调用顶掉进行中
		// 那次运行的审批与提问路由。
		if _, busy := a.mcpBusy.LoadOrStore(sessionID, true); busy {
			return mcpError("会话 %s 正在运行，请等它结束", clientID), nil
		}
		defer a.mcpBusy.Delete(sessionID)

		stop := a.routeMCPInteractions(ctx, req.Session, sessionID)
		defer stop()

		var events <-chan Event
		if opt.KeepSessions {
			events = a.RunSession(ctx, sessionID, in.Task)
		} else {
			events = a.RunWithSessionHistory(ctx, sessionID, nil, in.Task)
			defer a.CloseSession(sessionID)
		}

		report := mcpProgressReporter(ctx, req)
		var final strings.Builder
		var runErr error
		interrupted := false
		for ev := range events {
			switch ev.Type {
			case EventTextDelta:
				if ev.AgentPath == "" {
					final.WriteString(ev.Text)
				}
			case EventToolStart:
				if ev.AgentPath == "" {
					final.Reset()
					report("调用 " + ev.ToolName)
				}
			case EventSubAgentProgress:
				if ev.AgentActivity != "" {
					report(ev.AgentDesc + "：" + ev.AgentActivity)
				}
			case EventError:
				runErr = ev.Error
			case EventInterrupted:
				interrupted = true
			}
		}
		switch {
		case runErr != nil:
			return mcpError("运行出错: %v", runErr), nil
		case interrupted:
			return mcpError("运行已中断"), nil
		}
		text := final.String()
		structured := map[string]any{"text": text}
		if opt.KeepSessions {
			structured["session_id"] = clientID
			text += "\n\n[session_id: " + clientID + "]"
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, StructuredContent: structured}, nil
	}
}

// mcpProgressReporter 返回下发进度通知的函数（客户端没带进度令牌时为空操作）。
func mcpProgressReporter(ctx context.Context, req *sdk.CallToolRequest) func(string) {
	token := req.Params.GetProgressToken()
	if token == nil || req.Session == nil {
		return func(string) {}
	}
	var n atomic.Int64
	return func(msg string) {
		_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
			ProgressToken: token, Progress: float64(n.Add(1)), Message: msg,
		})
	}
}

// routeMCPInteractions 把会话 sessionID 运行中的审批（审批者是 PermissionHandler
// 时）与提问经 elicitation 交给 MCP 客户端，返回停止路由的函数。
func (a *App) routeMCPInteractions(ctx context.Context, ss *sdk.ServerSession, sessionID string) func() {
	canElicit := false
	if ss != nil {
		if p := ss.InitializeParams(); p != nil && p.Capabilities != nil && p.Capabilities.Elicitation != nil {
			canElicit = true
		}
	}
	a.mu.RLock()
	approver := a.config.approver
	asks := a.askUserHandler
	a.mu.RUnlock()

	var stops []func()
	if ph, ok := approver.(*PermissionHandler); ok {
		reqs, unsubscribe := ph.Subscribe(sessionID)
		stops = append(stops, unsubscribe)
		go func() {
			for r := range reqs {
				go mcpResolveApproval(ctx, ss, canElicit, r)
			}
		}()
	}
	if asks != nil {
		reqs, unsubscribe := asks.Subscribe(sessionID)
		stops = append(stops, unsubscribe)
		go func() {
			for r := range reqs {
				go mcpResolveQuestion(ctx, ss, canElicit, r)
			}
		}()
	}
	return func() {
		for _, s := range stops {
			s()
		}
	}
}

func mcpResolveApproval(ctx context.Context, ss *sdk.ServerSession, canElicit bool, r *PermissionRequest) {
	if !canElicit {
		r.Deny("MCP 客户端不支持确认（elicitation），无法批准该操作")
		return
	}
	input := string(r.ToolInput)
	if len(input) > 2000 {
		input = input[:2000] + "…"
	}
	who := "代理"
	if r.AgentPath != "" {
		who = "子代理 " + r.AgentPath
	}
	res, err := ss.Elicit(ctx, &sdk.ElicitParams{
		Message: fmt.Sprintf("%s请求执行工具 %s（权限：%s），参数：\n%s", who, r.ToolName, r.Permission, input),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approve": map[string]any{"type": "boolean", "title": "批准执行"},
				"reason":  map[string]any{"type": "string", "title": "不批准的原因（可选）"},
			},
			"required": []any{"approve"},
		},
	})
	switch {
	case err != nil:
		r.Deny("确认失败: " + err.Error())
	case res.Action != "accept":
		r.Deny("用户拒绝")
	case res.Content["approve"] == true:
		r.Approve(false)
	default:
		reason, _ := res.Content["reason"].(string)
		if reason == "" {
			reason = "用户拒绝"
		}
		r.Deny(reason)
	}
}

func mcpResolveQuestion(ctx context.Context, ss *sdk.ServerSession, canElicit bool, r *AskUserRequest) {
	if !canElicit {
		r.Respond("（MCP 客户端不支持提问，用户无法回答；请在不询问用户的情况下继续）")
		return
	}
	res, err := ss.Elicit(ctx, &sdk.ElicitParams{
		Message: r.Question,
		RequestedSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string", "title": "回答"}},
			"required":   []any{"answer"},
		},
	})
	switch {
	case err != nil:
		r.Respond("（提问失败：" + err.Error() + "）")
	case res.Action == "decline":
		r.Respond("（用户拒绝回答）")
	case res.Action != "accept":
		r.Respond("（用户取消了提问）")
	default:
		answer, _ := res.Content["answer"].(string)
		r.Respond(answer)
	}
}
