// Package loop — Stop Hook 系统。
//
// Stop Hooks 在每轮结束后运行，用于后验证。
// 如果某个 hook 返回 Block=true，循环将注入修正消息并继续，
// 而不是正常退出。
//
// 对齐 Claude Code 的 stopHook 机制。
package loop

import (
	"context"

	"github.com/Dream355873200/GoAgent/message"
)

// StopHookResult 是 stop hook 的执行结果。
type StopHookResult struct {
	// Block 如果为 true，表示 hook 阻止了正常退出。
	Block bool

	// Message 是 hook 需要注入对话的修正消息（仅 Block=true 时有效）。
	Message string

	// Reason 描述为什么 hook 阻止了退出。
	Reason string
}

// StopHook 是一个在每轮结束时运行的验证函数。
// 它检查助手的最后一次响应并决定是否允许退出。
type StopHook func(ctx context.Context, messages []message.Message, lastAssistantText string) StopHookResult

// StopHookRunner 管理和执行 stop hooks。
type StopHookRunner struct {
	hooks []StopHook
}

// NewStopHookRunner 创建一个新的 stop hook 运行器。
func NewStopHookRunner(hooks ...StopHook) *StopHookRunner {
	return &StopHookRunner{hooks: hooks}
}

// AddHook 注册一个新的 stop hook。
func (r *StopHookRunner) AddHook(hook StopHook) {
	r.hooks = append(r.hooks, hook)
}

// RunStopHooks 依次执行所有 stop hooks。
// 首个返回 Block=true 的结果生效，后续 hooks 不再执行。
// 如果所有 hooks 都通过，返回 nil。
func (r *StopHookRunner) RunStopHooks(ctx context.Context, messages []message.Message, lastAssistantText string) *StopHookResult {
	for _, hook := range r.hooks {
		result := hook(ctx, messages, lastAssistantText)
		if result.Block {
			return &result
		}
	}
	return nil
}

// HasHooks 返回是否注册了任何 stop hooks。
func (r *StopHookRunner) HasHooks() bool {
	return len(r.hooks) > 0
}

// --- 内置 Stop Hooks ---

// MaxOutputTokensStopHook 创建一个检测 max_output_tokens 截断的 stop hook。
// 当检测到输出被截断时，注入继续提示让模型恢复。
func MaxOutputTokensStopHook(maxRecoveries int) StopHook {
	recoveryCount := 0
	return func(_ context.Context, _ []message.Message, lastText string) StopHookResult {
		// 此 hook 的实际触发由循环中的 stopReason 控制。
		// 这里作为备用检查：如果文本以截断的代码块结尾。
		if recoveryCount >= maxRecoveries {
			return StopHookResult{Block: false}
		}

		// 检查是否有未关闭的代码块。
		openBlocks := 0
		for i := 0; i < len(lastText); i++ {
			if i+2 < len(lastText) && lastText[i:i+3] == "```" {
				openBlocks++
			}
		}

		if openBlocks%2 != 0 {
			recoveryCount++
			return StopHookResult{
				Block:   true,
				Message: "输出在代码块中途被截断。请从截断处继续，不要道歉或重复之前的内容。",
				Reason:  "unclosed_code_block",
			}
		}

		return StopHookResult{Block: false}
	}
}

// TokenBudgetStopHook 创建一个检测 token 预算未耗尽的 stop hook。
// 当模型过早停止（仍有大量 token 预算时），注入提示让模型继续。
func TokenBudgetStopHook(minUsageRatio float64) StopHook {
	return func(_ context.Context, messages []message.Message, _ string) StopHookResult {
		// 计算当前使用的 token 数。
		totalTokens := 0
		for _, msg := range messages {
			totalTokens += message.EstimateTokens(msg)
		}

		// 如果使用率低于阈值，说明可能还有工作未完成。
		// 这里只是骨架逻辑，实际判断需要更复杂的上下文分析。
		_ = minUsageRatio
		return StopHookResult{Block: false}
	}
}
