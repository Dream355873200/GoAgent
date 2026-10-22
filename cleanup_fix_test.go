package goagent

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

// v0.13 清理死代码时发现的缺陷的回归测试。

// app.Group(...).Use(mw) 注册的中间件必须作用于分组内的工具：此前分组既没有
// 登记到 App，也没有任何代码读取它，分组中间件从未执行。
func TestFix_ToolGroupMiddlewareApplies(t *testing.T) {
	var executed, otherExecuted atomic.Int32
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{
			provider.NewMockToolCall("c1", "guarded", map[string]any{"v": "x"}),
			provider.NewMockToolCall("c2", "other", map[string]any{"v": "y"}),
		}},
		{Text: "完成"},
	}}
	app := New(WithProvider(prov))
	app.Tool("guarded", ToolDef{
		Description: "受分组中间件保护的工具",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in echoIn) (string, error) {
			executed.Add(1)
			return "ran", nil
		},
	})
	app.Tool("other", ToolDef{
		Description: "分组之外的工具",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in echoIn) (string, error) {
			otherExecuted.Add(1)
			return "ran", nil
		},
	})
	var seen atomic.Int32
	app.Group("guarded").Use(MiddlewareFunc(func(ctx Context, toolName string, input json.RawMessage) *Decision {
		seen.Add(1)
		return DenyWith("分组策略拦截")
	}))

	evs := drainEvents(app.Run(t.Context(), "go"))

	if executed.Load() != 0 {
		t.Fatal("分组中间件拒绝后工具不应执行")
	}
	ev, ok := toolDone(evs, "guarded")
	if !ok || !ev.ToolIsError || !strings.Contains(ev.ToolResult, "分组策略拦截") {
		t.Fatalf("工具结果应为分组中间件的拒绝原因: %+v", ev)
	}
	if otherExecuted.Load() != 1 {
		t.Fatalf("分组外的工具不受影响，应执行 1 次，实际 %d", otherExecuted.Load())
	}
	if seen.Load() != 1 {
		t.Fatalf("分组中间件只应作用于分组内的工具，实际被调用 %d 次", seen.Load())
	}
}
