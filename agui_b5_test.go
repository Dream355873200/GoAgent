// agui_b5_test.go AG-UI 1.0 剖面（v0.14 B5）：每个发出的事件用官方 schema 校验，
// 并检查流级规则（首帧 RUN_STARTED、终止帧之后无事件、结束前一切已关闭、
// 子代理归属先开后用）；覆盖历史去重、审批与提问停车/恢复、停车超时、取消、
// 错误、自定义事件与子代理映射、能力描述。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/protocol/agui"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/google/jsonschema-go/jsonschema"
)

// aguiSchemas 按定义名缓存解析好的官方 schema（以 #/$defs/<名> 为入口）。
var aguiSchemas = struct {
	sync.Mutex
	raw  map[string]any
	defs map[string]*jsonschema.Resolved
}{defs: map[string]*jsonschema.Resolved{}}

func aguiSchema(t *testing.T, def string) *jsonschema.Resolved {
	t.Helper()
	aguiSchemas.Lock()
	defer aguiSchemas.Unlock()
	if rs := aguiSchemas.defs[def]; rs != nil {
		return rs
	}
	if aguiSchemas.raw == nil {
		data, err := os.ReadFile("protocol/agui/testdata/schema-1.0.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &aguiSchemas.raw); err != nil {
			t.Fatal(err)
		}
	}
	doc := map[string]any{}
	for k, v := range aguiSchemas.raw {
		doc[k] = v
	}
	doc["$ref"] = "#/$defs/" + def
	data, _ := json.Marshal(doc)
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", def, err)
	}
	aguiSchemas.defs[def] = rs
	return rs
}

// aguiDefOf 把事件类型名换成 schema 中的定义名：TEXT_MESSAGE_START → TextMessageStartEvent。
func aguiDefOf(typ string) string {
	var b strings.Builder
	for _, part := range strings.Split(strings.ToLower(typ), "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String() + "Event"
}

// validateAGUI 用官方 schema 校验一个对象（def 为定义名）。
func validateAGUI(t *testing.T, def string, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("不是 JSON: %v (%s)", err, raw)
	}
	if err := aguiSchema(t, def).Validate(v); err != nil {
		t.Fatalf("%s 不符合 AG-UI 1.0 schema: %v\n%s", def, err, raw)
	}
}

// aguiFrame 是收到的一个 AG-UI 事件（原始 JSON + 常用字段）。
type aguiFrame struct {
	raw json.RawMessage
	agui.Event
	Outcome *struct {
		Type               string           `json:"type"`
		Interrupts         []agui.Interrupt `json:"interrupts"`
		PendingToolCallIDs []string         `json:"pendingToolCallIds"`
	} `json:"outcome"`
}

// postAGUI 发一次 POST /agui，读完事件流；每帧做 schema 校验，整条流做规则检查。
func postAGUI(t *testing.T, base string, in agui.RunAgentInput, resumed bool) []aguiFrame {
	t.Helper()
	body, _ := json.Marshal(in)
	validateAGUI(t, "RunAgentInput", body)
	st := openSSEStream(t, "POST", base+"/agui", string(body), nil)
	defer st.close()
	var frames []aguiFrame
	for st.sc.Scan() {
		line := st.sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := json.RawMessage(strings.TrimPrefix(line, "data: "))
		var f aguiFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("帧解析失败: %v (%s)", err, raw)
		}
		f.raw = raw
		validateAGUI(t, aguiDefOf(f.Type), raw)
		frames = append(frames, f)
	}
	checkAGUIStream(t, frames, resumed)
	return frames
}

