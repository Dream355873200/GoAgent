// subagents_b3_test.go 子代理治理（v0.14 B3）：同步子代理可寻址与运行中指示、
// 按子代理中断而父运行继续、并发上限、事件全量转发、未送达指示转交父运行、
// 子代理内审批带归属、HTTP 端点、会话关闭中断子代理、后台子代理按 agent_id 停止。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// agentScript 按系统提示词里的标记区分调用方（主运行 MAIN / 子代理 SUB），
// 各自按调用次数取动作："tool:a,b" 同一轮发起多个工具调用，其他为文本。
// 记录每个调用方每次请求的最后一条用户侧文本，hook 在流开始前被调用。
type agentScript struct {
	steps map[string][]string
	hook  func(key string, call int)

	mu       sync.Mutex
	calls    map[string]int
	requests map[string][]string
}

func (p *agentScript) Capabilities() provider.Capabilities {
	return provider.Capabilities{SupportsTools: true}
}

func (p *agentScript) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}

func (p *agentScript) keyOf(req *provider.Request) string {
	key := ""
	for k := range p.steps {
		if strings.Contains(req.SystemPrompt, k) && len(k) > len(key) {
			key = k
		}
	}
	return key
}

func (p *agentScript) allText(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.requests[key], "\n---\n")
}

func (p *agentScript) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	key := p.keyOf(req)
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role == message.RoleUser {
			sb.WriteString(message.ExtractText(m))
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					sb.WriteString(b.Text)
				}
			}
			sb.WriteString("\n")
		}
	}
	p.mu.Lock()
	if p.calls == nil {
		p.calls, p.requests = map[string]int{}, map[string][]string{}
	}
	i := p.calls[key]
	p.calls[key]++
	p.requests[key] = append(p.requests[key], sb.String())
	p.mu.Unlock()
	if p.hook != nil {
		p.hook(key, i)
	}
	step := "done"
	if i < len(p.steps[key]) {
		step = p.steps[key][i]
	}
	ch := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(ch)
		if names, ok := strings.CutPrefix(step, "tool:"); ok {
			var blocks []message.ContentBlock
			for j, name := range strings.Split(names, ",") {
				id := fmt.Sprintf("%s-%d-%d-%s", key, i, j, name)
				input := json.RawMessage(`{}`)
				if strings.HasPrefix(name, "Agent_") {
					input = json.RawMessage(`{"task":"do it"}`)
				}
				if strings.HasSuffix(name, "_bg") {
					name = strings.TrimSuffix(name, "_bg")
					input = json.RawMessage(`{"task":"do it","run_in_background":true}`)
				}
				ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{ID: id, Name: name, Input: input}}
				blocks = append(blocks, message.ContentBlock{Type: "tool_use", ToolUseID: id, ToolName: name, Input: input})
			}
			ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopToolUse,
				Message: &message.Message{Role: message.RoleAssistant, Content: blocks}}
			return
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: step}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn,
			Message: &message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: step}}}}
	}()
	return ch, nil
}

// subApp 组装一个主运行 + 子代理 "sub" 的 App；子代理只有工具 probe。
func subApp(t *testing.T, prov *agentScript, probe func(ctx Context) (string, error), opts ...Option) *App {
	t.Helper()
	base := []Option{
		WithProvider(prov),
		WithSystemPrompt("MAIN"),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithApprover(AutoApprover()),
	}
	app := New(append(base, opts...)...)
	app.Tool("probe", ToolDef{Description: "probe", Input: struct{}{}, Permission: ReadOnly, Concurrent: true,
		Execute: func(ctx Context, in struct{}) (string, error) { return probe(ctx) }})
	tools, err := app.AgentTools("probe")
	if err != nil {
		t.Fatal(err)
	}
	app.Tool("Agent_sub", app.AgentTool(agent.Definition{Name: "sub", SystemPrompt: "SUB", Tools: tools, MaxTurns: 6}))
	return app
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runningAgent(app *App, sessionID string) (SubagentInfo, bool) {
	for _, info := range app.Subagents(sessionID) {
		if info.Status == AgentRunning {
			return info, true
		}
	}
	return SubagentInfo{}, false
}

// 同步子代理以 tool_use_id 登记、可查询；运行中指示在子代理自己的工具批边界
// 生效，不进入主运行；结束后状态与结果保留。
func TestB3_SyncSubagentSteer(t *testing.T) {
	release := make(chan struct{})
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub", "main done"},
		"SUB":  {"tool:probe", "sub done"},
	}}
	app := subApp(t, prov, func(Context) (string, error) { <-release; return "probed", nil })

	events := app.RunSession(context.Background(), "b3-steer", "go")
	var all []Event
	done := make(chan struct{})
	go func() { all = collect(events); close(done) }()

	var info SubagentInfo
	waitFor(t, "子代理登记", func() bool { var ok bool; info, ok = runningAgent(app, "b3-steer"); return ok })
	if info.ID != "MAIN-0-0-Agent_sub" || info.Path != info.ID || info.Name != "sub" || info.Background {
		t.Fatalf("同步子代理应以 tool_use_id 登记: %+v", info)
	}
	if err := app.SteerAgent("b3-steer", info.ID, "只看 src 目录"); err != nil {
		t.Fatalf("SteerAgent: %v", err)
	}
	close(release)
	<-done

	if !strings.Contains(prov.allText("SUB"), "只看 src 目录") {
		t.Fatal("指示应在子代理的下一次模型请求中出现")
	}
	if strings.Contains(prov.allText("MAIN"), "只看 src 目录") {
		t.Fatal("给子代理的指示不应进入主运行")
	}
	got, msgs, ok := app.Subagent("b3-steer", info.ID)
	if !ok || got.Status != AgentDone || got.Result != "sub done" || got.EndedAt == nil || len(msgs) == 0 {
		t.Fatalf("结束后应保留终态、结果与对话记录: %+v (%d 条消息)", got, len(msgs))
	}
	if err := app.SteerAgent("b3-steer", info.ID, "迟到的指示"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("已结束的子代理应返回 ErrAgentNotFound, got %v", err)
	}
	if all[len(all)-1].Type != EventDone {
		t.Fatalf("主运行应正常完成, got %v", all[len(all)-1].Type)
	}
}

