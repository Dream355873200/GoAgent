// Package anthropic 实现 provider.Provider 接口，对接 Anthropic Messages API。
//
// 基于 anthropic-sdk-go v1.x，支持：
//   - 流式 SSE 输出（Stream）
//   - 非流式调用（Complete，用于 autocompact 摘要等）
//   - Extended Thinking
//   - Tool Use / Tool Result
//   - Prompt Caching（cache_control）
//   - 过载检测 → OverloadError（供 loop fallback）
//   - 413 → PromptTooLongError（供 loop 触发 compaction）
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Provider 实现 provider.Provider，对接 Anthropic Claude API。
type Provider struct {
	client  anthropic.Client
	model   string
	baseURL string // 可选，自定义 API 地址
}

// Option 配置 Provider。
type Option func(*Provider)

// WithModel 设置模型 ID。默认 "claude-sonnet-4-6-v1"。
func WithModel(model string) Option {
	return func(p *Provider) {
		p.model = model
	}
}

// WithBaseURL 设置自定义 API 地址（兼容代理/自部署场景）。
func WithBaseURL(url string) Option {
	return func(p *Provider) {
		p.baseURL = url
	}
}

// New 创建 Anthropic Provider。
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		model: "claude-sonnet-4-6-v1",
	}
	for _, opt := range opts {
		opt(p)
	}

	// 构建 SDK client options。
	clientOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if p.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(p.baseURL))
	}
	p.client = anthropic.NewClient(clientOpts...)

	return p
}

// Stream 发起流式请求，返回 StreamEvent channel。
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: 构建请求参数失败: %w", err)
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	out := make(chan provider.StreamEvent, 64)

	go func() {
		defer close(out)
		defer stream.Close()

		// 状态：组装完整的 assistant message。
		var contentBlocks []message.ContentBlock
		// 当前正在累积的内容块。
		var currentText strings.Builder     // 累积文本 delta
		var currentThinking strings.Builder // 累积 thinking delta
		var currentToolJSON strings.Builder // 累积 tool_use input JSON delta
		var currentToolID string
		var currentToolName string
		var currentBlockType string // 当前块类型："text", "thinking", "tool_use"

		// 最终 usage 和 stop_reason。
		var finalUsage provider.Usage
		var finalStop provider.StopReason

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "message_start":
				// message_start 携带初始 usage。
				if event.Message.Usage.InputTokens > 0 {
					finalUsage.InputTokens = int(event.Message.Usage.InputTokens)
					finalUsage.CacheReadTokens = int(event.Message.Usage.CacheReadInputTokens)
					finalUsage.CacheCreateTokens = int(event.Message.Usage.CacheCreationInputTokens)
					out <- provider.StreamEvent{
						Type: provider.EventUsage,
						Usage: &provider.Usage{
							InputTokens:       int(event.Message.Usage.InputTokens),
							CacheReadTokens:   int(event.Message.Usage.CacheReadInputTokens),
							CacheCreateTokens: int(event.Message.Usage.CacheCreationInputTokens),
						},
					}
				}

			case "content_block_start":
				cb := event.ContentBlock
				currentBlockType = cb.Type
				switch cb.Type {
				case "text":
					currentText.Reset()
				case "tool_use":
					currentToolID = cb.ID
					currentToolName = cb.Name
					currentToolJSON.Reset()
				case "thinking":
					currentThinking.Reset()
				}

			case "content_block_delta":
				delta := event.Delta
				switch delta.Type {
				case "text_delta":
					currentText.WriteString(delta.Text)
					out <- provider.StreamEvent{
						Type: provider.EventTextDelta,
						Text: delta.Text,
					}

				case "thinking_delta":
					currentThinking.WriteString(delta.Thinking)
					out <- provider.StreamEvent{
						Type:     provider.EventThinkingDelta,
						Thinking: delta.Thinking,
					}

				case "input_json_delta":
					currentToolJSON.WriteString(delta.PartialJSON)

				case "signature_delta":
					// thinking 签名，忽略。
				}

			case "content_block_stop":
				switch currentBlockType {
				case "text":
					if text := currentText.String(); text != "" {
						contentBlocks = append(contentBlocks, message.ContentBlock{
							Type: "text",
							Text: text,
						})
					}
				case "thinking":
					if thinking := currentThinking.String(); thinking != "" {
						contentBlocks = append(contentBlocks, message.ContentBlock{
							Type:     "thinking",
							Thinking: thinking,
						})
					}
				case "tool_use":
					inputJSON := json.RawMessage(currentToolJSON.String())
					if len(inputJSON) == 0 {
						inputJSON = json.RawMessage("{}")
					}

					tc := &message.ToolCall{
						ID:    currentToolID,
						Name:  currentToolName,
						Input: inputJSON,
					}
					out <- provider.StreamEvent{
						Type:     provider.EventToolUseStart,
						ToolCall: tc,
					}

					// 追加到完整消息的 content blocks。
					contentBlocks = append(contentBlocks, message.ContentBlock{
						Type:      "tool_use",
						ToolUseID: currentToolID,
						ToolName:  currentToolName,
						Input:     inputJSON,
					})
				}
				currentBlockType = ""

			case "message_delta":
				// message_delta 携带 stop_reason 和最终 usage。
				if event.Delta.StopReason != "" {
					finalStop = mapStopReason(string(event.Delta.StopReason))
				}
				if event.Usage.OutputTokens > 0 {
					finalUsage.OutputTokens = int(event.Usage.OutputTokens)
					finalUsage.InputTokens = int(event.Usage.InputTokens)
					finalUsage.CacheReadTokens = int(event.Usage.CacheReadInputTokens)
					finalUsage.CacheCreateTokens = int(event.Usage.CacheCreationInputTokens)
				}

			case "message_stop":
				// 流结束，组装完整消息。
			}
		}

		// 检查流错误。
		if err := stream.Err(); err != nil {
			apiErr := classifyError(err)
			if apiErr != nil {
				out <- provider.StreamEvent{Type: provider.EventError, Error: apiErr}
			} else {
				out <- provider.StreamEvent{Type: provider.EventError, Error: err}
			}
			return
		}

		// 发送最终 usage。
		out <- provider.StreamEvent{
			Type:  provider.EventUsage,
			Usage: &finalUsage,
		}

		// 组装完整 assistant message（文本通过 contentBlocks 重建）。
		fullMsg := message.Message{
			Role:    message.RoleAssistant,
			Content: contentBlocks,
		}

		out <- provider.StreamEvent{
			Type:       provider.EventMessageComplete,
			Message:    &fullMsg,
			StopReason: finalStop,
		}
	}()

	return out, nil
}

