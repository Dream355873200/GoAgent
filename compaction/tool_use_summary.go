// Package compaction 实现上下文压缩系统。
package compaction

import (
	"strings"

	"github.com/Dream355873200/GoAgent/message"
)

// ToolUseSummaryConfig 配置工具使用摘要。
type ToolUseSummaryConfig struct {
	// MaxTools 摘要中最大工具数。
	MaxTools int
	// IncludeOutputs 是否包含输出摘要。
	IncludeOutputs bool
	// MaxOutputLen 单个输出最大长度。
	MaxOutputLen int
}

// DefaultToolUseSummaryConfig 返回默认配置。
func DefaultToolUseSummaryConfig() ToolUseSummaryConfig {
	return ToolUseSummaryConfig{
		MaxTools:       50,
		IncludeOutputs: false,
		MaxOutputLen:   100,
	}
}

// ToolUseSummary 工具使用摘要结果。
type ToolUseSummary struct {
	// ToolCalls 工具调用摘要。
	ToolCalls []ToolCallSummary
	// TotalTools 总工具调用数。
	TotalTools int
	// UniqueTools 唯一工具类型数。
	UniqueTools int
}

// ToolCallSummary 单个工具调用摘要。
type ToolCallSummary struct {
	ToolName  string
	CallCount int
	Inputs    []string // 简化的输入描述
}

// GenerateToolUseSummary 生成工具使用的摘要。
// 用于压缩时将多个工具调用折叠为摘要。
func GenerateToolUseSummary(messages []message.Message, cfg ToolUseSummaryConfig) *ToolUseSummary {
	if cfg.MaxTools == 0 {
		cfg = DefaultToolUseSummaryConfig()
	}

	// 收集所有工具调用
	toolCalls := make(map[string]*ToolCallSummary)
	totalCalls := 0

	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" {
				totalCalls++
				name := block.ToolName

				if _, ok := toolCalls[name]; !ok {
					toolCalls[name] = &ToolCallSummary{
						ToolName:  name,
						CallCount: 0,
						Inputs:    []string{},
					}
				}

				summary := toolCalls[name]
				summary.CallCount++

				// 简化输入
				if cfg.IncludeOutputs && len(block.Input) > 0 {
					inputStr := string(block.Input)
					if len(inputStr) > cfg.MaxOutputLen {
						inputStr = inputStr[:cfg.MaxOutputLen] + "..."
					}
					// 移除 JSON 格式
					inputStr = strings.ReplaceAll(inputStr, "\n", " ")
					inputStr = strings.ReplaceAll(inputStr, "{", "")
					inputStr = strings.ReplaceAll(inputStr, "}", "")
					if len(summary.Inputs) < 3 { // 最多记录 3 个输入示例
						summary.Inputs = append(summary.Inputs, inputStr)
					}
				}
			}
		}
	}

	// 转换为切片并排序
	result := make([]ToolCallSummary, 0, len(toolCalls))
	uniqueTools := 0
	for _, s := range toolCalls {
		result = append(result, *s)
		uniqueTools++
	}

	return &ToolUseSummary{
		ToolCalls:   result,
		TotalTools:  totalCalls,
		UniqueTools: uniqueTools,
	}
}

// FormatToolUseSummary 将工具摘要格式化为文本。
func FormatToolUseSummary(summary *ToolUseSummary) string {
	if summary == nil || summary.TotalTools == 0 {
		return "(无工具调用)"
	}

	var sb strings.Builder
	sb.WriteString("[工具使用摘要]\n")
	sb.WriteString("总调用次数: ")
	sb.WriteString(intToString(summary.TotalTools))
	sb.WriteString("\n")
	sb.WriteString("唯一工具数: ")
	sb.WriteString(intToString(summary.UniqueTools))
	sb.WriteString("\n\n")

	for i, tc := range summary.ToolCalls {
		if i >= 50 { // 最多显示 50 个
			sb.WriteString("...\n")
			break
		}
		sb.WriteString("- ")
		sb.WriteString(tc.ToolName)
		sb.WriteString(": ")
		sb.WriteString(intToString(tc.CallCount))
		sb.WriteString(" 次\n")
	}

	return sb.String()
}

// intToString 简单 int 转字符串。
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var sb strings.Builder
	for n > 0 {
		sb.WriteByte(byte('0' + n%10))
		n /= 10
	}
	// 反转
	result := sb.String()
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result = result[:i] + string(result[j]) + result[i+1:]
	}
	return result
}

// CompactToolCalls 将一系列工具调用折叠为单个摘要消息。
// 用于 microcompact 阶段。
func CompactToolCalls(messages []message.Message, cfg ToolUseSummaryConfig) []message.Message {
	if len(messages) == 0 {
		return messages
	}

	summary := GenerateToolUseSummary(messages, cfg)
	summaryText := FormatToolUseSummary(summary)

	// 保留最后一条用户消息和摘要
	var result []message.Message

	// 查找最后一条用户消息
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == message.RoleUser {
			lastUserIdx = i
			break
		}
	}

	// 保留用户消息
	if lastUserIdx >= 0 {
		result = append(result, messages[lastUserIdx])
	}

	// 添加摘要消息
	summaryMsg := message.Message{
		Role: message.RoleUser,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: "[工具使用已压缩]\n\n" + summaryText,
		}},
		Compacted:        true,
		IsCompactSummary: true,
	}
	result = append(result, summaryMsg)

	return result
}