// checkAGUIStream 检查流级规则（AG-UI 客户端校验器在这些地方会让运行失败）。
func checkAGUIStream(t *testing.T, frames []aguiFrame, resumed bool) {
	t.Helper()
	if len(frames) == 0 || frames[0].Type != agui.TypeRunStarted {
		t.Fatalf("首帧必须是 RUN_STARTED: %s", aguiTypes(frames))
	}
	last := frames[len(frames)-1].Type
	if last != agui.TypeRunFinished && last != agui.TypeRunError {
		t.Fatalf("末帧必须是 RUN_FINISHED 或 RUN_ERROR: %s", aguiTypes(frames))
	}
	text, reasoning, tools, subs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	seenTool := map[string]bool{}
	for i, f := range frames {
		if i > 0 && (f.Type == agui.TypeRunStarted) {
			t.Fatalf("RUN_STARTED 只能出现一次: %s", aguiTypes(frames))
		}
		if i < len(frames)-1 && (f.Type == agui.TypeRunFinished || f.Type == agui.TypeRunError) {
			t.Fatalf("终止帧之后不得再有事件: %s", aguiTypes(frames))
		}
		if f.SubagentRunID != "" && f.Type != agui.TypeSubagentStarted && !subs[f.SubagentRunID] {
			t.Fatalf("帧 %d %s 归属的子代理 %s 未开始（或已结束）", i, f.Type, f.SubagentRunID)
		}
		switch f.Type {
		case agui.TypeTextMessageStart:
			if text[f.MessageID] {
				t.Fatalf("文本消息 %s 重复打开", f.MessageID)
			}
			text[f.MessageID] = true
		case agui.TypeTextMessageContent:
			if !text[f.MessageID] {
				t.Fatalf("文本内容属于未打开的消息 %s", f.MessageID)
			}
		case agui.TypeTextMessageEnd:
			if !text[f.MessageID] {
				t.Fatalf("关闭未打开的消息 %s", f.MessageID)
			}
			delete(text, f.MessageID)
		case agui.TypeReasoningStart:
			reasoning[f.MessageID] = true
		case agui.TypeReasoningMessageContent, agui.TypeReasoningMessageStart, agui.TypeReasoningMessageEnd:
			if !reasoning[f.MessageID] {
				t.Fatalf("%s 不在 REASONING_START/END 之内", f.Type)
			}
		case agui.TypeReasoningEnd:
			delete(reasoning, f.MessageID)
		case agui.TypeToolCallStart:
			tools[f.ToolCallID] = true
			seenTool[f.ToolCallID] = true
		case agui.TypeToolCallArgs:
			if !tools[f.ToolCallID] {
				t.Fatalf("参数属于未开始的工具调用 %s", f.ToolCallID)
			}
		case agui.TypeToolCallEnd:
			delete(tools, f.ToolCallID)
		case agui.TypeToolCallResult:
			if !seenTool[f.ToolCallID] && !resumed {
				t.Fatalf("结果属于本流没有开始过的工具调用 %s", f.ToolCallID)
			}
		case agui.TypeSubagentStarted:
			subs[f.SubagentRunID] = true
		case agui.TypeSubagentFinished, agui.TypeSubagentError:
			delete(subs, f.SubagentRunID)
		}
	}
	if last == agui.TypeRunFinished && (len(text) > 0 || len(reasoning) > 0 || len(tools) > 0 || len(subs) > 0) {
		t.Fatalf("RUN_FINISHED 前仍有未关闭的消息/推理/工具/子代理: %v %v %v %v", text, reasoning, tools, subs)
	}
}

func aguiTypes(frames []aguiFrame) string {
	types := make([]string, len(frames))
	for i, f := range frames {
		types[i] = f.Type
	}
	return strings.Join(types, ",")
}

func findFrame(frames []aguiFrame, typ string) *aguiFrame {
	for i := range frames {
		if frames[i].Type == typ {
			return &frames[i]
		}
	}
	return nil
}

func userMsg(id, text string) agui.Message { return agui.Message{ID: id, Role: "user", Content: text} }

