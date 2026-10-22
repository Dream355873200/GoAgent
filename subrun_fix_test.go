package goagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 子运行（pipeline / 团队 / 子 agent）与流水线调度的回归测试。

// allowListApprover 只批准名单内的工具，记录每一次被询问的工具名。
type allowListApprover struct {
	mu    sync.Mutex
	allow map[string]bool
	asked []string
}

func (a *allowListApprover) Approve(toolName, input string, perm Permission) (bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, toolName)
	return a.allow[toolName], false
}

func (a *allowListApprover) askedFor(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, n := range a.asked {
		if n == name {
			return true
		}
	}
	return false
}

// dangerTool 是一个需要审批（Normal 级）的工具，记录实际执行次数。
func dangerTool(executed *atomic.Int32) ToolDef {
	return ToolDef{
		Description: "有副作用的操作",
		Input:       echoIn{},
		Permission:  Normal,
		Execute: func(ctx Context, in echoIn) (string, error) {
			executed.Add(1)
			return "done", nil
		},
	}
}

func toolDone(evs []Event, name string) (Event, bool) {
	for _, ev := range evs {
		if ev.Type == EventToolDone && ev.ToolName == name {
			return ev, true
		}
	}
	return Event{}, false
}

// 模型在运行中用 RunDynPipeline 编排的节点，其工具调用要过该运行的权限门。
// 此前节点工具一律按只读处理，一次对外层工具的批准就能让模型绕开审批执行
// 任意已注册工具。
func TestFix_DynPipelineInheritsPermissionGate(t *testing.T) {
	var executed atomic.Int32
	appr := &allowListApprover{allow: map[string]bool{}}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "launch", map[string]any{"v": ""})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("n1", "danger", map[string]any{"v": "x"})}},
		{Text: "node done"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithApprover(appr))
	app.Tool("danger", dangerTool(&executed))
	app.Tool("launch", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
		spec := &DynPipelineSpec{Nodes: []DynNodeSpec{{Name: "n1", Instruction: "do it", Tools: []string{"danger"}}}}
		return "ok", app.RunDynPipeline(ctx, spec, nil)
	}})

	evs := drainEvents(app.Run(context.Background(), "go"))
	if ev, ok := toolDone(evs, "launch"); !ok || ev.ToolIsError {
		t.Fatalf("launch 应正常完成: %+v", ev)
	}
	if !appr.askedFor("danger") {
		t.Fatal("节点工具应经过父运行的审批者")
	}
	if executed.Load() != 0 {
		t.Fatal("被拒绝的节点工具不应执行")
	}
}

// 宿主工具内部调用 RunPipeline：默认继承所在运行的权限门；TrustedTools
// 恢复旧行为（不经权限门）。
func TestFix_HostPipelineInRunInheritsGate(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		var executed atomic.Int32
		appr := &allowListApprover{allow: map[string]bool{}}
		prov := &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "launch", map[string]any{"v": ""})}},
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("n1", "danger", map[string]any{"v": "x"})}},
			{Text: "node done"},
			{Text: "main done"},
		}}
		app := New(WithProvider(prov), WithApprover(appr))
		app.UsePipeline(PipelineConfig{
			TrustedTools: trusted,
			Nodes: []PipelineNode{{
				Name:    "n1",
				Message: "go",
				Agent: &PipelineAgentDef{Name: "n1", Instruction: "x",
					Tools: []NamedTool{{Name: "danger", Def: dangerTool(&executed)}}},
			}},
		})
		app.Tool("launch", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
			return "ok", app.RunPipeline(ctx)
		}})
		drainEvents(app.Run(context.Background(), "go"))

		if trusted {
			if executed.Load() != 1 || appr.askedFor("danger") {
				t.Fatalf("TrustedTools: 工具应直接执行且不询问 (executed=%d)", executed.Load())
			}
		} else {
			if executed.Load() != 0 || !appr.askedFor("danger") {
				t.Fatalf("默认: 工具应经审批且被拒绝 (executed=%d)", executed.Load())
			}
		}
	}
}

