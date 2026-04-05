// Package openai 实现 OpenAI 兼容的 Provider。
//
// 支持任何兼容 OpenAI Chat Completions API 的服务，
// 包括 OpenAI、Azure OpenAI、OpenRouter、Ollama 等。
// 使用纯 net/http + SSE 解析，无第三方依赖。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/anthropic-community/goagent/message"
	"github.com/anthropic-community/goagent/provider"
)

// Config 是 OpenAI Provider 的配置。
type Config struct {
	// APIKey 是 API 密钥。留空表示无需鉴权（如本地 Ollama）。
	APIKey string

	// BaseURL 是 API 基础 URL。默认为 "http://localhost:11434/v1"（Ollama）。
	BaseURL string

	// Model 是模型名称。默认为 "qwen2.5:7b"。
	Model string

	// ContextWindow 是模型的上下文窗口大小。
	ContextWindow int

	// MaxOutputTokens 是最大输出 token 数。默认为 4096。
	MaxOutputTokens int

	// DisableTools 为 true 时不发送 tools 参数。
	// 用于不支持 function calling 的模型（如 RP 模型、旧模型等）。
	DisableTools bool
}

// Provider 实现 OpenAI 兼容的 provider.Provider 接口。
type Provider struct {
	mu     sync.RWMutex
	config Config
	client *http.Client
}

// New 创建一个新的 OpenAI Provider。
func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434/v1"
	}
	// 去除末尾斜杠。
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = "qwen-rp:latest"
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = 32_768
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 4096
	}
	return &Provider{
		config: cfg,
		client: &http.Client{},
	}
}

// ---------- OpenAI API 请求/响应类型 ----------

// chatRequest 是 OpenAI Chat Completions API 的请求体。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
}

// chatMessage 是 OpenAI 格式的消息。
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatTool 是 OpenAI 格式的工具定义。
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

// chatFunction 是工具的函数描述。
type chatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// chatToolCall 是助手发起的工具调用。
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

