// Package loop — Withholding 机制。
//
// Withholder 暂扣可恢复的流事件（如 413 prompt-too-long、max_output_tokens），
// 使循环能够在事件被最终提交之前尝试恢复策略。
//
// 对齐 Claude Code 的 withholding 逻辑。
package loop

import (
	"github.com/Dream355873200/GoAgent/provider"
)

// WithholdReason 描述暂扣事件的原因。
type WithholdReason int

const (
	// WithholdNone 不需要暂扣。
	WithholdNone WithholdReason = iota
	// WithholdPromptTooLong 413 错误，prompt 太长。
	WithholdPromptTooLong
	// WithholdMaxOutputTokens 输出达到 max_tokens 限制。
	WithholdMaxOutputTokens
)

// String 返回暂扣原因的名称。
func (r WithholdReason) String() string {
	switch r {
	case WithholdNone:
		return "none"
	case WithholdPromptTooLong:
		return "prompt_too_long"
	case WithholdMaxOutputTokens:
		return "max_output_tokens"
	default:
		return "unknown"
	}
}

// WithheldEvent 是一个被暂扣的流事件。
type WithheldEvent struct {
	Event  provider.StreamEvent
	Reason WithholdReason
}

// Withholder 管理可恢复流事件的暂扣。
type Withholder struct {
	held []WithheldEvent
}

// NewWithholder 创建一个新的 Withholder。
func NewWithholder() *Withholder {
	return &Withholder{}
}

// ShouldWithhold 判断给定的流事件是否应该被暂扣。
// 返回暂扣原因；如果不需要暂扣返回 WithholdNone。
func (w *Withholder) ShouldWithhold(ev provider.StreamEvent) WithholdReason {
	// 检查是否是 max_output_tokens 停止。
	if ev.Type == provider.EventMessageComplete && ev.StopReason == provider.StopMaxTokens {
		return WithholdMaxOutputTokens
	}

	// 检查是否是 prompt-too-long 错误。
	if ev.Type == provider.EventError && ev.Error != nil {
		if _, ok := ev.Error.(*provider.PromptTooLongError); ok {
			return WithholdPromptTooLong
		}
	}

	return WithholdNone
}

// Withhold 暂扣一个事件。
func (w *Withholder) Withhold(ev provider.StreamEvent, reason WithholdReason) {
	w.held = append(w.held, WithheldEvent{
		Event:  ev,
		Reason: reason,
	})
}

// Release 释放所有暂扣的事件并清空。
func (w *Withholder) Release() []WithheldEvent {
	events := w.held
	w.held = nil
	return events
}

// Clear 丢弃所有暂扣的事件（恢复成功时使用）。
func (w *Withholder) Clear() {
	w.held = nil
}

// HasHeld 返回是否有暂扣的事件。
func (w *Withholder) HasHeld() bool {
	return len(w.held) > 0
}

// HeldReasons 返回所有暂扣事件的原因列表。
func (w *Withholder) HeldReasons() []WithholdReason {
	reasons := make([]WithholdReason, len(w.held))
	for i, h := range w.held {
		reasons[i] = h.Reason
	}
	return reasons
}