// run_team 此前没有 Execute（每次调用都失败）；现在能执行，成员工具同样过
// 父运行的权限门。
func TestFix_RunTeamExecutesAndInheritsGate(t *testing.T) {
	var executed atomic.Int32
	appr := &allowListApprover{allow: map[string]bool{"run_team": true}}
	spec := map[string]any{
		"leader_instruction": "lead the team",
		"members": []any{map[string]any{
			"name": "w", "role": "worker", "tools": []string{"danger"},
		}},
	}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "run_team", map[string]any{"spec": spec, "task": "do it"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("l1", "assign_w", map[string]any{"task": "sub task"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("w1", "danger", map[string]any{"v": "x"})}},
		{Text: "member done"},
		{Text: "team summary"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithApprover(appr))
	app.Tool("danger", dangerTool(&executed))
	app.UseTools(app.CreateTeamTool())

	evs := drainEvents(app.Run(context.Background(), "go"))
	ev, ok := toolDone(evs, "run_team")
	if !ok || ev.ToolIsError || !strings.Contains(ev.ToolResult, "team summary") {
		t.Fatalf("run_team 应返回队长汇总: %+v", ev)
	}
	if !appr.askedFor("danger") || executed.Load() != 0 {
		t.Fatalf("成员工具应经审批且被拒绝 (executed=%d)", executed.Load())
	}
}

// AgentTools 交给子 agent 的工具同样过父运行的权限门与入参校验。
func TestFix_AgentToolsInheritGate(t *testing.T) {
	var executed atomic.Int32
	appr := &allowListApprover{allow: map[string]bool{"Agent_helper": true}}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "Agent_helper", map[string]any{"task": "help"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s1", "danger", map[string]any{"v": "x"})}},
		{Text: "sub done"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithApprover(appr))
	app.Tool("danger", dangerTool(&executed))
	tools, err := app.AgentTools("danger")
	if err != nil {
		t.Fatal(err)
	}
	app.Tool("Agent_helper", app.AgentTool(agent.Definition{Name: "helper", SystemPrompt: "x", Tools: tools, MaxTurns: 4}))

	drainEvents(app.Run(context.Background(), "go"))
	if !appr.askedFor("danger") || executed.Load() != 0 {
		t.Fatalf("子 agent 的工具应经审批且被拒绝 (executed=%d)", executed.Load())
	}

	// 入参不合规的调用直接返回错误，不执行。
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || executed.Load() != 0 {
		t.Fatalf("缺少必填字段应被拒绝: out=%q err=%v", out, err)
	}
}

// 子运行嵌套超过上限时报错，防止模型无限递归派生。
func TestFix_SubRunDepthLimit(t *testing.T) {
	ctx := context.Background()
	var err error
	for i := 0; i < MaxSubRunDepth; i++ {
		if ctx, err = enterSubRun(ctx, "x"); err != nil {
			t.Fatalf("第 %d 层不应报错: %v", i+1, err)
		}
	}
	app := New(WithProvider(provider.NewMockProvider("ok")))
	app.UsePipeline(PipelineConfig{Nodes: []PipelineNode{{Name: "a", Message: "go", Agent: &PipelineAgentDef{Name: "a", Instruction: "x"}}}})
	if err := app.RunPipeline(ctx); err == nil || !strings.Contains(err.Error(), "嵌套过深") {
		t.Fatalf("超过嵌套上限应报错: %v", err)
	}
}

// 工具入参按 schema 校验：缺必填字段的调用不执行，错误回给模型；可关闭。
func TestFix_ToolInputValidated(t *testing.T) {
	for _, validate := range []bool{true, false} {
		var executed atomic.Int32
		prov := &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("c1", "echo", map[string]any{})}},
			{Text: "done"},
		}}
		app := New(WithProvider(prov), WithToolInputValidation(validate))
		app.Tool("echo", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
			executed.Add(1)
			return "ok", nil
		}})
		evs := drainEvents(app.Run(context.Background(), "go"))
		ev, _ := toolDone(evs, "echo")
		if validate {
			if executed.Load() != 0 || !ev.ToolIsError || !strings.Contains(ev.ToolResult, "v") {
				t.Fatalf("应拒绝缺少必填字段的调用: executed=%d ev=%+v", executed.Load(), ev)
			}
		} else if executed.Load() != 1 {
			t.Fatal("关闭校验后应照常执行")
		}
	}
}

