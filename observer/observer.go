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
