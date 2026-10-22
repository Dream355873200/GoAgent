package goagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

// 统一执行路径（v0.13）的回归测试：子循环经 loop.Scope 结构性继承父运行的
// 检查链与观测出口。

// recordingObserver 记录工具启动与会话开始事件。
type recordingObserver struct {
	observer.NopObserver
	mu       sync.Mutex
	tools    []string
	sessions []string
}

func (o *recordingObserver) OnToolStart(ctx context.Context, name string, input json.RawMessage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tools = append(o.tools, name)
}

func (o *recordingObserver) OnSessionStart(ctx context.Context, sessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessions = append(o.sessions, sessionID)
}

func (o *recordingObserver) sawTool(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.tools {
		if t == name {
			return true
		}
	}
	return false
}

// 宿主工具里直接用 agent.NewRunner 派生的子 agent，不需要任何接线就继承
// 所在运行的权限门与 provider：工具按声明的级别（Normal）走审批。此前只有
// 经 App.AgentTools 包装的工具才会被检查。
func TestFix_RawRunnerInheritsGateStructurally(t *testing.T) {
	var executed atomic.Int32
	appr := &allowListApprover{allow: map[string]bool{"host_task": true}}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "host_task", map[string]any{"v": ""})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s1", "write_db", map[string]any{})}},
		{Text: "sub done"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithApprover(appr))
	app.Tool("host_task", ToolDef{
		Description: "宿主工具：内部派生子 agent",
		Input:       echoIn{},
		Permission:  Normal,
		Execute: func(ctx Context, in echoIn) (string, error) {
			res, err := agent.NewRunner(nil).Run(ctx, agent.Definition{
				Name: "inner",
				Tools: []agent.ToolDef{{
					Name:        "write_db",
					InputSchema: map[string]any{"type": "object"},
					Permission:  permission.LevelNormal,
					Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
						executed.Add(1)
						return "written", nil
					},
				}},
			}, "do it")
			if err != nil {
				return "", err
			}
			return res.FinalText, nil
		},
	})

	evs := drainEvents(app.Run(context.Background(), "go"))
	if ev, ok := toolDone(evs, "host_task"); !ok || ev.ToolIsError {
		t.Fatalf("宿主工具应正常完成（子 agent 沿用运行的 provider）: %+v", ev)
	}
	if !appr.askedFor("write_db") || executed.Load() != 0 {
		t.Fatalf("子 agent 的 Normal 级工具应经审批且被拒绝 (executed=%d)", executed.Load())
	}
}

// 团队成员的工具调用进入 App 的 observer：此前成员循环没有 observer，工具、
// 用量等观测数据全部缺失。子循环不额外发会话开始事件。
func TestFix_TeamMembersInheritObserver(t *testing.T) {
	obs := &recordingObserver{}
	spec := map[string]any{
		"leader_instruction": "lead the team",
		"members":            []any{map[string]any{"name": "w", "role": "worker", "tools": []string{"echo"}}},
	}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "run_team", map[string]any{"spec": spec, "task": "do it"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("l1", "assign_w", map[string]any{"task": "sub task"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("w1", "echo", map[string]any{"v": "x"})}},
		{Text: "member done"},
		{Text: "team summary"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithApprover(AutoApprover()), WithObservers(obs))
	app.Tool("echo", echoTool())
	app.UseTools(app.CreateTeamTool())

	drainEvents(app.Run(context.Background(), "go"))
	if !obs.sawTool("echo") || !obs.sawTool("assign_w") {
		t.Fatalf("成员与队长的工具调用应进入 observer: %v", obs.tools)
	}
	if len(obs.sessions) != 1 {
		t.Fatalf("只有主运行是一次会话，子循环不应发会话开始事件: %v", obs.sessions)
	}
}

// 子 agent 工具经 Context.Progress 上报的状态行转发到所在运行的事件流（旧
// 实现直接把外层 ctx 交给工具，这些事件本来就可见）。
func TestFix_SubAgentToolProgressForwarded(t *testing.T) {
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "Agent_helper", map[string]any{"task": "help"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s1", "slow", map[string]any{"v": ""})}},
		{Text: "sub done"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov))
	app.Tool("slow", ToolDef{
		Description: "上报进度的工具",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in echoIn) (string, error) {
			ctx.Progress("处理中 50%")
			return "ok", nil
		},
	})
	tools, err := app.AgentTools("slow")
	if err != nil {
		t.Fatal(err)
	}
	app.Tool("Agent_helper", app.AgentTool(agent.Definition{Name: "helper", Tools: tools, MaxTurns: 4}))

	found := false
	for _, ev := range drainEvents(app.Run(context.Background(), "go")) {
		if ev.Type == EventProgress && strings.HasPrefix(ev.StatusKey, "tool-progress") && ev.Text == "处理中 50%" {
			found = true
		}
	}
	if !found {
		t.Fatal("子 agent 工具的进度状态行应出现在主运行的事件流中")
	}
}

// 没有会话身份的运行（Run）各自使用独立的工具键值存储：此前所有这类运行
// 共享同一份（以空会话 ID 为键），一次运行写入的数据会被另一次读到。
func TestFix_EphemeralRunsIsolateToolStore(t *testing.T) {
	var seen []any
	var mu sync.Mutex
	newProv := func() provider.Provider {
		return &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("c1", "kv", map[string]any{"v": "secret"})}},
			{Text: "done"},
		}}
	}
	app := New(WithProvider(newProv()))
	app.Tool("kv", ToolDef{
		Description: "读写会话键值",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in echoIn) (string, error) {
			prev, _ := ctx.Load("k")
			mu.Lock()
			seen = append(seen, prev)
			mu.Unlock()
			ctx.Store("k", in.V)
			return "ok", nil
		},
	})
	drainEvents(app.Run(context.Background(), "first"))
	app.SetProvider(newProv())
	drainEvents(app.Run(context.Background(), "second"))

	if len(seen) != 2 || seen[0] != nil || seen[1] != nil {
		t.Fatalf("两次独立运行不应共享键值存储: %v", seen)
	}
}

// 同一会话的多次运行共享键值存储，CloseSession 之后从空状态开始。
func TestFix_SessionStoreSharedAcrossRunsUntilClose(t *testing.T) {
	var seen []any
	app := New(WithProvider(provider.NewMockProvider()))
	app.Tool("kv", ToolDef{
		Description: "读写会话键值",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in echoIn) (string, error) {
			prev, _ := ctx.Load("k")
			seen = append(seen, prev)
			ctx.Store("k", in.V)
			return "ok", nil
		},
	})
	run := func(v string) {
		app.SetProvider(&provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("c1", "kv", map[string]any{"v": v})}},
			{Text: "done"},
		}})
		drainEvents(app.RunWithSessionHistory(context.Background(), "s1", nil, "go"))
	}
	run("one")
	run("two")
	app.CloseSession("s1")
	run("three")

	want := []any{nil, "one", nil}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
}
