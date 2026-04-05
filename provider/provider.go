// Package provider defines the LLM provider interface.
// Framework users can implement this interface to support any model.
// Built-in implementations are provided for Anthropic, OpenAI, etc.
package provider

import (
	"context"

	"github.com/anthropic-community/goagent/message"
)

// Provider is the interface that all LLM backends must implement.
type Provider interface {
	// Stream sends a request and returns a channel of streaming events.
	// The channel is closed when the response is complete or an error occurs.
	// The caller should cancel the context to abort the stream.
	Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error)

	// Complete is a convenience method for non-streaming use cases
	// (e.g., autocompact summarization). Blocks until response is ready.
	Complete(ctx context.Context, req *Request) (*Response, error)

	// Capabilities returns the model's capability information.
	Capabilities() Capabilities
}

// Request 是发送给 LLM 的请求。
type Request struct {
	Messages     []message.Message `json:"messages"`
	SystemPrompt string            `json:"system,omitempty"`
	SystemBlocks []SystemBlock     `json:"system_blocks,omitempty"` // 多段 system prompt（用于 prompt caching）
	Tools        []ToolDefinition  `json:"tools,omitempty"`
	MaxTokens    int               `json:"max_tokens,omitempty"`
	Model        string            `json:"model,omitempty"`
	Thinking     *ThinkingConfig   `json:"thinking,omitempty"` // Extended Thinking 配置
}

// SystemBlock 是 system prompt 的一个段（用于 prompt caching）。
type SystemBlock struct {
	// Text 是段落的文本内容。
	Text string `json:"text"`
	// CacheControl 是此段的缓存控制。nil 表示不缓存。
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl 是 prompt caching 的控制参数。
type CacheControl struct {
	// Type 是缓存类型。目前只支持 "ephemeral"。
	Type string `json:"type"`
}

// ThinkingConfig 是 Extended Thinking 的请求参数。
type ThinkingConfig struct {
	// Type 固定为 "enabled"。
	Type string `json:"type"`
	// BudgetTokens 是 thinking 的 token 预算。
	BudgetTokens int `json:"budget_tokens"`
}

// ToolDefinition 是发送给 LLM 的工具 JSON Schema 描述。
type ToolDefinition struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	InputSchema  any           `json:"input_schema"`
	CacheControl *CacheControl `json:"cache_control,omitempty"` // 工具级缓存控制
}

// Response is the complete (non-streaming) response from the LLM.
type Response struct {
	Message    message.Message `json:"message"`
	Usage      Usage           `json:"usage"`
	StopReason StopReason      `json:"stop_reason"`
}

// StreamEvent is a single event in a streaming response.
type StreamEvent struct {
	Type EventType

	// For TextDelta events
	Text string

	// For ThinkingDelta events — 模型的思考过程增量输出
	Thinking string

	// For ToolUseStart events — a new tool call is being emitted
	ToolCall *message.ToolCall

	// For MessageComplete — the full assembled assistant message
	Message *message.Message

	// For Usage updates
	Usage *Usage

	// For Error events
	Error error

	// StopReason, set on MessageComplete
	StopReason StopReason
}

// EventType categorizes streaming events.
type EventType int

const (
	EventTextDelta       EventType = iota // incremental text output
	EventThinkingDelta                    // incremental thinking output
	EventToolUseStart                     // a tool_use block is fully parsed
	EventUsage                            // token usage update
	EventMessageComplete                  // full message assembled, stream done
	EventError                            // an error occurred
)

// StopReason indicates why the model stopped generating.
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopToolUse       StopReason = "tool_use"
	StopMaxTokens     StopReason = "max_tokens"
	StopPromptTooLong StopReason = "prompt_too_long"
)

// Usage tracks token consumption for a single request.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheReadTokens   int `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// Capabilities describes what the model can do.
type Capabilities struct {
	ContextWindow    int // total context window size in tokens
	MaxOutputTokens  int // maximum output tokens per response
	SupportsTools    bool
	SupportsVision   bool
	SupportsThinking bool
	SupportsCaching  bool
	ModelID          string // e.g. "claude-sonnet-4-6-v1"
}

// ModelSwitcher 是可选接口，Provider 实现后支持运行时切换模型。
type ModelSwitcher interface {
	// SetModel 动态切换当前使用的模型。
	SetModel(modelID string)
}

// PromptTooLongError is returned when the API rejects a request due to context size.
type PromptTooLongError struct {
	Message    string
	TokenCount int
	Limit      int
}

func (e *PromptTooLongError) Error() string {
	return e.Message
}

// MaxOutputTokensError is returned when the response was truncated.
type MaxOutputTokensError struct {
	PartialMessage message.Message
}

func (e *MaxOutputTokensError) Error() string {
	return "response truncated: max output tokens reached"
}

// OverloadError is returned when the model is overloaded (for fallback).
type OverloadError struct {
	Message string
}

func (e *OverloadError) Error() string {
	return e.Message
}
