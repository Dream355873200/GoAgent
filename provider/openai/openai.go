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
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Config 是 OpenAI Provider 的配置。
type Config struct {
	// APIKey 是 API 密钥。留空表示无需鉴权（如本地 Ollama）。
	APIKey string

	// BaseURL 是 API 基础 URL。默认为 "http://localhost:11434/v1"（Ollama）。
	BaseURL string

	// Model 是模型名称，可被 Request.Model 按请求覆盖。默认为 "qwen-rp:latest"。
	Model string

	// ContextWindow 是模型的上下文窗口大小。
	ContextWindow int

	// MaxOutputTokens 是最大输出 token 数。默认为 4096。
	MaxOutputTokens int

	// DisableTools 为 true 时不发送 tools 参数。
	// 用于不支持 function calling 的模型（如 RP 模型、旧模型等）。
	DisableTools bool

	// DisableStreamUsage 为 true 时流式请求不携带 stream_options.include_usage。
	// 官方 OpenAI 流式只有带上它才返回用量；个别不认识该字段而报错的后端可关闭。
	DisableStreamUsage bool

	// ExtraBody 原样合并进每个 chat/completions 请求体（浅合并，ExtraBody 优先）。
	// 逃生舱：各后端的私有参数（如某些网关的思考开关）无需改库即可透传。
	ExtraBody map[string]any

	// HTTPClient 自定义 HTTP 客户端（代理、追踪、测试中的 httprr 录制回放）；
	// 为空时使用内置客户端（限 2 分钟等待响应头）。不要设置 http.Client.Timeout：
	// 它覆盖整个流式响应，长回复会被截断。
	HTTPClient *http.Client
}

// Provider 实现 OpenAI 兼容的 provider.Provider 接口。
type Provider struct {
	mu     sync.RWMutex
	config Config
	client *http.Client
	// imgFormats 记录每个 端点|模型 实测可用的图片编码（datauri/base64）。
	// 首次图片 4xx 自动探测切换并记忆——对任意中转自愈，无需逐个适配。
	imgFormats map[string]string
	// toolImgOff 记录 端点|模型 是否拒绝工具结果内嵌图片（如 AWS Bedrock 通道
	// 只认 image/text 等块，不认 OpenAI 的 image_url）。命中后工具图片降级为
	// 文本占位重试并记忆，同通道后续请求直接降级。
	toolImgOff map[string]bool
	// thinkingKnobOff 记录 端点|模型 是否拒绝思考档位参数（reasoning_effort /
	// enable_thinking 校验错）。命中后停发全部思考参数。
	thinkingKnobOff map[string]bool
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
	// 不设 http.Client.Timeout（它覆盖整个流式响应，长回复会被误杀）；
	// 只限响应头等待，流式读取的空闲由 consumeStream 的看门狗兜底。
	client := cfg.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		client = &http.Client{Transport: transport}
	}
	return &Provider{
		config:     cfg,
		client:     client,
		imgFormats: map[string]string{},
		toolImgOff: map[string]bool{},
		thinkingKnobOff: map[string]bool{},
	}
}

// 响应头/流式空闲超时：慢思考模型（opus 等首字节可能数分钟）容易撞坏短超时，
// 默认放宽到 5 分钟；可经环境变量覆盖（FLAI_RESPONSE_HEADER_TIMEOUT /
// FLAI_STREAM_IDLE_TIMEOUT，支持 Go duration 如 "8m" 或纯秒数 "300"）。
var (
	// responseHeaderTimeout 请求发出后等待响应头的上限（网关排队/上游首字节慢）。
	responseHeaderTimeout = envDuration("FLAI_RESPONSE_HEADER_TIMEOUT", 5*time.Minute)
	// streamIdleTimeout 流式响应连续无数据的上限：上游连接不断但不再
	// 吐字节时，run 会永远卡在读流上，只能靠用户中断。
	streamIdleTimeout = envDuration("FLAI_STREAM_IDLE_TIMEOUT", 5*time.Minute)
)

// envDuration 读环境变量时长：非法/未设置回落默认值
func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// ---------- OpenAI API 请求/响应类型 ----------

