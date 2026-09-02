package observer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
)

// llmRecorder 同时实现 Observer 与 LLMObserver，记录 LLM 调用事件。
type llmRecorder struct {
	NopObserver
	starts  []*LLMCallInfo
	dones   []*LLMResult
	errors  []error
	toolIDs []string
}

func (r *llmRecorder) OnLLMStart(ctx context.Context, info *LLMCallInfo) {
	r.starts = append(r.starts, info)
}
func (r *llmRecorder) OnLLMDone(ctx context.Context, info *LLMCallInfo, result *LLMResult) {
	r.dones = append(r.dones, result)
}
func (r *llmRecorder) OnLLMError(ctx context.Context, info *LLMCallInfo, err error) {
	r.errors = append(r.errors, err)
}
func (r *llmRecorder) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	r.toolIDs = append(r.toolIDs, ToolCallIDFromContext(ctx))
}

// NopObserver 不实现 LLMObserver——普通 Observer 对 LLM 钩子零感知。
func TestLLMObserver_NopNotAffected(t *testing.T) {
	var o Observer = NopObserver{}
	if _, ok := o.(LLMObserver); ok {
		t.Fatal("NopObserver 不应实现 LLMObserver（普通实现零感知）")
	}
}

// 边界注入：IntoContext 注入的 observer 经 ResolveObserver 合并，
// LLM 钩子与普通事件都广播。
func TestIntoContext_BorderObserverBroadcast(t *testing.T) {
	rec := &llmRecorder{}
	base := &llmRecorder{}

	ctx := IntoContext(context.Background(), nil, rec)
	resolved := ResolveObserver(ctx, base)
	if resolved == nil {
		t.Fatal("注入后应返回合并视图")
	}

	// 普通 Observer 事件。
	resolved.OnSessionStart(ctx, "s1")
	// LLM 钩子。
	info := &LLMCallInfo{Model: "m", Turn: 1}
	resolved.(LLMObserver).OnLLMStart(ctx, info)
	resolved.(LLMObserver).OnLLMDone(ctx, info, &LLMResult{Model: "m", Duration: time.Second})
	resolved.(LLMObserver).OnLLMError(ctx, info, context.Canceled)

	if len(rec.starts) != 1 || len(rec.dones) != 1 || len(rec.errors) != 1 {
		t.Fatalf("边界 observer 应收到全部 LLM 钩子: starts=%d dones=%d errors=%d",
			len(rec.starts), len(rec.dones), len(rec.errors))
	}
	if len(base.starts) != 1 || len(base.dones) != 1 || len(base.errors) != 1 {
		t.Fatalf("base observer 应同时收到: starts=%d dones=%d errors=%d",
			len(base.starts), len(base.dones), len(base.errors))
	}
}

// 未注入时 ResolveObserver 原样返回 base（nil 安全）。
func TestResolveObserver_NoInjection(t *testing.T) {
	if got := ResolveObserver(context.Background(), nil); got != nil {
		t.Fatalf("无注入且 base=nil 应返回 nil, got %T", got)
	}
	base := &llmRecorder{}
	if got := ResolveObserver(context.Background(), base); got != Observer(base) {
		t.Fatal("无注入应原样返回 base")
	}
}

// 多次注入追加在链上（都广播）。
func TestIntoContext_ChainedInjection(t *testing.T) {
	a, b := &llmRecorder{}, &llmRecorder{}
	ctx := IntoContext(context.Background(), nil, a)
	ctx = IntoContext(ctx, nil, b)
	resolved := ResolveObserver(ctx, nil)
	resolved.OnSessionStart(ctx, "s1")
	if len(a.starts) != 0 && len(a.toolIDs) != 0 {
		// OnSessionStart 不产生 LLM 事件，只验证不 panic
	}
	resolved.(LLMObserver).OnLLMStart(ctx, &LLMCallInfo{Model: "m"})
	if len(a.starts) != 1 || len(b.starts) != 1 {
		t.Fatalf("链上两个 observer 都应收到: a=%d b=%d", len(a.starts), len(b.starts))
	}
}

// 工具调用 ID 的 ctx 注入与提取（span 关联）。
func TestWithToolCallID_RoundTrip(t *testing.T) {
	ctx := WithToolCallID(context.Background(), "tc-123")
	if got := ToolCallIDFromContext(ctx); got != "tc-123" {
		t.Fatalf("提取=%q, 期望 tc-123", got)
	}
	if got := ToolCallIDFromContext(context.Background()); got != "" {
		t.Fatalf("未注入应返回空串, got %q", got)
	}
}

// Usage 字段随 LLMResult 传递。
func TestLLMResult_CarriesUsage(t *testing.T) {
	u := &provider.Usage{InputTokens: 100, OutputTokens: 50}
	r := &LLMResult{Usage: u}
	if r.Usage.InputTokens != 100 || r.Usage.OutputTokens != 50 {
		t.Fatal("Usage 应原样携带")
	}
}
