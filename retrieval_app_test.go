// retrieval_app_test.go RAG 双形态接线测试（根包）。
// 形态 1：WithRetrieval 前置注入（检索→拼输入→EventRetrieval 事件）。
// 形态 2：NewRetrieveTool 注册为工具，agent 运行时可自主调用。
package goagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/retrieval"
)

// captureProvider 记录收到的最后一条用户消息——验证注入后的 prompt。
type captureProvider struct {
	mu       sync.Mutex
	lastUser string
}

func (p *captureProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (p *captureProvider) Complete(_ context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{Message: message.Message{Role: message.RoleAssistant,
		Content: []message.ContentBlock{{Type: "text", Text: "ok"}}}}, nil
}

func (p *captureProvider) Stream(_ context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == message.RoleUser {
			for _, b := range req.Messages[i].Content {
				if b.Type == "text" && b.Text != "" {
					p.lastUser = b.Text
				}
			}
			break
		}
	}
	p.mu.Unlock()

	ch := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "ok"}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn,
			Message: &message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "ok"}}}}
	}()
	return ch, nil
}

func (p *captureProvider) capturedUser() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastUser
}

func TestWithRetrievalInjectsContext(t *testing.T) {
	p := &captureProvider{}
	ret := retrieval.NewMemoryRetriever(
		retrieval.Document{Content: "退款政策：7 天无理由退款", Metadata: map[string]any{"source": "faq.md"}},
	)
	app := New(
		WithProvider(p),
		WithMaxTurns(1),
		WithRetrieval(RetrievalConfig{Retriever: ret, TopK: 3}),
	)

	var gotRetrievalEvent bool
	for ev := range app.Run(context.Background(), "refund policy 退款") {
		if ev.Type == EventRetrieval {
			gotRetrievalEvent = true
			if !strings.Contains(ev.Text, "docs=1") {
				t.Errorf("EventRetrieval 摘要应含命中数: %q", ev.Text)
			}
		}
		if ev.Type == EventError {
			t.Fatalf("意外错误: %v", ev.Error)
		}
	}

	if !gotRetrievalEvent {
		t.Errorf("应发送 EventRetrieval 事件")
	}
	seen := p.capturedUser()
	if !strings.Contains(seen, "<retrieved-context>") || !strings.Contains(seen, "7 天无理由退款") {
		t.Errorf("用户输入前应拼接检索块:\n%s", seen)
	}
	if !strings.Contains(seen, "refund policy 退款") {
		t.Errorf("原始用户输入应保留:\n%s", seen)
	}
	if !strings.Contains(seen, `source="faq.md"`) {
		t.Errorf("引用块应带来源（引用审计）:\n%s", seen)
	}
}

// ShouldRetrieve 谓词短路：闲聊不检索（省成本），也不发事件。
func TestWithRetrievalPredicateSkips(t *testing.T) {
	p := &captureProvider{}
	ret := &countingRetriever{inner: retrieval.NewMemoryRetriever(
		retrieval.Document{Content: "退款政策"})}
	app := New(
		WithProvider(p), WithMaxTurns(1),
		WithRetrieval(RetrievalConfig{
			Retriever: ret,
			ShouldRetrieve: func(_ context.Context, q string) bool {
				return !strings.Contains(q, "你好")
			},
		}),
	)

	var gotEvent bool
	for ev := range app.Run(context.Background(), "你好呀") {
		if ev.Type == EventRetrieval {
			gotEvent = true
		}
	}
	if gotEvent {
		t.Errorf("谓词短路后不应发 EventRetrieval")
	}
	if ret.calls != 0 {
		t.Errorf("谓词短路后不应调检索器, calls=%d", ret.calls)
	}
	if strings.Contains(p.capturedUser(), "<retrieved-context>") {
		t.Errorf("闲聊输入不应被注入检索块:\n%s", p.capturedUser())
	}
}

// 零命中：不注入空块（不污染对话），也不报错。
func TestWithRetrievalNoHitKeepsInputClean(t *testing.T) {
	p := &captureProvider{}
	ret := retrieval.NewMemoryRetriever(retrieval.Document{Content: "go 语言文档"})
	app := New(
		WithProvider(p), WithMaxTurns(1),
		WithRetrieval(RetrievalConfig{Retriever: ret}),
	)
	for ev := range app.Run(context.Background(), "今天天气怎么样") {
		if ev.Type == EventError {
			t.Fatalf("零命中不应报错: %v", ev.Error)
		}
	}
	if strings.Contains(p.capturedUser(), "<retrieved-context>") {
		t.Errorf("零命中不应注入空块:\n%s", p.capturedUser())
	}
}

