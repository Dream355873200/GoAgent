// Package scripted 提供按脚本应答的模型（provider.Provider），用于确定性地测试
// agent 循环：每次模型请求按顺序消费一个 Step——流式正文（可分块）、思考、
// 工具调用、错误、用量——并记录每次请求，供测试断言。
//
//	m := scripted.New(
//	    scripted.Tool("Read", map[string]any{"path": "a.go"}),
//	    scripted.Text("读完了，没问题。").WithUsage(120, 30),
//	)
//	app := goagent.New(goagent.WithProvider(m))
//	... 运行 ...
//	m.AssertDone(t) // 脚本恰好用完
//
// 主 agent 与子 agent 共用一个模型时，用 Route 按请求分流到各自的脚本：
//
//	m := scripted.New(scripted.Tool("Agent_explore", map[string]any{"task": "..."}), scripted.Text("完成"))
//	m.Route(scripted.SystemContains("你是代码探索助手"), scripted.Tool("Grep", ...), scripted.Text("找到 3 处"))
//
// 与 provider.MockProvider 的区别：脚本用完时返回错误（而不是重复最后一条），
// 支持思考、分块、错误、按请求断言与分流。
package scripted

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Step 是一次模型请求的应答（以及可选的请求断言）。
type Step struct {
	// Text 流式输出的正文。
	Text string
	// Thinking 正文之前流式输出的思考。
	Thinking string
	// ToolCalls 本轮发起的工具调用（ID 为空时自动生成 call-<请求序号>-<i>）。
	ToolCalls []ToolCall
	// Err 非空时请求直接失败（例如 &provider.RateLimitError{} 测试重试）。
	Err error
	// StreamErr 非空时在流出正文之后以流错误结束（测试流中断恢复）。
	StreamErr error
	// Usage 本轮上报的用量。
	Usage *provider.Usage
	// Stop 停止原因；为空时有工具调用为 tool_use，否则 end_turn。
	Stop provider.StopReason
	// ChunkSize 正文与思考按多少个字符一块流出（0 = 一块）。
	ChunkSize int
	// Check 对本次请求的断言；返回错误时请求失败，错误信息指出不符之处。
	Check func(*provider.Request) error
}

// ToolCall 是脚本中的一个工具调用。
type ToolCall struct {
	ID    string
	Name  string
	Input any // 序列化为 JSON；json.RawMessage 原样使用
}

// Text 返回输出正文的一步。
func Text(s string) Step { return Step{Text: s} }

// Tool 返回发起单个工具调用的一步。
func Tool(name string, input any) Step {
	return Step{ToolCalls: []ToolCall{{Name: name, Input: input}}}
}

// Tools 返回同一轮发起多个工具调用的一步（模型并行调用）。
func Tools(calls ...ToolCall) Step { return Step{ToolCalls: calls} }

// Fail 返回请求直接失败的一步。
func Fail(err error) Step { return Step{Err: err} }

// WithUsage 设置本步上报的输入、输出 token。
func (s Step) WithUsage(input, output int) Step {
	s.Usage = &provider.Usage{InputTokens: input, OutputTokens: output}
	return s
}

// WithThinking 设置本步在正文之前流出的思考。
func (s Step) WithThinking(thinking string) Step {
	s.Thinking = thinking
	return s
}

// WithChunkSize 设置正文与思考按多少个字符一块流出（测试增量渲染）。
func (s Step) WithChunkSize(n int) Step {
	s.ChunkSize = n
	return s
}

// Expect 设置对本次请求的断言。
func (s Step) Expect(check func(*provider.Request) error) Step {
	s.Check = check
	return s
}

// Matcher 判断一次请求属于哪个脚本（Route）。
type Matcher func(*provider.Request) bool

// SystemContains 匹配系统提示词包含 sub 的请求（区分主 agent 与子 agent）。
func SystemContains(sub string) Matcher {
	return func(r *provider.Request) bool { return strings.Contains(r.SystemPrompt, sub) }
}

// LastUserContains 匹配最后一条用户文本包含 sub 的请求。
func LastUserContains(sub string) Matcher {
	return func(r *provider.Request) bool {
		for i := len(r.Messages) - 1; i >= 0; i-- {
			if r.Messages[i].Role == message.RoleUser {
				if text := message.ExtractText(r.Messages[i]); text != "" {
					return strings.Contains(text, sub)
				}
			}
		}
		return false
	}
}

type route struct {
	match Matcher
	steps []Step
	next  int
}

// Model 是按脚本应答的模型。并发安全。
type Model struct {
	mu       sync.Mutex
	routes   []*route // 按添加顺序匹配，最后一个是默认脚本（匹配一切）
	requests []*provider.Request
	modelID  string
}

// New 创建模型，steps 为默认脚本（没有 Route 匹配时使用）。
func New(steps ...Step) *Model {
	return &Model{routes: []*route{{steps: steps}}, modelID: "scripted"}
}

// Route 添加一个脚本：match 为真的请求消费这个脚本（先于默认脚本，按添加顺序匹配）。
func (m *Model) Route(match Matcher, steps ...Step) *Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	def := m.routes[len(m.routes)-1]
	m.routes = append(m.routes[:len(m.routes)-1], &route{match: match, steps: steps}, def)
	return m
}

