// Package hooks 实现 Hook 框架。
//
// Hooks 是在 agent 循环的关键事件点触发的回调，允许用户
// 在不修改框架代码的情况下扩展行为。
//
// 支持的事件：
//   - PreToolUse:       工具执行前
//   - PostToolUse:      工具执行后
//   - Stop:             循环退出前
//   - PermissionRequest: 权限请求时
//   - SessionStart:     会话开始时
//
// 对齐 Claude Code 的 hooks 系统。
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// HookEvent 标识 hook 事件类型。
type HookEvent int

const (
	// EventPreToolUse 工具执行前触发。
	EventPreToolUse HookEvent = iota
	// EventPostToolUse 工具执行后触发。
	EventPostToolUse
	// EventStop 循环退出前触发。
	EventStop
	// EventPermissionRequest 权限请求时触发。
	EventPermissionRequest
	// EventSessionStart 会话开始时触发。
	EventSessionStart
)

// String 返回事件名称。
func (e HookEvent) String() string {
	switch e {
	case EventPreToolUse:
		return "pre_tool_use"
	case EventPostToolUse:
		return "post_tool_use"
	case EventStop:
		return "stop"
	case EventPermissionRequest:
		return "permission_request"
	case EventSessionStart:
		return "session_start"
	default:
		return "unknown"
	}
}

// HookContext 提供给 hook 的上下文信息。
type HookContext struct {
	// Event 是触发的事件类型。
	Event HookEvent

	// ToolName 是工具名称（仅 PreToolUse/PostToolUse 有效）。
	ToolName string

	// ToolInput 是工具输入（仅 PreToolUse/PostToolUse 有效）。
	ToolInput json.RawMessage

	// ToolResult 是工具结果（仅 PostToolUse 有效）。
	ToolResult string

	// ToolError 是工具执行错误（仅 PostToolUse 有效）。
	ToolError error

	// SessionID 是当前会话 ID。
	SessionID string

	// Custom 是自定义键值对，可在 hook 间传递数据。
	Custom map[string]any
}

// HookResult 是 hook 的执行结果。
type HookResult struct {
	// Block 如果为 true，阻止后续操作（如阻止工具执行）。
	Block bool

	// Message 是 hook 返回的消息（如阻止原因）。
	Message string

	// ModifiedInput 是修改后的工具输入（仅 PreToolUse 有效）。
	// 如果为 nil，使用原始输入。
	ModifiedInput json.RawMessage
}

// Hook 是 hook 回调接口。
type Hook interface {
	// Name 返回 hook 的名称，用于日志和调试。
	Name() string

	// Events 返回此 hook 关注的事件列表。
	Events() []HookEvent

	// Execute 执行 hook 并返回结果。
	Execute(ctx context.Context, hctx *HookContext) (*HookResult, error)
}

// Manager 管理所有注册的 hooks。
type Manager struct {
	mu    sync.RWMutex
	hooks map[HookEvent][]Hook
}

// NewManager 创建一个新的 hook 管理器。
func NewManager() *Manager {
	return &Manager{
		hooks: make(map[HookEvent][]Hook),
	}
}

// Register 注册一个 hook。hook 会自动根据其 Events() 关联到对应事件。
func (m *Manager) Register(hook Hook) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, event := range hook.Events() {
		m.hooks[event] = append(m.hooks[event], hook)
	}
}

// RunPreToolUse 执行所有 PreToolUse hooks。
// 如果任一 hook 返回 Block=true，停止执行并返回该结果。
func (m *Manager) RunPreToolUse(ctx context.Context, toolName string, input json.RawMessage, sessionID string) (*HookResult, error) {
	return m.runEvent(ctx, &HookContext{
		Event:     EventPreToolUse,
		ToolName:  toolName,
		ToolInput: input,
		SessionID: sessionID,
		Custom:    make(map[string]any),
	})
}

// RunPostToolUse 执行所有 PostToolUse hooks。
func (m *Manager) RunPostToolUse(ctx context.Context, toolName string, input json.RawMessage, result string, toolErr error, sessionID string) error {
	_, err := m.runEvent(ctx, &HookContext{
		Event:      EventPostToolUse,
		ToolName:   toolName,
		ToolInput:  input,
		ToolResult: result,
		ToolError:  toolErr,
		SessionID:  sessionID,
		Custom:     make(map[string]any),
	})
	return err
}

// RunStop 执行所有 Stop hooks。
// 如果任一 hook 返回 Block=true，表示循环不应退出。
func (m *Manager) RunStop(ctx context.Context, sessionID string) (*HookResult, error) {
	return m.runEvent(ctx, &HookContext{
		Event:     EventStop,
		SessionID: sessionID,
		Custom:    make(map[string]any),
	})
}

// RunSessionStart 执行所有 SessionStart hooks。
func (m *Manager) RunSessionStart(ctx context.Context, sessionID string) error {
	_, err := m.runEvent(ctx, &HookContext{
		Event:     EventSessionStart,
		SessionID: sessionID,
		Custom:    make(map[string]any),
	})
	return err
}

// runEvent 执行指定事件的所有 hooks。
func (m *Manager) runEvent(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	m.mu.RLock()
	hooks := m.hooks[hctx.Event]
	m.mu.RUnlock()

	for _, hook := range hooks {
		result, err := hook.Execute(ctx, hctx)
		if err != nil {
			return nil, fmt.Errorf("hook %q 执行失败: %w", hook.Name(), err)
		}
		if result != nil && result.Block {
			return result, nil
		}
	}
	return nil, nil
}

// HasHooks 返回指定事件是否注册了 hooks。
func (m *Manager) HasHooks(event HookEvent) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks[event]) > 0
}
