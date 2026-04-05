// Package message 定义 agent 循环中使用的核心消息类型。
// 这些类型是公开的 — 用户可以独立导入此包来构建自己的 agent 循环
// 或与其他 LLM 框架集成。
package message

import "encoding/json"

// Role 表示消息的发送者。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Message 是对话中的一个轮次。
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`

	// 压缩和循环系统内部使用的元数据。
	TokenEstimate     int  `json:"-"` // 估算的 token 数，由循环设置
	Compacted         bool `json:"-"` // 此消息是否已被压缩
	IsMeta            bool `json:"-"` // 元消息（恢复提示等）
	IsCompactBoundary bool `json:"-"` // 标记 prompt cache 的压缩边界
	IsCompactSummary  bool `json:"-"` // 标记压缩后的摘要消息
}

// ContentBlock 是消息中的一段内容。
type ContentBlock struct {
	Type string `json:"type"` // "text", "tool_use", "tool_result", "image", "thinking"

	// 文本内容
	Text string `json:"text,omitempty"`

	// 工具调用
	ToolUseID string          `json:"id,omitempty"`
	ToolName  string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`

	// 工具结果
	ForToolUseID string `json:"tool_use_id,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`

	// 图片内容
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	Source    any    `json:"source,omitempty"`

	// Thinking 内容（模型的推理过程）
	Thinking string `json:"thinking,omitempty"`
}

// ToolCall 从 tool_use 内容块中提取，供执行器使用。
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// NewUserMessage 创建一个简单的文本用户消息。
func NewUserMessage(text string) Message {
	return Message{
		Role:    RoleUser,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

// NewAssistantMessage 创建一个简单的文本助手消息。
func NewAssistantMessage(text string) Message {
	return Message{
		Role:    RoleAssistant,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

// NewToolResultMessage 创建一个工具结果消息。
func NewToolResultMessage(toolUseID, text string, isError bool) Message {
	return Message{
		Role: RoleUser,
		Content: []ContentBlock{{
			Type:         "tool_result",
			ForToolUseID: toolUseID,
			Text:         text,
			IsError:      isError,
		}},
	}
}

// NewMetaMessage 创建一个元用户消息（例如恢复提示）。
func NewMetaMessage(text string) Message {
	m := NewUserMessage(text)
	m.IsMeta = true
	return m
}

// ExtractToolCalls 返回消息中所有的 tool_use 块。
func ExtractToolCalls(msg Message) []ToolCall {
	var calls []ToolCall
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			calls = append(calls, ToolCall{
				ID:    block.ToolUseID,
				Name:  block.ToolName,
				Input: block.Input,
			})
		}
	}
	return calls
}

// ExtractText 返回消息中所有文本的拼接结果。
func ExtractText(msg Message) string {
	var text string
	for _, block := range msg.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return text
}

// ExtractThinking 返回消息中所有 thinking 内容的拼接结果。
func ExtractThinking(msg Message) string {
	var text string
	for _, block := range msg.Content {
		if block.Type == "thinking" {
			text += block.Thinking
		}
	}
	return text
}

// EstimateTokens 提供消息的类型感知 token 估算。
// 委托给 EstimateMessageTokens，后者处理不同的内容块类型。
func EstimateTokens(msg Message) int {
	return EstimateMessageTokens(msg)
}
