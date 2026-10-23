// Package oteltrace 把 GoAgent 的观测事件导出为 OpenTelemetry span，名称与属性遵循
// OpenTelemetry 生成式 AI 语义约定（gen_ai.*）：
//
//	invoke_agent <agent>        一次运行（会话的一条输入）
//	├── chat <model>            一次模型请求：用量、结束原因、请求与响应模型
//	└── execute_tool <tool>     一次工具调用：失败时 span 状态为 Error
//	    ├── chat <model>        子代理的模型请求与工具调用挂在派发它的 Agent_* 调用下
//	    └── execute_tool <tool>
//
// span 经 ctx 传递（observer.ContextObserver）：工具内部经 ctx 发起的调用——
// otelhttp 包装的 HTTP 客户端、带追踪的数据库驱动——自动成为 execute_tool 的
// 子 span。权限决定、压缩、自定义事件记为所在 span 的事件。宿主自带
// TracerProvider（导出到 Jaeger、OTLP 等）；运行 ctx 里已有 span 时（例如 HTTP
// 中间件建立的请求 span），invoke_agent 挂在它下面。
//
//	app := goagent.New(..., goagent.WithObservers(oteltrace.New(tp, oteltrace.Options{ProviderName: "anthropic"})))
//
// 默认不记录提示词与回复内容（语义约定把内容采集定为可选项）。
package oteltrace

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// 语义约定中的属性名（gen_ai.*）与本包的扩展属性（goagent.*）。
const (
	AttrOperationName       = "gen_ai.operation.name"
	AttrProviderName        = "gen_ai.provider.name"
	AttrAgentName           = "gen_ai.agent.name"
	AttrConversationID      = "gen_ai.conversation.id"
	AttrRequestModel        = "gen_ai.request.model"
	AttrRequestMaxTokens    = "gen_ai.request.max_tokens"
	AttrResponseModel       = "gen_ai.response.model"
	AttrFinishReasons       = "gen_ai.response.finish_reasons"
	AttrInputTokens         = "gen_ai.usage.input_tokens"
	AttrOutputTokens        = "gen_ai.usage.output_tokens"
	AttrCacheReadTokens     = "gen_ai.usage.cache_read.input_tokens"
	AttrCacheCreationTokens = "gen_ai.usage.cache_creation.input_tokens"
	AttrToolName            = "gen_ai.tool.name"
	AttrToolCallID          = "gen_ai.tool.call.id"
	AttrToolType            = "gen_ai.tool.type"
	AttrErrorType           = "error.type"

	AttrTurn       = "goagent.turn"
	AttrTurns      = "goagent.turns"
	AttrCostUSD    = "goagent.cost_usd"
	AttrAgentPath  = "goagent.agent_path"
	AttrDurationMs = "goagent.duration_ms"
)

// Options 配置导出。
type Options struct {
	// AgentName 是 invoke_agent span 的 gen_ai.agent.name（默认 "goagent"）。
	AgentName string
	// ProviderName 是 gen_ai.provider.name（如 "anthropic"、"openai"；空则不设置）。
	ProviderName string
	// MaxEventValueBytes 自定义事件的值记入 span 事件时的截断长度（默认 1024）。
	MaxEventValueBytes int
}

// Observer 实现 observer.Observer、observer.LLMObserver、observer.ContextObserver
// 与 observer.CustomEventObserver。并发安全。
type Observer struct {
	tracer trace.Tracer
	opts   Options

	mu    sync.Mutex
	runs  map[string]*runSpan                  // 运行 ID → invoke_agent
	tools map[string]trace.Span                // toolKey → execute_tool
	llm   map[*observer.LLMCallInfo]trace.Span // 进行中的 chat
}

type runSpan struct {
	span trace.Span
	cost float64
}

// New 创建导出器。tp 为 nil 时使用全局 TracerProvider（otel.GetTracerProvider）。
func New(tp trace.TracerProvider, opts ...Options) *Observer {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.AgentName == "" {
		o.AgentName = "goagent"
	}
	if o.MaxEventValueBytes <= 0 {
		o.MaxEventValueBytes = 1024
	}
	return &Observer{
		tracer: tp.Tracer("github.com/Dream355873200/GoAgent"),
		opts:   o,
		runs:   map[string]*runSpan{},
		tools:  map[string]trace.Span{},
		llm:    map[*observer.LLMCallInfo]trace.Span{},
	}
}

func (o *Observer) common(attrs ...attribute.KeyValue) []attribute.KeyValue {
	if o.opts.ProviderName != "" {
		attrs = append(attrs, attribute.String(AttrProviderName, o.opts.ProviderName))
	}
	return attrs
}