// WithModelID 设置 Capabilities 报告的模型 ID（默认 "scripted"）。
func (m *Model) WithModelID(id string) *Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelID = id
	return m
}

// Requests 返回收到的全部请求（按顺序）。
func (m *Model) Requests() []*provider.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*provider.Request(nil), m.requests...)
}

// Remaining 返回所有脚本中尚未消费的步数。
func (m *Model) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.routes {
		n += len(r.steps) - r.next
	}
	return n
}

// AssertDone 断言脚本恰好用完。
func (m *Model) AssertDone(t testing.TB) {
	t.Helper()
	if n := m.Remaining(); n > 0 {
		t.Fatalf("scripted: 还有 %d 步脚本没有被用到（模型请求比预期少）", n)
	}
}

// Capabilities 实现 provider.Provider。
func (m *Model) Capabilities() provider.Capabilities {
	m.mu.Lock()
	defer m.mu.Unlock()
	return provider.Capabilities{SupportsTools: true, SupportsThinking: true, ModelID: m.modelID, ContextWindow: 200_000}
}

// next 取出这次请求对应的一步。
func (m *Model) next(req *provider.Request) (Step, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	n := len(m.requests)
	for _, r := range m.routes {
		if r.match != nil && !r.match(req) {
			continue
		}
		if r.next >= len(r.steps) {
			return Step{}, n, fmt.Errorf("scripted: 脚本已用完（第 %d 次模型请求没有对应的步骤）", n)
		}
		s := r.steps[r.next]
		r.next++
		return s, n, nil
	}
	return Step{}, n, fmt.Errorf("scripted: 第 %d 次模型请求没有匹配的脚本", n)
}

// Complete 实现 provider.Provider（把 Stream 的结果收拢成一条消息）。
func (m *Model) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	ch, err := m.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var resp provider.Response
	for ev := range ch {
		switch ev.Type {
		case provider.EventMessageComplete:
			if ev.Message != nil {
				resp.Message = *ev.Message
			}
			resp.StopReason = ev.StopReason
		case provider.EventUsage:
			if ev.Usage != nil {
				resp.Usage = *ev.Usage
			}
		case provider.EventError:
			return nil, ev.Error
		}
	}
	return &resp, nil
}

// Stream 实现 provider.Provider。
func (m *Model) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	step, n, err := m.next(req)
	if err != nil {
		return nil, err
	}
	if step.Check != nil {
		if err := step.Check(req); err != nil {
			return nil, fmt.Errorf("scripted: 第 %d 次模型请求不符合预期: %w", n, err)
		}
	}
	if step.Err != nil {
		return nil, step.Err
	}
	msg := &message.Message{Role: message.RoleAssistant}
	var calls []*message.ToolCall
	for i, c := range step.ToolCalls {
		input, err := encodeInput(c.Input)
		if err != nil {
			return nil, fmt.Errorf("scripted: 工具 %s 的入参无法序列化: %w", c.Name, err)
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call-%d-%d", n, i)
		}
		calls = append(calls, &message.ToolCall{ID: id, Name: c.Name, Input: input})
	}
	if step.Thinking != "" {
		msg.Content = append(msg.Content, message.ContentBlock{Type: "thinking", Thinking: step.Thinking})
	}
	if step.Text != "" {
		msg.Content = append(msg.Content, message.ContentBlock{Type: "text", Text: step.Text})
	}
	for _, c := range calls {
		msg.Content = append(msg.Content, message.ContentBlock{Type: "tool_use", ToolUseID: c.ID, ToolName: c.Name, Input: c.Input})
	}
	stop := step.Stop
	if stop == "" {
		stop = provider.StopEndTurn
		if len(calls) > 0 {
			stop = provider.StopToolUse
		}
	}

	out := make(chan provider.StreamEvent, 16)
	go func() {
		defer close(out)
		send := func(ev provider.StreamEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, chunk := range chunks(step.Thinking, step.ChunkSize) {
			if !send(provider.StreamEvent{Type: provider.EventThinkingDelta, Thinking: chunk}) {
				return
			}
		}
		for _, chunk := range chunks(step.Text, step.ChunkSize) {
			if !send(provider.StreamEvent{Type: provider.EventTextDelta, Text: chunk}) {
				return
			}
		}
		if step.StreamErr != nil {
			send(provider.StreamEvent{Type: provider.EventError, Error: step.StreamErr})
			return
		}
		for _, c := range calls {
			if !send(provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: c}) {
				return
			}
		}
		if step.Usage != nil {
			u := *step.Usage
			if !send(provider.StreamEvent{Type: provider.EventUsage, Usage: &u}) {
				return
			}
		}
		send(provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: stop, Message: msg})
	}()
	return out, nil
}

func encodeInput(v any) (json.RawMessage, error) {
	switch in := v.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		return in, nil
	case string:
		return json.RawMessage(in), nil
	}
	return json.Marshal(v)
}

// chunks 把 s 按 size 个字符切块（size <= 0 时整体一块；空串没有块）。
func chunks(s string, size int) []string {
	if s == "" {
		return nil
	}
	if size <= 0 {
		return []string{s}
	}
	var out []string
	runes := []rune(s)
	for len(runes) > 0 {
		n := min(size, len(runes))
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

var _ provider.Provider = (*Model)(nil)
