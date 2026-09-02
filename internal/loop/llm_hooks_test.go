package loop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/observer"
)

// llmHookObserver 记录 LLM 调用钩子序列。
type llmHookObserver struct {
	observer.NopObserver
	mu      sync.Mutex
	events  []string
	lastIfo *observer.LLMCallInfo
	lastRes *observer.LLMResult
	toolCtx []string
}

func (o *llmHookObserver) OnLLMStart(ctx context.Context, info *observer.LLMCallInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "start")
	o.lastIfo = info
}

func (o *llmHookObserver) OnLLMDone(ctx context.Context, info *observer.LLMCallInfo, result *observer.LLMResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "done")
	o.lastRes = result
}

func (o *llmHookObserver) OnLLMError(ctx context.Context, info *observer.LLMCallInfo, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "error")
}

func (o *llmHookObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolCtx = append(o.toolCtx, observer.ToolCallIDFromContext(ctx))
}

// 正常一轮：OnLLMStart → OnLLMDone（含产出统计），无 error。
func TestLLMHooks_StartDoneSequence(t *testing.T) {
	obs := &llmHookObserver{}
	l := New(Config{
		Provider:  &probeStreamProvider{model: "llm-probe"},
		MaxTurns:  3,
		SessionID: "sess-llm-1",
		Observer:  obs,
	})
	for range l.Run(context.Background(), "hi") {
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 2 || obs.events[0] != "start" || obs.events[1] != "done" {
		t.Fatalf("事件序列=%v, 期望 [start done]", obs.events)
	}
	if obs.lastIfo == nil || obs.lastIfo.Model != "llm-probe" || obs.lastIfo.Turn != 1 {
		t.Fatalf("LLMCallInfo 缺失或错误: %+v", obs.lastIfo)
	}
	if obs.lastRes == nil || obs.lastRes.NumToolCalls != 0 || obs.lastRes.OutputTextLen == 0 {
		t.Fatalf("LLMResult 缺失或统计错误: %+v", obs.lastRes)
	}
	if obs.lastRes.StopReason == "" {
		t.Fatal("StopReason 应携带（end_turn）")
	}
}

// 流错误：OnLLMStart → OnLLMError。
func TestLLMHooks_ErrorPath(t *testing.T) {
	obs := &llmHookObserver{}
	l := New(Config{
		Provider:  &cancelStreamProvider{},
		MaxTurns:  3,
		SessionID: "sess-llm-2",
		Observer:  obs,
	})
	for range l.Run(context.Background(), "hi") {
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 2 || obs.events[0] != "start" || obs.events[1] != "error" {
		t.Fatalf("事件序列=%v, 期望 [start error]", obs.events)
	}
}

// 未实现 LLMObserver 的普通 Observer（NopObserver）零影响。
func TestLLMHooks_NopObserverUnaffected(t *testing.T) {
	l := New(Config{
		Provider:  &probeStreamProvider{model: "llm-probe"},
		MaxTurns:  2,
		SessionID: "sess-llm-3",
		Observer:  observer.NopObserver{},
	})
	for range l.Run(context.Background(), "hi") {
	}
}
