// Package observer 定义统一的可观测性接口。
//
// Agent 循环在关键事件点通过 Observer 接口向外推送事件，
// 开发者可以实现此接口接入 Prometheus、审计日志、外部监控系统等。
//
// Observer 是对 Hooks 系统的补充：
//   - Hooks：预拦截，适合权限控制和流程干预
//   - Observer：后推送，适合监控、计费和审计
//
// 示例：
//
//	func MyObserver() observer.Observer {
//	    return &myObserver{}
//	}
//
//	app := goagent.New(
//	    goagent.WithObservers(MyObserver()),
//	    goagent.WithCostTracking(),
//	    goagent.WithAnalytics(),
//	)
package observer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
)

// EventKind 分类 Observer 的事件类型。
type EventKind string

const (
	// Token 用量
	EventTokenUsage EventKind = "token_usage"

	// 工具执行
	EventToolStart EventKind = "tool_start"
	EventToolDone  EventKind = "tool_done"
	EventToolError EventKind = "tool_error"

	// 权限
	EventPermissionRequest EventKind = "permission_request"
	EventPermissionGranted EventKind = "permission_granted"
	EventPermissionDenied  EventKind = "permission_denied"

	// 上下文压缩
	EventCompaction EventKind = "compaction"

	// 会话生命周期
	EventSessionStart EventKind = "session_start"
	EventSessionEnd   EventKind = "session_end"

	// 错误
	EventError EventKind = "error"

	// LLM 调用（可选接口 LLMObserver）
	EventLLMStart EventKind = "llm_start"
	EventLLMDone  EventKind = "llm_done"
	EventLLMError EventKind = "llm_error"
)

// Observer 接收来自 Agent 循环的事件。
//
// 所有方法同步调用，实现者不应在方法内阻塞。
// 如需异步处理，实现者应在内部启动 goroutine。
//
// 实现者可以将事件转发到任意外部系统：
//   - Prometheus / OpenTelemetry 指标
//   - 结构化日志（审计）
//   - 消息队列（Kafka / RabbitMQ）
//   - 数据库（审计表）
type Observer interface {
	// OnTokenUsage 在 LLM 调用返回 Token 用量信息时调用。
	OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64)

	// OnToolStart 在工具开始执行时调用。
	OnToolStart(ctx context.Context, toolName string, input json.RawMessage)

	// OnToolDone 在工具成功完成时调用。
	OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration)

	// OnToolError 在工具执行出错时调用。
	OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration)

	// OnPermissionRequest 在需要用户审批权限时调用。
	OnPermissionRequest(ctx context.Context, toolName string, input string, permission string)

	// OnPermissionGranted 在权限被授予时调用。
	OnPermissionGranted(ctx context.Context, toolName string, permission string)

	// OnPermissionDenied 在权限被拒绝时调用。
	OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string)

	// OnCompaction 在上下文压缩发生时调用。
	OnCompaction(ctx context.Context, tokensFreed int, reason string)

	// OnSessionStart 在会话开始时调用。
	OnSessionStart(ctx context.Context, sessionID string)

	// OnSessionEnd 在会话结束时调用。
	OnSessionEnd(ctx context.Context, sessionID string, totalTurns int)

	// OnError 在发生错误时调用。
	OnError(ctx context.Context, err error)
}

// NopObserver 是无操作Observer，用于嵌入测试或作为默认值。
type NopObserver struct{}

func (NopObserver) OnTokenUsage(context.Context, string, *provider.Usage, float64)             {}
func (NopObserver) OnToolStart(context.Context, string, json.RawMessage)                       {}
func (NopObserver) OnToolDone(context.Context, string, json.RawMessage, string, time.Duration) {}
func (NopObserver) OnToolError(context.Context, string, json.RawMessage, error, time.Duration) {}
func (NopObserver) OnPermissionRequest(context.Context, string, string, string)                {}
func (NopObserver) OnPermissionGranted(context.Context, string, string)                        {}
func (NopObserver) OnPermissionDenied(context.Context, string, string, string)                 {}
func (NopObserver) OnCompaction(context.Context, int, string)                                  {}
func (NopObserver) OnSessionStart(context.Context, string)                                     {}
func (NopObserver) OnSessionEnd(context.Context, string, int)                                  {}
func (NopObserver) OnError(context.Context, error)                                             {}

// MultiObserver 将事件广播给多个 Observer。
// 线程安全，可并发使用。
type MultiObserver struct {
	observers []Observer
}

// NewMultiObserver 创建一个新的 MultiObserver。
func NewMultiObserver(observers ...Observer) *MultiObserver {
	return &MultiObserver{observers: observers}
}

// Add 注册一个额外的 Observer。
func (m *MultiObserver) Add(o Observer) {
	m.observers = append(m.observers, o)
}

// OnTokenUsage 广播给所有 Observer。
func (m *MultiObserver) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
	for _, o := range m.observers {
		o.OnTokenUsage(ctx, model, usage, costUSD)
	}
}

// OnToolStart 广播给所有 Observer。
func (m *MultiObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	for _, o := range m.observers {
		o.OnToolStart(ctx, toolName, input)
	}
}

// OnToolDone 广播给所有 Observer。
func (m *MultiObserver) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
	for _, o := range m.observers {
		o.OnToolDone(ctx, toolName, input, result, duration)
	}
}

// OnToolError 广播给所有 Observer。
func (m *MultiObserver) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
	for _, o := range m.observers {
		o.OnToolError(ctx, toolName, input, err, duration)
	}
}

