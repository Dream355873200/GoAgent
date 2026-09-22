package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// flakyStreamProvider 在 failOn 列出的调用序号（1-based）上流出部分
// 内容后返回 EventError（模拟网络断/SSE 断），其余正常完成。记录每次
// 收到的请求，供断言「锚点消息进入了重开的流」。
type flakyStreamProvider struct {
	mu       sync.Mutex
	calls    int
	failOn   []int
	requests []*provider.Request
	// withTool 为 true 时首次流先发一个 tool_use 再中断（工具锚点场景）。
	withTool bool
	// successToolUntil 成功流在该序号（1-based）之前发 tool_use（推进
	// 轮次用），之后发正文结束。0 = 成功流一律发正文。
	successToolUntil int
}

func (p *flakyStreamProvider) fails(n int) bool {
	for _, f := range p.failOn {
		if f == n {
			return true
		}
	}
	return false
}

func (p *flakyStreamProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "flaky-probe", ContextWindow: 100000, SupportsTools: true}
}

func (p *flakyStreamProvider) record(req *provider.Request) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.requests = append(p.requests, req)
	return p.calls
}

func (p *flakyStreamProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 8)
	n := p.record(req)
	go func() {
		defer close(ch)
		if p.fails(n) {
			if p.withTool {
				ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{
					ID: "call-1", Name: "echo", Input: json.RawMessage(`{"x":1}`),
				}}
			} else {
				ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "partial answer, "}
			}
			ch <- provider.StreamEvent{Type: provider.EventError, Error: errors.New("connection reset")}
			return
		}
		if p.successToolUntil > 0 && n < p.successToolUntil {
			ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{
				ID: "call-" + strings.Repeat("x", n), Name: "echo", Input: json.RawMessage(`{}`),
			}}
			ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopToolUse}
			return
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "resumed"}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

func (p *flakyStreamProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

// collect 跑完整个循环，收集事件。
func collectEvents(t *testing.T, l *Loop, input string) []Event {
	t.Helper()
	var evts []Event
	for ev := range l.Run(context.Background(), input) {
		evts = append(evts, ev)
	}
	return evts
}

func hasEvent(evts []Event, typ int) bool {
	for _, ev := range evts {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// 流中断后部分正文固化为锚点，注入续跑提示重开流，整轮正常完成。
func TestStreamRecovery_PartialTextAnchored(t *testing.T) {
	p := &flakyStreamProvider{failOn: []int{1}}
	l := New(Config{Provider: p, MaxTurns: 5, SessionID: "sess-rec-1"})

	evts := collectEvents(t, l, "hi")
	if hasEvent(evts, EvtError) {
		t.Fatal("一次流中断不应整轮报错")
	}
	if !hasEvent(evts, EvtDone) {
		t.Fatal("恢复后应正常完成")
	}

	msgs := l.FinalMessages()
	var sawPartial, sawMeta bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "partial answer") {
				sawPartial = true
			}
		}
		if m.IsMeta && strings.Contains(strings.Join(blockTexts(m), " "), "interrupted") {
			sawMeta = true
		}
	}
	if !sawPartial {
		t.Fatal("中断前流出的部分正文应固化为锚点消息")
	}
	if !sawMeta {
		t.Fatal("应注入续跑提示（meta user 消息）")
	}

	// 重开的流必须携带锚点：第二次请求里有部分正文 + 续跑提示。
	if len(p.requests) != 2 {
		t.Fatalf("Stream 应被调用 2 次, got %d", len(p.requests))
	}
	var reqSawPartial, reqSawMeta bool
	for _, m := range p.requests[1].Messages {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "partial answer") {
				reqSawPartial = true
			}
		}
		if m.IsMeta {
			reqSawMeta = true
		}
	}
	if !reqSawPartial || !reqSawMeta {
		t.Fatalf("重开的流缺锚点: partial=%v meta=%v", reqSawPartial, reqSawMeta)
	}
}

