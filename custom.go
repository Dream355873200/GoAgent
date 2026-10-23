// custom.go 自定义事件。
//
// 宿主经同一条事件流下发领域信号（画布指令、设备状态、节点进度……），不必
// 塞进 progress 文本或 ask_user 载荷：
//
//	// 工具、中间件、hook 中（随所在运行的事件流下发）：
//	ctx.Emit("animecreater.canvas_command", CanvasCommand{Action: "add_node"})
//
//	// 宿主从外部向会话推送：
//	app.EmitEvent(sessionID, "amc.device_changed", DeviceState{...})
//
// 名称形如 <命名空间>.<事件名>，点号分隔；goagent. 前缀保留给框架。值是任意
// 可 JSON 序列化的值，序列化在发射点完成，失败返回错误。自定义事件是展示层
// 信号，不进入会话记录；会话事件日志照常保存（可续传）。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/schema"
)

// ErrNotInRun 表示 ctx 不来自任何运行（不在工具、中间件、hook 的执行中），
// 自定义事件无处下发。
var ErrNotInRun = errors.New("goagent: ctx 不属于任何运行，自定义事件无处下发")

// ErrSessionNotActive 表示会话既没有进行中的运行，也没有启用会话事件日志，
// EmitEvent 无处下发。
var ErrSessionNotActive = errors.New("goagent: 会话没有进行中的运行且未启用会话事件日志")

// reservedEventPrefix 是框架保留的自定义事件命名空间。
const reservedEventPrefix = "goagent."

var eventNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+$`)

// checkEventName 校验宿主使用的事件名：<命名空间>.<事件名>，且不占用保留前缀。
func checkEventName(name string) error {
	if !eventNamePattern.MatchString(name) {
		return fmt.Errorf("goagent: 自定义事件名 %q 不合法：应为 <命名空间>.<事件名>，只含字母、数字、_、-，以点号分隔", name)
	}
	if strings.HasPrefix(name, reservedEventPrefix) {
		return fmt.Errorf("goagent: 自定义事件名 %q 使用了框架保留的 goagent. 前缀", name)
	}
	return nil
}

// EventInfo 是一个已注册的自定义事件（GET /protocol 的 custom_events 列出）。
type EventInfo struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema,omitempty"`
}

// eventRegistry 是已注册的自定义事件 schema（RegisterEvent）。
type eventRegistry struct {
	mu     sync.RWMutex
	events map[string]map[string]any
}

func (r *eventRegistry) validate(name string, value json.RawMessage) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	s := r.events[name]
	r.mu.RUnlock()
	if s == nil {
		return nil
	}
	if err := schema.Validate(s, value); err != nil {
		return fmt.Errorf("goagent: 自定义事件 %q 的值不符合注册的 schema: %w", name, err)
	}
	return nil
}

type eventRegistryKey struct{}

func withEventRegistry(ctx context.Context, r *eventRegistry) context.Context {
	return context.WithValue(ctx, eventRegistryKey{}, r)
}

func eventRegistryFrom(ctx context.Context) *eventRegistry {
	r, _ := ctx.Value(eventRegistryKey{}).(*eventRegistry)
	return r
}

// RegisterEvent 为自定义事件登记 schema：此后发射该事件时按它校验值（不符合
// 时 Emit / EmitEvent 返回错误），GET /protocol 在 custom_events 中列出它。
// s 可以是 JSON Schema（map[string]any 或 json.RawMessage），也可以是一个 Go
// 值——按其类型反射生成 schema（与 ToolDef.Input 相同）。不登记的事件不校验。
func (a *App) RegisterEvent(name string, s any) error {
	if err := checkEventName(name); err != nil {
		return err
	}
	var m map[string]any
	switch v := s.(type) {
	case nil:
	case map[string]any:
		m = v
	case json.RawMessage:
		if err := json.Unmarshal(v, &m); err != nil {
			return fmt.Errorf("goagent: 自定义事件 %q 的 schema 不是 JSON 对象: %w", name, err)
		}
	default:
		m = schema.Generate(v)
	}
	a.events.mu.Lock()
	defer a.events.mu.Unlock()
	if a.events.events == nil {
		a.events.events = map[string]map[string]any{}
	}
	a.events.events[name] = m
	return nil
}

