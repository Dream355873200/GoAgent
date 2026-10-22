package goagent

import (
	"context"
	"log/slog"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/observer"
)

// Context is passed to tool Execute functions.
// It provides access to the current session, logging, and shared state.
type Context struct {
	context.Context

	// SessionID is the unique identifier for this agent session.
	SessionID string

	// WorkDir is the current working directory.
	WorkDir string

	// Sandbox 当前运行的沙箱会话（nil = 无沙箱，默认）。
	// 由 WithSandbox 启用、App.run / RunPipeline / 节点级覆盖注入。
	// 内置文件/Bash 工具据此做路径重写与策略检查；自定义工具可直接
	// 读取（如需要感知自己是否在沙箱内）。见 sandbox.go。
	Sandbox SandboxSession

	// Logger is a structured logger for the tool.
	Logger *slog.Logger

	// Progress reports an intermediate progress message to the UI.
	// The message is displayed but not sent to the LLM.
	Progress func(msg string)

	// Store saves a value into the session-scoped key-value store.
	// This allows tools to share data with each other within a session.
	Store func(key string, val any)

	// Load retrieves a value from the session-scoped key-value store.
	Load func(key string) (any, bool)
}

// sessCtxKey 是会话上下文（SessionID/WorkDir）的 context value 键类型。
// 独立于 pipeline 的 ctxKey，不导出以防碰撞。
type sessCtxKey int

const (
	ctxKeySessionID sessCtxKey = iota
	ctxKeyWorkDir
	ctxKeySandbox
	ctxKeyAskUser
	ctxKeySessionRuntime
	ctxKeyDepth
)

// withAskUserHandler 把 App 的提问处理器放进运行 ctx，内置 AskUser 工具
// 据此按会话路由提问。
func withAskUserHandler(ctx context.Context, h *AskUserHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyAskUser, h)
}

// AskUserHandlerFromContext 返回本次运行所属 App 的提问处理器（未配置时为 nil）。
func AskUserHandlerFromContext(ctx context.Context) *AskUserHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(ctxKeyAskUser).(*AskUserHandler)
	return h
}

// WithSessionContext 把会话标识与工作目录注入 context。
// App.run 在启动 agent 循环前自动调用（工作目录来自 WithSessionWorkDir
// 配置的解析函数）；业务层用 RunWithHistory 自管会话时也可手动注入。
// 之后经 newContextFromStd 透出到工具的 goagent.Context 字段。
func WithSessionContext(ctx context.Context, sessionID, workDir string) context.Context {
	ctx = context.WithValue(ctx, ctxKeySessionID, sessionID)
	return context.WithValue(ctx, ctxKeyWorkDir, workDir)
}

// SessionIDFromContext 从 context 提取会话标识（未注入返回空串）。
// nil context 安全。
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeySessionID).(string); ok {
		return v
	}
	return ""
}

// WorkDirFromContext 从 context 提取会话工作目录（未注入返回空串）。
// nil context 安全。
func WorkDirFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyWorkDir).(string); ok {
		return v
	}
	return ""
}

// WithSandboxSession 把沙箱会话注入 context。
// App.run（WithSandbox 配置时）、RunPipeline（run 级）与节点级覆盖
// 自动调用；之后经 newContextFromStd 透出到工具的 Context.Sandbox 字段。
//
// 覆盖契约（ctx 链上后注入覆盖先注入的，子覆盖父）：同一 ctx 链上多次
// 调用时，后注入的沙箱会话生效——context.WithValue 的标准语义。
// benchmark trial 嵌套 pipeline 时，节点级覆盖（runNode）注入在
// run 级（RunPipeline）之后派生的 ctx 上，因此节点沙箱覆盖 run 沙箱；
// 需要子沿用父沙箱的宿主不要在子层再调 WithSandboxSession。
func WithSandboxSession(ctx context.Context, sb SandboxSession) context.Context {
	return context.WithValue(ctx, ctxKeySandbox, sb)
}

// SandboxFromContext 从 context 提取沙箱会话（未注入返回 nil）。
// nil context 安全。
func SandboxFromContext(ctx context.Context) SandboxSession {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(ctxKeySandbox).(SandboxSession); ok {
		return v
	}
	return nil
}

// newContextFromStd wraps a standard context.Context into a goagent.Context.
// 会话值（SessionID/WorkDir/Sandbox）从 context value 透出——由 App.run 经
// WithSessionContext 注入。Logger 取运行所属 App 的日志器；Progress 把进度
// 作为状态行事件发给本次运行的事件流（不进入模型上下文）；Store/Load 读写
// 本会话的键值存储（同一会话的工具之间共享，跨会话隔离）。
func newContextFromStd(ctx context.Context) Context {
	c := Context{
		Context:   ctx,
		SessionID: SessionIDFromContext(ctx),
		WorkDir:   WorkDirFromContext(ctx),
		Sandbox:   SandboxFromContext(ctx),
		Logger:    slog.Default(),
		Progress:  func(string) {},
		Store:     func(string, any) {},
		Load:      func(string) (any, bool) { return nil, false },
	}
	if s := loop.ScopeFrom(ctx); s != nil && s.Logger != nil {
		c.Logger = s.Logger
	}
	statusKey := "tool-progress"
	if id := observer.ToolCallIDFromContext(ctx); id != "" {
		statusKey += ":" + id
	}
	c.Progress = func(msg string) {
		loop.EmitFromTool(ctx, loop.Event{Type: loop.EvtProgress, StatusKey: statusKey, Text: msg})
	}
	if rt := sessionRuntimeFrom(ctx); rt != nil {
		c.Store = func(key string, val any) { rt.kv.Store(key, val) }
		c.Load = func(key string) (any, bool) { return rt.kv.Load(key) }
	}
	return c
}
