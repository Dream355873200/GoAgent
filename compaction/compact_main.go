// Package compaction 实现四层上下文压缩系统。
//
// 对齐 Claude Code 的 compactConversation 流程。
package compaction

import (
	"context"
	"fmt"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// CompactConfig 配置完整压缩流程。
type CompactConfig struct {
	// Provider 是用于生成摘要的 LLM 提供者。
	// 如果设置了 Provider，会使用 Provider.Complete 调用 LLM。
	Provider provider.Provider
	// Summarizer 是用于生成摘要的函数（向后兼容）。
	// 如果 Provider 未设置，会使用 Summarizer。
	Summarizer Summarizer
	// CustomInstructions 是自定义指令（传递给 LLM）。
	CustomInstructions string
	// IsAutoCompact 是否为自动压缩。
	IsAutoCompact bool
	// ContextWindow 上下文窗口大小。
	ContextWindow int
	// Threshold 触发压缩的 token 阈值。
	Threshold int
	// SessionMemoryCfg SessionMemory 配置（可选）。
	SessionMemoryCfg *SessionMemoryConfig
	// MicrocompactCfg 微压缩配置。
	MicrocompactCfg MicrocompactConfig
	// PreCompactHooks 压缩前执行的钩子（可选）。
	PreCompactHooks *PreCompactHookRunner
	// PostCompactCleanup 压缩后执行的清理（可选）。
	PostCompactCleanup *PostCompactCleanupRunner
	// PreCompactTokenCount 压缩前的 token 数（用于日志）。
	PreCompactTokenCount int
	// PromptFile 外部 compact prompt 文件路径（空则用嵌入默认值）。
	PromptFile string
}

// DefaultCompactConfig 返回默认配置。
func DefaultCompactConfig() CompactConfig {
	return CompactConfig{
		MicrocompactCfg: DefaultMicrocompactConfig(),
		Threshold:       0, // 使用默认值
	}
}

// Compact 执行完整压缩流程。
// 对齐 Claude Code 的 compactConversation 函数。
//
// 流程（对齐 Claude Code）：
// 1. 执行 PreCompact Hooks
// 2. 尝试 SessionMemory 压缩（无自定义指令时）
// 3. 微压缩（仅手动压缩时，自动压缩跳过此步）
// 4. 调用 LLM 生成对话摘要
// 5. 执行 PostCompact Cleanup
func Compact(ctx context.Context, messages []message.Message, cfg CompactConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("Compact: 没有消息可压缩")
	}

	// 触发类型
	trigger := PreCompactTriggerManual
	if cfg.IsAutoCompact {
		trigger = PreCompactTriggerAuto
	}

	// 1. 执行 PreCompact Hooks
	mergedInstructions := cfg.CustomInstructions
	var displayMessage string
	if cfg.PreCompactHooks != nil && len(cfg.PreCompactHooks.hooks) > 0 {
		newInstr, displayMsgs, hookErr := cfg.PreCompactHooks.Run(ctx, trigger, cfg.CustomInstructions)
		if hookErr != nil {
			return nil, fmt.Errorf("PreCompact hooks failed: %w", hookErr)
		}
		if newInstr != "" {
			mergedInstructions = newInstr
		}
		if len(displayMsgs) > 0 {
			displayMessage = mergeDisplayMessages(displayMsgs...)
		}
	}

	// 2. 尝试 SessionMemory 压缩（无自定义指令时）
	if mergedInstructions == "" && cfg.SessionMemoryCfg != nil {
		if result := TrySessionMemoryCompaction(ctx, messages, *cfg.SessionMemoryCfg, cfg.Threshold); result != nil {
			result.SessionMemoryUsed = true
			// 执行 PostCompact Cleanup
			if cfg.PostCompactCleanup != nil {
				cfg.PostCompactCleanup.Run(ctx)
			}
			return result, nil
		}
	}

	// 3. 微压缩（仅手动压缩时执行，自动压缩跳过此步）
	// 对齐 Claude Code：autoCompact 直接调用 compactConversation，不走 microcompact
	messagesToSummarize := messages
	if !cfg.IsAutoCompact {
		microResult := MicrocompactMessages(messages, cfg.MicrocompactCfg)
		messagesToSummarize = microResult.Messages
	}

	// 4. 调用 LLM 摘要
	var summary string
	var err error

	if cfg.Provider != nil {
		// 使用 Provider
		summary, err = CompactSummary(ctx, messagesToSummarize, SummaryConfig{
			Provider:           cfg.Provider,
			CustomInstructions: mergedInstructions,
			IsAutoCompact:      cfg.IsAutoCompact,
			ContextWindow:      cfg.ContextWindow,
			PromptFile:         cfg.PromptFile,
		})
	} else if cfg.Summarizer != nil {
		// 使用 Summarizer 函数（向后兼容）
		summary, err = compactWithSummarizer(ctx, messagesToSummarize, cfg)
	} else {
		return nil, fmt.Errorf("Compact: 需要 Provider 或 Summarizer")
	}

	if err != nil {
		return nil, fmt.Errorf("Compact: 摘要生成失败: %w", err)
	}

	// 5. 构建结果消息
	resultMessages := buildCompactedMessages(summary, messagesToSummarize)

	// 计算释放的 token
	tokensBefore := cfg.PreCompactTokenCount
	if tokensBefore == 0 {
		tokensBefore = estimateMessagesTokens(messages)
	}
	tokensAfter := estimateMessagesTokens(resultMessages)
	tokensFreed := tokensBefore - tokensAfter
	if tokensFreed < 0 {
		tokensFreed = 0
	}

	result := &CompactionResult{
		Messages:           resultMessages,
		Summary:            summary,
		TokensFreed:        tokensFreed,
		PreCompactTokens:   tokensBefore,
		PostCompactTokens:  tokensAfter,
		UserDisplayMessage: displayMessage,
	}

	// 6. 执行 PostCompact Cleanup
	if cfg.PostCompactCleanup != nil {
		cfg.PostCompactCleanup.Run(ctx)
	}

	return result, nil
}

