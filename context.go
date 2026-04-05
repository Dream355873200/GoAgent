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

// newContextFromStd wraps a standard context.Context into a goagent.Context.
// Used internally when we only have a plain context (e.g., from middleware adapter).
func newContextFromStd(ctx context.Context) Context {
	return Context{
		Context:  ctx,
		Logger:   slog.Default(),
		Progress: func(string) {},
		Store:    func(string, any) {},
		Load:     func(string) (any, bool) { return nil, false },
	}
}
