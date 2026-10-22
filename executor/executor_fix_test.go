package executor

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// 工具 panic 被恢复为错误结果，不带走进程；PanicHandler 收到堆栈。
func TestExecuteOne_PanicRecovered(t *testing.T) {
	var gotStack atomic.Bool
	e := New(Config{PanicHandler: func(name string, r any, stack []byte) {
		gotStack.Store(len(stack) > 0 && name == "boom")
	}})
	se := NewStreamingExecutor(e)
	se.Add(context.Background(), ToolCall{ID: "p", Name: "boom", Class: ClassReadOnly,
		Execute: func(context.Context, json.RawMessage) (string, error) { panic("kaboom") }})
	res := se.Wait(context.Background())
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("panic 应转为错误结果: %+v", res)
	}
	if !gotStack.Load() {
		t.Fatal("PanicHandler 应收到工具名和堆栈")
	}
}

// Execute 为 nil（如未实现的工具定义）返回错误而不是 panic。
func TestExecuteOne_NilExecute(t *testing.T) {
	res := New(Config{}).Execute(context.Background(), []ToolCall{{ID: "x", Name: "noimpl"}})
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("nil Execute 应返回错误结果: %+v", res)
	}
}

// ctx 取消后：排队中的工具不再启动，忽略取消的在跑工具在宽限期后被放弃，Wait 不会永久阻塞。
func TestStreamingWait_CancelIsBounded(t *testing.T) {
	old := CancelGrace
	CancelGrace = 200 * time.Millisecond
	defer func() { CancelGrace = old }()

	se := NewStreamingExecutor(New(Config{}))
	ctx, cancel := context.WithCancel(context.Background())
	stuck := make(chan struct{})
	defer close(stuck)
	started := make(chan struct{})
	var queuedRan atomic.Bool

	se.Add(ctx, ToolCall{ID: "w1", Name: "stuck", Class: ClassExclusive,
		Execute: func(context.Context, json.RawMessage) (string, error) { close(started); <-stuck; return "late", nil }})
	se.Add(ctx, ToolCall{ID: "w2", Name: "queued", Class: ClassExclusive,
		Execute: func(context.Context, json.RawMessage) (string, error) { queuedRan.Store(true); return "x", nil }})

	<-started
	cancel()
	start := time.Now()
	res := se.Wait(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Wait 在取消后阻塞了 %v", d)
	}
	if queuedRan.Load() {
		t.Fatal("取消后不应再启动排队中的工具")
	}
	if len(res) != 1 || res[0].ToolUseID != "w2" || !res[0].IsError {
		t.Fatalf("排队工具应以已取消结果返回、卡住的工具不应出现: %+v", res)
	}
}

// Poll 取走的结果不会在 Wait 中重复出现；两者合并恰好覆盖全部工具。
func TestStreamingPollAndWaitPartition(t *testing.T) {
	se := NewStreamingExecutor(New(Config{}))
	ctx := context.Background()
	fast := func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	se.Add(ctx, ToolCall{ID: "a", Name: "a", Class: ClassReadOnly, Execute: fast})
	time.Sleep(50 * time.Millisecond)
	polled := se.Poll()
	se.Add(ctx, ToolCall{ID: "b", Name: "b", Class: ClassReadOnly, Execute: fast})
	waited := se.Wait(ctx)
	seen := map[string]int{}
	for _, r := range append(polled, waited...) {
		seen[r.ToolUseID]++
	}
	if seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("每个结果应恰好出现一次: polled=%v waited=%v", polled, waited)
	}
}
