// Package loop — 消息预处理管线。
//
// Preprocessor 将压缩边界过滤、工具结果预算截断、以及四层压缩
// 统一编排为一个可组合的管线。
//
// 对齐 Claude Code 的 preprocess → compaction 流程。
package loop

import (
	"context"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/message"
)

// PreprocessResult 包含预处理的结果。
type PreprocessResult struct {
	// Messages 是预处理后的消息切片。
	Messages []message.Message

	// TokensFreed 是所有层释放的总 token 数。
	TokensFreed int

	// WasCompacted 标记是否触发了自动压缩（Layer 4）。
	WasCompacted bool

	// WarningState 是预处理后的 token 警告状态。
	WarningState message.TokenWarningState
}

// Preprocessor 协调消息预处理管线。
type Preprocessor struct {
	compaction    *compaction.Manager
	contextWindow int
	maxOutput     int
}

// NewPreprocessor 创建一个新的预处理器。
func NewPreprocessor(cm *compaction.Manager, contextWindow, maxOutput int) *Preprocessor {
	return &Preprocessor{
		compaction:    cm,
		contextWindow: contextWindow,
		maxOutput:     maxOutput,
	}
}

// Pipeline 执行完整的 6 层预处理管线：
//  1. 应用压缩边界 — 过滤边界前消息
//  2. 委托 compaction.Manager 的四层压缩（budget/snip/micro/collapse/auto）
//  3. 计算 token 警告状态
func (p *Preprocessor) Pipeline(ctx context.Context, messages []message.Message) PreprocessResult {
	result := PreprocessResult{
		Messages: messages,
	}

	// 第 1 层：应用压缩边界 — 仅保留边界之后的消息。
	result.Messages = compaction.MessagesAfterCompactBoundary(result.Messages)

	// 第 2 层：委托给 compaction.Manager 执行四层压缩。
	if p.compaction != nil {
		compacted, freed := p.compaction.Apply(ctx, result.Messages, p.contextWindow)
		if freed > 0 {
			result.Messages = compacted
			result.TokensFreed = freed
			result.WasCompacted = true
		}
	}

	// 第 3 层：计算 token 警告状态。
	effectiveWindow := message.EffectiveContextWindow(p.contextWindow, p.maxOutput)
	currentTokens := message.EstimateMessagesTokens(result.Messages)
	result.WarningState = message.CalculateTokenWarningState(currentTokens, effectiveWindow)

	return result
}

// ShouldBlock 检查当前 token 使用量是否已达到硬性阻断阈值。
func (p *Preprocessor) ShouldBlock(messages []message.Message) bool {
	effectiveWindow := message.EffectiveContextWindow(p.contextWindow, p.maxOutput)
	currentTokens := message.EstimateMessagesTokens(messages)
	state := message.CalculateTokenWarningState(currentTokens, effectiveWindow)
	return state == message.TokenStateBlocking
}

// ShouldAutoCompact 检查是否应该触发自动压缩。
func (p *Preprocessor) ShouldAutoCompact(messages []message.Message) bool {
	effectiveWindow := message.EffectiveContextWindow(p.contextWindow, p.maxOutput)
	currentTokens := message.EstimateMessagesTokens(messages)
	threshold := message.AutoCompactThreshold(effectiveWindow)
	return currentTokens >= threshold
}
