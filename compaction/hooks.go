// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"context"
	"fmt"
)

// PreCompactHook 在压缩开始前调用的钩子。
// 对齐 Claude Code 的 executePreCompactHooks。
type PreCompactHook interface {
	// Name 返回钩子名称。
	Name() string
	// Execute 在压缩开始前执行。
	// 返回自定义指令（会合并到压缩 prompt）和用户显示消息。
	Execute(ctx context.Context, trigger PreCompactTrigger, customInstructions string) *PreCompactResult
}

// PreCompactTrigger 压缩触发类型。
type PreCompactTrigger string

const (
	PreCompactTriggerAuto   PreCompactTrigger = "auto"
	PreCompactTriggerManual PreCompactTrigger = "manual"
)

// PreCompactResult 预压缩钩子的结果。
type PreCompactResult struct {
	// NewCustomInstructions 新的自定义指令（会合并到压缩 prompt）。
	NewCustomInstructions string
	// UserDisplayMessage 用户显示的消息。
	UserDisplayMessage string
	// Error 错误（如果有）。
	Error error
}

// PostCompactCleanup 压缩后的清理钩子。
// 对齐 Claude Code 的 runPostCompactCleanup。
type PostCompactCleanup interface {
	// Name 返回清理器名称。
	Name() string
	// Execute 在压缩完成后执行清理。
	Execute(ctx context.Context)
}

// PreCompactHookRunner 运行预压缩钩子。
type PreCompactHookRunner struct {
	hooks []PreCompactHook
}

// NewPreCompactHookRunner 创建预压缩钩子运行器。
func NewPreCompactHookRunner(hooks ...PreCompactHook) *PreCompactHookRunner {
	return &PreCompactHookRunner{hooks: hooks}
}

// Run 执行所有预压缩钩子。
// 对齐 Claude Code 的 executePreCompactHooks。
func (r *PreCompactHookRunner) Run(ctx context.Context, trigger PreCompactTrigger, customInstructions string) (mergedInstructions string, displayMessages []string, err error) {
	if len(r.hooks) == 0 {
		return customInstructions, nil, nil
	}

	var merged customInstructionsBuilder
	merged.Write(customInstructions)

	var displayMsgs []string

	for _, hook := range r.hooks {
		result := hook.Execute(ctx, trigger, merged.String())
		if result.Error != nil {
			return "", nil, fmt.Errorf("PreCompact hook %q failed: %w", hook.Name(), result.Error)
		}
		if result.NewCustomInstructions != "" {
			merged.Write(result.NewCustomInstructions)
		}
		if result.UserDisplayMessage != "" {
			displayMsgs = append(displayMsgs, result.UserDisplayMessage)
		}
	}

	return merged.String(), displayMsgs, nil
}

// customInstructionsBuilder 辅助构建自定义指令字符串。
type customInstructionsBuilder struct {
	parts []string
}

func (b *customInstructionsBuilder) Write(s string) {
	if s != "" {
		b.parts = append(b.parts, s)
	}
}

func (b *customInstructionsBuilder) String() string {
	result := ""
	for i, part := range b.parts {
		if i > 0 {
			result += "\n\n"
		}
		result += part
	}
	return result
}

// PostCompactCleanupRunner 运行压缩后清理。
type PostCompactCleanupRunner struct {
	cleanups []PostCompactCleanup
}

// NewPostCompactCleanupRunner 创建压缩后清理运行器。
func NewPostCompactCleanupRunner(cleanups ...PostCompactCleanup) *PostCompactCleanupRunner {
	return &PostCompactCleanupRunner{cleanups: cleanups}
}

// Run 执行所有压缩后清理。
// 对齐 Claude Code 的 runPostCompactCleanup。
func (r *PostCompactCleanupRunner) Run(ctx context.Context) {
	for _, cleanup := range r.cleanups {
		cleanup.Execute(ctx)
	}
}

// DefaultPostCompactCleanup 默认压缩后清理实现。
// 对齐 Claude Code 的 runPostCompactCleanup。
type DefaultPostCompactCleanup struct{}

// NewDefaultPostCompactCleanup 创建默认压缩后清理。
func NewDefaultPostCompactCleanup() *DefaultPostCompactCleanup {
	return &DefaultPostCompactCleanup{}
}

func (c *DefaultPostCompactCleanup) Name() string {
	return "default"
}

func (c *DefaultPostCompactCleanup) Execute(ctx context.Context) {
	// 重置 microcompact 状态
	resetMicrocompactState()

	// 重置 session memory 压缩状态
	ResetSMCompactState()
}

// resetMicrocompactState 重置 microcompact 内部状态。
func resetMicrocompactState() {
	// 重置所有内部计数器
	// 注意：这里可能需要根据实际的 microcompact 状态来调整
}

// PreCompactHookFunc 函数类型的 PreCompactHook 适配器。
type PreCompactHookFunc func(ctx context.Context, trigger PreCompactTrigger, customInstructions string) *PreCompactResult

func (f PreCompactHookFunc) Name() string {
	return "anonymous"
}

func (f PreCompactHookFunc) Execute(ctx context.Context, trigger PreCompactTrigger, customInstructions string) *PreCompactResult {
	return f(ctx, trigger, customInstructions)
}

// PostCompactCleanupFunc 函数类型的 PostCompactCleanup 适配器。
type PostCompactCleanupFunc func(ctx context.Context)

func (f PostCompactCleanupFunc) Name() string {
	return "anonymous"
}

func (f PostCompactCleanupFunc) Execute(ctx context.Context) {
	f(ctx)
}

// mergeDisplayMessages 合并多个用户显示消息。
// 对齐 Claude Code 的 mergeDisplayMessages。
func mergeDisplayMessages(messages ...string) string {
	var result string
	for i, msg := range messages {
		if msg == "" {
			continue
		}
		if i > 0 {
			result += "\n"
		}
		result += msg
	}
	return result
}