func aguiServer(t *testing.T, app *App) string {
	t.Helper()
	srv := httptest.NewServer(newHTTPMux(app))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 基本运行：工具调用 + 正文；MESSAGES_SNAPSHOT 的消息 id 与流式 id、客户端
// 的 user 消息 id 一致；第二次请求按 id 去重，不会把第一条消息再喂一遍。
func TestB5_RunSnapshotAndDedupe(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{"MAIN": {"tool:probe", "第一轮完成", "第二轮完成"}}}
	app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	app.Tool("probe", ToolDef{Description: "probe", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) { return "probed", nil }})
	base := aguiServer(t, app)

	frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-a", RunID: "r1", Messages: []agui.Message{userMsg("u1", "你好")}}, false)
	if f := frames[0]; f.ProtocolVersion != "1.0" || f.ThreadID != "b5-a" || f.RunID != "r1" {
		t.Fatalf("RUN_STARTED 应声明协议版本并回显 thread/run: %+v", f.Event)
	}
	start := findFrame(frames, agui.TypeToolCallStart)
	if start == nil || start.ParentMessageID == "" {
		t.Fatal("TOOL_CALL_START 必须带 parentMessageId")
	}
	textStart := findFrame(frames, agui.TypeTextMessageStart)
	snap := findFrame(frames, agui.TypeMessagesSnapshot)
	if snap == nil || textStart == nil {
		t.Fatalf("应有文本消息与 MESSAGES_SNAPSHOT: %s", aguiTypes(frames))
	}
	ids := map[string]string{}
	for _, m := range snap.Messages {
		ids[m.ID] = m.Role
	}
	if ids["u1"] != "user" || ids[start.ParentMessageID] != "assistant" || ids[textStart.MessageID] != "assistant" {
		t.Fatalf("快照 id 应与客户端 user 消息及流式消息一致: %v", ids)
	}
	if r := findFrame(frames, agui.TypeToolCallResult); r == nil || ids[r.MessageID] != "tool" {
		t.Fatal("TOOL_CALL_RESULT 的 messageId 应与快照中的 tool 消息一致")
	}
	if frames[len(frames)-1].Type != agui.TypeRunFinished || frames[len(frames)-1].Outcome != nil {
		t.Fatalf("应以成功的 RUN_FINISHED 结束: %s", frames[len(frames)-1].raw)
	}

	history := append(append([]agui.Message{}, snap.Messages...), userMsg("u2", "继续"))
	postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-a", RunID: "r2", Messages: history}, false)
	lastReq := prov.requests["MAIN"][len(prov.requests["MAIN"])-1]
	if strings.Count(lastReq, "你好") != 1 || strings.Count(lastReq, "继续") != 1 {
		t.Fatalf("第二次请求应只把新消息作为输入（按 id 去重）, 模型看到:\n%s", lastReq)
	}

	errFrames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-a", RunID: "r3", Messages: history}, false)
	if f := errFrames[len(errFrames)-1]; f.Type != agui.TypeRunError || f.Code != "no_input" {
		t.Fatalf("没有新 user 消息时应 RUN_ERROR{no_input}: %s", f.raw)
	}
}