// toolKey 标识一次工具调用：运行、所在循环（ctx 当前的 span——主循环是
// invoke_agent，子代理是派发它的 execute_tool）与调用 ID。OnToolStart、
// ToolContext、OnToolDone 拿到的 ctx 都来自同一个循环，算出同一个键；子代理
// 与主循环的调用 ID 即使相同也不会混淆。
func toolKey(ctx context.Context) string {
	return observer.RunIDFromContext(ctx) + "/" +
		trace.SpanContextFromContext(ctx).SpanID().String() + "/" +
		observer.ToolCallIDFromContext(ctx)
}

// OnSessionStart 开始 invoke_agent span（RunContext 把它放进运行的 ctx）。
func (o *Observer) OnSessionStart(ctx context.Context, sessionID string) {
	attrs := o.common(
		attribute.String(AttrOperationName, "invoke_agent"),
		attribute.String(AttrAgentName, o.opts.AgentName))
	if sessionID != "" && sessionID != "ephemeral" {
		attrs = append(attrs, attribute.String(AttrConversationID, sessionID))
	}
	_, span := o.tracer.Start(ctx, "invoke_agent "+o.opts.AgentName,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	o.mu.Lock()
	o.runs[observer.RunIDFromContext(ctx)] = &runSpan{span: span}
	o.mu.Unlock()
}

// RunContext 把 invoke_agent span 放进运行此后使用的 ctx。
func (o *Observer) RunContext(ctx context.Context) context.Context {
	o.mu.Lock()
	run := o.runs[observer.RunIDFromContext(ctx)]
	o.mu.Unlock()
	if run == nil {
		return ctx
	}
	return trace.ContextWithSpan(ctx, run.span)
}

// ToolContext 把 execute_tool span 放进工具执行使用的 ctx。
func (o *Observer) ToolContext(ctx context.Context) context.Context {
	o.mu.Lock()
	span := o.tools[toolKey(ctx)]
	o.mu.Unlock()
	if span == nil {
		return ctx
	}
	return trace.ContextWithSpan(ctx, span)
}

// OnSessionEnd 结束 invoke_agent span。
func (o *Observer) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {
	runID := observer.RunIDFromContext(ctx)
	o.mu.Lock()
	run := o.runs[runID]
	delete(o.runs, runID)
	o.mu.Unlock()
	if run == nil {
		return
	}
	run.span.SetAttributes(attribute.Int(AttrTurns, totalTurns))
	if run.cost > 0 {
		run.span.SetAttributes(attribute.Float64(AttrCostUSD, run.cost))
	}
	run.span.End()
}

// OnError 把循环级错误记到所在 span（用户中断记为事件，不算失败）。
func (o *Observer) OnError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if errors.Is(err, context.Canceled) {
		span.AddEvent("goagent.cancelled")
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.String(AttrErrorType, errorType(err)))
}

// OnLLMStart 开始 chat span。
func (o *Observer) OnLLMStart(ctx context.Context, info *observer.LLMCallInfo) {
	attrs := o.common(
		attribute.String(AttrOperationName, "chat"),
		attribute.String(AttrRequestModel, info.Model),
		attribute.Int(AttrTurn, info.Turn))
	if info.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrRequestMaxTokens, info.MaxTokens))
	}
	if path := goagent.AgentPathFromContext(ctx); path != "" {
		attrs = append(attrs, attribute.String(AttrAgentPath, path))
	}
	_, span := o.tracer.Start(ctx, "chat "+info.Model,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	o.mu.Lock()
	o.llm[info] = span
	o.mu.Unlock()
}

func (o *Observer) takeLLM(info *observer.LLMCallInfo) trace.Span {
	o.mu.Lock()
	defer o.mu.Unlock()
	span := o.llm[info]
	delete(o.llm, info)
	return span
}

// OnLLMDone 记下用量与结束原因并结束 chat span。
func (o *Observer) OnLLMDone(ctx context.Context, info *observer.LLMCallInfo, result *observer.LLMResult) {
	span := o.takeLLM(info)
	if span == nil {
		return
	}
	if result != nil {
		span.SetAttributes(attribute.String(AttrResponseModel, result.Model))
		if result.StopReason != "" {
			span.SetAttributes(attribute.StringSlice(AttrFinishReasons, []string{result.StopReason}))
		}
		if u := result.Usage; u != nil {
			// gen_ai.usage.input_tokens 按约定包含缓存命中与写入的部分。
			span.SetAttributes(
				attribute.Int(AttrInputTokens, u.InputTokens+u.CacheReadTokens+u.CacheCreateTokens),
				attribute.Int(AttrOutputTokens, u.OutputTokens))
			if u.CacheReadTokens > 0 {
				span.SetAttributes(attribute.Int(AttrCacheReadTokens, u.CacheReadTokens))
			}
			if u.CacheCreateTokens > 0 {
				span.SetAttributes(attribute.Int(AttrCacheCreationTokens, u.CacheCreateTokens))
			}
		}
	}
	span.End()
}