// chatRequest 是 OpenAI Chat Completions API 的请求体。
type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	Tools           []chatTool     `json:"tools,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"` // 仅流式请求可带，非流式带上会被官方 API 拒绝
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"` // 思考强度（按模型家族翻译，见 thinking.go）
	EnableThinking  *bool          `json:"enable_thinking,omitempty"`  // 开关型模型（GLM/Qwen）的思考开关

	// imgFormat 是组装本请求图片时使用的编码记忆值，供图片 4xx 自适应判断。
	imgFormat string
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 是 OpenAI 格式的消息。
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatContentPart 多模态 content 分段（OpenAI content-parts 格式）。
type chatContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"` // data URI 或 http(s) 地址
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
	// OpenAI 标准缓存计量（prompt cache 命中的输入 token）；多数中转/模型
	// 不返回该字段，缺省时缓存命中率为 0、UI 不展示缓存行。
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// cacheRead 提取缓存命中的输入 token 数（无字段时为 0）。
func (u *chatUsage) cacheRead() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// toUsage 归一到 provider.Usage 口径：prompt_tokens 含缓存命中部分，InputTokens 需扣除。
func (u *chatUsage) toUsage() provider.Usage {
	cached := u.cacheRead()
	return provider.Usage{
		InputTokens:     max(u.PromptTokens-cached, 0),
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: cached,
	}
}

// streamChunk 是 SSE 流中的一个数据块。
type streamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object,omitempty"`
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
	// ReasoningContent 是推理型模型（DeepSeek-R1/V4、Qwen3 等）的思考增量，
	// 经 delta.reasoning_content 下发、与 content 并行。缺失则被静默丢弃，
	// 表现为「模型整段思考、正文为空」。
	ReasoningContent *string `json:"reasoning_content,omitempty"`
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

// applyExtraBody 把 Config.ExtraBody 浅合并进已序列化的请求体（ExtraBody 优先）。
func (p *Provider) applyExtraBody(jsonBody []byte) ([]byte, error) {
	p.mu.RLock()
	extra := p.config.ExtraBody
	p.mu.RUnlock()
	if len(extra) == 0 {
		return jsonBody, nil
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBody, &m); err != nil {
		return jsonBody, err
	}
	for k, v := range extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// marshalChat 序列化请求体（含 ExtraBody 合并）。
func (p *Provider) marshalChat(body chatRequest) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	if jsonBody, err = p.applyExtraBody(jsonBody); err != nil {
		return nil, fmt.Errorf("合并 ExtraBody 失败: %w", err)
	}
	return jsonBody, nil
}

// doChat 发送请求体并做状态码检查。
func (p *Provider) doChat(ctx context.Context, jsonBody []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.BaseURL+"/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		dumpFailedRequest(jsonBody)
		return nil, p.handleHTTPError(resp)
	}
	return resp, nil
}

// sendChat 发送对话请求；若因图片编码被 4xx 拒绝（各家中转对 data URI /
// 裸 base64 支持不一），自动切换编码重试一次并按 端点|模型 记忆——一次
// 探测，之后所有请求直接用对的格式，无需逐家中转适配。若因通道不支持
// 工具结果内嵌图片被拒（Bedrock 校验 image_url 块），降级为文本占位重试
// 一次并记忆（见 maybeDisableToolImages）。
func (p *Provider) sendChat(ctx context.Context, req *provider.Request, stream bool) (*http.Response, error) {
	body := p.buildChatRequest(req, stream)
	jsonBody, err := p.marshalChat(body)
	if err != nil {
		return nil, err
	}
	resp, err := p.doChat(ctx, jsonBody)
	if err != nil && requestHasImage(req.Messages) {
		if p.maybeSwitchImageFormat(body.Model, body.imgFormat, err) {
			body = p.buildChatRequest(req, stream)
			if jsonBody, err = p.marshalChat(body); err == nil {
				resp, err = p.doChat(ctx, jsonBody)
			}
		} else if p.maybeDisableToolImages(body.Model, err) {
			body = p.buildChatRequest(req, stream)
			if jsonBody, err = p.marshalChat(body); err == nil {
				resp, err = p.doChat(ctx, jsonBody)
			}
		}
	}
	if err != nil && req.ReasoningEffort != "" && p.maybeDisableThinkingKnob(body.Model, err) {
		// 思考档位参数被端点拒绝：记忆并停发参数重试一次
		body = p.buildChatRequest(req, stream)
		if jsonBody, err = p.marshalChat(body); err == nil {
			resp, err = p.doChat(ctx, jsonBody)
		}
	}
	return resp, err
}

