// Package executor — 增强的工具追踪系统。
//
// TrackedTool 追踪每个工具调用的完整生命周期状态，
// 支持中断、中止兄弟工具和合成结果。
//
// 对齐 Claude Code 的 toolExecution.ts 中的工具状态管理。
package executor

import (
	"context"
	"fmt"
	"sync"
)

// ToolState 表示工具执行的生命周期状态。
type ToolState int

const (
	// ToolQueued 工具已排队，等待执行。
	ToolQueued ToolState = iota
	// ToolExecuting 工具正在执行中。
	ToolExecuting
	// ToolCompleted 工具已正常完成。
	ToolCompleted
	// ToolAborted 工具被中止（由兄弟中止或用户取消）。
	ToolAborted
	// ToolYielded 工具主动让出（中断行为为 "block" 时）。
	ToolYielded
)

// String 返回工具状态名称。
func (s ToolState) String() string {
	switch s {
	case ToolQueued:
		return "queued"
	case ToolExecuting:
		return "executing"
	case ToolCompleted:
		return "completed"
	case ToolAborted:
		return "aborted"
	case ToolYielded:
		return "yielded"
	default:
		return "unknown"
	}
}

// TrackedTool 追踪单个工具调用的完整状态。
type TrackedTool struct {
	// Call 是原始的工具调用信息。
	Call ToolCall

	// State 是当前的执行状态。
	State ToolState

	// Cancel 是取消此工具执行的函数。
	Cancel context.CancelFunc

	// InterruptMode 是此工具的中断模式："cancel" 或 "block"。
	InterruptMode string

	// Result 是工具的执行结果（仅在 Completed 或 Aborted 后有效）。
	Result *ToolResult

	mu sync.Mutex
}

// SetState 安全地设置工具状态。
func (t *TrackedTool) SetState(state ToolState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.State = state
}

// GetState 安全地获取工具状态。
func (t *TrackedTool) GetState() ToolState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.State
}

// SetResult 安全地设置工具结果。
func (t *TrackedTool) SetResult(result ToolResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Result = &result
}

// TrackedExecutor 是增强的执行器，追踪所有工具的完整生命周期。
type TrackedExecutor struct {
	executor            *Executor
	mu                  sync.RWMutex
	tracked             map[string]*TrackedTool // toolUseID -> TrackedTool
	siblingAbortEnabled bool
}

// NewTrackedExecutor 创建一个新的追踪执行器。
func NewTrackedExecutor(exec *Executor) *TrackedExecutor {
	return &TrackedExecutor{
		executor:            exec,
		tracked:             make(map[string]*TrackedTool),
		siblingAbortEnabled: true,
	}
}

// SetSiblingAbort 启用或禁用兄弟中止功能。
func (te *TrackedExecutor) SetSiblingAbort(enabled bool) {
	te.mu.Lock()
	defer te.mu.Unlock()
	te.siblingAbortEnabled = enabled
}

// Track 注册一个工具调用进行追踪。
func (te *TrackedExecutor) Track(call ToolCall, interruptMode string) *TrackedTool {
	if interruptMode == "" {
		interruptMode = "cancel"
	}

	tt := &TrackedTool{
		Call:          call,
		State:         ToolQueued,
		InterruptMode: interruptMode,
	}

	te.mu.Lock()
	te.tracked[call.ID] = tt
	te.mu.Unlock()

	return tt
}

// ExecuteTracked 执行一个被追踪的工具调用。
func (te *TrackedExecutor) ExecuteTracked(ctx context.Context, tt *TrackedTool) ToolResult {
	// 创建可取消的子上下文。
	execCtx, cancel := context.WithCancel(ctx)
	tt.mu.Lock()
	tt.Cancel = cancel
	tt.State = ToolExecuting
	tt.mu.Unlock()

	// 执行工具。
	result := te.executor.executeOne(execCtx, tt.Call)

	// 更新状态。
	tt.mu.Lock()
	if execCtx.Err() != nil && tt.State == ToolExecuting {
		tt.State = ToolAborted
		result = ToolResult{
			ToolUseID: tt.Call.ID,
			Name:      tt.Call.Name,
			Content:   "工具执行被中止",
			IsError:   true,
		}
	} else {
		tt.State = ToolCompleted
	}
	tt.Result = &result
	tt.mu.Unlock()

	cancel() // 清理上下文

	return result
}

// AbortSiblings 中止除指定工具外的所有正在执行的兄弟工具。
// 当某个工具（如 Bash）返回错误时，取消其他兄弟工具。
// 对齐 Claude Code 的 sibling abort 逻辑。
func (te *TrackedExecutor) AbortSiblings(exceptID string) int {
	te.mu.RLock()
	defer te.mu.RUnlock()

	if !te.siblingAbortEnabled {
		return 0
	}

	aborted := 0
	for id, tt := range te.tracked {
		if id == exceptID {
			continue
		}
		tt.mu.Lock()
		if tt.State == ToolExecuting && tt.Cancel != nil {
			if tt.InterruptMode == "cancel" {
				tt.Cancel()
				tt.State = ToolAborted
				aborted++
			}
			// "block" 模式的工具不会被中止，让它们自然完成。
		}
		tt.mu.Unlock()
	}
	return aborted
}

// SyntheticResults 为所有未完成的工具生成合成 tool_result。
// 确保每个 tool_use 都有对应的 tool_result。
func (te *TrackedExecutor) SyntheticResults() []ToolResult {
	te.mu.RLock()
	defer te.mu.RUnlock()

	var results []ToolResult
	for _, tt := range te.tracked {
		tt.mu.Lock()
		if tt.State == ToolQueued || tt.State == ToolExecuting {
			results = append(results, ToolResult{
				ToolUseID: tt.Call.ID,
				Name:      tt.Call.Name,
				Content:   fmt.Sprintf("工具 %q 被中断（状态: %s）", tt.Call.Name, tt.State.String()),
				IsError:   true,
			})
		} else if tt.State == ToolAborted && tt.Result == nil {
			results = append(results, ToolResult{
				ToolUseID: tt.Call.ID,
				Name:      tt.Call.Name,
				Content:   "工具执行被中止",
				IsError:   true,
			})
		}
		tt.mu.Unlock()
	}
	return results
}

// GetTracked 返回指定 ID 的追踪工具。
func (te *TrackedExecutor) GetTracked(toolUseID string) *TrackedTool {
	te.mu.RLock()
	defer te.mu.RUnlock()
	return te.tracked[toolUseID]
}

// AllCompleted 返回是否所有追踪的工具都已完成。
func (te *TrackedExecutor) AllCompleted() bool {
	te.mu.RLock()
	defer te.mu.RUnlock()

	for _, tt := range te.tracked {
		state := tt.GetState()
		if state == ToolQueued || state == ToolExecuting {
			return false
		}
	}
	return true
}

// Reset 清除所有追踪状态。
func (te *TrackedExecutor) Reset() {
	te.mu.Lock()
	defer te.mu.Unlock()
	te.tracked = make(map[string]*TrackedTool)
}
