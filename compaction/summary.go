// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// stripImageMarkers 剥离内联图片标记（含全部 base64 数据）——摘要模型只需
// 知道「这里曾有截图」，原图数据会让摘要输入爆掉 token 甚至请求失败。
var imageMarkerRe = regexp.MustCompile(`(?s)\[IMAGE [^\]]{100,}\]`)

func stripImageMarkers(s string) string {
	return imageMarkerRe.ReplaceAllString(s, "[图片已省略]")
}

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
	// PromptFile 外部 compact prompt 文件路径（空则用嵌入默认值）。
	PromptFile string
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
	prompt := GetCompactPrompt(cfg.CustomInstructions, mode, cfg.PromptFile)

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

// buildConversationText 将消息转换为摘要输入文本。
// text 块全量保留；tool_use 序列化为一行调用摘要（工具名+参数要点），
// tool_result 取正文并截断——执行证据链（改了哪些文件/跑了什么命令/
// 结果如何）是摘要质量的生命线：压缩后模型全靠它恢复「做到哪了」，
// 缺了它摘要会误报进度，模型随之输出错误内容或提前收轮。
// 图片标记剥离（base64 会撑爆摘要输入）。
func buildConversationText(messages []message.Message) string {
	var parts []string
	for _, msg := range messages {
		if s := summaryLineFor(msg); s != "" {
			parts = append(parts, fmt.Sprintf("[%s]: %s\n", msg.Role, s))
		}
	}
	return strings.Join(parts, "\n")
}

const (
	// 摘要输入里单块的截断上限与尾部保留量：足够还原事实，又不让单个
	// 超长结果淹没摘要模型。
	summaryBlockMax  = 1200
	summaryBlockTail = 300
	summaryClipMark  = "\n[...过长已截断...]\n"
)

func clipForSummary(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= summaryBlockMax {
		return s
	}
	return headTail(s, summaryBlockMax-summaryBlockTail, summaryBlockTail, summaryClipMark)
}

// summaryLineFor 把一条消息序列化为摘要输入行（含工具块）。
func summaryLineFor(msg message.Message) string {
	var b strings.Builder
	for _, blk := range msg.Content {
		switch blk.Type {
		case "text":
			if t := strings.TrimSpace(stripImageMarkers(blk.Text)); t != "" {
				b.WriteString(t)
				b.WriteString("\n")
			}
		case "tool_use":
			input := clipForSummary(string(blk.Input))
			if input == "" || input == "null" {
				input = "{}"
			}
			fmt.Fprintf(&b, "调用工具 %s(%s)\n", blk.ToolName, input)
		case "tool_result":
			t := clipForSummary(stripImageMarkers(blk.Text))
			if t == "" {
				t = "（空结果）"
			}
			if blk.IsError {
				t = "[错误] " + t
			}
			b.WriteString("工具结果: ")
			b.WriteString(t)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
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
		prompt := GetCompactPrompt(cfg.CustomInstructions, mode, cfg.PromptFile)

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
