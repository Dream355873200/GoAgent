// Package message 定义 agent 循环中使用的核心消息类型。
// 这些类型是公开的 — 用户可以独立导入此包来构建自己的 agent 循环
// 或与其他 LLM 框架集成。
package message

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

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

	// UID 是对话记录中的消息身份：由 agent 循环在消息进入对话时分配，
	// 持久化按 UID 去重追加。为空表示派生消息（压缩摘要、折叠占位、
	// 文件恢复等只属于模型视图的内容），不写入对话记录。
	UID string `json:"uid,omitempty"`

	// 压缩和循环系统内部使用的元数据。
	TokenEstimate     int  `json:"-"` // 估算的 token 数，由循环设置
	Compacted         bool `json:"-"` // 此消息是否已被压缩
	IsMeta            bool `json:"is_meta,omitempty"` // 元消息（恢复提示等）：持久化保留（模型上下文），展示层回放必须过滤
	IsCompactBoundary bool `json:"-"` // 标记 prompt cache 的压缩边界

	// IsCompactSummary 标记压缩摘要。agent 循环把自动摘要作为摘要记录追加进
	// 对话记录（它同时是元消息，展示层回放不显示）；compaction.Project 据此
	// 从对话记录还原发给模型的视图，重新加载时不必再请求模型摘要一遍。
	IsCompactSummary bool `json:"is_compact_summary,omitempty"`
	// CompactBoundary 是摘要之后保留的第一条消息的 UID：对话记录中在它之前
	// 的消息都已被摘要覆盖。为空表示摘要覆盖了摘要记录之前的全部消息。
	CompactBoundary string `json:"compact_boundary,omitempty"`
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
	// Signature 是 thinking 块的签名。Anthropic 要求回传 thinking 块时附带
	// 原始签名，缺失会被 API 拒绝。redacted_thinking 块的密文存放在 Data。
	Signature string `json:"signature,omitempty"`
}

// NewUID 生成一个随机的消息 UID。
func NewUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("message: crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// CloneContent 返回消息内容块的独立副本。压缩等需要改写内容的逻辑必须
// 先克隆：Message 是值类型，但 Content 切片与原消息共享底层数组。
func (m Message) CloneContent() Message {
	if m.Content != nil {
		m.Content = append([]ContentBlock(nil), m.Content...)
	}
	return m
}

// IsToolResultMessage 报告消息是否为只含 tool_result 块的用户消息。
func IsToolResultMessage(m Message) bool {
	if m.Role != RoleUser || len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// HasToolUse 报告消息是否发出过工具调用（含 tool_use 块）。
func HasToolUse(m Message) bool {
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
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