// InterruptAgent 只中断子代理：主运行收到明确的"被用户中断"错误结果后继续完成。
func TestB3_InterruptAgentParentContinues(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub", "main done"},
		"SUB":  {"tool:probe", "sub done"},
	}}
	app := subApp(t, prov, func(ctx Context) (string, error) { <-ctx.Done(); return "", ctx.Err() })

	events := app.RunSession(context.Background(), "b3-int", "go")
	var all []Event
	done := make(chan struct{})
	go func() { all = collect(events); close(done) }()
	var info SubagentInfo
	waitFor(t, "子代理登记", func() bool { var ok bool; info, ok = runningAgent(app, "b3-int"); return ok })
	if err := app.InterruptAgent("b3-int", info.ID, "方向错了"); err != nil {
		t.Fatalf("InterruptAgent: %v", err)
	}
	<-done

	var result *Event
	for i := range all {
		if all[i].Type == EventToolDone && all[i].ToolName == "Agent_sub" {
			result = &all[i]
		}
	}
	if result == nil || !result.ToolIsError || !strings.Contains(result.ToolResult, "被用户中断") || !strings.Contains(result.ToolResult, "方向错了") {
		t.Fatalf("主运行应收到被中断的错误结果, got %+v", result)
	}
	if all[len(all)-1].Type != EventDone {
		t.Fatalf("主运行应继续并正常完成, got %v", all[len(all)-1].Type)
	}
	if got, _, _ := app.Subagent("b3-int", info.ID); got.Status != AgentCancelled || got.Activity != "方向错了" {
		t.Fatalf("子代理状态应为 cancelled 并记下原因: %+v", got)
	}
	var cancelled bool
	for _, ev := range all {
		if ev.Type == EventSubAgentProgress && ev.AgentStatus == AgentCancelled && ev.AgentPath == info.ID {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("应发出 status=cancelled 且带归属路径的 subagent_progress")
	}
}

// 同时运行的子代理数达到上限时，多出来的 Agent_* 调用返回错误。
func TestB3_ConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub,Agent_sub", "main done"},
		"SUB":  {"tool:probe", "sub done"},
	}}
	app := subApp(t, prov, func(Context) (string, error) { <-release; return "ok", nil }, WithMaxSubagents(1))
	events := app.RunSession(context.Background(), "b3-cap", "go")
	var all []Event
	done := make(chan struct{})
	go func() { all = collect(events); close(done) }()
	waitFor(t, "第一个子代理运行", func() bool { _, ok := runningAgent(app, "b3-cap"); return ok })
	// 第二个调用撞上上限会立即以错误结束；随后放行第一个。
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	var capped int
	for _, ev := range all {
		if ev.Type == EventToolDone && ev.ToolName == "Agent_sub" && ev.ToolIsError && strings.Contains(ev.ToolResult, "上限") {
			capped++
		}
	}
	if capped != 1 {
		t.Fatalf("应恰好一个调用因并发上限失败, got %d", capped)
	}
}

