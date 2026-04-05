package goagent

import (
	"encoding/json"
	"strings"
)

// Middleware intercepts tool calls before and after execution.
// Use middleware for logging, rate limiting, auditing, cost control, etc.
//
// Example (global middleware):
//
//	type LogMiddleware struct{}
//	func (m *LogMiddleware) BeforeTool(ctx goagent.Context, name string, input json.RawMessage) *goagent.Decision {
//	    log.Printf("calling tool %s", name)
//	    return nil // continue
//	}
//	func (m *LogMiddleware) AfterTool(ctx goagent.Context, name string, result *goagent.Result, err error) {
//	    log.Printf("tool %s done, err=%v", name, err)
//	}
//
//	app.Use(&LogMiddleware{})
//
// Example (grouped middleware):
//
//	// 方式1: WithTools 指定中间件作用于特定工具
//	app.Use(&RateLimitMiddleware{}, WithTools("Bash", "Write", "Edit"))
//
//	// 方式2: Group 分组注册，类似 Gin
//	fileGroup := app.Group("Read", "Write", "Edit", "Glob", "Grep")
//	fileGroup.Use(&RateLimitMiddleware{}, &AuditMiddleware{})
//
//	bashGroup := app.Group("Bash")
//	bashGroup.Use(&BashTimeoutMiddleware{})
type Middleware interface {
	// BeforeTool is called before a tool executes.
	// Return nil to allow the tool to run.
	// Return a Decision to override the permission system's decision.
	BeforeTool(ctx Context, toolName string, input json.RawMessage) *Decision

	// AfterTool is called after a tool executes (whether it succeeded or failed).
	AfterTool(ctx Context, toolName string, result *Result, err error)
}

// Decision is returned by middleware to override tool execution.
type Decision struct {
	// Allow: true to force-allow, false to force-deny.
	Allow bool

	// Reason is a human-readable explanation shown to the user.
	Reason string
}

// DenyWith creates a deny Decision with the given reason.
func DenyWith(reason string) *Decision {
	return &Decision{Allow: false, Reason: reason}
}

// AllowWith creates an allow Decision with the given reason.
func AllowWith(reason string) *Decision {
	return &Decision{Allow: true, Reason: reason}
}

// MiddlewareFunc is an adapter to use ordinary functions as Middleware.
// Only handles BeforeTool; AfterTool is a no-op.
type MiddlewareFunc func(ctx Context, toolName string, input json.RawMessage) *Decision

func (f MiddlewareFunc) BeforeTool(ctx Context, toolName string, input json.RawMessage) *Decision {
	return f(ctx, toolName, input)
}

func (f MiddlewareFunc) AfterTool(Context, string, *Result, error) {}

// toolFilter determines which tools a middleware applies to.
type toolFilter struct {
	tools map[string]bool
}

// matches checks if the given tool name matches this filter.
// An empty filter matches all tools.
func (f *toolFilter) matches(toolName string) bool {
	if f == nil || len(f.tools) == 0 {
		return true // no filter = match all
	}
	return f.tools[toolName]
}

// WithTools returns an option that restricts the preceding middleware
// to only apply to the specified tools.
//
//	app.Use(&RateLimitMiddleware{}, WithTools("Bash", "Write", "Edit"))
//
// If multiple WithTools are passed, the middleware applies to the union of all tools.
func WithTools(toolNames ...string) MiddlewareOption {
	f := &toolFilter{
		tools: make(map[string]bool, len(toolNames)),
	}
	for _, name := range toolNames {
		f.tools[name] = true
	}
	return MiddlewareOptionFunc(func(cfg *middlewareConfig) {
		cfg.filter = f
	})
}

// MiddlewareOption configures a middleware registration.
type MiddlewareOption interface {
	apply(cfg *middlewareConfig)
}

type middlewareConfig struct {
	middleware Middleware
	filter     *toolFilter
}

type MiddlewareOptionFunc func(cfg *middlewareConfig)

func (f MiddlewareOptionFunc) apply(cfg *middlewareConfig) {
	f(cfg)
}

