package oteltrace_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/observer/oteltrace"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
	"github.com/Dream355873200/GoAgent/session"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func attr(s sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func hasEvent(s sdktrace.ReadOnlySpan, name string) bool {
	for _, e := range s.Events() {
		if e.Name == name {
			return true
		}
	}
	return false
}

// 主运行调用工具 echo（发自定义事件）、失败工具 fail 和子代理；子代理也调用
// echo（调用 ID 与主运行相同）。span 树、属性、状态与事件都应符合 gen_ai.* 约定，
// 工具内部经 ctx 发起的追踪调用挂在 execute_tool 下。
func TestSpanTree(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())

	m := scripted.New(
		scripted.Tools(
			scripted.ToolCall{ID: "c1", Name: "echo", Input: map[string]string{"s": "main"}},
			scripted.ToolCall{ID: "c2", Name: "fail"},
			scripted.ToolCall{ID: "c3", Name: "Agent_sub", Input: map[string]string{"task": "看看"}},
		).WithUsage(100, 10),
		scripted.Text("全部完成").WithUsage(200, 5),
	)
	m.Route(scripted.SystemContains("SUB"),
		scripted.Tools(scripted.ToolCall{ID: "c1", Name: "echo", Input: map[string]string{"s": "sub"}}).WithUsage(30, 3),
		scripted.Text("子代理完成").WithUsage(40, 4),
	)
	app := goagent.New(
		goagent.WithProvider(m),
		goagent.WithSystemPrompt("MAIN"),
		goagent.WithSessionManager(session.NewManager(session.NewMemoryStore())),
		goagent.WithApprover(goagent.AutoApprover()),
		goagent.WithObservers(oteltrace.New(tp, oteltrace.Options{ProviderName: "test"})),
	)
	inner := tp.Tracer("tool-internal")
	app.Tool("echo", goagent.ToolDef{Description: "echo", Input: struct {
		S string `json:"s"`
	}{}, Permission: goagent.ReadOnly, Concurrent: true,
		Execute: func(ctx goagent.Context, in struct {
			S string `json:"s"`
		}) (string, error) {
			_, span := inner.Start(ctx, "db.query "+in.S)
			span.End()
			if err := ctx.Emit("test.echoed", map[string]string{"s": in.S}); err != nil {
				return "", err
			}
			return in.S, nil
		}})
	app.Tool("fail", goagent.ToolDef{Description: "fail", Input: struct{}{}, Permission: goagent.ReadOnly, Concurrent: true,
		Execute: func(goagent.Context, struct{}) (string, error) { return "", errors.New("磁盘满了") }})
	tools, err := app.AgentTools("echo")
	if err != nil {
		t.Fatal(err)
	}
	app.Tool("Agent_sub", app.AgentTool(agent.Definition{Name: "sub", SystemPrompt: "SUB", Tools: tools, MaxTurns: 4}))

	ctx, host := tp.Tracer("host").Start(context.Background(), "POST /chat")
	for ev := range app.RunSession(ctx, "otel-1", "开始") {
		if ev.Type == goagent.EventError {
			t.Fatalf("运行出错: %v", ev.Error)
		}
	}
	host.End()
	m.AssertDone(t)

	spans := rec.Ended()
	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = append(byName[s.Name()], s)
	}
	one := func(name string) sdktrace.ReadOnlySpan {
		t.Helper()
		if len(byName[name]) != 1 {
			var names []string
			for _, s := range spans {
				names = append(names, s.Name())
			}
			t.Fatalf("应恰好有一个 %q span，全部: %v", name, names)
		}
		return byName[name][0]
	}
	childOf := func(child, parent sdktrace.ReadOnlySpan) bool {
		return child.Parent().SpanID() == parent.SpanContext().SpanID()
	}

	hostSpan := one("POST /chat")
	run := one("invoke_agent goagent")
	if !childOf(run, hostSpan) {
		t.Fatal("invoke_agent 应挂在宿主 ctx 里已有的 span 下")
	}
	if attr(run, oteltrace.AttrConversationID).AsString() != "otel-1" || attr(run, oteltrace.AttrOperationName).AsString() != "invoke_agent" ||
		attr(run, oteltrace.AttrProviderName).AsString() != "test" || attr(run, oteltrace.AttrTurns).AsInt64() < 1 {
		t.Fatalf("invoke_agent 属性: %v", run.Attributes())
	}

	chats := byName["chat scripted"]
	if len(chats) != 4 {
		t.Fatalf("应有 4 个 chat span（主 2 + 子 2），得到 %d", len(chats))
	}
	agentTool := one("execute_tool Agent_sub")
	var mainChats, subChats int
	for _, c := range chats {
		switch {
		case childOf(c, run):
			mainChats++
			if attr(c, oteltrace.AttrAgentPath).Type() != attribute.INVALID {
				t.Fatal("主运行的 chat 不应带 agent_path")
			}
		case childOf(c, agentTool):
			subChats++
			if attr(c, oteltrace.AttrAgentPath).AsString() != "c3" {
				t.Fatalf("子代理的 chat 应带 agent_path=c3: %v", c.Attributes())
			}
		default:
			t.Fatalf("chat span 挂错了父节点: %v", c.Attributes())
		}
		if c.SpanKind() != trace.SpanKindClient || attr(c, oteltrace.AttrRequestModel).AsString() != "scripted" ||
			attr(c, oteltrace.AttrResponseModel).AsString() != "scripted" || len(attr(c, oteltrace.AttrFinishReasons).AsStringSlice()) != 1 {
			t.Fatalf("chat 属性: %v", c.Attributes())
		}
	}
	if mainChats != 2 || subChats != 2 {
		t.Fatalf("主运行 chat %d 个、子代理 chat %d 个，应各 2 个", mainChats, subChats)
	}
	var sawUsage bool
	for _, c := range chats {
		if attr(c, oteltrace.AttrInputTokens).AsInt64() == 200 && attr(c, oteltrace.AttrOutputTokens).AsInt64() == 5 {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("chat span 应带 gen_ai.usage.* 用量")
	}

	echoes := byName["execute_tool echo"]
	if len(echoes) != 2 {
		t.Fatalf("应有 2 个 echo 调用（主运行与子代理调用 ID 相同也不能混淆），得到 %d", len(echoes))
	}
	for _, e := range echoes {
		sub := childOf(e, agentTool)
		if !sub && !childOf(e, run) {
			t.Fatal("echo span 挂错了父节点")
		}
		if attr(e, oteltrace.AttrToolCallID).AsString() != "c1" || attr(e, oteltrace.AttrToolName).AsString() != "echo" || e.Status().Code == codes.Error {
			t.Fatalf("echo 属性/状态: %v %v", e.Attributes(), e.Status())
		}
		if !hasEvent(e, "goagent.custom_event") {
			t.Fatal("工具里 Emit 的自定义事件应记在它的 execute_tool span 上")
		}
		want := "db.query main"
		if sub {
			want = "db.query sub"
		}
		if !childOf(one(want), e) {
			t.Fatalf("工具内部经 ctx 开的 span %q 应挂在 execute_tool 下", want)
		}
	}
	if !childOf(agentTool, run) {
		t.Fatal("Agent_sub 调用应挂在 invoke_agent 下")
	}

	failed := one("execute_tool fail")
	if failed.Status().Code != codes.Error || attr(failed, oteltrace.AttrErrorType).AsString() != "tool_error" ||
		!strings.Contains(failed.Status().Description, "磁盘满了") {
		t.Fatalf("失败工具应为 Error 状态: %v %v", failed.Status(), failed.Attributes())
	}
}

// 模型请求失败：chat span 为 Error 状态并带 error.type；运行 span 记下错误。
func TestLLMErrorSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())

	m := scripted.New(scripted.Fail(&provider.PromptTooLongError{Message: "too long"}))
	app := goagent.New(goagent.WithProvider(m), goagent.WithObservers(oteltrace.New(tp)))
	for range app.Run(context.Background(), "hi") {
	}
	var chat, run sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "chat scripted":
			chat = s
		case "invoke_agent goagent":
			run = s
		}
	}
	if chat == nil || run == nil {
		t.Fatalf("应有 chat 与 invoke_agent span: %d 个", len(rec.Ended()))
	}
	if chat.Status().Code != codes.Error || attr(chat, oteltrace.AttrErrorType).AsString() != "prompt_too_long" {
		t.Fatalf("chat 失败状态: %v %v", chat.Status(), chat.Attributes())
	}
	if run.Status().Code != codes.Error {
		t.Fatalf("运行 span 应记下错误: %v", run.Status())
	}
	if _, ok := attr(run, oteltrace.AttrConversationID).AsInterface().(string); ok {
		t.Fatal("临时运行不应设置 gen_ai.conversation.id")
	}
}
