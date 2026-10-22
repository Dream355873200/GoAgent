package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/budget"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// scriptProvider 按调用序号执行脚本，事件逐个异步到达（可插入延迟），
// 并记录每次请求的快照。
type scriptProvider struct {
	mu       sync.Mutex
	calls    int
	requests []provider.Request
	caps     provider.Capabilities
	script   func(ctx context.Context, n int, ch chan<- provider.StreamEvent)
}

func (p *scriptProvider) Capabilities() provider.Capabilities {
	if p.caps.ModelID != "" {
		return p.caps
	}
	return provider.Capabilities{ModelID: "script", ContextWindow: 100000, SupportsTools: true}
}

func (p *scriptProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	snap := *req
	snap.Messages = append([]message.Message(nil), req.Messages...)
	p.requests = append(p.requests, snap)
	p.mu.Unlock()
	ch := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(ch)
		p.script(ctx, n, ch)
	}()
	return ch, nil
}

func (p *scriptProvider) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

func (p *scriptProvider) request(i int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[i]
}

func toolUse(id, name string) provider.StreamEvent {
	return provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{ID: id, Name: name, Input: json.RawMessage(`{}`)}}
}

func done(stop provider.StopReason) provider.StreamEvent {
	return provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: stop}
}

func text(s string) provider.StreamEvent {
	return provider.StreamEvent{Type: provider.EventTextDelta, Text: s}
}

func resultFor(msgs []message.Message, id string) (string, bool) {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ForToolUseID == id {
				return b.Text, true
			}
		}
	}
	return "", false
}

func drain(l *Loop, ctx context.Context, input string) []Event {
	var evs []Event
	for ev := range l.Run(ctx, input) {
		evs = append(evs, ev)
	}
	return evs
}

func readTool(name string, fn func(context.Context, json.RawMessage) (string, error)) ToolEntry {
	return ToolEntry{Name: name, Class: 3, ExecuteFn: fn} // executor.ClassReadOnly
}

// 工具在模型流还没结束时就完成（被流内 Poll 取走），其结果必须进入下一轮请求。
func TestLoopFix_ResultPolledDuringStreamKept(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("t1", "fast")
			time.Sleep(60 * time.Millisecond)
			ch <- toolUse("t2", "fast")
			time.Sleep(60 * time.Millisecond)
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- text("ok")
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p, Tools: []ToolEntry{readTool("fast", func(context.Context, json.RawMessage) (string, error) {
		return "FAST", nil
	})}})
	drain(l, context.Background(), "go")
	second := p.request(1).Messages
	for _, id := range []string{"t1", "t2"} {
		if r, ok := resultFor(second, id); !ok || r != "FAST" {
			t.Fatalf("%s 的真实结果应进入第二轮请求，得到 %q %v", id, r, ok)
		}
	}
	if err := message.ValidatePairing(l.FinalMessages()); err != nil {
		t.Fatalf("历史配对非法: %v", err)
	}
}

// stop_reason=max_tokens 时注入续写提示重试，而不是把截断输出当完成。
func TestLoopFix_MaxTokensRecovery(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- text("partial")
			ch <- done(provider.StopMaxTokens)
			return
		}
		ch <- text(" rest")
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p})
	drain(l, context.Background(), "go")
	if p.calls != 2 {
		t.Fatalf("截断后应续写重试一次，模型调用 %d 次", p.calls)
	}
	last := p.request(1).Messages
	if m := last[len(last)-1]; !m.IsMeta || !strings.Contains(message.ExtractText(m), "Output token limit") {
		t.Fatalf("续写请求应以续写提示结尾: %+v", m)
	}
}

