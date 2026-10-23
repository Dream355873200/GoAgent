package loop

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

type ctxObsKey string

// ctxObs 经 ContextObserver 往运行与工具的 ctx 里放值，并记录各回调看到的值。
type ctxObs struct {
	llmHookObserver
	seen []string
}

func (o *ctxObs) RunContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxObsKey("run"), "R")
}

func (o *ctxObs) ToolContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxObsKey("tool"), "T:"+observer.ToolCallIDFromContext(ctx))
}

func (o *ctxObs) note(what string, ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	run, _ := ctx.Value(ctxObsKey("run")).(string)
	tool, _ := ctx.Value(ctxObsKey("tool")).(string)
	o.seen = append(o.seen, what+"|"+run+"|"+tool)
}

func (o *ctxObs) OnLLMStart(ctx context.Context, info *observer.LLMCallInfo) {
	o.note("llm", ctx)
	o.llmHookObserver.OnLLMStart(ctx, info)
}

func (o *ctxObs) OnPermissionGranted(ctx context.Context, toolName, permission string) {
	o.note("granted", ctx)
}

func (o *ctxObs) OnSessionEnd(ctx context.Context, sessionID string, turns int) {
	o.note("end", ctx)
}

type allowAll struct{}

func (allowAll) Approve(string, string, permission.Level) (bool, bool) { return true, false }

// RunContext 的值对整个运行可见（模型请求、工具、会话结束）；ToolContext 的值
// 只在这次工具执行里（含权限检查），并带着调用 ID。
func TestContextObserver_RunAndToolContext(t *testing.T) {
	p := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- toolUse("c1", "probe")
			ch <- done(provider.StopToolUse)
			return
		}
		ch <- text("ok")
		ch <- done(provider.StopEndTurn)
	}}
	obs := &ctxObs{}
	gate := permission.NewGate(allowAll{})
	probe := ToolEntry{Name: "probe", Class: 1, Permission: permission.LevelRequireApproval,
		ExecuteFn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			obs.note("exec", ctx)
			return "done", nil
		}}
	l := New(Config{Provider: p, Tools: []ToolEntry{probe}, Observer: obs, Permission: gate, SessionID: "s"})
	for _, ev := range drain(l, context.Background(), "go") {
		if ev.Type == EvtError {
			t.Fatalf("不应报错: %v", ev.Error)
		}
	}
	want := []string{"llm|R|", "granted|R|T:c1", "exec|R|T:c1", "llm|R|", "end|R|"}
	if !slices.Equal(obs.seen, want) {
		t.Fatalf("各回调看到的 ctx 值:\n got  %v\n want %v", obs.seen, want)
	}
}

// 换后备模型、流中断后锚点恢复重开的请求：每次 OnLLMStart 恰好对应一次
// OnLLMDone 或 OnLLMError（此前这两条路径不报 OnLLMError，追踪 span 永不结束）。
func TestLLMHooks_EveryStartIsClosed(t *testing.T) {
	fb := &scriptProvider{script: func(ctx context.Context, n int, ch chan<- provider.StreamEvent) {
		if n == 1 {
			ch <- text("半截")
			ch <- provider.StreamEvent{Type: provider.EventError, Error: errors.New("连接被重置")}
			return
		}
		ch <- text("完整")
		ch <- done(provider.StopEndTurn)
	}}
	obs := &llmHookObserver{}
	l := New(Config{Provider: overloaded{}, Fallback: fb, Observer: obs, SessionID: "s"})
	for _, ev := range drain(l, context.Background(), "go") {
		if ev.Type == EvtError {
			t.Fatalf("不应报错: %v", ev.Error)
		}
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	want := []string{"start", "error", "start", "error", "start", "done"}
	if !slices.Equal(obs.events, want) {
		t.Fatalf("LLM 钩子序列 %v, 期望 %v", obs.events, want)
	}
}