// 审批停车：本次运行以 interrupt 结束；resume 批准后新流直接给出原工具调用
// 的结果并继续；拒绝时工具结果为权限被拒绝。
func TestB5_ApprovalParkAndResume(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint("approved=", approve), func(t *testing.T) {
			prov := &agentScript{steps: map[string][]string{"MAIN": {"tool:danger", "收尾"}}}
			var ran bool
			app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(session.NewManager(session.NewMemoryStore())),
				WithApprover(NewPermissionHandler()), WithYoloClassifier(false))
			app.Tool("danger", ToolDef{Description: "danger", Input: struct{}{}, Permission: Normal,
				Execute: func(ctx Context, in struct{}) (string, error) { ran = true; return "did it", nil }})
			base := aguiServer(t, app)

			frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-p", RunID: "r1", Messages: []agui.Message{userMsg("u1", "做吧")}}, false)
			end := frames[len(frames)-1]
			if end.Outcome == nil || end.Outcome.Type != "interrupt" || len(end.Outcome.Interrupts) != 1 {
				t.Fatalf("应以 interrupt 结束: %s", end.raw)
			}
			it := end.Outcome.Interrupts[0]
			start := findFrame(frames, agui.TypeToolCallStart)
			if it.Reason != "tool_call" || it.ToolCallID != start.ToolCallID || it.ResponseSchema == nil || it.ExpiresAt == "" {
				t.Fatalf("中断应指向工具调用并带 responseSchema/expiresAt: %+v", it)
			}

			blocked := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-p", RunID: "rx", Messages: []agui.Message{userMsg("u9", "插一句")}}, false)
			if f := blocked[len(blocked)-1]; f.Type != agui.TypeRunError || f.Code != "pending_interrupts" {
				t.Fatalf("有待作答的中断时新请求应 RUN_ERROR{pending_interrupts}: %s", f.raw)
			}

			payload, _ := json.Marshal(map[string]any{"approved": approve})
			resumed := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-p", RunID: "r2", Messages: []agui.Message{userMsg("u1", "做吧")},
				Resume: []agui.ResumeEntry{{InterruptID: it.ID, Status: "resolved", Payload: payload}}}, true)
			if findFrame(resumed, agui.TypeToolCallStart) != nil {
				t.Fatal("恢复后的运行不应重发 TOOL_CALL_START")
			}
			res := findFrame(resumed, agui.TypeToolCallResult)
			if res == nil || res.ToolCallID != it.ToolCallID {
				t.Fatalf("恢复后应直接给出原工具调用的结果: %s", aguiTypes(resumed))
			}
			content, _ := res.Content.(string)
			if approve && (content != "did it" || !ran) {
				t.Fatalf("批准后工具应执行: %q", content)
			}
			if !approve && (ran || !strings.Contains(content, "权限被拒绝")) {
				t.Fatalf("拒绝后工具不应执行: %q", content)
			}
			if f := resumed[len(resumed)-1]; f.Type != agui.TypeRunFinished || f.Outcome != nil {
				t.Fatalf("恢复后的运行应成功结束: %s", f.raw)
			}
			again := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-p", RunID: "r3", Messages: []agui.Message{userMsg("u1", "做吧")},
				Resume: []agui.ResumeEntry{{InterruptID: it.ID, Status: "resolved", Payload: payload}}}, true)
			if f := again[len(again)-1]; f.Type != agui.TypeRunError || f.Code != "no_pending_interrupt" {
				t.Fatalf("重复 resume 应 RUN_ERROR{no_pending_interrupt}: %s", f.raw)
			}
		})
	}
}

// 提问停车：reason=ask_user，resume 的 answer 交给工具。
func TestB5_AskUserParkAndResume(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{"MAIN": {"tool:ask", "好的"}}}
	app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	app.SetAskUserHandler(NewAskUserHandler())
	var answer string
	app.Tool("ask", ToolDef{Description: "ask", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			a, err := AskUserHandlerFromContext(ctx).AskSessionCtx(ctx, ctx.SessionID, "喜欢什么颜色？", map[string]any{"kind": "choice"})
			answer = a
			return "answered: " + a, err
		}})
	base := aguiServer(t, app)
	frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-q", RunID: "r1", Messages: []agui.Message{userMsg("u1", "问我")}}, false)
	end := frames[len(frames)-1]
	if end.Outcome == nil || len(end.Outcome.Interrupts) != 1 || end.Outcome.Interrupts[0].Reason != "ask_user" || end.Outcome.Interrupts[0].Message != "喜欢什么颜色？" {
		t.Fatalf("提问应以 ask_user 中断停车: %s", end.raw)
	}
	it := end.Outcome.Interrupts[0]
	if it.Metadata["goagent.payload"] == nil {
		t.Fatal("结构化载荷应放在 metadata 的 goagent.payload")
	}
	resumed := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-q", RunID: "r2", Messages: []agui.Message{userMsg("u1", "问我")},
		Resume: []agui.ResumeEntry{{InterruptID: it.ID, Status: "resolved", Payload: json.RawMessage(`{"answer":"蓝色"}`)}}}, true)
	if answer != "蓝色" || resumed[len(resumed)-1].Type != agui.TypeRunFinished {
		t.Fatalf("回答应交给工具并完成运行: answer=%q %s", answer, aguiTypes(resumed))
	}
}

