// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// SummaryConfig 配置摘要生成。
type SummaryConfig struct {
	// Provider 是用于生成摘要的 LLM 提供者。
	Provider provider.Provider
	// CustomInstructions 是自定义指令。
	CustomInstructions string
	// IsAutoCompact 是否为自动压缩。
	IsAutoCompact bool
	// ContextWindow 上下文窗口大小。
	ContextWindow int
}

// CompactSummary 调用 LLM 生成对话摘要。
// 对齐 Claude Code 的 streamCompactSummary 函数。
func CompactSummary(ctx context.Context, messages []message.Message, cfg SummaryConfig) (string, error) {
	if cfg.Provider == nil {
		return "", fmt.Errorf("CompactSummary 需要 Provider")
	}

	// 选择摘要模式
	mode := CompactModeBase
	if cfg.IsAutoCompact {
		mode = CompactModePartial
	}

	// 构建摘要 prompt
	prompt := GetCompactPrompt(cfg.CustomInstructions, mode)

	// 构建请求消息
	reqMessages := []message.Message{
		message.NewUserMessage(prompt),
	}

	// 将对话内容作为上下文追加
	conversationText := buildConversationText(messages)
	reqMessages = append(reqMessages, message.NewUserMessage(conversationText))

	// 调用 LLM
	req := &provider.Request{
		Messages:     reqMessages,
		MaxTokens:    4096,
		SystemPrompt: "",
	}

	resp, err := cfg.Provider.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("CompactSummary 调用 LLM 失败: %w", err)
	}

	// 提取文本
	summary := message.ExtractText(resp.Message)

	// 格式化摘要
	formatted := FormatCompactSummary(summary)

	return formatted, nil
}

// buildConversationText 将消息转换为对话文本。
func buildConversationText(messages []message.Message) string {
	var parts []string
	for _, msg := range messages {
		text := message.ExtractText(msg)
		if text != "" {
			parts = append(parts, fmt.Sprintf("[%s]: %s\n", msg.Role, text))
		}
	}
	return strings.Join(parts, "\n")
}

// CompactSummaryStream 流式调用 LLM 生成摘要。
// 返回一个 channel，接收摘要文本片段。
func CompactSummaryStream(ctx context.Context, messages []message.Message, cfg SummaryConfig) (<-chan string, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("CompactSummaryStream 需要 Provider")
	}

	out := make(chan string, 1)

	go func() {
		defer close(out)

		// 选择摘要模式
		mode := CompactModeBase
		if cfg.IsAutoCompact {
			mode = CompactModePartial
		}

		// 构建摘要 prompt
		prompt := GetCompactPrompt(cfg.CustomInstructions, mode)

		// 构建请求消息
		conversationText := buildConversationText(messages)
		fullPrompt := prompt + "\n\n" + conversationText

		req := &provider.Request{
			Messages:     []message.Message{message.NewUserMessage(fullPrompt)},
			MaxTokens:    4096,
			SystemPrompt: "",
		}

		stream, err := cfg.Provider.Stream(ctx, req)
		if err != nil {
			return
		}

		for event := range stream {
			if event.Text != "" {
				out <- event.Text
			}
		}
	}()

	return out, nil
}
