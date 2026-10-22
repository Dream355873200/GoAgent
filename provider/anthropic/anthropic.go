// Package anthropic 实现 provider.Provider 接口，对接 Anthropic Messages API。
//
// 基于 anthropic-sdk-go v1.x，支持：
//   - 流式 SSE 输出（Stream）
//   - 非流式调用（Complete，用于 autocompact 摘要等）
//   - Extended Thinking（thinking / redacted_thinking 块连同签名原样回传）
//   - Tool Use / Tool Result
//   - Prompt Caching（cache_control）
//   - 429 → RateLimitError（携带 Retry-After，供 loop 等待重试）
//   - 529 / 5xx → OverloadError（供 loop fallback）
//   - 413、400 prompt is too long → PromptTooLongError（供 loop 触发 compaction）
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Provider 实现 provider.Provider，对接 Anthropic Claude API。
type Provider struct {
	client  anthropic.Client
	baseURL string // 可选，自定义 API 地址

	mu    sync.RWMutex // 保护 model：SetModel 可与进行中的请求并发
	model string
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
//
// 请求阶段的失败（HTTP 错误、连接失败，已经过 SDK 自带重试）同步返回，调用方
// 据错误类型决定等待重试、切换后备模型或压缩；流开始后的失败经 EventError 下发。
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: 构建请求参数失败: %w", err)
	}

	// NewStreaming 同步完成 HTTP 请求，返回时 Err 非空即表示流尚未开始。
	stream := p.client.Messages.NewStreaming(ctx, params)
	if err := stream.Err(); err != nil {
		stream.Close()
		return nil, classifyError(err)
	}

	out := make(chan provider.StreamEvent, 64)
	go consumeStream(stream, out)
	return out, nil
}

// streamBlock 是流式响应中正在累积的内容块。
type streamBlock struct {
	typ       string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	data      string // redacted_thinking 的密文
	toolID    string
	toolName  string
	toolJSON  strings.Builder
}

// contentBlock 把累积完成的块转为消息内容块；没有可回传内容时返回 false。
func (b *streamBlock) contentBlock() (message.ContentBlock, bool) {
	switch b.typ {
	case "text":
		if b.text.Len() > 0 {
			return message.ContentBlock{Type: "text", Text: b.text.String()}, true
		}
	case "thinking":
		if b.thinking.Len() > 0 || b.signature.Len() > 0 {
			return message.ContentBlock{
				Type:      "thinking",
				Thinking:  b.thinking.String(),
				Signature: b.signature.String(),
			}, true
		}
	case "redacted_thinking":
		if b.data != "" {
			return message.ContentBlock{Type: "redacted_thinking", Data: b.data}, true
		}
	case "tool_use":
		input := json.RawMessage(b.toolJSON.String())
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return message.ContentBlock{
			Type:      "tool_use",
			ToolUseID: b.toolID,
			ToolName:  b.toolName,
			Input:     input,
		}, true
	}
	return message.ContentBlock{}, false
}

