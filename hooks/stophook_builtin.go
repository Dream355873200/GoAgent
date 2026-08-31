// Package hooks — 内置 Stop Hooks。
//
// 从 internal/loop/stophook.go 迁入（双轨合并：stop 扩展点统一到
// hooks 包，internal 的 StopHookRunner 已删除）。
package hooks

import (
	"context"
	"fmt"
)

// StopHookFunc 是最简的 stop hook 形态：一个函数直接注册。
// 返回 block=true 时阻止循环退出，message 作为修正消息注入对话。
type StopHookFunc func(ctx context.Context, hctx *HookContext) (block bool, message string)

type stopHookAdapter struct {
	name string
	fn   StopHookFunc
}

// NewStopHook 把函数包装为 Hook（Events 只含 EventStop）。
func NewStopHook(name string, fn StopHookFunc) Hook {
	return &stopHookAdapter{name: name, fn: fn}
}

func (a *stopHookAdapter) Name() string        { return a.name }
func (a *stopHookAdapter) Events() []HookEvent { return []HookEvent{EventStop} }
func (a *stopHookAdapter) Execute(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	block, msg := a.fn(ctx, hctx)
	return &HookResult{Block: block, Message: msg}, nil
}

// MaxOutputTokensStopHook 检测输出被截断的内置 stop hook（启发式备用）。
//
// 检测手段：数最后回复里 ``` 的个数——奇数 = 有代码块没闭合 =
// 输出疑似在代码块中途被截断。注意引擎的 stopReason==StopMaxTokens
// 恢复（提上限/续写提示）已在 loop 主循环处理且优先于此；本 hook
// 覆盖的是 stopReason 未报截断（部分 provider 不报）时的文本兜底。
//
// 触发时注入修正消息让模型从截断处继续（不道歉不重复）。
func MaxOutputTokensStopHook(maxRecoveries int) Hook {
	recovered := 0
	return NewStopHook("max_output_tokens", func(_ context.Context, hctx *HookContext) (bool, string) {
		if recovered >= maxRecoveries {
			return false, ""
		}
		last := hctx.LastAssistantText
		openBlocks := 0
		for i := 0; i+2 < len(last); i++ {
			if last[i:i+3] == "```" {
				openBlocks++
			}
		}
		if openBlocks%2 != 0 {
			recovered++
			return true, fmt.Sprintf(
				"输出在代码块中途被截断（检测到 %d 个未闭合标记）。请从截断处继续，不要道歉或重复之前的内容。",
				openBlocks)
		}
		return false, ""
	})
}