// OnPermissionRequest 广播给所有 Observer。
func (m *MultiObserver) OnPermissionRequest(ctx context.Context, toolName string, input string, permission string) {
	for _, o := range m.observers {
		o.OnPermissionRequest(ctx, toolName, input, permission)
	}
}

// OnPermissionGranted 广播给所有 Observer。
func (m *MultiObserver) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
	for _, o := range m.observers {
		o.OnPermissionGranted(ctx, toolName, permission)
	}
}

// OnPermissionDenied 广播给所有 Observer。
func (m *MultiObserver) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
	for _, o := range m.observers {
		o.OnPermissionDenied(ctx, toolName, permission, reason)
	}
}

// OnCompaction 广播给所有 Observer。
func (m *MultiObserver) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
	for _, o := range m.observers {
		o.OnCompaction(ctx, tokensFreed, reason)
	}
}

// OnSessionStart 广播给所有 Observer。
func (m *MultiObserver) OnSessionStart(ctx context.Context, sessionID string) {
	for _, o := range m.observers {
		o.OnSessionStart(ctx, sessionID)
	}
}

// OnSessionEnd 广播给所有 Observer。
func (m *MultiObserver) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {
	for _, o := range m.observers {
		o.OnSessionEnd(ctx, sessionID, totalTurns)
	}
}

// OnError 广播给所有 Observer。
func (m *MultiObserver) OnError(ctx context.Context, err error) {
	for _, o := range m.observers {
		o.OnError(ctx, err)
	}
}

// Observable 是可以被观测的对象。
// loop.Config 中的 Observer 字段接受此接口。
type Observable interface {
	Observer
}

// LLMCallInfo 描述一次 LLM 调用的请求侧信息（OnLLMStart 入参）。
type LLMCallInfo struct {
	// Model 被调用的模型 ID。
	Model string
	// Turn 是当前循环轮次（1 起）。
	Turn int
	// NumMessages 请求携带的消息条数。
	NumMessages int
	// NumTools 请求携带的工具定义数。
	NumTools int
	// MaxTokens 请求的 max_tokens（0 = 未设置，由 API 决定）。
	MaxTokens int
	// InputTokensEst 请求侧 token 估算（上轮 API 实际值或客户端估算）。
	InputTokensEst int
}

// LLMResult 描述一次 LLM 调用的产出侧信息（OnLLMDone 入参）。
type LLMResult struct {
	// Model 实际产出响应的模型 ID（过载切换后备后与请求侧不同）。
	Model string
	// Turn 是当前循环轮次（1 起）。
	Turn int
	// Duration 从 Stream 建立到流消费完毕的耗时。
	Duration time.Duration
	// StopReason 停止原因（end_turn / max_tokens / tool_use…，可能为空）。
	StopReason string
	// OutputText 正文文本总长度（字节）。
	OutputTextLen int
	// ThinkingLen 思考文本总长度（字节）。
	ThinkingLen int
	// NumToolCalls 本轮产生的工具调用数。
	NumToolCalls int
	// Usage 本轮 token 用量（流中 EventUsage 携带，可能为 nil）。
	Usage *provider.Usage
}

// LLMObserver 是 LLM 调用级的可选观察接口。
//
// Observer 主接口保持稳定（现有实现零改动），需要追踪每次 LLM 调用
// （入参/出参/耗时）的宿主额外实现本接口——tracing 视角里模型调用
// 不再是黑洞：OnLLMStart 在 Stream 建立前触发，OnLLMDone 在流消费
// 完毕后触发，OnLLMError 在调用失败时触发。三者在 loop 内由
// ObserverToLLM 适配器统一分发，普通 Observer 无需感知。
type LLMObserver interface {
	// OnLLMStart 在每次 LLM 调用（Stream）发起前调用。
	OnLLMStart(ctx context.Context, info *LLMCallInfo)
	// OnLLMDone 在流消费完毕（本轮响应完整落地）后调用。
	// info 携带请求侧信息，result 携带产出侧信息。
	OnLLMDone(ctx context.Context, info *LLMCallInfo, result *LLMResult)
	// OnLLMError 在 LLM 调用失败（重试耗尽/过载无后备/流中断）时调用。
	OnLLMError(ctx context.Context, info *LLMCallInfo, err error)
}

// ctxToolCallIDKey 是工具调用 span 关联的 context 键（不导出防碰撞）。
type ctxToolCallIDKey struct{}

// WithToolCallID 把工具调用 ID 塞进 ctx。loop 在 OnToolStart 前调用，
// 同一次调用的 OnToolDone/OnToolError 携带同一个 ctx——宿主实现
// Observer 时无需自行配对，直接从 ctx 提取 ID 即可拿到 trace/span 关联。
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxToolCallIDKey{}, id)
}

// ToolCallIDFromContext 提取 WithToolCallID 注入的调用 ID
// （未注入返回空串——非 loop 路径或旧版本）。
func ToolCallIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxToolCallIDKey{}).(string); ok {
		return v
	}
	return ""
}

// SyncObservable 是 Observable 的线程安全版本。
// 用于需要并发安全的场景。
type SyncObservable struct {
	mu  sync.RWMutex
	obs []Observer
}

func NewSyncObservable() *SyncObservable {
	return &SyncObservable{}
}

func (s *SyncObservable) Add(o Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs = append(s.obs, o)
}

func (s *SyncObservable) Observer() Observer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.obs) == 0 {
		return NopObserver{}
	}
	if len(s.obs) == 1 {
		return s.obs[0]
	}
	mo := &MultiObserver{observers: make([]Observer, len(s.obs))}
	copy(mo.observers, s.obs)
	return mo
}