// consumeStream 消费 SDK 事件流并转为 provider.StreamEvent，结束时关闭 out。
func consumeStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], out chan<- provider.StreamEvent) {
	defer close(out)
	defer stream.Close()

	var (
		blocks     []message.ContentBlock // 按到达顺序组装的完整消息内容
		cur        *streamBlock
		startUsage provider.Usage
		deltaUsage provider.Usage
		stopReason provider.StopReason
	)

	for stream.Next() {
		event := stream.Current()

		switch event.Type {
		case "message_start":
			u := event.Message.Usage
			startUsage = provider.Usage{
				InputTokens:       int(u.InputTokens),
				OutputTokens:      int(u.OutputTokens),
				CacheReadTokens:   int(u.CacheReadInputTokens),
				CacheCreateTokens: int(u.CacheCreationInputTokens),
			}

		case "content_block_start":
			cb := event.ContentBlock
			cur = &streamBlock{typ: cb.Type, toolID: cb.ID, toolName: cb.Name, data: cb.Data}
			cur.signature.WriteString(cb.Signature)
			// 官方 API 的起始块内容为空，兼容直接在起始块给出内容的实现。
			if cb.Type == "text" && cb.Text != "" {
				cur.text.WriteString(cb.Text)
				out <- provider.StreamEvent{Type: provider.EventTextDelta, Text: cb.Text}
			}
			if cb.Type == "thinking" && cb.Thinking != "" {
				cur.thinking.WriteString(cb.Thinking)
				out <- provider.StreamEvent{Type: provider.EventThinkingDelta, Thinking: cb.Thinking}
			}

		case "content_block_delta":
			if cur == nil {
				continue
			}
			delta := event.Delta
			switch delta.Type {
			case "text_delta":
				cur.text.WriteString(delta.Text)
				out <- provider.StreamEvent{Type: provider.EventTextDelta, Text: delta.Text}
			case "thinking_delta":
				cur.thinking.WriteString(delta.Thinking)
				out <- provider.StreamEvent{Type: provider.EventThinkingDelta, Thinking: delta.Thinking}
			case "signature_delta":
				cur.signature.WriteString(delta.Signature)
			case "input_json_delta":
				cur.toolJSON.WriteString(delta.PartialJSON)
			}

		case "content_block_stop":
			if cur == nil {
				continue
			}
			if cb, ok := cur.contentBlock(); ok {
				if cb.Type == "tool_use" {
					out <- provider.StreamEvent{
						Type:     provider.EventToolUseStart,
						ToolCall: &message.ToolCall{ID: cb.ToolUseID, Name: cb.ToolName, Input: cb.Input},
					}
				}
				blocks = append(blocks, cb)
			}
			cur = nil

		case "message_delta":
			if event.Delta.StopReason != "" {
				stopReason = mapStopReason(string(event.Delta.StopReason))
			}
			u := event.Usage
			deltaUsage = provider.Usage{
				InputTokens:       int(u.InputTokens),
				OutputTokens:      int(u.OutputTokens),
				CacheReadTokens:   int(u.CacheReadInputTokens),
				CacheCreateTokens: int(u.CacheCreationInputTokens),
			}
		}
	}

	if err := stream.Err(); err != nil {
		out <- provider.StreamEvent{Type: provider.EventError, Error: classifyError(err)}
		return
	}

	// 两处用量都是累计值，message_delta 的 input 部分可能为 0：逐字段取大，
	// 整条响应只上报这一次。
	out <- provider.StreamEvent{
		Type:  provider.EventUsage,
		Usage: provider.MergeUsage(&startUsage, &deltaUsage),
	}
	out <- provider.StreamEvent{
		Type:       provider.EventMessageComplete,
		Message:    &message.Message{Role: message.RoleAssistant, Content: blocks},
		StopReason: stopReason,
	}
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
		return nil, classifyError(err)
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

// Capabilities 返回当前模型的能力描述。
func (p *Provider) Capabilities() provider.Capabilities {
	return capabilitiesFor(p.currentModel())
}

// Ensure Provider implements the interface.
var _ provider.Provider = (*Provider)(nil)

// SetModel 动态切换当前使用的模型（实现 provider.ModelSwitcher 接口）。
func (p *Provider) SetModel(modelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.model = modelID
}

func (p *Provider) currentModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model
}

// ── 内部辅助函数 ──────────────────────────────────────────────────

// thinkingHeadroom 是 max_tokens 不足以容纳思考预算时，为正文额外预留的 token 数。
const thinkingHeadroom = 4096

// buildParams 将 provider.Request 转换为 SDK 的 MessageNewParams。
func (p *Provider) buildParams(req *provider.Request) (anthropic.MessageNewParams, error) {
	model := req.Model
	if model == "" {
		model = p.currentModel()
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = int64(capabilitiesFor(model).MaxOutputTokens)
	}

	params := anthropic.MessageNewParams{
		Model:    anthropic.Model(model),
		Messages: convertMessages(req.Messages),
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
	// 显式 ThinkingConfig 优先；否则 ReasoningEffort 档位映射为思考预算
	// （low=8k / medium=16k / high=32k，off=不启用思考）。
	if budget := thinkingBudget(req); budget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: budget},
		}
		// API 要求 max_tokens > budget_tokens，否则直接 400。
		if maxTokens <= budget {
			maxTokens = budget + thinkingHeadroom
		}
	}
	params.MaxTokens = maxTokens

	return params, nil
}