// MaxResultSizeChars 截断作用于写入历史的结果，且截断点在 UTF-8 边界。
func TestLoopFix_MaxResultSizeChars(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("b1", "big")
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- done(provider.StopEndTurn)
	}}
	big := ToolEntry{Name: "big", MaxResultSizeChars: 100, ExecuteFn: func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("汉", 5000), nil
	}}
	l := New(Config{Provider: p, Tools: []ToolEntry{big}})
	drain(l, context.Background(), "go")
	r, _ := resultFor(p.request(1).Messages, "b1")
	if len(r) > 300 || !strings.Contains(r, "结果被截断") {
		t.Fatalf("结果应被截断，实际长度 %d", len(r))
	}
	if !utf8Valid(r) {
		t.Fatal("截断产生了非法 UTF-8")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

// 流中途用户取消：已执行（有副作用）的工具及其真实结果要记入历史。
func TestLoopFix_CancelMidStreamRecordsExecutedTool(t *testing.T) {
	var executed atomic.Bool
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		ch <- text("doing it")
		ch <- toolUse("w1", "write")
		<-ctx.Done()
		ch <- provider.StreamEvent{Type: provider.EventError, Error: ctx.Err()}
	}}
	l := New(Config{Provider: p, Tools: []ToolEntry{readTool("write", func(context.Context, json.RawMessage) (string, error) {
		executed.Store(true)
		return "file written", nil
	})}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(80 * time.Millisecond); cancel() }()
	evs := drain(l, ctx, "go")
	if !executed.Load() {
		t.Fatal("前置条件：工具应已执行")
	}
	if r, ok := resultFor(l.FinalMessages(), "w1"); !ok || r != "file written" {
		t.Fatalf("已执行工具的真实结果应记入历史: %q %v", r, ok)
	}
	if err := message.ValidatePairing(l.FinalMessages()); err != nil {
		t.Fatalf("中断后历史配对非法: %v", err)
	}
	if evs[len(evs)-1].Type != EvtInterrupted {
		t.Fatalf("最后一个事件应为 EvtInterrupted，得到 %d", evs[len(evs)-1].Type)
	}
}

type overloaded struct{}

func (overloaded) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "primary", ContextWindow: 100000, SupportsTools: true, SupportsCaching: true}
}
func (overloaded) Stream(context.Context, *provider.Request) (<-chan provider.StreamEvent, error) {
	return nil, &provider.OverloadError{Message: "overloaded"}
}
func (overloaded) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, &provider.OverloadError{Message: "overloaded"}
}

// 主模型过载切到后备模型后，只读 SystemPrompt 的实现也能拿到系统提示词。
func TestLoopFix_FallbackKeepsSystemPrompt(t *testing.T) {
	fb := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		ch <- text("hi")
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: overloaded{}, Fallback: fb, SystemPrompt: "BE CAREFUL"})
	drain(l, context.Background(), "q")
	if got := fb.request(0).SystemPrompt; got != "BE CAREFUL" {
		t.Fatalf("后备请求的 SystemPrompt=%q", got)
	}
}

// 未配置 Executor / MaxTurns 时使用默认值，而不是空指针 panic 或立即报错。
func TestLoopFix_NilExecutorDefaults(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("x", "t")
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p, Tools: []ToolEntry{readTool("t", func(context.Context, json.RawMessage) (string, error) { return "ok", nil })}})
	for _, ev := range drain(l, context.Background(), "go") {
		if ev.Type == EvtError {
			t.Fatalf("不应报错: %v", ev.Error)
		}
	}
}

// 同一响应的多次用量上报合并为一次（逐字段取最大值），上下文占用含缓存。
func TestLoopFix_UsageMergedOncePerRequest(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		ch <- provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100, CacheReadTokens: 5000}}
		ch <- text("hi")
		ch <- provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 5000}}
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p})
	var usages []*provider.Usage
	for _, ev := range drain(l, context.Background(), "go") {
		if ev.Type == EvtUsageUpdate {
			usages = append(usages, ev.Usage)
		}
	}
	if len(usages) != 1 {
		t.Fatalf("应只有一次用量事件，得到 %d", len(usages))
	}
	if u := usages[0]; u.InputTokens != 100 || u.OutputTokens != 40 || u.CacheReadTokens != 5000 {
		t.Fatalf("合并结果不对: %+v", *u)
	}
}

