// transcript_test.go 对话记录落地与会话上下文推进测试。
//
// 覆盖：WithTranscriptSink 在轮次完成与 run 结束时回调完整消息列表（含
// tool_use/tool_result）；queue 车道续跑看得到上一轮对话；RunSession 自动
// 持久化不重复、不跳号（MemoryStore 共享会话对象的场景）。
package goagent

import (
	"context"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/session"
)

func countBlocks(msgs []message.Message, typ string) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == typ {
				n++
			}
		}
	}
	return n
}

func TestTranscriptSinkCarriesToolCalls(t *testing.T) {
	p := &steerTestProvider{script: []string{"[tool:noop]", "完成"}}
	var mu sync.Mutex
	var calls int
	var last []message.Message
	app := New(
		WithProvider(p),
		WithMaxTurns(10),
		WithTranscriptSink(func(sessionID string, msgs []message.Message) {
			mu.Lock()
			defer mu.Unlock()
			if sessionID != "sink-1" {
				t.Errorf("sessionID = %q, want sink-1", sessionID)
			}
			calls++
			last = append([]message.Message(nil), msgs...)
		}),
	)
	app.Tool("noop", ToolDef{Description: "空转", Input: struct{}{}, Execute: func(ctx Context, in struct{}) (string, error) { return "ok", nil }})

	for range app.RunWithSessionHistory(context.Background(), "sink-1", nil, "开始") {
	}

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("sink 调用 %d 次，期望至少 2 次（轮次完成 + run 结束）", calls)
	}
	if countBlocks(last, "tool_use") != 1 || countBlocks(last, "tool_result") != 1 {
		t.Fatalf("最终记录应含 1 个 tool_use 与 1 个 tool_result，实际 %d/%d", countBlocks(last, "tool_use"), countBlocks(last, "tool_result"))
	}
	if got := message.ExtractText(last[len(last)-1]); got != "完成" {
		t.Fatalf("最后一条应为助手回复「完成」，实际 %q", got)
	}
}

func TestQueuedContinuationSeesPreviousRun(t *testing.T) {
	app, hub, p := newSteerTestApp(t, []string{"第一轮回复", "第二轮回复"})
	hub.Enqueue("cont-1", "排队的第二条")
	for range app.RunWithSessionHistory(context.Background(), "cont-1", nil, "第一条") {
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) != 2 {
		t.Fatalf("期望 2 次模型请求，实际 %d", len(p.reqs))
	}
	second := p.reqs[1].Messages
	texts := make([]string, 0, len(second))
	for _, m := range second {
		texts = append(texts, message.ExtractText(m))
	}
	want := []string{"第一条", "第一轮回复", "排队的第二条"}
	if len(texts) != len(want) {
		t.Fatalf("续跑请求的上下文 = %q，期望 %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("续跑请求的上下文 = %q，期望 %q", texts, want)
		}
	}
}

func TestRunSessionPersistsEachMessageOnce(t *testing.T) {
	store := session.NewMemoryStore()
	p := &steerTestProvider{script: []string{"[tool:noop]", "第一轮完成", "第二轮完成"}}
	app := New(WithProvider(p), WithMaxTurns(10), WithSessionManager(session.NewManager(store)))
	app.Tool("noop", ToolDef{Description: "空转", Input: struct{}{}, Execute: func(ctx Context, in struct{}) (string, error) { return "ok", nil }})

	for range app.RunSession(context.Background(), "persist-1", "第一条") {
	}
	sess, _ := store.Get(context.Background(), "persist-1")
	// user + assistant(tool_use) + user(tool_result) + assistant(text)
	if len(sess.Messages) != 4 {
		t.Fatalf("第一轮后应有 4 条消息，实际 %d", len(sess.Messages))
	}
	for range app.RunSession(context.Background(), "persist-1", "第二条") {
	}
	sess, _ = store.Get(context.Background(), "persist-1")
	if len(sess.Messages) != 6 {
		t.Fatalf("第二轮后应有 6 条消息，实际 %d", len(sess.Messages))
	}
	if got := message.ExtractText(sess.Messages[4]); got != "第二条" {
		t.Fatalf("第 5 条应为第二轮用户输入，实际 %q", got)
	}
}
