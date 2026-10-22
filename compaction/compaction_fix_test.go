package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

func assistantCalls(ids ...string) message.Message {
	m := message.Message{Role: message.RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, message.ContentBlock{Type: "tool_use", ToolUseID: id, ToolName: "Read"})
	}
	return m
}

// 压缩层不得就地改写入参：调用方的历史与已持久化的消息保持原样。
func TestFix_ApplyDoesNotMutateInput(t *testing.T) {
	big := strings.Repeat("x", 60_000)
	msgs := []message.Message{
		message.NewUserMessage("任务"),
		assistantCalls("r1"),
		message.NewToolResultMessage("r1", big, false),
		message.NewAssistantMessage("好的"),
	}
	m := NewManager(Config{Layers: []Layer{LayerBudget, LayerMicro}})
	out, freed := m.Apply(context.Background(), msgs, 1_000_000)
	if freed == 0 || len(out[2].Content[0].Text) >= len(big) {
		t.Fatal("前置条件：大结果应被截断")
	}
	if msgs[2].Content[0].Text != big {
		t.Fatalf("入参被就地改写：长度 %d", len(msgs[2].Content[0].Text))
	}
}

// 折叠的切点按 API 轮次对齐：并行工具调用 + 插话之后也不会留下孤儿 tool_result。
func TestFix_CollapseKeepsPairing(t *testing.T) {
	var msgs []message.Message
	msgs = append(msgs, message.NewUserMessage("原始任务：重构模块"))
	for i := 0; i < 6; i++ {
		a, b := "a"+string(rune('0'+i)), "b"+string(rune('0'+i))
		msgs = append(msgs,
			assistantCalls(a, b),
			message.NewToolResultMessage(a, strings.Repeat("内容", 2000), false),
			message.NewToolResultMessage(b, strings.Repeat("内容", 2000), false),
			message.NewUserMessage("插话"),
		)
	}
	msgs = append(msgs, message.NewAssistantMessage("完成"))
	c := &collapseCompressor{}
	out, freed := c.apply(msgs, 0)
	if freed <= 0 {
		t.Fatal("前置条件：应发生折叠")
	}
	if err := message.ValidatePairing(out); err != nil {
		t.Fatalf("折叠后配对非法: %v", err)
	}
}

// 反复折叠保留原始任务，不会变成"摘要的摘要"。
func TestFix_CollapseRepeatedKeepsOriginalTask(t *testing.T) {
	msgs := []message.Message{message.NewUserMessage("原始任务：把支付模块迁移到新网关")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, message.NewAssistantMessage("进展"), message.NewUserMessage("继续"))
	}
	c := &collapseCompressor{}
	out, _ := c.apply(msgs, 0)
	for i := 0; i < 10; i++ {
		out = append(out, message.NewAssistantMessage("进展"), message.NewUserMessage("继续"))
	}
	out, _ = c.apply(out, 0)
	if !strings.Contains(message.ExtractText(out[0]), "把支付模块迁移到新网关") {
		t.Fatalf("两次折叠后原始任务丢失: %q", message.ExtractText(out[0]))
	}
}

type summaryProvider struct{ calls int }

func (p *summaryProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "sum", ContextWindow: 10_000}
}
func (p *summaryProvider) Stream(context.Context, *provider.Request) (<-chan provider.StreamEvent, error) {
	return nil, nil
}
func (p *summaryProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.calls++
	return &provider.Response{Message: message.NewAssistantMessage("这是摘要")}, nil
}

// SetProvider 后 Layer 4 自动摘要真正生效，且结果配对合法。
func TestFix_AutoSummaryWiredFromProvider(t *testing.T) {
	var msgs []message.Message
	msgs = append(msgs, message.NewUserMessage("任务"))
	for i := 0; i < 8; i++ {
		id := "t" + string(rune('0'+i))
		msgs = append(msgs, assistantCalls(id), message.NewToolResultMessage(id, strings.Repeat("长内容", 600), false))
	}
	p := &summaryProvider{}
	m := NewManager(Config{Layers: []Layer{LayerAuto}})
	m.SetProvider(p)
	out, freed := m.Apply(context.Background(), msgs, 10_000)
	if p.calls != 1 || freed <= 0 {
		t.Fatalf("应调用一次摘要模型并释放空间: calls=%d freed=%d", p.calls, freed)
	}
	if !strings.Contains(message.ExtractText(out[0]), "这是摘要") {
		t.Fatal("首条消息应是摘要")
	}
	if err := message.ValidatePairing(out); err != nil {
		t.Fatalf("摘要后配对非法: %v", err)
	}
}