// thinkingBudget 返回本次请求的思考预算，0 表示不启用思考。
func thinkingBudget(req *provider.Request) int64 {
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		return int64(req.Thinking.BudgetTokens)
	}
	switch req.ReasoningEffort {
	case "low":
		return 8192
	case "medium":
		return 16384
	case "high":
		return 32768
	}
	return 0
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
				blocks = append(blocks, toolResultBlock(cb))

			case "thinking":
				// API 校验签名：无签名的块（旧历史、其他 provider 产生）回传必被拒，只能丢弃。
				if cb.Signature != "" {
					blocks = append(blocks, anthropic.NewThinkingBlock(cb.Signature, cb.Thinking))
				}

			case "redacted_thinking":
				if cb.Data != "" {
					blocks = append(blocks, anthropic.NewRedactedThinkingBlock(cb.Data))
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
				Type:      "thinking",
				Thinking:  cb.Thinking,
				Signature: cb.Signature,
			})
		case "redacted_thinking":
			blocks = append(blocks, message.ContentBlock{
				Type: "redacted_thinking",
				Data: cb.Data,
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

// classifyError 把 SDK 返回的错误归类为 provider 约定的错误类型（loop 据此
// 等待重试 / 切换后备模型 / 压缩），无法归类时原样返回。
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		if typed := classifyStatus(apiErr.StatusCode, apiErrorMessage(apiErr), header); typed != nil {
			return typed
		}
		return err
	}

	// 兜底：拿不到结构化错误时按文本识别。SDK 的 HTTP 错误文本形如
	// `POST "url": 429 Too Many Requests {...}`；流中途的 SSE error 事件
	// 只有错误体文本（如 overloaded_error）。
	msg := err.Error()
	if m := sdkStatusPattern.FindStringSubmatch(msg); m != nil {
		status, _ := strconv.Atoi(m[1])
		if typed := classifyStatus(status, msg, nil); typed != nil {
			return typed
		}
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "rate_limit_error"):
		return &provider.RateLimitError{Message: msg}
	case strings.Contains(lower, "overloaded_error"):
		return &provider.OverloadError{Message: msg}
	case strings.Contains(lower, "prompt is too long"):
		return &provider.PromptTooLongError{Message: msg}
	}
	return err
}

var sdkStatusPattern = regexp.MustCompile(`": (\d{3}) `)

// classifyStatus 按 HTTP 状态码映射错误类型，不属于可恢复类别时返回 nil。
func classifyStatus(status int, msg string, header http.Header) error {
	switch {
	case status == http.StatusTooManyRequests:
		// 速率限制：同账号同限额，切后备模型无济于事，只能等待。
		return &provider.RateLimitError{Message: msg, RetryAfter: parseRetryAfter(header)}
	case status == http.StatusRequestEntityTooLarge:
		return &provider.PromptTooLongError{Message: msg}
	case status == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "prompt is too long"):
		return &provider.PromptTooLongError{Message: msg}
	case status >= http.StatusInternalServerError: // 含 529 overloaded
		return &provider.OverloadError{Message: msg}
	}
	return nil
}

// apiErrorMessage 返回 SDK 错误的文本。SDK 的 Error() 不判空 Request/Response，
// 两者缺失时自行拼装，避免 panic。
func apiErrorMessage(e *anthropic.Error) string {
	if e.Request != nil && e.Response != nil {
		return e.Error()
	}
	return fmt.Sprintf("anthropic: HTTP %d %s %s", e.StatusCode, http.StatusText(e.StatusCode), e.RawJSON())
}

// maxRetryAfter 是 Retry-After 的上限，防止异常值溢出 time.Duration。
const maxRetryAfter = 24 * time.Hour

