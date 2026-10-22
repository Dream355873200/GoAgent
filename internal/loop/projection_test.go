package loop

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// windowProvider 以给定的上下文窗口包装 MockProvider，并记录每次请求的首条消息。
type windowProvider struct {
	*provider.MockProvider
	window int

	mu    sync.Mutex
	first []string
}

func (p *windowProvider) Capabilities() provider.Capabilities {
	c := p.MockProvider.Capabilities()
	c.ContextWindow = p.window
	return c
}

func (p *windowProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	if len(req.Messages) > 0 {
		p.first = append(p.first, message.ExtractText(req.Messages[0]))
	}
	p.mu.Unlock()
	return p.MockProvider.Stream(ctx, req)
}

func withUID(m message.Message) message.Message {
	m.UID = message.NewUID()
	return m
}

// 自动摘要不改写对话记录：摘要作为摘要记录追加（随 OnMessage 落盘），原始
// 消息原样保留；把记录传回下一次运行时由 Project 还原视图，不再请求模型摘要。
func TestTranscriptKeepsOriginalsAndSummaryRecord(t *testing.T) {
	var summaries atomic.Int32
	newComp := func() *compaction.Manager {
		return compaction.NewManager(compaction.Config{
			Layers:               []compaction.Layer{compaction.LayerAuto},
			AutoCompactThreshold: 0.5,
			Summarizer: func(ctx context.Context, prompt string) (string, error) {
				summaries.Add(1)
				return "早先的对话：用户在调试登录问题。", nil
			},
		})
	}
	big := func(tag string) string { return tag + strings.Repeat("这是很长的历史内容。", 400) }
	history := []message.Message{
		withUID(message.NewUserMessage(big("问题一"))),
		withUID(message.NewAssistantMessage(big("回答一"))),
		withUID(message.NewUserMessage(big("问题二"))),
		withUID(message.NewAssistantMessage(big("回答二"))),
		withUID(message.NewUserMessage("最近的问题")),
		withUID(message.NewAssistantMessage("最近的回答")),
	}
	prov := &windowProvider{MockProvider: provider.NewMockProvider("第一次回答", "第二次回答"), window: 4000}

	var persisted []message.Message
	l := New(Config{
		Provider:        prov,
		Compaction:      newComp(),
		InitialMessages: history,
		OnMessage:       func(m message.Message) { persisted = append(persisted, m) },
	})
	for range l.Run(context.Background(), "继续") {
	}
	if summaries.Load() != 1 {
		t.Fatalf("超过阈值应自动摘要一次，实际 %d 次", summaries.Load())
	}

	tr := l.FinalMessages()
	byUID := map[string]message.Message{}
	for _, m := range tr {
		byUID[m.UID] = m
	}
	for _, h := range history {
		got, ok := byUID[h.UID]
		if !ok || message.ExtractText(got) != message.ExtractText(h) {
			t.Fatalf("对话记录丢失或改写了原始消息 %q", message.ExtractText(h)[:12])
		}
	}
	var recs []message.Message
	for _, m := range tr {
		if m.IsCompactSummary {
			recs = append(recs, m)
		}
	}
	if len(recs) != 1 || recs[0].UID == "" || !recs[0].IsMeta || recs[0].CompactBoundary == "" {
		t.Fatalf("对话记录应恰有一条带 UID、边界的摘要记录: %+v", recs)
	}
	if _, ok := byUID[recs[0].CompactBoundary]; !ok {
		t.Fatal("摘要记录的边界应指向对话记录中的消息")
	}
	found := false
	for _, m := range persisted {
		if m.IsCompactSummary && m.UID == recs[0].UID {
			found = true
		}
	}
	if !found {
		t.Fatal("摘要记录应经 OnMessage 交给持久化")
	}

	// 模拟重新加载：新的压缩管理器、同一份对话记录。
	l2 := New(Config{Provider: prov, Compaction: newComp(), InitialMessages: tr})
	for range l2.Run(context.Background(), "再继续") {
	}
	if summaries.Load() != 1 {
		t.Fatalf("从对话记录还原视图不应再次请求摘要，实际共 %d 次", summaries.Load())
	}
	prov.mu.Lock()
	first := prov.first[len(prov.first)-1]
	prov.mu.Unlock()
	if !strings.HasPrefix(first, "[对话已压缩]") {
		t.Fatalf("重新加载后的请求应以摘要开头: %.40q", first)
	}
}

// Project：没有摘要记录时原样返回；边界找不到时放弃投影。
func TestProjectEdgeCases(t *testing.T) {
	a := withUID(message.NewUserMessage("a"))
	b := withUID(message.NewAssistantMessage("b"))
	if got := compaction.Project([]message.Message{a, b}); len(got) != 2 {
		t.Fatalf("没有摘要记录时应原样返回: %d", len(got))
	}
	s := message.NewUserMessage("[对话已压缩] x")
	s.IsCompactSummary, s.IsMeta, s.UID, s.CompactBoundary = true, true, "s", "missing"
	got := compaction.Project([]message.Message{a, b, s})
	if len(got) != 2 || got[0].UID != a.UID {
		t.Fatalf("边界找不到时应退回完整记录（去掉摘要记录）: %+v", got)
	}
	s.CompactBoundary = b.UID
	got = compaction.Project([]message.Message{a, b, s})
	if len(got) != 2 || !got[0].IsCompactSummary || got[1].UID != b.UID || !got[0].Compacted {
		t.Fatalf("视图应为摘要加边界起的消息: %+v", got)
	}
}