// dumpFailedRequest 调试辅助：FLAI_DEBUG_REQUEST=1 时，把被端点拒绝的请求
// 体完整落盘到临时目录，便于定位是哪个内容触发的 4xx。请求体含完整对话，
// 只能属主可读：CreateTemp 以 0600 创建，且文件名唯一、并发失败不会互相覆盖。
func dumpFailedRequest(jsonBody []byte) {
	if os.Getenv("FLAI_DEBUG_REQUEST") != "1" {
		return
	}
	f, err := os.CreateTemp("", "ycode-failed-request-"+time.Now().Format("20060102-150405")+"-*.json")
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(jsonBody)
}

// Stream 实现流式 API 调用。
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	resp, err := p.sendChat(ctx, req, true)
	if err != nil {
		return nil, err
	}

	// 启动 SSE 流消费 goroutine。
	out := make(chan provider.StreamEvent, 64)
	go p.consumeStream(ctx, resp, out)
	return out, nil
}

// Complete 实现非流式 API 调用。
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	resp, err := p.sendChat(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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
		usage = chatResp.Usage.toUsage()
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
	streamUsage := !p.config.DisableStreamUsage
	p.mu.RUnlock()
	if req.Model != "" {
		model = req.Model
	}

	imgFormat := p.imageFormat(model)
	cr := chatRequest{
		Model:     model,
		Messages:  p.toOpenAIMessages(imgFormat, !p.toolImagesOff(model), req.SystemPrompt, req.Messages),
		Stream:    stream,
		imgFormat: imgFormat,
	}
	if stream && streamUsage {
		cr.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.MaxTokens > 0 {
		cr.MaxTokens = req.MaxTokens
	}

	// 思考强度：按模型家族翻译（ThinkingLevels 声明档位，resolveThinking
	// 出参数）；端点曾拒绝档位参数的（thinkingKnobOff）直接停发。
	if !p.thinkingKnobDisabled(model) {
		cr.ReasoningEffort, cr.EnableThinking = resolveThinking(model, req.ReasoningEffort)
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

	switch code := resp.StatusCode; {
	case code == http.StatusRequestEntityTooLarge:
		return &provider.PromptTooLongError{
			Message: fmt.Sprintf("请求过大 (HTTP 413): %s", string(body)),
		}
	case code == http.StatusTooManyRequests:
		// 429 = 速率限制（TPM/RPM 配额），等待后重试必然可行。
		// 与 529 过载区分：OverloadError 会触发后备模型切换，
		// 而速率限制切模型无济于事（同一账号同限额）——必须等待。
		return &provider.RateLimitError{
			Message:    fmt.Sprintf("速率限制 (HTTP 429): %s", string(body)),
			RetryAfter: parseRetryAfter(resp.Header),
		}
	case code == 529:
		return &provider.OverloadError{
			Message: fmt.Sprintf("服务过载 (HTTP 529): %s", string(body)),
		}
	case code >= 500:
		return &provider.OverloadError{
			Message: fmt.Sprintf("服务器错误 (HTTP %d): %s", code, string(body)),
		}
	case code >= 400 && isContextOverflow(body):
		// 各家超窗多返回 400（个别 422），只能按错误体识别。
		return &provider.PromptTooLongError{
			Message: fmt.Sprintf("上下文超限 (HTTP %d): %s", code, string(body)),
		}
	}
	return fmt.Errorf("API 错误 (HTTP %d): %s", resp.StatusCode, string(body))
}

// contextOverflowMarkers 是各后端超出上下文窗口时错误体中的特征文本（小写）。
var contextOverflowMarkers = []string{
	"context_length_exceeded", // OpenAI / Azure
	"maximum context length",  // OpenAI、vLLM、DeepSeek、OpenRouter、Mistral
	"prompt is too long",      // Anthropic 兼容端点
	"too many tokens",
	"context window",
	"exceed_context_size",                  // llama.cpp server
	"exceeds the maximum number of tokens", // Gemini 兼容端点
	"exceeded model token limit",           // Moonshot
}

func isContextOverflow(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, m := range contextOverflowMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
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
	// 思考伪标签分流：模型偶尔把手写思考（<think>/<thinking>/<thought>）
	// 混进 content 通道，这里改道 thinking，避免污染正文与历史。
	thinkFilter := &thinkStreamFilter{}
	// finish 块与随后的纯 usage 块都可能携带用量（后者可能晚于 finish_reason
	// 到达）：只保留最后一次（累计终值），流结束时上报一次。
	var usage *chatUsage
	finished := false

	// 空闲看门狗：超时关闭响应体，阻塞中的 Scan 随即返回错误。
	var idleFired atomic.Bool
	idle := time.AfterFunc(streamIdleTimeout, func() {
		idleFired.Store(true)
		resp.Body.Close()
	})
	defer idle.Stop()

	scanner := bufio.NewScanner(resp.Body)
	// 增大缓冲区以处理大型响应。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		idle.Reset(streamIdleTimeout)
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

		// 跳过 MiniMax 等在流结束后发送的非 chunk 总结块（object="chat.completion" 而非 "chat.completion.chunk"）。
		if chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
			continue
		}

		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		// 处理文本增量。先过伪标签分流器：混进 content 的手写思考改道
		// thinking 通道，其余照常累积与转发。
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			text, think := thinkFilter.Feed(*choice.Delta.Content)
			if think != "" {
				out <- provider.StreamEvent{
					Type:     provider.EventThinkingDelta,
					Thinking: think,
				}
			}
			if text != "" {
				assistantText.WriteString(text)
				out <- provider.StreamEvent{
					Type: provider.EventTextDelta,
					Text: text,
				}
			}
		}

		// 处理推理增量（DeepSeek-R1/V4 等经 delta.reasoning_content 下发）。
		// 只作展示流转发（loop 层会累积为 thinking block），不拼进 assistantText。
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			out <- provider.StreamEvent{
				Type:     provider.EventThinkingDelta,
				Thinking: *choice.Delta.ReasoningContent,
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

		// 处理结束原因。个别后端会重复下发 finish_reason，只认第一次，
		// 否则工具调用会被重复派发。
		if finished || choice.FinishReason == nil || *choice.FinishReason == "" {
			continue
		}
		finished = true
		finishReason := *choice.FinishReason

		// 按 index 升序：工具串行执行的先后次序必须与模型给出的一致。
		var calls []message.ToolCall
		for _, idx := range slices.Sorted(maps.Keys(toolStates)) {
			state := toolStates[idx]
			if state.name == "" {
				continue
			}
			args := state.arguments.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, message.ToolCall{ID: state.id, Name: state.name, Input: json.RawMessage(args)})
		}

		// 在流结束前，发射已完成的工具调用。
		if finishReason == "tool_calls" || finishReason == "stop" {
			for i := range calls {
				out <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &calls[i]}
			}
		}

		// 吐出分流器悬挂缓冲（跨 chunk 截断的标签残余），再组装完整消息。
		if tailText, tailThink := thinkFilter.Flush(); tailText != "" || tailThink != "" {
			if tailThink != "" {
				out <- provider.StreamEvent{
					Type:     provider.EventThinkingDelta,
					Thinking: tailThink,
				}
			}
			if tailText != "" {
				assistantText.WriteString(tailText)
				out <- provider.StreamEvent{
					Type: provider.EventTextDelta,
					Text: tailText,
				}
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
		for _, c := range calls {
			fullMsg.Content = append(fullMsg.Content, message.ContentBlock{
				Type:      "tool_use",
				ToolUseID: c.ID,
				ToolName:  c.Name,
				Input:     c.Input,
			})
		}

		// 发送完成事件。
		out <- provider.StreamEvent{
			Type:       provider.EventMessageComplete,
			Message:    &fullMsg,
			StopReason: p.fromFinishReason(finishReason),
		}
	}

	if usage != nil {
		u := usage.toUsage()
		out <- provider.StreamEvent{Type: provider.EventUsage, Usage: &u}
	}

	if err := scanner.Err(); err != nil {
		// context 取消是正常的用户主动中断，不作为错误报告。
		if ctx.Err() != nil {
			return
		}
		if idleFired.Load() {
			err = fmt.Errorf("上游 %s 无数据，已断开", streamIdleTimeout)
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

// imageURLFor 按记忆的编码组装图片。默认 OpenAI 标准 data URI；
// FLAI_IMAGE_URL_FORMAT=base64 可强制。中转兼容差异由 maybeSwitchImageFormat
// 在首个图片 4xx 时自动探测解决，不逐家适配。
func imageURLFor(format, media, b64 string) string {
	if format == "base64" || strings.EqualFold(os.Getenv("FLAI_IMAGE_URL_FORMAT"), "base64") {
		return b64
	}
	return "data:image/" + media + ";base64," + b64
}

// imgFormatKey 缓存键：端点 + 模型（同一中转不同模型要求可能不同）。
// BaseURL 构造后不再变化，无需加锁。
func (p *Provider) imgFormatKey(model string) string {
	return p.config.BaseURL + "|" + model
}

// imageFormat 返回 端点|模型 记忆的图片编码（"" 表示默认 data URI）。
func (p *Provider) imageFormat(model string) string {
	key := p.imgFormatKey(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.imgFormats[key]
}

// maybeSwitchImageFormat 图片相关 4xx 时切换编码并记忆，返回 true 表示
// 值得用另一种格式重试一次。used 是失败请求组装时读到的编码。
func (p *Provider) maybeSwitchImageFormat(model, used string, err error) bool {
	if err == nil {
		return false
	}
	var (
		ptl  *provider.PromptTooLongError
		rate *provider.RateLimitError
		over *provider.OverloadError
	)
	if errors.As(err, &ptl) || errors.As(err, &rate) || errors.As(err, &over) {
		return false // 已识别的错误与图片编码无关，错误体里出现 image 字样也不应误切
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "image_url") {
		return false // 结构性拒绝：通道不认 image_url 块，换编码没用（交给 maybeDisableToolImages）
	}
	hit := strings.Contains(msg, "base64") || strings.Contains(msg, "image") ||
		strings.Contains(msg, "图片") || strings.Contains(msg, "格式") || strings.Contains(msg, "解析")
	if !hit {
		return false
	}
	key := p.imgFormatKey(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.imgFormats == nil { // 防御：任何构造路径漏掉初始化也不崩
		p.imgFormats = map[string]string{}
	}
	// 仅当记忆值仍是失败请求所用的编码时才切换：并发失败的请求只切一次，
	// 不会把编码来回翻转；已被其他请求切换过的直接用新值重试。
	if p.imgFormats[key] == used {
		next := "base64"
		if used == "base64" {
			next = "datauri"
		}
		p.imgFormats[key] = next
	}
	return true
}

// maybeDisableToolImages 通道拒绝工具结果内嵌图片时降级重试。识别不变量：
// image_url 块只可能是本框架随工具结果发出的——任何端点/中转的 400 引用它，
// 都说明该通道不收这种块（Bedrock 校验、Anthropic 原生校验、中转透传校验，
// 措辞各异但块名一致），按 端点|模型 记忆并降级为文本占位重试一次。
// 与图片编码切换互斥：编码切换处理「格式不对」，这里处理「根本不收」。
func (p *Provider) maybeDisableToolImages(model string, err error) bool {
	if err == nil {
		return false
	}
	if !strings.Contains(strings.ToLower(err.Error()), "image_url") {
		return false
	}
	key := p.imgFormatKey(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.toolImgOff == nil { // 防御：任何构造路径漏掉初始化也不崩
		p.toolImgOff = map[string]bool{}
	}
	p.toolImgOff[key] = true
	return true
}

// toolImagesOff 该 端点|模型 是否已知不支持工具结果内嵌图片（命中后组装期
// 直接降级，不再发 image_url 块去撞 400）。
func (p *Provider) toolImagesOff(model string) bool {
	key := p.imgFormatKey(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.toolImgOff[key]
}

// requestHasImage 请求中是否存在待发送的内联图片。
func requestHasImage(msgs []message.Message) bool {
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && strings.HasPrefix(block.Text, "[IMAGE ") {
				return true
			}
		}
	}
	return false
}

// toOpenAIMessages 将框架消息转换为 OpenAI API 消息格式；imgFormat 是图片编码
// 记忆值（见 imageFormat），toolImgs=false 时工具结果内嵌图片降级为文本占位
// （部分通道如 Bedrock 不认 tool_result 里的 image_url 块，见 maybeDisableToolImages）。
func (p *Provider) toOpenAIMessages(imgFormat string, toolImgs bool, systemPrompt string, msgs []message.Message) []chatMessage {
	var result []chatMessage

	// 系统提示作为第一条消息。
	if systemPrompt != "" {
		result = append(result, chatMessage{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	// tool_calls 配对修复：OpenAI 协议要求带 tool_calls 的 assistant 消息
	// 后面必须紧跟每个 id 的 tool 结果。压缩/截断层可能弄丢配对的 tool
	// 消息（导致 400 insufficient tool messages），这里在发送前校验配对，
	// 缺失的自动补一条占位 tool 消息。
	pendingIDs := []string{}
	pendingSet := map[string]bool{}
	flushPendingToolResults := func() {
		if len(pendingSet) == 0 {
			pendingIDs = nil
			return
		}
		for _, id := range pendingIDs {
			if id == "" || !pendingSet[id] {
				continue
			}
			result = append(result, chatMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    "[工具结果已缺失：历史压缩时被移除；如需该信息请重新调用该工具]",
			})
		}
		pendingIDs = nil
		pendingSet = map[string]bool{}
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

		if !hasToolResult {
			flushPendingToolResults()
		}

		if hasToolResult {
			// 每个 tool_result 都要作为独立的 tool 角色消息。
			for _, block := range msg.Content {
				if block.Type == "tool_result" {
					content := block.Text
					if block.IsError {
						content = "Error: " + content
					}
					// 多模态图片通道：工具结果以 [IMAGE png <base64>] 开头时，
					// 转成 OpenAI 多模态 content 数组（text + image_url data URI）。
					// 供视觉类工具（截图裁决）把图片送进支持视觉的模型。
					// 防线：base64 必须可解码（压缩截断/损坏的标记直接降级为
					// 文本），坏图发给任何端点都会 400 并卡死整轮。
					if img, ok := message.SplitImageMarker(content); ok {
						if !img.Valid {
							// 标记里的图片数据损坏（截断/压缩损伤）→ 降级为纯文本
							result = append(result, chatMessage{
								Role:       "tool",
								Content:    "[图片已省略：数据无效]\n" + img.Body,
								ToolCallID: block.ForToolUseID,
							})
							continue
						}
						text := img.Body
						if text == "" {
							text = "[截图已附上]"
						}
						if !toolImgs {
							// 该 端点|模型 实测不支持工具结果内嵌图片（Bedrock 通道
							// 只认 image/text 等块）：降级为文本占位，指回原图来源
							result = append(result, chatMessage{
								Role:       "tool",
								Content:    "[图片已省略：当前模型通道不支持工具结果内嵌图片；如需查看画面请重新调用产生该图的工具]\n" + text,
								ToolCallID: block.ForToolUseID,
							})
							continue
						}
						result = append(result, chatMessage{
							Role: "tool",
							Content: []chatContentPart{
								{Type: "text", Text: text},
								{Type: "image_url", ImageURL: &chatImageURL{URL: imageURLFor(imgFormat, img.Media, img.Data)}},
							},
							ToolCallID: block.ForToolUseID,
						})
						continue
					}
					result = append(result, chatMessage{
						Role:       "tool",
						Content:    content,
						ToolCallID: block.ForToolUseID,
					})
				}
			}
			for _, block := range msg.Content {
				if block.Type == "tool_result" {
					delete(pendingSet, block.ForToolUseID)
				}
			}
			continue
		}

		openaiMsg := chatMessage{
			Role: string(msg.Role),
		}

		// 思考不回传历史：OpenAI 兼容 API 无思考块，此前用 <thinking> 标签
		// 包裹回填，实测会教模型模仿该格式把手写思考混进 content（泄漏进
		// 正文），且智谱/DeepSeek 官方均建议 reasoning 不进下一轮上下文。
		// 正文里的伪标签块（历史会话已落盘的泄漏残留）就地清洗。
		text := stripThinkTags(message.ExtractText(msg))
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

		for _, tc := range toolCalls {
			if tc.ID != "" {
				pendingIDs = append(pendingIDs, tc.ID)
				pendingSet[tc.ID] = true
			}
		}

		result = append(result, openaiMsg)
	}

	// 收尾：仍有未应答的 tool_calls（历史末尾被压缩截断）→ 补占位
	flushPendingToolResults()
	return result
}