// 默认只发 subagent_progress；full 模式下子代理的正文与工具事件带 AgentPath 进入
// 父流，但不进入父运行的对话记录。
func TestB3_SubagentStreamModes(t *testing.T) {
	for _, mode := range []string{SubagentStreamSummary, SubagentStreamFull} {
		t.Run(mode, func(t *testing.T) {
			prov := &agentScript{steps: map[string][]string{
				"MAIN": {"tool:Agent_sub", "main done"},
				"SUB":  {"tool:probe", "sub-final-text"},
			}}
			mgr := session.NewManager(session.NewMemoryStore())
			app := subApp(t, prov, func(Context) (string, error) { return "ok", nil }, WithSubagentStream(mode), WithSessionManager(mgr))
			all := collect(app.RunSession(context.Background(), "b3-mode", "go"))
			var subText, subTool bool
			for _, ev := range all {
				if ev.AgentPath == "" {
					continue
				}
				switch {
				case ev.Type == EventTextDelta && ev.Text == "sub-final-text":
					subText = true
				case ev.Type == EventToolStart && ev.ToolName == "probe":
					subTool = true
				case ev.Type == EventTextDelta || ev.Type == EventToolStart:
					t.Fatalf("意外的带路径事件: %+v", ev)
				}
			}
			if full := mode == SubagentStreamFull; subText != full || subTool != full {
				t.Fatalf("模式 %s：子代理正文转发=%v 工具转发=%v", mode, subText, subTool)
			}
			sess, _ := mgr.Get(context.Background(), "b3-mode")
			for _, m := range sess.Messages {
				if m.Role == message.RoleAssistant && strings.Contains(message.ExtractText(m), "sub-final-text") {
					t.Fatal("子代理的正文不应作为助手消息进入父运行的对话记录")
				}
			}
		})
	}
}

// 子代理在指示送达前结束：指示转交给父运行，在父运行的下一个工具批边界生效。
func TestB3_LeftoverGuideGoesToParent(t *testing.T) {
	inFinal := make(chan struct{})
	steered := make(chan struct{})
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub", "main done"},
		"SUB":  {"tool:probe", "sub done"},
	}}
	prov.hook = func(key string, call int) {
		if key == "SUB" && call == 1 {
			close(inFinal)
			<-steered
		}
	}
	app := subApp(t, prov, func(Context) (string, error) { return "ok", nil }, WithSteering())
	events := app.RunSession(context.Background(), "b3-left", "go")
	done := make(chan struct{})
	go func() { collect(events); close(done) }()
	<-inFinal
	info, ok := runningAgent(app, "b3-left")
	if !ok {
		t.Fatal("子代理应在运行")
	}
	if err := app.SteerAgent("b3-left", info.ID, "补充：用中文写"); err != nil {
		t.Fatal(err)
	}
	close(steered)
	<-done
	main := prov.allText("MAIN")
	if !strings.Contains(main, "补充：用中文写") || !strings.Contains(main, "未送达") {
		t.Fatalf("未送达的指示应转交给主运行, 主运行请求:\n%s", main)
	}
}