// Complete 非流式调用，阻塞等待完整响应。
// 用于 autocompact 摘要等不需要流式的场景。
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: 构建请求参数失败: %w", err)
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		apiErr := classifyError(err)
		if apiErr != nil {
			return nil, apiErr
		}
		return nil, err
	}

	// 转换响应。
	msg := convertResponseMessage(resp)
	usage := provider.Usage{
		InputTokens:       int(resp.Usage.InputTokens),
		OutputTokens:      int(resp.Usage.OutputTokens),
		CacheReadTokens:   int(resp.Usage.CacheReadInputTokens),
		CacheCreateTokens: int(resp.Usage.CacheCreationInputTokens),
	}

	return &provider.Response{
		Message:    msg,
		Usage:      usage,
		StopReason: mapStopReason(string(resp.StopReason)),
	}, nil
}

// Capabilities 返回模型能力描述。
func (p *Provider) Capabilities() provider.Capabilities {
	caps, ok := modelCapabilities[p.model]
	if !ok {
		return provider.Capabilities{
			ContextWindow:   200_000,
			MaxOutputTokens: 64_000,
			SupportsTools:   true,
			SupportsVision:  true,
			SupportsCaching: true,
			ModelID:         p.model,
		}
	}
	return caps
}

// Ensure Provider implements the interface.
var _ provider.Provider = (*Provider)(nil)

// SetModel 动态切换当前使用的模型（实现 provider.ModelSwitcher 接口）。
func (p *Provider) SetModel(modelID string) {
	p.model = modelID
}