// OnLLMError 把失败记到 chat span 并结束它。
func (o *Observer) OnLLMError(ctx context.Context, info *observer.LLMCallInfo, err error) {
	span := o.takeLLM(info)
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String(AttrErrorType, errorType(err)))
	}
	span.End()
}

// OnToolStart 开始 execute_tool span。
func (o *Observer) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	attrs := o.common(
		attribute.String(AttrOperationName, "execute_tool"),
		attribute.String(AttrToolName, toolName),
		attribute.String(AttrToolType, "function"))
	if id := observer.ToolCallIDFromContext(ctx); id != "" {
		attrs = append(attrs, attribute.String(AttrToolCallID, id))
	}
	if path := goagent.AgentPathFromContext(ctx); path != "" {
		attrs = append(attrs, attribute.String(AttrAgentPath, path))
	}
	_, span := o.tracer.Start(ctx, "execute_tool "+toolName,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	o.mu.Lock()
	o.tools[toolKey(ctx)] = span
	o.mu.Unlock()
}

func (o *Observer) takeTool(ctx context.Context) trace.Span {
	key := toolKey(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	span := o.tools[key]
	delete(o.tools, key)
	return span
}

// OnToolDone 结束 execute_tool span。
func (o *Observer) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
	if span := o.takeTool(ctx); span != nil {
		span.SetAttributes(attribute.Int64(AttrDurationMs, duration.Milliseconds()))
		span.End()
	}
}

// OnToolError 把失败记到 execute_tool span 并结束它。
func (o *Observer) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
	span := o.takeTool(ctx)
	if span == nil {
		return
	}
	msg := "tool error"
	if err != nil {
		msg = err.Error()
		span.RecordError(err)
	}
	span.SetStatus(codes.Error, msg)
	span.SetAttributes(attribute.String(AttrErrorType, "tool_error"), attribute.Int64(AttrDurationMs, duration.Milliseconds()))
	span.End()
}

// OnPermissionRequest 记为工具 span 的事件。
func (o *Observer) OnPermissionRequest(ctx context.Context, toolName string, input string, permission string) {
	trace.SpanFromContext(ctx).AddEvent("goagent.permission.request", trace.WithAttributes(
		attribute.String(AttrToolName, toolName), attribute.String("goagent.permission", permission)))
}

// OnPermissionGranted 记为工具 span 的事件。
func (o *Observer) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
	trace.SpanFromContext(ctx).AddEvent("goagent.permission.granted", trace.WithAttributes(
		attribute.String(AttrToolName, toolName), attribute.String("goagent.permission", permission)))
}

// OnPermissionDenied 记为工具 span 的事件。
func (o *Observer) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
	trace.SpanFromContext(ctx).AddEvent("goagent.permission.denied", trace.WithAttributes(
		attribute.String(AttrToolName, toolName), attribute.String("goagent.permission", permission),
		attribute.String("goagent.reason", reason)))
}

// OnCompaction 记为所在 span 的事件。
func (o *Observer) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
	trace.SpanFromContext(ctx).AddEvent("goagent.compaction", trace.WithAttributes(
		attribute.Int("goagent.tokens_freed", tokensFreed), attribute.String("goagent.reason", reason)))
}

// OnTokenUsage 累计运行的估算成本（运行结束时写入 goagent.cost_usd）。
func (o *Observer) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if run := o.runs[observer.RunIDFromContext(ctx)]; run != nil {
		run.cost += costUSD
	}
}

// OnCustomEvent 记为所在 span 的事件（值按 MaxEventValueBytes 截断）。
func (o *Observer) OnCustomEvent(ctx context.Context, name string, value json.RawMessage) {
	v := string(value)
	if len(v) > o.opts.MaxEventValueBytes {
		v = v[:o.opts.MaxEventValueBytes] + "…"
	}
	trace.SpanFromContext(ctx).AddEvent("goagent.custom_event", trace.WithAttributes(
		attribute.String("goagent.event.name", name), attribute.String("goagent.event.value", v)))
}

// errorType 给错误归类（error.type 属性）。
func errorType(err error) string {
	var rate *provider.RateLimitError
	var overload *provider.OverloadError
	var tooLong *provider.PromptTooLongError
	switch {
	case errors.As(err, &rate):
		return "rate_limit"
	case errors.As(err, &overload):
		return "overloaded"
	case errors.As(err, &tooLong):
		return "prompt_too_long"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}

var (
	_ observer.Observer            = (*Observer)(nil)
	_ observer.LLMObserver         = (*Observer)(nil)
	_ observer.ContextObserver     = (*Observer)(nil)
	_ observer.CustomEventObserver = (*Observer)(nil)
)
