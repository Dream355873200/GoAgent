// mcpserver_b7_test.go 把 GoAgent 暴露为 MCP 服务端（v0.14 B7）：直接调用已注册
// 工具（注解、schema 校验、deny 规则、图片、会话键值、进度），以及把 agent
// 作为工具调用（审批与提问经 elicitation 交给客户端、进度通知、延续会话、
// 客户端不支持 elicitation 时的退路）。
package goagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
	"github.com/Dream355873200/GoAgent/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpTestClient(t *testing.T, app *App, opts MCPServerOptions, copts *sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	h, err := app.MCPHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, copts).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func mcpCall(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestB7_MCPServerTools(t *testing.T) {
	app := New(WithPermissionRules(NewPermissionRules().Deny("blocked", "")))
	type sIn struct {
		S string `json:"s"`
	}
	app.Tool("echo", ToolDef{Description: "回显", Input: sIn{}, Permission: ReadOnly,
		Execute: func(ctx Context, in sIn) (string, error) {
			ctx.Progress("回显中")
			return in.S, nil
		}})
	app.Tool("wipe", ToolDef{Description: "清空", Input: struct{}{}, Permission: Dangerous,
		Execute: func(Context, struct{}) (string, error) { return "wiped", nil }})
	app.Tool("blocked", ToolDef{Description: "被规则禁止", Input: struct{}{}, Permission: Normal,
		Execute: func(Context, struct{}) (string, error) { return "不应执行", nil }})
	app.Tool("hidden", ToolDef{Description: "未暴露", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(Context, struct{}) (string, error) { return "", nil }})
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	app.Tool("shot", ToolDef{Description: "截图", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(Context, struct{}) (string, error) { return "[IMAGE png " + png + "]\n截图说明", nil }})
	type kvIn struct {
		S string `json:"s,omitempty"`
	}
	app.Tool("kv", ToolDef{Description: "会话键值", Input: kvIn{}, Permission: ReadOnly,
		Execute: func(ctx Context, in kvIn) (string, error) {
			if in.S != "" {
				ctx.Store("k", in.S)
				return "stored", nil
			}
			v, _ := ctx.Load("k")
			s, _ := v.(string)
			return s, nil
		}})

	if _, err := app.MCPServer(MCPServerOptions{Tools: []string{"nope"}}); err == nil {
		t.Fatal("暴露未注册的工具应报错")
	}
	if _, err := app.MCPServer(MCPServerOptions{Agent: &MCPAgentTool{}}); err == nil {
		t.Fatal("没有 provider 时暴露 agent 应报错")
	}

	var mu sync.Mutex
	var progress []string
	cs := mcpTestClient(t, app, MCPServerOptions{Name: "srv", Tools: []string{"echo", "wipe", "blocked", "shot", "kv"}}, &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Message)
			mu.Unlock()
		},
	})
	if got := cs.InitializeResult().ServerInfo.Name; got != "srv" {
		t.Fatalf("服务器名: %q", got)
	}
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		byName[tl.Name] = tl
	}
	if len(byName) != 5 || byName["hidden"] != nil {
		t.Fatalf("只应暴露列出的工具: %v", byName)
	}
	if a := byName["echo"].Annotations; a == nil || !a.ReadOnlyHint || *a.DestructiveHint {
		t.Fatalf("echo 注解: %+v", a)
	}
	if a := byName["wipe"].Annotations; a == nil || a.ReadOnlyHint || !*a.DestructiveHint {
		t.Fatalf("wipe 注解: %+v", a)
	}

	params := &sdk.CallToolParams{Name: "echo", Arguments: map[string]any{"s": "你好"}}
	params.SetProgressToken("p1")
	res, err := cs.CallTool(context.Background(), params)
	if err != nil || res.IsError || resultText(res) != "你好" {
		t.Fatalf("echo: %+v %v", res, err)
	}
	if res := mcpCall(t, cs, "echo", map[string]any{"s": 1}); !res.IsError || !strings.Contains(resultText(res), "schema") {
		t.Fatalf("参数不符合 schema 应返回错误结果: %q", resultText(res))
	}
	if res := mcpCall(t, cs, "blocked", map[string]any{}); !res.IsError || !strings.Contains(resultText(res), "权限被拒绝") {
		t.Fatalf("deny 规则应生效: %q", resultText(res))
	}
	if res := mcpCall(t, cs, "wipe", map[string]any{}); res.IsError || resultText(res) != "wiped" {
		t.Fatalf("默认模式下需要确认的工具交给客户端确认后直接执行: %q", resultText(res))
	}
	res = mcpCall(t, cs, "shot", map[string]any{})
	img, ok := res.Content[0].(*sdk.ImageContent)
	if !ok || img.MIMEType != "image/png" || string(img.Data) != "\x89PNG fake" || resultText(res) != "截图说明" {
		t.Fatalf("内联图片应转为 MCP 图片: %+v", res.Content)
	}
	mcpCall(t, cs, "kv", map[string]any{"s": "记住我"})
	if got := resultText(mcpCall(t, cs, "kv", map[string]any{})); got != "记住我" {
		t.Fatalf("同一 MCP 会话的调用共享会话键值: %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(progress) != 1 || progress[0] != "回显中" {
		t.Fatalf("Context.Progress 应转成进度通知: %v", progress)
	}
}