// 停车超时按拒绝处理：之后的 resume 得到 no_pending_interrupt。
func TestB5_ParkTimeout(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{"MAIN": {"tool:danger", "收尾"}}}
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(mgr),
		WithApprover(NewPermissionHandler()), WithYoloClassifier(false), WithAGUI(AGUIConfig{ParkTimeout: 50 * time.Millisecond}))
	app.Tool("danger", ToolDef{Description: "danger", Input: struct{}{}, Permission: Normal,
		Execute: func(ctx Context, in struct{}) (string, error) { return "did it", nil }})
	base := aguiServer(t, app)
	frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-t", RunID: "r1", Messages: []agui.Message{userMsg("u1", "做吧")}}, false)
	it := frames[len(frames)-1].Outcome.Interrupts[0]
	waitIdle(t, mgr, "b5-t")
	resumed := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-t", RunID: "r2", Messages: []agui.Message{userMsg("u1", "做吧")},
		Resume: []agui.ResumeEntry{{InterruptID: it.ID, Status: "resolved", Payload: json.RawMessage(`{"approved":true}`)}}}, true)
	if f := resumed[len(resumed)-1]; f.Code != "no_pending_interrupt" {
		t.Fatalf("超时后 resume 应 RUN_ERROR{no_pending_interrupt}: %s", f.raw)
	}
	sess, _ := mgr.Get(context.Background(), "b5-t")
	var denied bool
	for _, m := range sess.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && strings.Contains(b.Text, "超时") {
				denied = true
			}
		}
	}
	if !denied {
		t.Fatal("停车超时应按拒绝处理（工具结果说明超时）")
	}
}

// 用户中断：RUN_FINISHED{outcome: cancelled}；模型报错：RUN_ERROR。
func TestB5_CancelledAndError(t *testing.T) {
	prov := &agentScript{steps: map[string][]string{"MAIN": {"tool:block", "x"}}}
	app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	app.SetInterruptHandler(NewInterruptHandler())
	entered := make(chan struct{})
	app.Tool("block", ToolDef{Description: "block", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) { close(entered); <-ctx.Done(); return "", ctx.Err() }})
	base := aguiServer(t, app)
	go func() {
		<-entered
		resp := postJSON(t, base+"/interrupt", `{"session_id":"b5-c","reason":"停"}`)
		resp.Body.Close()
	}()
	frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b5-c", RunID: "r1", Messages: []agui.Message{userMsg("u1", "跑")}}, false)
	if end := frames[len(frames)-1]; end.Outcome == nil || end.Outcome.Type != "cancelled" {
		t.Fatalf("中断应以 cancelled 结束: %s", end.raw)
	}

	failing := New(WithProvider(errProvider{}), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	frames = postAGUI(t, aguiServer(t, failing), agui.RunAgentInput{ThreadID: "b5-e", RunID: "r1", Messages: []agui.Message{userMsg("u1", "跑")}}, false)
	if end := frames[len(frames)-1]; end.Type != agui.TypeRunError || !strings.Contains(end.Message, "模型不可用") {
		t.Fatalf("模型报错应以 RUN_ERROR 结束: %s", end.raw)
	}
}

// errProvider 每次请求都失败。
type errProvider struct{}

func (errProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (errProvider) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("模型不可用")
}
func (errProvider) Stream(context.Context, *provider.Request) (<-chan provider.StreamEvent, error) {
	return nil, errors.New("模型不可用")
}