// 未设置预算时，连续的短输出工具轮不会被"收益递减"静默终止。
func TestLoopFix_NoBudgetNoDiminishingStop(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n <= 5 {
			ch <- provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 1}}
			ch <- toolUse("c"+string(rune('0'+n)), "t")
			ch <- provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 20}}
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- text("finished")
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{
		Provider: p,
		Budget:   budget.NewTracker(budget.Config{}),
		Tools:    []ToolEntry{readTool("t", func(context.Context, json.RawMessage) (string, error) { return "ok", nil })},
	})
	drain(l, context.Background(), "go")
	if p.calls != 6 {
		t.Fatalf("应跑完 5 个工具轮后正常结束，模型只被调用了 %d 次", p.calls)
	}
}

// provider 在完成事件里给出的 thinking 块（带签名）原样进入历史。
func TestLoopFix_ThinkingSignaturePreserved(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		ch <- provider.StreamEvent{Type: provider.EventThinkingDelta, Thinking: "hmm"}
		ch <- text("answer")
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn, Message: &message.Message{
			Role: message.RoleAssistant,
			Content: []message.ContentBlock{
				{Type: "thinking", Thinking: "hmm", Signature: "sig-123"},
				{Type: "redacted_thinking", Data: "opaque"},
				{Type: "text", Text: "answer"},
			},
		}}
	}}
	l := New(Config{Provider: p})
	drain(l, context.Background(), "go")
	msgs := l.FinalMessages()
	am := msgs[len(msgs)-1]
	if am.Content[0].Signature != "sig-123" || am.Content[1].Type != "redacted_thinking" {
		t.Fatalf("thinking 签名/redacted_thinking 应保留: %+v", am.Content)
	}
}

// 历史里的孤儿 tool_result（压缩或损坏留下的）在发送前被修复。
func TestLoopFix_RequestSanitized(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p, InitialMessages: []message.Message{
		message.NewUserMessage("[摘要]"),
		message.NewToolResultMessage("gone", "孤儿结果", false),
		message.NewAssistantMessage("好的"),
	}})
	drain(l, context.Background(), "继续")
	if err := message.ValidatePairing(p.request(0).Messages); err != nil {
		t.Fatalf("发送的请求仍然非法: %v", err)
	}
}

// 新进入对话的消息都有 UID（持久化据此去重），历史消息保持原样。
func TestLoopFix_UIDsAssigned(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("u1", "t")
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- text("done")
		ch <- done(provider.StopEndTurn)
	}}
	hist := []message.Message{message.NewUserMessage("old"), message.NewAssistantMessage("old reply")}
	l := New(Config{Provider: p, InitialMessages: hist, Tools: []ToolEntry{readTool("t", func(context.Context, json.RawMessage) (string, error) { return "ok", nil })}})
	drain(l, context.Background(), "new")
	msgs := l.FinalMessages()
	if msgs[0].UID != "" || msgs[1].UID != "" {
		t.Fatal("历史消息不应被改写")
	}
	seen := map[string]bool{}
	for _, m := range msgs[2:] {
		if m.UID == "" || seen[m.UID] {
			t.Fatalf("新消息应有唯一 UID: %+v", m)
		}
		seen[m.UID] = true
	}
}

// 工具 panic 转为错误结果，循环继续而不是带走进程。
func TestLoopFix_ToolPanicRecovered(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("p1", "boom")
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- text("recovered")
		ch <- done(provider.StopEndTurn)
	}}
	l := New(Config{Provider: p, Tools: []ToolEntry{readTool("boom", func(context.Context, json.RawMessage) (string, error) { panic("kaboom") })}})
	drain(l, context.Background(), "go")
	if r, ok := resultFor(l.FinalMessages(), "p1"); !ok || !strings.Contains(r, "panic") {
		t.Fatalf("panic 应转为错误结果: %q", r)
	}
}