// ── 内部辅助函数 ──────────────────────────────────────────────────

// buildParams 将 provider.Request 转换为 SDK 的 MessageNewParams。
func (p *Provider) buildParams(req *provider.Request) (anthropic.MessageNewParams, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		caps := p.Capabilities()
		maxTokens = int64(caps.MaxOutputTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		Messages:  convertMessages(req.Messages),
	}

	// System prompt。
	if len(req.SystemBlocks) > 0 {
		params.System = convertSystemBlocks(req.SystemBlocks)
	} else if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}

	// Tools。
	if len(req.Tools) > 0 {
		params.Tools = convertToolDefs(req.Tools)
	}

	// Extended Thinking。
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{
				BudgetTokens: int64(req.Thinking.BudgetTokens),
			},
		}
	}

	return params, nil
}

// convertMessages 将内部 message.Message 列表转为 SDK MessageParam 列表。
func convertMessages(msgs []message.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, msg := range msgs {
		var blocks []anthropic.ContentBlockParamUnion
		for _, cb := range msg.Content {
			switch cb.Type {
			case "text":
				blocks = append(blocks, anthropic.NewTextBlock(cb.Text))

			case "tool_use":
				// assistant 发出的 tool_use 块。
				var input any
				if len(cb.Input) > 0 {
					_ = json.Unmarshal(cb.Input, &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(cb.ToolUseID, input, cb.ToolName))

			case "tool_result":
				blocks = append(blocks, anthropic.NewToolResultBlock(cb.ForToolUseID, cb.Text, cb.IsError))

			case "thinking":
				if cb.Thinking != "" {
					// thinking 块需要 signature，如果没有则跳过。
					// 一般来说，回传 thinking 给 API 时需要原始 signature。
					blocks = append(blocks, anthropic.NewThinkingBlock("", cb.Thinking))
				}

			case "image":
				if cb.Data != "" && cb.MediaType != "" {
					blocks = append(blocks, anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
						Data:      cb.Data,
						MediaType: anthropic.Base64ImageSourceMediaType(cb.MediaType),
					}))
				}
			}
		}

		if len(blocks) == 0 {
			continue
		}

		role := anthropic.MessageParamRole(msg.Role)
		result = append(result, anthropic.MessageParam{
			Role:    role,
			Content: blocks,
		})
	}
	return result
}