// reasoningScript 在 agentScript 的每次响应前先流出一段思考。
type reasoningScript struct{ agentScript }

func (p *reasoningScript) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	inner, err := p.agentScript.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(out)
		out <- provider.StreamEvent{Type: provider.EventThinkingDelta, Thinking: "先想一想"}
		for ev := range inner {
			out <- ev
		}
	}()
	return out, nil
}

// 自定义事件、思考、子代理（全量转发）都映射为合法的 AG-UI 事件。
func TestB5_CustomReasoningAndSubagents(t *testing.T) {
	prov := &reasoningScript{agentScript: agentScript{steps: map[string][]string{
		"MAIN": {"tool:Agent_sub", "主运行完成"},
		"SUB":  {"tool:emit", "子代理完成"},
	}}}
	app := New(WithProvider(prov), WithSystemPrompt("MAIN"), WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithApprover(AutoApprover()), WithSubagentStream(SubagentStreamFull))
	app.Tool("emit", emitterTool("acme.canvas_command"))
	tools, _ := app.AgentTools("emit")
	app.Tool("Agent_sub", app.AgentTool(agent.Definition{Name: "sub", SystemPrompt: "SUB", Tools: tools, MaxTurns: 4}))
	frames := postAGUI(t, aguiServer(t, app), agui.RunAgentInput{ThreadID: "b5-s", RunID: "r1", Messages: []agui.Message{userMsg("u1", "派个子代理")}}, false)

	started := findFrame(frames, agui.TypeSubagentStarted)
	if started == nil || started.Name != "sub" || started.ParentToolCallID == "" {
		t.Fatalf("应发 SUBAGENT_STARTED（带名称与父工具调用）: %s", aguiTypes(frames))
	}
	var subText, customOK, reasoning, finished bool
	for _, f := range frames {
		switch {
		case f.Type == agui.TypeTextMessageContent && f.SubagentRunID == started.SubagentRunID:
			subText = true
		case f.Type == agui.TypeCustom && f.Name == "acme.canvas_command" && f.SubagentRunID == started.SubagentRunID:
			customOK = true
		case f.Type == agui.TypeReasoningMessageContent:
			reasoning = true
		case f.Type == agui.TypeSubagentFinished && f.SubagentRunID == started.SubagentRunID:
			finished = true
		}
		if f.Metadata != nil && f.Type != agui.TypeRunStarted && f.Type != agui.TypeRunFinished && f.Type != agui.TypeMessagesSnapshot {
			if _, ok := f.Metadata["goagent.seq"]; !ok {
				t.Fatalf("映射自会话日志的事件应在 metadata 里带 goagent.seq: %s", f.raw)
			}
		}
	}
	if !subText || !customOK || !reasoning || !finished {
		t.Fatalf("子代理正文=%v 自定义事件=%v 推理=%v 子代理结束=%v\n%s", subText, customOK, reasoning, finished, aguiTypes(frames))
	}
}

// GET /agui/capabilities 符合 AgentCapabilities；/protocol 声明两个剖面。
func TestB5_Capabilities(t *testing.T) {
	app := New(WithProvider(&agentScript{}), WithApprover(NewPermissionHandler()))
	base := aguiServer(t, app)
	resp, err := http.Get(base + "/agui/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	validateAGUI(t, "AgentCapabilities", raw)
	var caps agui.AgentCapabilities
	_ = json.Unmarshal(raw, &caps)
	if !caps.HumanInTheLoop.Interrupts || !caps.HumanInTheLoop.Approvals || !caps.Tools.ParallelCalls {
		t.Fatalf("能力描述: %s", raw)
	}
	var desc struct {
		Profiles []string `json:"profiles"`
	}
	getJSON(t, base+"/protocol", &desc)
	if strings.Join(desc.Profiles, ",") != "v1,agui-1.0" {
		t.Fatalf("/protocol 应声明 v1 与 agui-1.0 两个剖面: %v", desc.Profiles)
	}
}
