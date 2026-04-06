// Package provider 提供 LLM provider 接口和 mock 实现。
package provider

import (
	"context"
	"encoding/json"

	"github.com/Dream355873200/GoAgent/message"
)

// MockProvider 是用于测试的 mock LLM provider。
//
// 通过 Responses 字段预设每次 Stream 调用的行为。
// 每个 Response 描述一轮 LLM 响应（纯文本或 tool_use）。
// 按 Responses 顺序消费，用完循环使用最后一个。
type MockProvider struct {
	// Responses 预设的响应列表（按消费顺序）。
	Responses []MockResponse

	// responseIdx 当前消费到的索引。
	responseIdx int
}

// MockResponse 描述一轮 mock LLM 响应。
type MockResponse struct {
	// Text 纯文本输出（与 ToolCalls 互斥）。
	Text string

	// ToolCalls 要模拟的工具调用列表。
	// 每个元素包含 Name 和 Input（会被 JSON 序列化）。
	ToolCalls []MockToolCall
}

// MockToolCall 描述一次 mock 工具调用。
type MockToolCall struct {
	ID    string
	Name  string
	Input any // 会被 json.Marshal
}

// NewMockProvider 创建一个只返回纯文本的 MockProvider。
func NewMockProvider(texts ...string) *MockProvider {
	resps := make([]MockResponse, len(texts))
	for i, t := range texts {
		resps[i] = MockResponse{Text: t}
	}
	return &MockProvider{Responses: resps}
}

// NewMockToolCall 创建一个 MockToolCall。
func NewMockToolCall(id, name string, input any) MockToolCall {
	return MockToolCall{ID: id, Name: name, Input: input}
}

// nextResponse 返回下一个预设响应。
func (m *MockProvider) nextResponse() MockResponse {
	if m.responseIdx < len(m.Responses) {
		resp := m.Responses[m.responseIdx]
		m.responseIdx++
		return resp
	}
	return m.Responses[len(m.Responses)-1]
}

// Stream 返回一个 channel，按预设发送事件后关闭。
func (m *MockProvider) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 8)
	resp := m.nextResponse()

	// 构建 assistant message。
	var contentBlocks []message.ContentBlock

	if resp.Text != "" {
		contentBlocks = append(contentBlocks, message.ContentBlock{
			Type: "text", Text: resp.Text,
		})
		ch <- StreamEvent{Type: EventTextDelta, Text: resp.Text}
	}

	stopReason := StopEndTurn
	for _, tc := range resp.ToolCalls {
		inputBytes, _ := json.Marshal(tc.Input)
		contentBlocks = append(contentBlocks, message.ContentBlock{
			Type:      "tool_use",
			ToolUseID: tc.ID,
			ToolName:  tc.Name,
			Input:     inputBytes,
		})
		ch <- StreamEvent{
			Type: EventToolUseStart,
			ToolCall: &message.ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: inputBytes,
			},
		}
		stopReason = StopToolUse
	}

	msg := message.Message{
		Role:    message.RoleAssistant,
		Content: contentBlocks,
	}

	ch <- StreamEvent{
		Type:       EventMessageComplete,
		Message:    &msg,
		StopReason: stopReason,
	}

	go func() {
		close(ch)
	}()

	return ch, nil
}

// Complete 返回预设的完整响应。
func (m *MockProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	resp := m.nextResponse()
	return &Response{
		Message:    message.NewAssistantMessage(resp.Text),
		StopReason: StopEndTurn,
	}, nil
}

// Capabilities 返回 mock 的能力信息。
func (m *MockProvider) Capabilities() Capabilities {
	return Capabilities{
		ContextWindow:   200000,
		MaxOutputTokens: 8192,
		SupportsTools:   true,
		ModelID:         "mock-provider",
	}
}