// ToolGroup represents a group of tools that share middleware.
// Similar to Gin router groups.
//
// Example:
//
//	fileGroup := app.Group("Read", "Write", "Edit", "Glob", "Grep")
//	fileGroup.Use(&RateLimitMiddleware{}, &AuditMiddleware{})
//
//	bashGroup := app.Group("Bash")
//	bashGroup.Use(&BashTimeoutMiddleware{})
type ToolGroup struct {
	app         *App
	toolNames   []string
	middlewares []Middleware
}

// Group creates a new tool group with the given tool names.
// The group can then have middleware attached with Use().
//
//	bashGroup := app.Group("Bash")
//	bashGroup.Use(&TimeoutMiddleware{})
func (a *App) Group(toolNames ...string) *ToolGroup {
	return &ToolGroup{
		app:       a,
		toolNames: toolNames,
	}
}

// Use adds middleware to this group.
// Middleware are executed in the order they are added.
//
//	g := app.Group("Bash")
//	g.Use(&TimeoutMiddleware{}, &RateLimitMiddleware{})
func (g *ToolGroup) Use(middlewares ...Middleware) {
	g.middlewares = append(g.middlewares, middlewares...)
}

// MiddlewareWithFilter wraps a middleware with a tool filter.
type MiddlewareWithFilter struct {
	Middleware Middleware
	Filter     *toolFilter
}

// BeforeTool delegates to the inner Middleware's BeforeTool.
func (m MiddlewareWithFilter) BeforeTool(ctx Context, toolName string, input json.RawMessage) *Decision {
	return m.Middleware.BeforeTool(ctx, toolName, input)
}

// AfterTool delegates to the inner Middleware's AfterTool.
func (m MiddlewareWithFilter) AfterTool(ctx Context, toolName string, result *Result, err error) {
	m.Middleware.AfterTool(ctx, toolName, result, err)
}

// buildMiddlewareList builds the final middleware list for the app.
// Global middlewares (via Use()) apply to all tools.
// Group middlewares apply only to their designated tools.
func (a *App) buildMiddlewareList() []MiddlewareWithFilter {
	var result []MiddlewareWithFilter

	// Add global middlewares (no filter = match all)
	for _, mw := range a.middlewares {
		result = append(result, MiddlewareWithFilter{
			Middleware: mw,
			Filter:     nil, // nil matches all
		})
	}

	// Add grouped middlewares
	for _, g := range a.groups {
		f := &toolFilter{
			tools: make(map[string]bool, len(g.toolNames)),
		}
		for _, name := range g.toolNames {
			f.tools[name] = true
		}
		for _, mw := range g.middlewares {
			result = append(result, MiddlewareWithFilter{
				Middleware: mw,
				Filter:     f,
			})
		}
	}

	return result
}

// matches returns true if the toolName matches this middleware's filter.
// A nil filter means "match all tools".
func (mwf *MiddlewareWithFilter) matches(toolName string) bool {
	if mwf.Filter == nil {
		return true
	}
	return mwf.Filter.matches(toolName)
}

// matches returns true if any middleware in the list matches the given tool.
func MiddlewareList(mwls []MiddlewareWithFilter, toolName string) bool {
	for _, mwl := range mwls {
		if mwl.matches(toolName) {
			return true
		}
	}
	return false
}

// ToolGroupFilter returns a filter that matches any tool in the group.
func ToolGroupFilter(toolNames ...string) *toolFilter {
	f := &toolFilter{
		tools: make(map[string]bool, len(toolNames)),
	}
	for _, name := range toolNames {
		f.tools[name] = true
	}
	return f
}

// StringSet is a simple string set for tool name matching.
type StringSet map[string]bool

// NewStringSet creates a new string set from a list of strings.
func NewStringSet(toolNames ...string) StringSet {
	s := make(StringSet)
	for _, name := range toolNames {
		s[name] = true
	}
	return s
}

// Contains returns true if the set contains the given tool name.
func (s StringSet) Contains(toolName string) bool {
	return s[toolName]
}

// ContainsPrefix returns true if any pattern in the set matches the tool name prefix.
// Pattern format: "Bash*" matches "Bash", "BashCommand", etc.
func (s StringSet) ContainsPrefix(toolName string) bool {
	for pattern := range s {
		if strings.HasPrefix(toolName, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}
