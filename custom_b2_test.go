// custom_b2_test.go 自定义事件（v0.14 B2）：工具内 Emit、宿主 EmitEvent、
// schema 登记与校验、线上 custom 帧、子 agent 内发射的事件到达父流、观察者。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// promptScript 按系统提示词区分调用方（主运行 / 子 agent），各自按轮次
// 依次返回工具调用或文本。
type promptScript struct {
	mu    sync.Mutex
	calls map[string]int
	// steps[key] 是该调用方各轮的动作："tool:<名字>" 或文本。key 为系统提示词中出现的标记。
	steps map[string][]string
}

func (p *promptScript) Capabilities() provider.Capabilities {
	return provider.Capabilities{SupportsTools: true}
}

func (p *promptScript) Complete(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}

func (p *promptScript) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	key := ""
	for k := range p.steps {
		if strings.Contains(req.SystemPrompt, k) && len(k) > len(key) {
			key = k
		}
	}
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	ck := key + "|" + SessionIDFromContext(ctx)
	i := p.calls[ck]
	p.calls[ck]++
	p.mu.Unlock()
	steps := p.steps[key]
	step := "done"
	if i < len(steps) {
		step = steps[i]
	}
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		if name, ok := strings.CutPrefix(step, "tool:"); ok {
			id := key + "-call-" + name
			input := json.RawMessage(`{}`)
			if strings.HasPrefix(name, "Agent_") {
				input = json.RawMessage(`{"task":"go"}`)
			}
			ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{ID: id, Name: name, Input: input}}
			ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopToolUse, Message: &message.Message{
				Role:    message.RoleAssistant,
				Content: []message.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: name, Input: input}},
			}}
			return
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: step}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn, Message: &message.Message{
			Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: step}},
		}}
	}()
	return ch, nil
}

type canvasCmd struct {
	Action string `json:"action" required:"true"`
	Node   string `json:"node,omitempty"`
}

func emitterTool(name string) ToolDef {
	return ToolDef{Description: "emit", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			if err := ctx.Emit(name, canvasCmd{Action: "add_node", Node: "n1"}); err != nil {
				return "", err
			}
			return "emitted", nil
		}}
}

func customEvents(events []Event) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Type == EventCustom {
			out = append(out, ev)
		}
	}
	return out
}