// convertSystemBlocks 转换多段 system prompt（带 cache_control）。
func convertSystemBlocks(blocks []provider.SystemBlock) []anthropic.TextBlockParam {
	var result []anthropic.TextBlockParam
	for _, b := range blocks {
		tb := anthropic.TextBlockParam{
			Text: b.Text,
		}
		if b.CacheControl != nil {
			tb.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		result = append(result, tb)
	}
	return result
}

// convertToolDefs 将 provider.ToolDefinition 转为 SDK ToolUnionParam。
func convertToolDefs(tools []provider.ToolDefinition) []anthropic.ToolUnionParam {
	var result []anthropic.ToolUnionParam
	for _, t := range tools {
		// 将 InputSchema (any) 转为 ToolInputSchemaParam。
		schema := convertInputSchema(t.InputSchema)

		tp := anthropic.ToolParam{
			Name:        t.Name,
			Description: param.Opt[string]{Value: t.Description},
			InputSchema: schema,
		}
		if t.CacheControl != nil {
			tp.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}

		result = append(result, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return result
}

// convertInputSchema 将 any 类型的 JSON Schema 转为 SDK ToolInputSchemaParam。
func convertInputSchema(schema any) anthropic.ToolInputSchemaParam {
	if schema == nil {
		return anthropic.ToolInputSchemaParam{}
	}

	// schema 通常是 map[string]any 或已经是结构化的 JSON Schema。
	data, err := json.Marshal(schema)
	if err != nil {
		return anthropic.ToolInputSchemaParam{}
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return anthropic.ToolInputSchemaParam{}
	}

	result := anthropic.ToolInputSchemaParam{}

	if props, ok := raw["properties"]; ok {
		result.Properties = props
	}
	if req, ok := raw["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				result.Required = append(result.Required, s)
			}
		}
	}

	// 删除已处理的字段，剩余放入 ExtraFields。
	extra := make(map[string]any)
	for k, v := range raw {
		if k == "type" || k == "properties" || k == "required" {
			continue
		}
		extra[k] = v
	}
	if len(extra) > 0 {
		result.ExtraFields = extra
	}

	return result
}

// convertResponseMessage 将 SDK Message 响应转为内部 message.Message。
func convertResponseMessage(resp *anthropic.Message) message.Message {
	var blocks []message.ContentBlock
	for _, cb := range resp.Content {
		switch cb.Type {
		case "text":
			blocks = append(blocks, message.ContentBlock{
				Type: "text",
				Text: cb.Text,
			})
		case "tool_use":
			inputData, _ := json.Marshal(cb.Input)
			blocks = append(blocks, message.ContentBlock{
				Type:      "tool_use",
				ToolUseID: cb.ID,
				ToolName:  cb.Name,
				Input:     inputData,
			})
		case "thinking":
			blocks = append(blocks, message.ContentBlock{
				Type:     "thinking",
				Thinking: cb.Thinking,
			})
		}
	}
	return message.Message{
		Role:    message.RoleAssistant,
		Content: blocks,
	}
}

// mapStopReason 将 Anthropic 的 stop_reason 字符串映射为 provider.StopReason。
func mapStopReason(reason string) provider.StopReason {
	switch reason {
	case "end_turn":
		return provider.StopEndTurn
	case "tool_use":
		return provider.StopToolUse
	case "max_tokens":
		return provider.StopMaxTokens
	default:
		return provider.StopReason(reason)
	}
}

// httpError 用于从 SDK 错误中提取 HTTP 状态码。
// anthropic-sdk-go 的 apierror.Error 是 internal 的，
// 所以通过接口检测 StatusCode 字段。
type httpError interface {
	error
	StatusCode() int
}

// statusCodeError 通过反射风格的结构体字段提取 HTTP 状态码。
// SDK 的 error 类型有 StatusCode int 字段但没有方法，
// 所以我们检查错误消息中的 HTTP 状态码。
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	// SDK 错误消息格式包含 HTTP 状态码，如 "POST /v1/messages: 429 ..."
	errMsg := err.Error()

	// 检查 overload 相关状态码。
	if containsHTTPStatus(errMsg, 429) || containsHTTPStatus(errMsg, 503) || containsHTTPStatus(errMsg, 529) {
		return &provider.OverloadError{Message: errMsg}
	}

	// 检查 prompt too long。
	if containsHTTPStatus(errMsg, 413) {
		return &provider.PromptTooLongError{Message: errMsg}
	}

	return nil
}

// containsHTTPStatus 检查错误消息中是否包含指定的 HTTP 状态码。
func containsHTTPStatus(errMsg string, code int) bool {
	// SDK 错误格式: 'POST "url": 429 Too Many Requests ...'
	codeStr := fmt.Sprintf(": %d ", code)
	return strings.Contains(errMsg, codeStr)
}

// modelCapabilities 映射模型 ID 到能力描述。
var modelCapabilities = map[string]provider.Capabilities{
	"claude-opus-4-6-v1": {
		ContextWindow:    200_000,
		MaxOutputTokens:  64_000,
		SupportsTools:    true,
		SupportsVision:   true,
		SupportsThinking: true,
		SupportsCaching:  true,
		ModelID:          "claude-opus-4-6-v1",
	},
	"claude-sonnet-4-6-v1": {
		ContextWindow:    200_000,
		MaxOutputTokens:  64_000,
		SupportsTools:    true,
		SupportsVision:   true,
		SupportsThinking: true,
		SupportsCaching:  true,
		ModelID:          "claude-sonnet-4-6-v1",
	},
	"claude-haiku-4-5-20251001": {
		ContextWindow:   200_000,
		MaxOutputTokens: 64_000,
		SupportsTools:   true,
		SupportsVision:  true,
		SupportsCaching: true,
		ModelID:         "claude-haiku-4-5-20251001",
	},
}
