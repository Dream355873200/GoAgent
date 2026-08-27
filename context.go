package goagent

import (
	"context"
	"log/slog"
)

// Context is passed to tool Execute functions.
// It provides access to the current session, logging, and shared state.
type Context struct {
	context.Context

	// SessionID is the unique identifier for this agent session.
	SessionID string

	// WorkDir is the current working directory.
	WorkDir string

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
)

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

// newContextFromStd wraps a standard context.Context into a goagent.Context.
// Used internally when we only have a plain context (e.g., from middleware adapter).
// 会话值（SessionID/WorkDir）从 context value 透出——由 App.run 经
// WithSessionContext 注入，工具据此实现「按会话隔离」的工作目录等行为。
func newContextFromStd(ctx context.Context) Context {
	return Context{
		Context:   ctx,
		SessionID: SessionIDFromContext(ctx),
		WorkDir:   WorkDirFromContext(ctx),
		Logger:    slog.Default(),
		Progress:  func(string) {},
		Store:     func(string, any) {},
		Load:      func(string) (any, bool) { return nil, false },
	}
}