// agent 作为工具：审批与提问经 elicitation 交给客户端，进度通知下发，
// KeepSessions 延续会话。
func TestB7_MCPServerAgent(t *testing.T) {
	toolResults := func(r *provider.Request) map[string]string {
		out := map[string]string{}
		for _, msg := range r.Messages {
			for _, b := range msg.Content {
				if b.Type == "tool_result" {
					out[b.ForToolUseID] = b.Text
				}
			}
		}
		return out
	}
	m := scripted.New(
		scripted.Tools(scripted.ToolCall{ID: "w", Name: "write", Input: map[string]string{"path": "a.txt"}}),
		scripted.Tools(scripted.ToolCall{ID: "q", Name: "ask", Input: map[string]string{"question": "喜欢什么颜色？"}}),
		scripted.Text("写好了，颜色是蓝色").Expect(func(r *provider.Request) error {
			got := toolResults(r)
			if got["w"] != "wrote a.txt" || got["q"] != "蓝色" {
				return errors.New("审批与提问的结果应回到模型: " + got["w"] + " / " + got["q"])
			}
			return nil
		}),
		scripted.Text("记得：蓝色").Expect(func(r *provider.Request) error {
			for _, msg := range r.Messages {
				for _, b := range msg.Content {
					if b.Type == "text" && strings.Contains(b.Text, "写好了，颜色是蓝色") {
						return nil
					}
				}
			}
			return errors.New("带 session_id 的第二次调用应延续会话历史")
		}),
	)
	app := New(
		WithProvider(m),
		WithApprover(NewPermissionHandler()),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
	)
	app.Tool("write", ToolDef{Description: "写文件", Input: struct {
		Path string `json:"path"`
	}{}, Permission: RequireApproval,
		Execute: func(_ Context, in struct {
			Path string `json:"path"`
		}) (string, error) {
			return "wrote " + in.Path, nil
		}})
	app.Tool("ask", ToolDef{Description: "提问", Input: struct {
		Question string `json:"question"`
	}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct {
			Question string `json:"question"`
		}) (string, error) {
			return AskUserHandlerFromContext(ctx).AskSessionCtx(ctx, ctx.SessionID, in.Question, nil)
		}})

	var mu sync.Mutex
	var elicited, progress []string
	cs := mcpTestClient(t, app, MCPServerOptions{Agent: &MCPAgentTool{KeepSessions: true}}, &sdk.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			mu.Lock()
			elicited = append(elicited, req.Params.Message)
			mu.Unlock()
			if strings.Contains(req.Params.Message, "write") {
				return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
			}
			return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"answer": "蓝色"}}, nil
		},
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Message)
			mu.Unlock()
		},
	})

	params := &sdk.CallToolParams{Name: "run_agent", Arguments: map[string]any{"task": "写文件并问我喜欢的颜色"}}
	params.SetProgressToken("p")
	res, err := cs.CallTool(context.Background(), params)
	if err != nil || res.IsError {
		t.Fatalf("agent 调用: %+v %v", res, err)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Text      string `json:"text"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(structured, &out)
	if out.Text != "写好了，颜色是蓝色" || out.SessionID == "" || !strings.Contains(resultText(res), "[session_id: "+out.SessionID+"]") {
		t.Fatalf("agent 结果: %s / %q", structured, resultText(res))
	}
	mu.Lock()
	if len(elicited) != 2 || !strings.Contains(elicited[0], "write") || elicited[1] != "喜欢什么颜色？" {
		t.Fatalf("审批与提问应各经一次 elicitation: %v", elicited)
	}
	if len(progress) < 2 || progress[0] != "调用 write" || progress[1] != "调用 ask" {
		t.Fatalf("工具调用应以进度通知下发: %v", progress)
	}
	mu.Unlock()

	res = mcpCall(t, cs, "run_agent", map[string]any{"task": "还记得吗", "session_id": out.SessionID})
	if res.IsError || !strings.HasPrefix(resultText(res), "记得：蓝色") {
		t.Fatalf("延续会话: %q", resultText(res))
	}
	if res := mcpCall(t, cs, "run_agent", map[string]any{"task": "x", "session_id": "../etc"}); !res.IsError {
		t.Fatal("非法 session_id 应被拒绝")
	}
	m.AssertDone(t)
}

// 客户端不支持 elicitation：审批被拒绝（原因交给模型），运行照常结束。
func TestB7_MCPServerAgentWithoutElicitation(t *testing.T) {
	m := scripted.New(
		scripted.Tools(scripted.ToolCall{ID: "w", Name: "write"}),
		scripted.Text("没能写").Expect(func(r *provider.Request) error {
			for _, msg := range r.Messages {
				for _, b := range msg.Content {
					if b.Type == "tool_result" && strings.Contains(b.Text, "不支持确认") {
						return nil
					}
				}
			}
			return errors.New("拒绝原因应交给模型")
		}),
	)
	app := New(WithProvider(m), WithApprover(NewPermissionHandler()))
	app.Tool("write", ToolDef{Description: "写文件", Input: struct{}{}, Permission: RequireApproval,
		Execute: func(Context, struct{}) (string, error) { return "wrote", nil }})
	cs := mcpTestClient(t, app, MCPServerOptions{Agent: &MCPAgentTool{Name: "delegate"}}, nil)
	res := mcpCall(t, cs, "delegate", map[string]any{"task": "写"})
	if res.IsError || resultText(res) != "没能写" {
		t.Fatalf("结果: %q", resultText(res))
	}
	m.AssertDone(t)
}