// 模型给的节点名会成为文件名，必须是安全的单段名称。
func TestFix_DynPipelineRejectsUnsafeNodeName(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider("ok")))
	for _, name := range []string{"../evil", "a/b", `a\b`, "CON"} {
		_, err := app.BuildDynPipeline(&DynPipelineSpec{Nodes: []DynNodeSpec{{Name: name, Instruction: "x"}}})
		if err == nil {
			t.Fatalf("节点名 %q 应被拒绝", name)
		}
	}
}

// 动态 pipeline 不再占用 App 的 pipeline 配置：宿主配置的 pipeline 不被覆盖，
// 且每次 RunPipeline 都是新的运行实例（可重复、可并发运行）。
func TestFix_DynPipelineDoesNotReplaceHostPipeline(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider("ok")))
	var hostResults atomic.Int32
	app.UsePipeline(PipelineConfig{Nodes: []PipelineNode{{
		Name:         "host",
		Message:      "go",
		Agent:        &PipelineAgentDef{Name: "host", Instruction: "x"},
		OnTaskResult: func(any) { hostResults.Add(1) },
	}}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.RunDynPipeline(ctx, &DynPipelineSpec{Nodes: []DynNodeSpec{{Name: "dyn", Instruction: "y"}}}, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- app.RunPipeline(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hostResults.Load() != 3 {
		t.Fatalf("宿主 pipeline 应运行 3 次，实际 %d", hostResults.Load())
	}
}

// 在会话运行中启动的动态 pipeline 沿用该会话的工作目录（此前被清空）。
func TestFix_DynPipelineKeepsSessionWorkDir(t *testing.T) {
	dir := t.TempDir()
	var got atomic.Value
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("m1", "launch", map[string]any{"v": ""})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("n1", "probe", map[string]any{"v": ""})}},
		{Text: "node done"},
		{Text: "main done"},
	}}
	app := New(WithProvider(prov), WithSessionWorkDir(func(string) string { return dir }))
	app.Tool("probe", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
		got.Store(ctx.WorkDir)
		return "ok", nil
	}})
	app.Tool("launch", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
		spec := &DynPipelineSpec{Nodes: []DynNodeSpec{{Name: "n1", Instruction: "probe", Tools: []string{"probe"}}}}
		return "ok", app.RunDynPipeline(ctx, spec, nil)
	}})
	drainEvents(app.RunWithSessionHistory(context.Background(), "s-1", nil, "go"))
	if got.Load() != dir {
		t.Fatalf("节点工具的工作目录 = %v，应为会话工作目录 %q", got.Load(), dir)
	}
}

// ---------------------------------------------------------------------------
// 调度与审核
// ---------------------------------------------------------------------------

// orderProvider 按节点记录模型调用的开始/结束顺序，可让指定节点变慢。
type orderProvider struct {
	mu   sync.Mutex
	log  []string
	slow map[string]time.Duration
}

func (p *orderProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (p *orderProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{Message: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "ok"}}}}, nil
}

func (p *orderProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	node, _ := ctx.Value(PipelineNodeNameKey).(string)
	p.mu.Lock()
	p.log = append(p.log, "start:"+node)
	p.mu.Unlock()
	if d := p.slow[node]; d > 0 {
		time.Sleep(d)
	}
	p.mu.Lock()
	p.log = append(p.log, "end:"+node)
	p.mu.Unlock()
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "ok"}
	ch <- provider.StreamEvent{
		Type:       provider.EventMessageComplete,
		StopReason: provider.StopEndTurn,
		Message:    &message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "ok"}}},
	}
	close(ch)
	return ch, nil
}