// 流中断时已派发工具的真实结果保留：锚点 assistant(tool_use) 与
// tool_result 成对进入重开的流。
func TestStreamRecovery_ToolResultPreserved(t *testing.T) {
	p := &flakyStreamProvider{failOn: []int{1}, withTool: true}
	tools := []ToolEntry{{
		Name:       "echo",
		ExecuteFn:  func(_ context.Context, _ json.RawMessage) (string, error) { return "ok", nil },
		Permission: 0, // ReadOnly：无 PreCheck，直接执行
	}}
	l := New(Config{Provider: p, Tools: tools, MaxTurns: 5, SessionID: "sess-rec-2"})

	evts := collectEvents(t, l, "hi")
	if hasEvent(evts, EvtError) {
		t.Fatal("流中断不应整轮报错")
	}
	if !hasEvent(evts, EvtDone) {
		t.Fatal("恢复后应正常完成")
	}
	if !hasEvent(evts, EvtToolDone) {
		t.Fatal("中断前派发的工具结果应上报")
	}

	// 第二次请求里 tool_use 与 tool_result 成对出现。
	var sawToolUse, sawToolResult bool
	for _, m := range p.requests[1].Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if b.ToolName == "echo" {
					sawToolUse = true
				}
			case "tool_result":
				if strings.Contains(b.Text, "ok") {
					sawToolResult = true
				}
			}
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Fatalf("重开的流缺工具锚点: tool_use=%v tool_result=%v", sawToolUse, sawToolResult)
	}
}

// alwaysFailProvider 每次流都先出正文再中断——验证熔断。
type alwaysFailProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *alwaysFailProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "fail-probe", ContextWindow: 100000}
}

func (p *alwaysFailProvider) Stream(_ context.Context, _ *provider.Request) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	ch := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "x"}
		ch <- provider.StreamEvent{Type: provider.EventError, Error: errors.New("connection reset")}
	}()
	return ch, nil
}

func (p *alwaysFailProvider) Complete(_ context.Context, _ *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}

// 连续中断超过上限后熔断：报错退出而不是无限重试。
func TestStreamRecovery_BreakerTrips(t *testing.T) {
	p := &alwaysFailProvider{}
	l := New(Config{Provider: p, MaxTurns: 5, SessionID: "sess-rec-3"})

	var gotError bool
	for ev := range l.Run(context.Background(), "hi") {
		if ev.Type == EvtError {
			gotError = true
		}
		if ev.Type == EvtDone {
			t.Fatal("持续中断不应完成")
		}
	}
	if !gotError {
		t.Fatal("熔断后应报错退出")
	}
	if got := p.calls; got != maxStreamRetries+1 {
		t.Fatalf("熔断前应尝试 %d 次 (初始+重试%d), got %d", maxStreamRetries+1, maxStreamRetries, got)
	}
}

// 成功轮次后熔断计数清零：只对「连续中断」计数——失败 2 次 → 成功 →
// 再失败 2 次 → 成功全程不熔断（若计数不重置，第 5 次中断就会熔断）。
// 成功流发 tool_use 推进轮次，让 run 跨越多次模型调用。
func TestStreamRecovery_CounterResetsAfterSuccess(t *testing.T) {
	p := &flakyStreamProvider{failOn: []int{1, 2, 4, 5}, successToolUntil: 6}
	tools := []ToolEntry{{
		Name:      "echo",
		ExecuteFn: func(_ context.Context, _ json.RawMessage) (string, error) { return "ok", nil },
	}}
	l := New(Config{Provider: p, Tools: tools, MaxTurns: 10, SessionID: "sess-rec-4"})

	evts := collectEvents(t, l, "hi")
	if hasEvent(evts, EvtError) {
		t.Fatal("中断不连续时不应熔断")
	}
	if !hasEvent(evts, EvtDone) {
		t.Fatal("应正常完成")
	}
	if got := len(p.requests); got != 6 {
		t.Fatalf("Stream 应被调用 6 次, got %d", got)
	}
}

func blockTexts(m message.Message) []string {
	var out []string
	for _, b := range m.Content {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}