func collect(ch <-chan Event) []Event {
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// 工具里 Emit 的事件出现在 SDK 事件流与 /chat 的 custom 帧上，且不进入会话记录。
func TestB2_EmitFromToolReachesStreams(t *testing.T) {
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(
		WithProvider(&promptScript{steps: map[string][]string{"": {"tool:emit", "ok"}}}),
		WithSessionManager(mgr),
		WithSystemPrompt("main"),
	)
	app.Tool("emit", emitterTool("acme.canvas_command"))

	got := customEvents(collect(app.RunSession(context.Background(), "b2-sdk", "go")))
	if len(got) != 1 || got[0].CustomName != "acme.canvas_command" || !strings.Contains(string(got[0].CustomValue), `"add_node"`) {
		t.Fatalf("SDK 事件流应收到自定义事件, got %+v", got)
	}
	sess, _ := mgr.Get(context.Background(), "b2-sdk")
	for _, m := range sess.Messages {
		if strings.Contains(message.ExtractText(m), "add_node") {
			t.Fatal("自定义事件不应进入会话记录")
		}
	}

	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()
	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"go","session_id":"b2-http"}`, nil)
	defer st.close()
	frames := st.until(t, isType(protocol.TypeCustom))
	f := frames[len(frames)-1]
	if f.Name != "acme.canvas_command" || f.SessionSeq <= 0 {
		t.Fatalf("custom 帧应带 name 与 session_seq: %+v", f)
	}
	var v canvasCmd
	if err := json.Unmarshal(f.Value, &v); err != nil || v.Action != "add_node" || v.Node != "n1" {
		t.Fatalf("custom 帧的 value 应为原值的 JSON: %s (%v)", f.Value, err)
	}
}

// 事件名与值的校验；ctx 不属于任何运行时返回 ErrNotInRun。
func TestB2_EmitValidation(t *testing.T) {
	for _, name := range []string{"nodot", "goagent.reserved", "a..b", "a.b c", ".a", ""} {
		if err := checkEventName(name); err == nil {
			t.Errorf("事件名 %q 应被拒绝", name)
		}
	}
	for _, name := range []string{"acme.x", "amc.device_changed", "a.b.c", "x-y.z_1"} {
		if err := checkEventName(name); err != nil {
			t.Errorf("事件名 %q 应合法: %v", name, err)
		}
	}
	if err := Emit(context.Background(), "acme.x", 1); !errors.Is(err, ErrNotInRun) {
		t.Fatalf("运行之外 Emit 应返回 ErrNotInRun, got %v", err)
	}
	app := New(WithSessionStream(SessionStreamConfig{}))
	if err := app.EmitEvent("s", "acme.x", make(chan int)); err == nil || !strings.Contains(err.Error(), "无法序列化") {
		t.Fatalf("不可序列化的值应报错, got %v", err)
	}
}

// 登记 schema 后发射时校验（Go 类型与 JSON Schema 两种形态），GET /protocol 列出。
func TestB2_RegisterEventSchema(t *testing.T) {
	app := New(WithSessionStream(SessionStreamConfig{}))
	if err := app.RegisterEvent("acme.canvas_command", canvasCmd{}); err != nil {
		t.Fatal(err)
	}
	if err := app.RegisterEvent("acme.level", map[string]any{"type": "integer"}); err != nil {
		t.Fatal(err)
	}
	if err := app.RegisterEvent("goagent.x", nil); err == nil {
		t.Fatal("保留前缀不能登记")
	}
	if err := app.EmitEvent("s", "acme.canvas_command", map[string]any{"node": "n1"}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("缺少必填字段应被 schema 拒绝, got %v", err)
	}
	if err := app.EmitEvent("s", "acme.level", "high"); err == nil {
		t.Fatal("类型不符应被 schema 拒绝")
	}
	if err := app.EmitEvent("s", "acme.canvas_command", canvasCmd{Action: "add_node"}); err != nil {
		t.Fatalf("合法值应通过: %v", err)
	}
	if err := app.EmitEvent("s", "acme.unregistered", []int{1, 2}); err != nil {
		t.Fatalf("未登记的事件不校验: %v", err)
	}

	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/protocol")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var desc protocol.Description
	if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
		t.Fatal(err)
	}
	if len(desc.CustomEvents) != 2 || desc.CustomEvents[0].Name != "acme.canvas_command" || desc.CustomEvents[0].Schema == nil {
		t.Fatalf("/protocol 应列出登记的自定义事件, got %+v", desc.CustomEvents)
	}
}

// EmitEvent：会话有进行中的运行时进入该运行的事件流；空闲时写入会话事件
// 日志；两者都没有时返回 ErrSessionNotActive。
func TestB2_EmitEventRouting(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	app := New(
		WithProvider(&promptScript{steps: map[string][]string{"": {"tool:hold", "ok"}}}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
	)
	app.Tool("hold", ToolDef{Description: "hold", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			close(entered)
			<-release
			return "held", nil
		}})

	if err := app.EmitEvent("b2-route", "acme.ping", 1); !errors.Is(err, ErrSessionNotActive) {
		t.Fatalf("既无运行也无事件日志时应返回 ErrSessionNotActive, got %v", err)
	}

	events := app.RunSession(context.Background(), "b2-route", "go")
	var got []Event
	done := make(chan struct{})
	go func() {
		got = collect(events)
		close(done)
	}()
	<-entered
	if err := app.EmitEvent("b2-route", "acme.ping", map[string]int{"n": 1}); err != nil {
		t.Fatalf("运行中 EmitEvent: %v", err)
	}
	close(release)
	<-done
	cs := customEvents(got)
	if len(cs) != 1 || cs[0].CustomName != "acme.ping" {
		t.Fatalf("运行中的 EmitEvent 应进入该运行的事件流, got %+v", cs)
	}

	app2 := New(WithSessionStream(SessionStreamConfig{}))
	w := app2.WatchSession("idle", WatchFromNow)
	defer w.Close()
	if err := app2.EmitEvent("idle", "acme.ping", "x"); err != nil {
		t.Fatalf("空闲会话 + 事件日志: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := w.Next(ctx)
	if err != nil || ev.Event.Type != EventCustom || ev.Event.CustomName != "acme.ping" || ev.RunID != "" {
		t.Fatalf("空闲会话的 EmitEvent 应写入会话事件日志, got %+v %v", ev, err)
	}
}

// 子 agent 的工具发射的自定义事件到达主运行的事件流。
func TestB2_SubAgentEmitReachesParent(t *testing.T) {
	prov := &promptScript{steps: map[string][]string{
		"":         {"tool:Agent_sub", "parent done"},
		"SUBAGENT": {"tool:emit", "sub done"},
	}}
	app := New(WithProvider(prov), WithSessionManager(session.NewManager(session.NewMemoryStore())), WithApprover(AutoApprover()))
	app.Tool("emit", emitterTool("acme.from_sub"))
	tools, err := app.AgentTools("emit")
	if err != nil {
		t.Fatal(err)
	}
	app.Tool("Agent_sub", app.AgentTool(agent.Definition{Name: "sub", SystemPrompt: "SUBAGENT", Tools: tools, MaxTurns: 4}))

	cs := customEvents(collect(app.RunSession(context.Background(), "b2-sub", "go")))
	if len(cs) != 1 || cs[0].CustomName != "acme.from_sub" {
		t.Fatalf("子 agent 内发射的事件应到达主运行的事件流, got %+v", cs)
	}
}

// hookObserver 记录自定义事件与 LLM 调用钩子。
type hookObserver struct {
	observer.NopObserver
	mu     sync.Mutex
	custom []string
	llm    int
}

func (o *hookObserver) OnCustomEvent(_ context.Context, name string, _ json.RawMessage) {
	o.mu.Lock()
	o.custom = append(o.custom, name)
	o.mu.Unlock()
}

func (o *hookObserver) OnLLMStart(context.Context, *observer.LLMCallInfo) {
	o.mu.Lock()
	o.llm++
	o.mu.Unlock()
}
func (o *hookObserver) OnLLMDone(context.Context, *observer.LLMCallInfo, *observer.LLMResult) {}
func (o *hookObserver) OnLLMError(context.Context, *observer.LLMCallInfo, error)              {}

// App 有多个观察者（成本追踪 + 宿主观察者）时，可选的 LLM 调用钩子与自定义
// 事件钩子照样送达宿主观察者（此前 MultiObserver 不转发可选接口，LLM 钩子
// 在这种配置下静默失效）。
func TestB2_ObserversReceiveOptionalHooksThroughMulti(t *testing.T) {
	rec := &hookObserver{}
	app := New(
		WithProvider(&promptScript{steps: map[string][]string{"": {"tool:emit", "ok"}}}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithCostTracking(),
		WithObservers(rec),
	)
	app.Tool("emit", emitterTool("acme.observed"))
	collect(app.RunSession(context.Background(), "b2-obs", "go"))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.llm == 0 {
		t.Fatal("多个观察者时 LLM 调用钩子应送达宿主观察者")
	}
	if len(rec.custom) != 1 || rec.custom[0] != "acme.observed" {
		t.Fatalf("观察者应收到自定义事件, got %v", rec.custom)
	}
}