// 子代理里发起的审批带归属路径；HTTP 端点：/agents 列出、/chat 与 /queue 带
// agent_id 投递指示、/interrupt 带 agent_id 只中断子代理。
func TestB3_HTTPSurface(t *testing.T) {
	ph := NewPermissionHandler()
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub", "main done"},
		"SUB":  {"tool:danger", "tool:probe", "tool:probe", "sub done"},
	}}
	app := New(
		WithProvider(prov),
		WithSystemPrompt("MAIN"),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithApprover(ph),
		WithYoloClassifier(false),
	)
	gate := make(chan struct{})
	second := make(chan struct{})
	var probes int
	var probeMu sync.Mutex
	app.Tool("danger", ToolDef{Description: "danger", Input: struct{}{}, Permission: Normal,
		Execute: func(ctx Context, in struct{}) (string, error) { return "did", nil }})
	app.Tool("probe", ToolDef{Description: "probe", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			probeMu.Lock()
			probes++
			n := probes
			probeMu.Unlock()
			if n == 1 {
				<-gate
				return "first", nil
			}
			close(second)
			<-ctx.Done()
			return "", ctx.Err()
		}})
	tools, _ := app.AgentTools("danger", "probe")
	app.Tool("Agent_sub", app.AgentTool(agent.Definition{Name: "sub", SystemPrompt: "SUB", Tools: tools, MaxTurns: 6}))
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"go","session_id":"b3-http"}`, nil)
	defer st.close()
	// 第一个审批是 Agent_sub 自己（主运行），第二个来自子代理里的 danger。
	first := st.until(t, isType(protocol.TypeNeedApproval))
	if f := first[len(first)-1]; f.ToolName != "Agent_sub" || f.AgentPath != "" {
		t.Fatalf("主运行的审批不应带归属路径: %+v", f)
	}
	approve(t, srv.URL, first[len(first)-1].RequestID)
	frames := st.until(t, isType(protocol.TypeNeedApproval))
	perm := frames[len(frames)-1]
	if perm.ToolName != "danger" || perm.AgentPath != "MAIN-0-0-Agent_sub" {
		t.Fatalf("子代理里的审批应带归属路径: %+v", perm)
	}
	approve(t, srv.URL, perm.RequestID)

	var listed []SubagentInfo
	waitFor(t, "子代理出现在 /agents", func() bool {
		listed = nil
		getJSON(t, srv.URL+"/agents?session_id=b3-http", &listed)
		return len(listed) == 1 && listed[0].Status == AgentRunning
	})
	id := listed[0].ID

	steer := openSSEStream(t, "POST", srv.URL+"/chat", fmt.Sprintf(`{"message":"注意边界","session_id":"b3-http","agent_id":%q}`, id), nil)
	sf := steer.rest(t)
	steer.close()
	if len(sf) != 2 || sf[0].Type != protocol.TypeSteer || sf[0].AgentPath != id || !sf[1].Steered {
		t.Fatalf("/chat 带 agent_id 应回执 steer{agent_path} + metadata{steered}, got %s", frameTypes(sf))
	}
	bad := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"x","session_id":"b3-http","agent_id":"nope"}`, nil)
	bf := bad.rest(t)
	bad.close()
	if len(bf) != 2 || bf[0].Type != protocol.TypeError {
		t.Fatalf("不存在的子代理应返回 error, got %s", frameTypes(bf))
	}
	resp := postJSON(t, srv.URL+"/queue", fmt.Sprintf(`{"message":"再补一句","session_id":"b3-http","agent_id":%q}`, id))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/queue 带 agent_id status %d", resp.StatusCode)
	}
	close(gate) // 放行第一次 probe：工具批边界注入两条指示，子代理带着它们发起下一次请求
	<-second
	if sub := prov.allText("SUB"); !strings.Contains(sub, "注意边界") || !strings.Contains(sub, "再补一句") {
		t.Fatalf("经 /chat 与 /queue 投递的指示应到达子代理, 子代理请求: %s", sub)
	}

	resp = postJSON(t, srv.URL+"/interrupt", fmt.Sprintf(`{"session_id":"b3-http","agent_id":%q,"reason":"够了"}`, id))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/interrupt 带 agent_id status %d", resp.StatusCode)
	}
	rest := st.rest(t)
	var sawDone bool
	for _, f := range rest {
		if f.Type == protocol.TypeDone {
			sawDone = true
		}
	}
	if !sawDone || rest[len(rest)-1].Type != protocol.TypeMetadata {
		t.Fatalf("只中断子代理时主运行应正常完成, got %s", frameTypes(rest))
	}
	var detail struct {
		Agent    SubagentInfo      `json:"agent"`
		Messages []message.Message `json:"messages"`
	}
	getJSON(t, srv.URL+"/agents/"+id+"?session_id=b3-http", &detail)
	if detail.Agent.Status != AgentCancelled || detail.Agent.Activity != "够了" {
		t.Fatalf("/agents/{id} 应返回终态与原因: %+v", detail.Agent)
	}
}

func approve(t *testing.T, base, requestID string) {
	t.Helper()
	resp := postJSON(t, base+"/approve", fmt.Sprintf(`{"request_id":%q,"allow":true}`, requestID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/approve status %d", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// 后台子代理：同样登记（带 task_id），可按 agent_id 经 TaskStop 停止；会话关闭时
// 运行中的子代理被中断。
func TestB3_BackgroundSubagent(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub_bg", "main done"},
		"SUB":  {"tool:probe", "sub done"},
	}}
	app := subApp(t, prov, func(ctx Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }, WithBgTaskTools())
	collect(app.RunSession(context.Background(), "b3-bg", "go"))
	var info SubagentInfo
	waitFor(t, "后台子代理登记", func() bool { var ok bool; info, ok = runningAgent(app, "b3-bg"); return ok })
	if !info.Background || info.TaskID == "" {
		t.Fatalf("后台子代理应带 task_id: %+v", info)
	}
	if bt := app.BgTasks().Get(info.ID); bt == nil || bt.ID != info.TaskID {
		t.Fatal("bgtask 应能按 agent_id（tool_use_id）查到任务")
	}
	app.CloseSession("b3-bg")
	waitFor(t, "后台子代理被中断", func() bool {
		bt := app.BgTasks().Get(info.TaskID)
		return bt != nil && bt.Status.IsTerminal()
	})
}