// parseRetryAfter 解析 Retry-After 响应头（秒数或 HTTP 日期），缺失或无效时返回 0。
func parseRetryAfter(header http.Header) time.Duration {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if !(secs > 0) { // 同时排除 NaN
			return 0
		}
		d = time.Duration(min(secs, maxRetryAfter.Seconds()) * float64(time.Second))
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return max(min(d, maxRetryAfter), 0)
}

// defaultCapabilities 是未登记模型的能力描述（ModelID 由调用方填入）。
var defaultCapabilities = provider.Capabilities{
	ContextWindow:   200_000,
	MaxOutputTokens: 64_000,
	SupportsTools:   true,
	SupportsVision:  true,
	SupportsCaching: true,
}

// modelCapabilities 按去掉平台前缀与版本/日期后缀后的模型 ID 登记能力描述。
var modelCapabilities = map[string]provider.Capabilities{
	"claude-opus-4-6": {
		ContextWindow:    200_000,
		MaxOutputTokens:  64_000,
		SupportsTools:    true,
		SupportsVision:   true,
		SupportsThinking: true,
		SupportsCaching:  true,
	},
	"claude-sonnet-4-6": {
		ContextWindow:    200_000,
		MaxOutputTokens:  64_000,
		SupportsTools:    true,
		SupportsVision:   true,
		SupportsThinking: true,
		SupportsCaching:  true,
	},
	"claude-haiku-4-5": {
		ContextWindow:   200_000,
		MaxOutputTokens: 64_000,
		SupportsTools:   true,
		SupportsVision:  true,
		SupportsCaching: true,
	},
}

// capabilitiesFor 容错查找模型能力：精确匹配 → 去掉平台前缀与版本/日期后缀后
// 匹配 → 按家族推断（opus/sonnet 支持 thinking）→ 默认值。ModelID 始终是传入的 ID。
func capabilitiesFor(model string) provider.Capabilities {
	caps, ok := modelCapabilities[model]
	if !ok {
		caps, ok = modelCapabilities[baseModelID(model)]
	}
	if !ok {
		caps = defaultCapabilities
		lower := strings.ToLower(model)
		caps.SupportsThinking = strings.Contains(lower, "opus") || strings.Contains(lower, "sonnet")
	}
	caps.ModelID = model
	return caps
}

// modelIDSuffix 匹配模型 ID 末尾的版本或日期后缀：-v1、-v1:0（Bedrock）、
// -20251001、@20251001（Vertex）。
var modelIDSuffix = regexp.MustCompile(`(-v\d+(:\d+)?|[-@]\d{8})$`)

// baseModelID 归一化模型 ID，例如
// "us.anthropic.claude-sonnet-4-6-20250929-v1:0" → "claude-sonnet-4-6"。
func baseModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(id, "claude-"); i > 0 {
		id = id[i:]
	}
	for {
		trimmed := modelIDSuffix.ReplaceAllString(id, "")
		if trimmed == id {
			return id
		}
		id = trimmed
	}
}

// toolResultBlock 工具结果转 Anthropic 内容块。结果以内联图片标记开头时转成
// image + text 两段（否则模型只会收到一大段 base64 文字，既看不到图又白耗 token）；
// 图片数据损坏时降级为纯文本。
func toolResultBlock(cb message.ContentBlock) anthropic.ContentBlockParamUnion {
	img, ok := message.SplitImageMarker(cb.Text)
	if !ok {
		return anthropic.NewToolResultBlock(cb.ForToolUseID, cb.Text, cb.IsError)
	}
	if !img.Valid {
		return anthropic.NewToolResultBlock(cb.ForToolUseID, "[图片已省略：数据无效]\n"+img.Body, cb.IsError)
	}
	content := []anthropic.ToolResultBlockParamContentUnion{{
		OfImage: &anthropic.ImageBlockParam{Source: anthropic.ImageBlockParamSourceUnion{
			OfBase64: &anthropic.Base64ImageSourceParam{Data: img.Data, MediaType: anthropic.Base64ImageSourceMediaType(img.MIME())},
		}},
	}}
	if img.Body != "" {
		content = append(content, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: img.Body}})
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
		ToolUseID: cb.ForToolUseID,
		Content:   content,
		IsError:   anthropic.Bool(cb.IsError),
	}}
}