// chatFunctionCall 是工具调用中的函数部分。
type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatResponse 是非流式响应。
type chatResponse struct {
	ID      string       `json:"id"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

// chatChoice 是响应中的一个选择。
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatUsage 是 token 使用统计。
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// streamChunk 是 SSE 流中的一个数据块。
type streamChunk struct {
	ID      string              `json:"id"`
	Choices []streamChunkChoice `json:"choices"`
	Usage   *chatUsage          `json:"usage,omitempty"`
}

// streamChunkChoice 是流式块中的一个选择。
type streamChunkChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// streamDelta 是流式增量内容。
type streamDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   *string          `json:"content,omitempty"`
	ToolCalls []streamToolCall `json:"tool_calls,omitempty"`
}

// streamToolCall 是流式工具调用增量。
type streamToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function streamFuncDelta `json:"function,omitempty"`
}

// streamFuncDelta 是流式函数调用增量。
type streamFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ---------- Provider 接口实现 ----------

// Stream 实现流式 API 调用。
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	// 构建请求体。
	body := p.buildChatRequest(req, true)

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.BaseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}

	// 检查 HTTP 状态码。
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, p.handleHTTPError(resp)
	}

	// 启动 SSE 流消费 goroutine。
	out := make(chan provider.StreamEvent, 64)
	go p.consumeStream(ctx, resp, out)
	return out, nil
}

// Complete 实现非流式 API 调用。
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	body := p.buildChatRequest(req, false)

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.BaseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, p.handleHTTPError(resp)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("API 返回空的 choices")
	}

	choice := chatResp.Choices[0]
	msg := p.fromOpenAIMessage(choice.Message)
	stopReason := p.fromFinishReason(choice.FinishReason)

	var usage provider.Usage
	if chatResp.Usage != nil {
		usage = provider.Usage{
			InputTokens:  chatResp.Usage.PromptTokens,
			OutputTokens: chatResp.Usage.CompletionTokens,
		}
	}

	return &provider.Response{
		Message:    msg,
		Usage:      usage,
		StopReason: stopReason,
	}, nil
}

// Capabilities 返回 provider 的能力信息。
func (p *Provider) Capabilities() provider.Capabilities {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return provider.Capabilities{
		ContextWindow:   p.config.ContextWindow,
		MaxOutputTokens: p.config.MaxOutputTokens,
		SupportsTools:   !p.config.DisableTools,
		ModelID:         p.config.Model,
	}
}

// SetModel 动态切换当前使用的模型（实现 provider.ModelSwitcher 接口）。
func (p *Provider) SetModel(modelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Model = modelID
}

// ---------- 内部方法 ----------

// setHeaders 设置 HTTP 请求头。
func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	}
}

// buildChatRequest 构建 OpenAI Chat Completions 请求。
func (p *Provider) buildChatRequest(req *provider.Request, stream bool) chatRequest {
	p.mu.RLock()
	model := p.config.Model
	disableTools := p.config.DisableTools
	p.mu.RUnlock()

	msgs := toOpenAIMessages(req.SystemPrompt, req.Messages)
	cr := chatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   stream,
	}
	if req.MaxTokens > 0 {
		cr.MaxTokens = req.MaxTokens
	}

	// 转换工具定义（仅在未禁用工具时）。
	if len(req.Tools) > 0 && !disableTools {
		for _, t := range req.Tools {
			cr.Tools = append(cr.Tools, chatTool{
				Type: "function",
				Function: chatFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
	}

	return cr
}

// handleHTTPError 处理非 200 HTTP 响应。
func (p *Provider) handleHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case 413:
		return &provider.PromptTooLongError{
			Message: fmt.Sprintf("请求过大 (HTTP 413): %s", string(body)),
		}
	case 429:
		return &provider.OverloadError{
			Message: fmt.Sprintf("速率限制 (HTTP 429): %s", string(body)),
		}
	case 529:
		return &provider.OverloadError{
			Message: fmt.Sprintf("服务过载 (HTTP 529): %s", string(body)),
		}
	default:
		if resp.StatusCode >= 500 {
			return &provider.OverloadError{
				Message: fmt.Sprintf("服务器错误 (HTTP %d): %s", resp.StatusCode, string(body)),
			}
		}
		return fmt.Errorf("API 错误 (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// consumeStream 消费 SSE 流并发送事件到通道。
func (p *Provider) consumeStream(ctx context.Context, resp *http.Response, out chan<- provider.StreamEvent) {
	defer close(out)
	defer resp.Body.Close()

	// 追踪工具调用状态（用于合并增量）。
	type toolCallState struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolStates := make(map[int]*toolCallState)
	var assistantText strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	// 增大缓冲区以处理大型响应。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		// 检查上下文取消。
		select {
		case <-ctx.Done():
			out <- provider.StreamEvent{Type: provider.EventError, Error: ctx.Err()}
			return
		default:
		}

		line := scanner.Text()

		// SSE 格式: "data: {...}" 或 "data: [DONE]"。
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 跳过无法解析的行。
			continue
		}

		if len(chunk.Choices) == 0 {
			// 可能是只有 usage 的最终块。
			if chunk.Usage != nil {
				out <- provider.StreamEvent{
					Type: provider.EventUsage,
					Usage: &provider.Usage{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
					},
				}
			}
			continue
		}

		choice := chunk.Choices[0]

		// 处理文本增量。
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			text := *choice.Delta.Content
			assistantText.WriteString(text)
			out <- provider.StreamEvent{
				Type: provider.EventTextDelta,
				Text: text,
			}
		}

		// 处理工具调用增量。
		for _, tc := range choice.Delta.ToolCalls {
			state, exists := toolStates[tc.Index]
			if !exists {
				state = &toolCallState{}
				toolStates[tc.Index] = state
			}
			if tc.ID != "" {
				state.id = tc.ID
			}
			if tc.Function.Name != "" {
				state.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				state.arguments.WriteString(tc.Function.Arguments)
			}
		}

		// 处理结束原因。
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason := *choice.FinishReason

			// 在流结束前，发射已完成的工具调用。
			if finishReason == "tool_calls" || finishReason == "stop" {
				for _, state := range toolStates {
					if state.name != "" {
						args := state.arguments.String()
						if args == "" {
							args = "{}"
						}
						out <- provider.StreamEvent{
							Type: provider.EventToolUseStart,
							ToolCall: &message.ToolCall{
								ID:    state.id,
								Name:  state.name,
								Input: json.RawMessage(args),
							},
						}
					}
				}
			}

			// 发送 usage（如果有的话）。
			if chunk.Usage != nil {
				out <- provider.StreamEvent{
					Type: provider.EventUsage,
					Usage: &provider.Usage{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
					},
				}
			}

			// 组装完整的 assistant message。
			fullMsg := message.Message{Role: message.RoleAssistant}
			if text := assistantText.String(); text != "" {
				fullMsg.Content = append(fullMsg.Content, message.ContentBlock{
					Type: "text",
					Text: text,
				})
			}
			for _, state := range toolStates {
				if state.name != "" {
					args := state.arguments.String()
					if args == "" {
						args = "{}"
					}
					fullMsg.Content = append(fullMsg.Content, message.ContentBlock{
						Type:      "tool_use",
						ToolUseID: state.id,
						ToolName:  state.name,
						Input:     json.RawMessage(args),
					})
				}
			}

			// 发送完成事件。
			out <- provider.StreamEvent{
				Type:       provider.EventMessageComplete,
				Message:    &fullMsg,
				StopReason: p.fromFinishReason(finishReason),
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// context 取消是正常的用户主动中断，不作为错误报告。
		if ctx.Err() != nil {
			return
		}
		out <- provider.StreamEvent{Type: provider.EventError, Error: fmt.Errorf("读取 SSE 流失败: %w", err)}
	}
}

// fromOpenAIMessage 将 OpenAI 格式的消息转换回框架消息。
func (p *Provider) fromOpenAIMessage(msg chatMessage) message.Message {
	result := message.Message{
		Role: message.RoleAssistant,
	}

	// 处理文本内容。
	if msg.Content != nil {
		switch v := msg.Content.(type) {
		case string:
			if v != "" {
				result.Content = append(result.Content, message.ContentBlock{
					Type: "text",
					Text: v,
				})
			}
		}
	}

	// 处理工具调用。
	for _, tc := range msg.ToolCalls {
		result.Content = append(result.Content, message.ContentBlock{
			Type:      "tool_use",
			ToolUseID: tc.ID,
			ToolName:  tc.Function.Name,
			Input:     json.RawMessage(tc.Function.Arguments),
		})
	}

	return result
}

// fromFinishReason 将 OpenAI finish_reason 转换为框架 StopReason。
func (p *Provider) fromFinishReason(reason string) provider.StopReason {
	switch reason {
	case "stop":
		return provider.StopEndTurn
	case "tool_calls":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	case "content_filter":
		return provider.StopEndTurn
	default:
		return provider.StopEndTurn
	}
}

// ---------- 消息转换 ----------

// toOpenAIMessages 将框架消息转换为 OpenAI API 消息格式。
func toOpenAIMessages(systemPrompt string, msgs []message.Message) []chatMessage {
	var result []chatMessage

	// 系统提示作为第一条消息。
	if systemPrompt != "" {
		result = append(result, chatMessage{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	for _, msg := range msgs {
		// 检查是否包含 tool_result — 需要特殊处理。
		hasToolResult := false
		for _, block := range msg.Content {
			if block.Type == "tool_result" {
				hasToolResult = true
				break
			}
		}

		if hasToolResult {
			// 每个 tool_result 都要作为独立的 tool 角色消息。
			for _, block := range msg.Content {
				if block.Type == "tool_result" {
					content := block.Text
					if block.IsError {
						content = "Error: " + content
					}
					result = append(result, chatMessage{
						Role:       "tool",
						Content:    content,
						ToolCallID: block.ForToolUseID,
					})
				}
			}
			continue
		}

		openaiMsg := chatMessage{
			Role: string(msg.Role),
		}

		// 提取文本内容。
		text := message.ExtractText(msg)
		if text != "" {
			openaiMsg.Content = text
		}

		// 处理工具调用（assistant 消息）。
		toolCalls := message.ExtractToolCalls(msg)
		if len(toolCalls) > 0 {
			for _, tc := range toolCalls {
				openaiMsg.ToolCalls = append(openaiMsg.ToolCalls, chatToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: chatFunctionCall{
						Name:      tc.Name,
						Arguments: string(tc.Input),
					},
				})
			}
		}

		result = append(result, openaiMsg)
	}
	return result
}