// DependsOn 必须等依赖真正完成。此前没有 supervisor 时所有节点一开始就被
// 标记为"审核完成"，任何一个节点结束都会连带启动所有尚未启动的节点。
func TestFix_PipelineDependsOnHonoredWithoutSupervisor(t *testing.T) {
	prov := &orderProvider{slow: map[string]time.Duration{"b": 150 * time.Millisecond}}
	node := func(name string, deps ...string) PipelineNode {
		return PipelineNode{Name: name, Message: "go", DependsOn: deps,
			Agent: &PipelineAgentDef{Name: name, Instruction: "x", MaxTurns: 1}}
	}
	p := newPipeline(PipelineConfig{Nodes: []PipelineNode{node("a"), node("b", "a"), node("c", "b")}}, &App{provider: prov})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:a", "end:a", "start:b", "end:b", "start:c", "end:c"}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if strings.Join(prov.log, ",") != strings.Join(want, ",") {
		t.Fatalf("执行顺序 = %v，应为 %v", prov.log, want)
	}
}

// 审核节点：打回后重新入队、重做、再批准；OnTaskResult 只收到被批准的真实
// 结果；下游等审核全部处置完才启动。
func TestFix_PipelineReviewRejectThenApprove(t *testing.T) {
	genProv := provider.NewMockProvider("v1", "v2")
	supProv := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s1", "reject_result", map[string]any{"node_name": "gen", "index": 0, "guidance": "more detail"})}},
		{Text: "rejected"},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s2", "approve_result", map[string]any{"node_name": "gen", "index": 0})}},
		{Text: "approved"},
	}}
	var mu sync.Mutex
	var results []any
	resultsSeenByDownstream := -1
	cfg := PipelineConfig{
		Supervisor: &PipelineAgentDef{Name: "sup", Instruction: "review", Provider: supProv},
		Nodes: []PipelineNode{
			{
				Name: "gen", Message: "make", Review: true,
				Agent: &PipelineAgentDef{Name: "gen", Instruction: "x", Provider: genProv, MaxTurns: 1},
				OnTaskResult: func(r any) {
					mu.Lock()
					results = append(results, r)
					mu.Unlock()
				},
			},
			{
				Name: "after", DependsOn: []string{"gen"},
				MessageFunc: func(context.Context) string {
					mu.Lock()
					resultsSeenByDownstream = len(results)
					mu.Unlock()
					return "go"
				},
				Agent: &PipelineAgentDef{Name: "after", Instruction: "x", Provider: provider.NewMockProvider("after ok"), MaxTurns: 1},
			},
		},
	}
	p := newPipeline(cfg, &App{provider: genProv})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0] != "v2" {
		t.Fatalf("OnTaskResult 应只收到被批准的重做结果 v2，实际 %v", results)
	}
	if resultsSeenByDownstream != 1 {
		t.Fatalf("下游应在审核处置完后才启动（启动时看到 %d 条结果）", resultsSeenByDownstream)
	}
}

// 审核轮里没被处置的结果自动批准；批准多条时每条都交付自己的结果。
func TestFix_PipelineReviewBatchPartialDisposition(t *testing.T) {
	var mu sync.Mutex
	var results []string
	supProv := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("s1", "approve_result", map[string]any{"node_name": "gen", "index": 1})}},
		{Text: "done"},
	}}
	cfg := PipelineConfig{
		Supervisor: &PipelineAgentDef{Name: "sup", Instruction: "review", Provider: supProv},
		Nodes: []PipelineNode{{
			Name: "gen", Review: true, ReviewBatch: 3, Concurrency: 1,
			Agent: &PipelineAgentDef{Name: "gen", Instruction: "x", Provider: provider.NewMockProvider("r"), MaxTurns: 1},
			OnTaskResult: func(r any) {
				mu.Lock()
				results = append(results, r.(string))
				mu.Unlock()
			},
		}},
	}
	p := newPipeline(cfg, &App{})
	for i := 0; i < 3; i++ {
		p.msgQueues["gen"].Push("task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 3 {
		t.Fatalf("3 条结果都应交付（1 条显式批准 + 2 条兜底批准），实际 %v", results)
	}
}