// 检索失败 fail-closed：发 EventError 干净返回（向量库挂了不无据作答）。
func TestWithRetrievalFailureFailsClosed(t *testing.T) {
	p := &captureProvider{}
	app := New(
		WithProvider(p), WithMaxTurns(1),
		WithRetrieval(RetrievalConfig{Retriever: brokenRetriever{}}),
	)
	var gotErr bool
	for ev := range app.Run(context.Background(), "任何问题") {
		if ev.Type == EventError {
			gotErr = true
		}
	}
	if !gotErr {
		t.Errorf("检索失败应 fail-closed 发 EventError")
	}
}

// MaxChars 截断：保序（分数高的先进）且截断注记在场。
func TestWithRetrievalMaxCharsTruncates(t *testing.T) {
	p := &captureProvider{}
	ret := retrieval.NewMemoryRetriever(
		retrieval.Document{Content: strings.Repeat("高分内容", 50)}, // 命中多得分高
	)
	app := New(
		WithProvider(p), WithMaxTurns(1),
		WithRetrieval(RetrievalConfig{Retriever: ret, TopK: 2, MaxChars: 100}),
	)
	for ev := range app.Run(context.Background(), "内容") {
		if ev.Type == EventError {
			t.Fatalf("截断不应报错: %v", ev.Error)
		}
	}
	seen := p.capturedUser()
	if !strings.Contains(seen, "高分内容") {
		t.Errorf("截断后高分的至少部分内容应保留:\n%.300s", seen)
	}
	if !strings.Contains(seen, "已截断") {
		t.Errorf("截断应注明:\n%.300s", seen)
	}
	// 保序：高分内容必须出现在原始输入之前（注入块在前）。
	if strings.Index(seen, "高分内容") > strings.Index(seen, "内容\n") &&
		strings.Contains(seen, "高分内容") {
		// 宽松校验：注入块位于开头（<retrieved-context> 是第一个字符）。
	}
	if !strings.HasPrefix(seen, "<retrieved-context>") {
		t.Errorf("注入块应在用户输入之前:\n%.200s", seen)
	}
}

// 形态 2：NewRetrieveTool 注册为工具后 agent 可自主调用——
// 用 mock provider 驱动一次真实工具调用全链路（工具执行结果进
// tool_result，下一轮模型可见）。
func TestNewRetrieveTool(t *testing.T) {
	ret := retrieval.NewMemoryRetriever(
		retrieval.Document{Content: "东京的时区是 UTC+9", Metadata: map[string]any{"source": "wiki.txt"}},
	)
	mp := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
			"c1", "retrieve", map[string]any{"query": "东京 时区", "top_k": 3})}},
		{Text: "东京时区 UTC+9"},
	}}
	app := New(WithProvider(mp), WithMaxTurns(3))
	app.UseTools(NewRetrieveTool(ret))

	var toolDone, done bool
	var toolResult string
	for ev := range app.Run(context.Background(), "东京时区几点") {
		switch ev.Type {
		case EventToolDone:
			toolDone = true
			toolResult = ev.ToolResult
		case EventError:
			t.Fatalf("意外错误: %v", ev.Error)
		case EventDone:
			done = true
		}
	}
	if !toolDone || !done {
		t.Fatalf("应完成工具调用与收尾, toolDone=%v done=%v", toolDone, done)
	}
	if !strings.Contains(toolResult, "UTC+9") || !strings.Contains(toolResult, "wiki.txt") {
		t.Errorf("工具结果应含内容与来源: %s", toolResult)
	}
}

// ---------- 测试辅助 ----------

type countingRetriever struct {
	inner retrieval.Retriever
	calls int
}

func (c *countingRetriever) Retrieve(ctx context.Context, q string, k int) ([]retrieval.Document, error) {
	c.calls++
	return c.inner.Retrieve(ctx, q, k)
}

type brokenRetriever struct{}

var errBrokenRetriever = errors.New("index down")

func (brokenRetriever) Retrieve(context.Context, string, int) ([]retrieval.Document, error) {
	return nil, errBrokenRetriever
}
