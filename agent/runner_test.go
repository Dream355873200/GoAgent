package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// requestRecorder 包装 provider，记录每次请求的模型与输出上限。
type requestRecorder struct {
	provider.Provider
	mu        sync.Mutex
	models    []string
	maxTokens []int
}

func (r *requestRecorder) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	r.mu.Lock()
	r.models = append(r.models, req.Model)
	r.maxTokens = append(r.maxTokens, req.MaxTokens)
	r.mu.Unlock()
	return r.Provider.Stream(ctx, req)
}

func toolCalls(calls ...provider.MockToolCall) provider.MockResponse {
	return provider.MockResponse{ToolCalls: calls}
}

// 子 agent 的工具 panic 被恢复为错误结果：旧实现在裸 goroutine 里执行工具，
// 一次 panic 会带崩整个进程。
func TestRunnerRecoversToolPanic(t *testing.T) {
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		toolCalls(provider.NewMockToolCall("c1", "boom", map[string]any{})),
		{Text: "工具出错了，已收尾"},
	}}
	def := Definition{
		Name: "panicky",
		Tools: []ToolDef{{
			Name:        "boom",
			InputSchema: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				panic("工具内部错误")
			},
		}},
	}
	var results []string
	res, err := NewRunner(prov).OnEvent(func(ev AgentEvent) {
		if ev.Type == "tool_done" {
			results = append(results, ev.Result)
		}
	}).Run(context.Background(), def, "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "工具出错了，已收尾" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}
	if len(results) != 1 || !strings.Contains(results[0], "panic") {
		t.Fatalf("panic 应作为工具错误结果交给模型: %q", results)
	}
}

// Serial 工具不与同一轮的其他调用并行；默认的工具并行执行。
func TestRunnerHonorsSerialTools(t *testing.T) {
	run := func(serial bool) int32 {
		var cur, peak atomic.Int32
		exec := func(ctx context.Context, input json.RawMessage) (string, error) {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			cur.Add(-1)
			return "ok", nil
		}
		prov := &provider.MockProvider{Responses: []provider.MockResponse{
			toolCalls(
				provider.NewMockToolCall("a", "work", map[string]any{}),
				provider.NewMockToolCall("b", "work", map[string]any{}),
				provider.NewMockToolCall("c", "work", map[string]any{}),
			),
			{Text: "done"},
		}}
		def := Definition{Name: "w", Tools: []ToolDef{{
			Name: "work", InputSchema: map[string]any{"type": "object"}, Serial: serial, Execute: exec,
		}}}
		if _, err := NewRunner(prov).Run(context.Background(), def, "go"); err != nil {
			t.Fatal(err)
		}
		return peak.Load()
	}
	if p := run(true); p != 1 {
		t.Fatalf("Serial 工具的最大并发应为 1，实际 %d", p)
	}
	if p := run(false); p < 2 {
		t.Fatalf("默认工具应并行执行，实际最大并发 %d", p)
	}
}

// 达到最大轮次不算失败：返回已有结果（与旧实现一致）。
func TestRunnerMaxTurnsReturnsResult(t *testing.T) {
	var resps []provider.MockResponse
	for i := 0; i < 5; i++ {
		resps = append(resps, toolCalls(provider.NewMockToolCall("c", "noop", map[string]any{})))
	}
	prov := &provider.MockProvider{Responses: resps}
	def := Definition{Name: "loopy", MaxTurns: 2, Tools: []ToolDef{{
		Name: "noop", InputSchema: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) { return "ok", nil },
	}}}
	res, err := NewRunner(prov).Run(context.Background(), def, "go")
	if err != nil {
		t.Fatalf("达到最大轮次不应返回错误: %v", err)
	}
	if res.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", res.TurnCount)
	}
}

// Definition.Model 与 MaxTokens 随每次请求下发。
func TestRunnerPassesModelAndMaxTokens(t *testing.T) {
	rec := &requestRecorder{Provider: provider.NewMockProvider("hi")}
	def := Definition{Name: "m", Model: "small-model", MaxTokens: 777}
	if _, err := NewRunner(rec).Run(context.Background(), def, "go"); err != nil {
		t.Fatal(err)
	}
	if len(rec.models) != 1 || rec.models[0] != "small-model" || rec.maxTokens[0] != 777 {
		t.Fatalf("请求应携带子 agent 的模型与输出上限: models=%v max=%v", rec.models, rec.maxTokens)
	}
}

// 子 agent 能处理只发完整消息、不发增量事件的 provider（工具调用照常执行）。
func TestRunnerNonIncrementalProvider(t *testing.T) {
	prov := &completeOnlyProvider{replies: []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{
			Type: "tool_use", ToolUseID: "c1", ToolName: "echo", Input: json.RawMessage(`{}`),
		}}},
		message.NewAssistantMessage("结束"),
	}}
	var ran atomic.Int32
	def := Definition{Name: "c", Tools: []ToolDef{{
		Name: "echo", InputSchema: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) { ran.Add(1); return "ok", nil },
	}}}
	res, err := NewRunner(prov).Run(context.Background(), def, "go")
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || res.FinalText != "结束" {
		t.Fatalf("工具应执行一次且返回最终文本: ran=%d final=%q", ran.Load(), res.FinalText)
	}
}

type completeOnlyProvider struct {
	mu      sync.Mutex
	replies []message.Message
	n       int
}

func (p *completeOnlyProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "complete-only", SupportsTools: true}
}

func (p *completeOnlyProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.replies[p.n]
	p.n++
	return &provider.Response{Message: m}, nil
}

func (p *completeOnlyProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.StreamEvent{Type: provider.EventMessageComplete, Message: &resp.Message}
	close(ch)
	return ch, nil
}