// 多 worker 并发产出时宿主回调串行执行（回调内无锁写共享状态，-race 下验证）。
func TestFix_PipelineCallbacksSerialized(t *testing.T) {
	var results []any
	events := map[string]int{}
	cfg := PipelineConfig{
		Nodes: []PipelineNode{{
			Name: "w", Concurrency: 4,
			Agent:        &PipelineAgentDef{Name: "w", Instruction: "x", MaxTurns: 1},
			OnTaskResult: func(r any) { results = append(results, r) },
		}},
		OnEvent: func(ev PipelineEvent) { events[ev.Type]++ },
	}
	p := newPipeline(cfg, &App{provider: provider.NewMockProvider("r")})
	for i := 0; i < 20; i++ {
		p.msgQueues["w"].Push("task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(results) != 20 || events["node_output"] != 20 {
		t.Fatalf("results=%d node_output=%d，应各为 20", len(results), events["node_output"])
	}
}

// 推送量超过队列容量、而消费者还没启动（DependsOn 生产者）时不再死锁。
func TestFix_PipelinePushBeyondQueueSizeNoDeadlock(t *testing.T) {
	type pushIn struct {
		N int `json:"n"`
	}
	push := NamedTool{Name: "push", Def: ToolDef{Input: pushIn{}, Permission: ReadOnly, Execute: func(ctx Context, in pushIn) (string, error) {
		q := GetMessageQueue(ctx, "sink")
		for i := 0; i < in.N; i++ {
			q.Push("item")
		}
		return "pushed", nil
	}}}
	prov := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("p1", "push", map[string]any{"n": 50})}},
		{Text: "src done"},
		{Text: "item done"},
	}}
	var got atomic.Int32
	cfg := PipelineConfig{Nodes: []PipelineNode{
		{Name: "src", Message: "go", Injects: []string{"sink"},
			Agent: &PipelineAgentDef{Name: "src", Instruction: "x", Tools: []NamedTool{push}, MaxTurns: 3}},
		{Name: "sink", DependsOn: []string{"src"}, QueueSize: 2, Concurrency: 2,
			Agent:        &PipelineAgentDef{Name: "sink", Instruction: "x", MaxTurns: 1},
			OnTaskResult: func(any) { got.Add(1) }},
	}}
	p := newPipeline(cfg, &App{provider: prov})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 50 {
		t.Fatalf("sink 应处理 50 条，实际 %d", got.Load())
	}
}

// 取消时：未处置的事务被回滚，Run 返回 ctx 的错误而不是节点错误。
func TestFix_PipelineCancelRollsBackOpenTransactions(t *testing.T) {
	var rolledBack, committed atomic.Int32
	block := make(chan struct{})
	supProv := &blockingProvider{release: block, started: make(chan struct{})}
	cfg := PipelineConfig{
		Supervisor: &PipelineAgentDef{Name: "sup", Instruction: "review", Provider: supProv},
		TransactionFactory: func(context.Context) Transaction {
			return &countingTx{rolledBack: &rolledBack, committed: &committed}
		},
		Nodes: []PipelineNode{{
			Name: "gen", Message: "make", Review: true,
			Agent: &PipelineAgentDef{Name: "gen", Instruction: "x", Provider: provider.NewMockProvider("v1"), MaxTurns: 1},
		}},
	}
	p := newPipeline(cfg, &App{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-supProv.started
		cancel()
	}()
	err := p.Run(ctx)
	close(block)
	if err != context.Canceled {
		t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
	}
	if rolledBack.Load() != 1 || committed.Load() != 0 {
		t.Fatalf("审核中的事务应回滚: rolledBack=%d committed=%d", rolledBack.Load(), committed.Load())
	}
}

type countingTx struct {
	rolledBack, committed *atomic.Int32
}

func (c *countingTx) Rollback(context.Context) error { c.rolledBack.Add(1); return nil }
func (c *countingTx) Commit()                        { c.committed.Add(1) }

// blockingProvider 的 Stream 阻塞到 ctx 取消（模拟审核中的长调用）。
type blockingProvider struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (b *blockingProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (b *blockingProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, context.Canceled
}

func (b *blockingProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return nil, context.Canceled
	}
}