// RegisteredEvents 返回已注册的自定义事件（按名称排序）。
func (a *App) RegisteredEvents() []EventInfo {
	a.events.mu.RLock()
	defer a.events.mu.RUnlock()
	out := make([]EventInfo, 0, len(a.events.events))
	for name, s := range a.events.events {
		out = append(out, EventInfo{Name: name, Schema: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Emit 在运行之中发射自定义事件：ctx 来自工具、中间件或 hook 的执行时，事件
// 随所在运行的事件流下发（会话运行同时进入会话事件日志）；子 agent 的工具里
// 发射的事件同样到达最外层的流。ctx 不属于任何运行时返回 ErrNotInRun。
func Emit(ctx context.Context, name string, value any) error {
	if err := checkEventName(name); err != nil {
		return err
	}
	return emitInRun(ctx, name, value)
}

// Emit 是 goagent.Emit 的方法形式：工具里直接 ctx.Emit(name, value)。
func (c Context) Emit(name string, value any) error {
	return Emit(c.Context, name, value)
}

// emitInRun 是 Emit 去掉名称校验的形态（框架用 goagent. 前缀的事件）。
func emitInRun(ctx context.Context, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("goagent: 自定义事件 %q 的值无法序列化: %w", name, err)
	}
	if err := eventRegistryFrom(ctx).validate(name, raw); err != nil {
		return err
	}
	if !loop.EmitFromTool(ctx, loop.Event{Type: loop.EvtCustom, CustomName: name, CustomValue: raw}) {
		return ErrNotInRun
	}
	if s := loop.ScopeFrom(ctx); s != nil {
		notifyCustomEvent(ctx, s.Observer, name, raw)
	}
	return nil
}

// EmitEvent 从运行之外向会话推送自定义事件：会话有进行中的运行时随该运行的
// 事件流下发（SDK 消费方与 HTTP 客户端都收到）；没有时写入会话事件日志（HTTP
// 模式或 WithSessionStream），挂在该会话上的连接收到。两者都没有时返回
// ErrSessionNotActive。
func (a *App) EmitEvent(sessionID, name string, value any) error {
	if err := checkEventName(name); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("goagent: 自定义事件 %q 的值无法序列化: %w", name, err)
	}
	if err := a.events.validate(name, raw); err != nil {
		return err
	}
	ev := Event{Type: EventCustom, CustomName: name, CustomValue: raw}
	if !a.runSinks.emit(sessionID, ev) && !a.publishSessionEvent(sessionID, ev) {
		return ErrSessionNotActive
	}
	notifyCustomEvent(WithSessionContext(context.Background(), sessionID, ""), a.obsRegistry.Observer(), name, raw)
	return nil
}

func notifyCustomEvent(ctx context.Context, o observer.Observer, name string, value json.RawMessage) {
	if co, ok := o.(observer.CustomEventObserver); ok {
		co.OnCustomEvent(ctx, name, value)
	}
}

// runSinks 登记各会话进行中的运行的事件出口，EmitEvent 从运行之外向其中
// 注入事件。运行结束（出口注销）后的注入不会发生。
type runSinks struct {
	mu    sync.Mutex
	seq   uint64
	sinks map[string]map[uint64]*runSink
}

// runSink 是一次运行的事件出口：注入与运行结束可能并发，关闭后的注入
// 返回 false，不向已不再被消费的通道发送。
type runSink struct {
	mu     sync.RWMutex
	out    chan<- Event
	done   chan struct{}
	closed bool
}

// register 登记一次运行的出口，返回注销函数（运行结束前调用）。
func (r *runSinks) register(sessionID string, out chan<- Event) func() {
	s := &runSink{out: out, done: make(chan struct{})}
	r.mu.Lock()
	if r.sinks == nil {
		r.sinks = map[string]map[uint64]*runSink{}
	}
	r.seq++
	id := r.seq
	if r.sinks[sessionID] == nil {
		r.sinks[sessionID] = map[uint64]*runSink{}
	}
	r.sinks[sessionID][id] = s
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if m := r.sinks[sessionID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(r.sinks, sessionID)
			}
		}
		r.mu.Unlock()
		close(s.done)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}
}

// emit 把事件注入会话中最早登记的进行中运行；没有时返回 false。
func (r *runSinks) emit(sessionID string, ev Event) bool {
	r.mu.Lock()
	var target *runSink
	var first uint64
	for id, s := range r.sinks[sessionID] {
		if target == nil || id < first {
			target, first = s, id
		}
	}
	r.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.RLock()
	defer target.mu.RUnlock()
	if target.closed {
		return false
	}
	select {
	case target.out <- ev:
		return true
	case <-target.done:
		return false
	}
}