// compactWithSummarizer 使用 Summarizer 函数生成摘要。
func compactWithSummarizer(ctx context.Context, messages []message.Message, cfg CompactConfig) (string, error) {
	// 构建对话文本
	conversationText := buildConversationText(messages)

	// 如果对话太长，截断
	maxChars := cfg.ContextWindow * 3
	if maxChars <= 0 {
		maxChars = 200_000
	}
	if len(conversationText) > maxChars {
		conversationText = conversationText[len(conversationText)-maxChars:]
	}

	prompt := "Summarize the following conversation. Preserve key decisions, " +
		"file paths, code changes, and important context. Be concise.\n\n" +
		conversationText

	return cfg.Summarizer(ctx, prompt)
}

// buildCompactedMessages 构建压缩后的消息列表。
func buildCompactedMessages(summary string, recentMessages []message.Message) []message.Message {
	// 构建摘要消息
	summaryMsg := message.Message{
		Role: message.RoleUser,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: "[对话已压缩]\n\n" + summary,
		}},
		Compacted:        true,
		IsCompactSummary: true,
	}

	// 保留最近的消息（通常 2 条）
	recentCount := 2
	if len(recentMessages) < recentCount {
		recentCount = len(recentMessages)
	}

	result := []message.Message{summaryMsg}
	if recentCount > 0 {
		result = append(result, recentMessages[len(recentMessages)-recentCount:]...)
	}

	return result
}

// ShouldCompact 检查是否应该触发自动压缩。
func ShouldCompact(messages []message.Message, contextWindow int, threshold float64) bool {
	if threshold <= 0 {
		threshold = 0.8
	}

	currentTokens := estimateMessagesTokens(messages)
	effectiveWindow := contextWindow - 20_000 // 保留输出空间
	compactThreshold := int(float64(effectiveWindow) * threshold)

	return currentTokens >= compactThreshold
}
