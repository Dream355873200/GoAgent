package scripted

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

func drain(t *testing.T, ch <-chan provider.StreamEvent) (text, thinking string, calls []*message.ToolCall, usage *provider.Usage, stop provider.StopReason, err error) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventThinkingDelta:
			thinking += ev.Thinking
		case provider.EventToolUseStart:
			calls = append(calls, ev.ToolCall)
		case provider.EventUsage:
			usage = ev.Usage
		case provider.EventMessageComplete:
			stop = ev.StopReason
		case provider.EventError:
			err = ev.Error
		}
	}
	return
}

func userReq(system, text string) *provider.Request {
	return &provider.Request{SystemPrompt: system, Messages: []message.Message{message.NewUserMessage(text)}}
}

// 按顺序消费：分块正文、思考、工具调用（自动 ID）、用量与停止原因；用完报错。
func TestScriptOrderAndExhaustion(t *testing.T) {
	m := New(
		Text("你好世界").WithThinking("想").WithUsage(10, 2).WithChunkSize(2),
		Tools(ToolCall{Name: "a", Input: map[string]int{"n": 1}}, ToolCall{ID: "fixed", Name: "b"}),
	)
	ctx := context.Background()

	ch, err := m.Stream(ctx, userReq("", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	text, thinking, calls, usage, stop, _ := drain(t, ch)
	if text != "你好世界" || thinking != "想" || len(calls) != 0 || usage.InputTokens != 10 || stop != provider.StopEndTurn {
		t.Fatalf("第一步: text=%q thinking=%q usage=%+v stop=%s", text, thinking, usage, stop)
	}

	ch, _ = m.Stream(ctx, userReq("", "again"))
	_, _, calls, _, stop, _ = drain(t, ch)
	if len(calls) != 2 || calls[0].ID != "call-2-0" || string(calls[0].Input) != `{"n":1}` || calls[1].ID != "fixed" || string(calls[1].Input) != "{}" || stop != provider.StopToolUse {
		t.Fatalf("第二步: %+v stop=%s", calls, stop)
	}
	m.AssertDone(t)

	if _, err := m.Stream(ctx, userReq("", "x")); err == nil || !strings.Contains(err.Error(), "脚本已用完") {
		t.Fatalf("脚本用完应报错, got %v", err)
	}
	if len(m.Requests()) != 3 {
		t.Fatalf("应记录全部 3 次请求, got %d", len(m.Requests()))
	}
}

// Route 按请求分流；Expect 断言请求；Fail 与 StreamErr 模拟失败。
func TestScriptRoutesExpectAndFailures(t *testing.T) {
	boom := errors.New("boom")
	m := New(Text("main"), Fail(boom))
	m.Route(SystemContains("SUB"), Text("sub").Expect(func(r *provider.Request) error {
		if !strings.Contains(r.SystemPrompt, "SUB") {
			return errors.New("not sub")
		}
		return nil
	}))
	m.Route(LastUserContains("断流"), Step{Text: "半截", StreamErr: errors.New("网络断开")})
	ctx := context.Background()

	ch, _ := m.Stream(ctx, userReq("SUB agent", "x"))
	if text, _, _, _, _, _ := drain(t, ch); text != "sub" {
		t.Fatalf("SUB 请求应走子脚本, got %q", text)
	}
	ch, _ = m.Stream(ctx, userReq("main agent", "x"))
	if text, _, _, _, _, _ := drain(t, ch); text != "main" {
		t.Fatalf("其他请求应走默认脚本, got %q", text)
	}
	ch, _ = m.Stream(ctx, userReq("main agent", "请断流"))
	if text, _, _, _, _, err := drain(t, ch); text != "半截" || err == nil {
		t.Fatalf("StreamErr 应在正文之后以流错误结束: %q %v", text, err)
	}
	if _, err := m.Stream(ctx, userReq("main agent", "x")); !errors.Is(err, boom) {
		t.Fatalf("Fail 应让请求直接失败, got %v", err)
	}

	strict := New(Text("x").Expect(func(*provider.Request) error { return errors.New("缺少工具定义") }))
	if _, err := strict.Stream(ctx, userReq("", "x")); err == nil || !strings.Contains(err.Error(), "缺少工具定义") {
		t.Fatalf("断言失败应让请求报错并说明原因, got %v", err)
	}
}

// WithChunkSize 让正文按字符分块流出（中文按字符而不是字节切）。
func TestScriptChunks(t *testing.T) {
	m := New(Text("你好世界").WithChunkSize(3))
	ch, err := m.Stream(context.Background(), userReq("", "x"))
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for ev := range ch {
		if ev.Type == provider.EventTextDelta {
			parts = append(parts, ev.Text)
		}
	}
	if strings.Join(parts, "|") != "你好世|界" {
		t.Fatalf("分块: %q", parts)
	}
}

// Complete 把流收拢为一条消息。
func TestScriptComplete(t *testing.T) {
	m := New(Text("done").WithUsage(3, 4))
	resp, err := m.Complete(context.Background(), userReq("", "x"))
	if err != nil || message.ExtractText(resp.Message) != "done" || resp.Usage.OutputTokens != 4 || resp.StopReason != provider.StopEndTurn {
		t.Fatalf("Complete: %+v %v", resp, err)
	}
}
