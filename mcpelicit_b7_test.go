// mcpelicit_b7_test.go MCP elicitation ↔ ask_user（v0.14 B7）：WithMCP 连接的服务器
// 在工具调用中发起 elicitation，转成所在会话的提问；回答按表单校验，不合格时
// 附上原因重新提问；单字段表单可以直接回答文本。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/mcp"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
	"github.com/Dream355873200/GoAgent/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func elicitingMCPServer(t *testing.T) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "el", Version: "1"}, nil)
	obj := map[string]any{"type": "object"}
	reply := func(res *sdk.ElicitResult, err error) (*sdk.CallToolResult, error) {
		if err != nil {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "error " + err.Error()}}}, nil
		}
		b, _ := json.Marshal(res.Content)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: res.Action + " " + string(b)}}}, nil
	}
	server.AddTool(&sdk.Tool{Name: "signup", InputSchema: obj}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return reply(req.Session.Elicit(ctx, &sdk.ElicitParams{Message: "注册信息", RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "title": "姓名"},
				"age":  map[string]any{"type": "integer"},
			},
			"required": []any{"name"},
		}}))
	})
	server.AddTool(&sdk.Tool{Name: "confirm", InputSchema: obj}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return reply(req.Session.Elicit(ctx, &sdk.ElicitParams{Message: "确定删除吗", RequestedSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required":   []any{"ok"},
		}}))
	})
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestB7_MCPElicitationViaAskUser(t *testing.T) {
	results := func(r *provider.Request) map[string]string {
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
		scripted.Tools(scripted.ToolCall{ID: "s", Name: "mcp__el__signup"}),
		scripted.Tools(scripted.ToolCall{ID: "c", Name: "mcp__el__confirm"}),
		scripted.Text("完成").Expect(func(r *provider.Request) error {
			got := results(r)
			if got["s"] != `accept {"age":36,"name":"Ada"}` {
				return errors.New("signup 结果: " + got["s"])
			}
			if got["c"] != `accept {"ok":true}` {
				return errors.New("confirm 结果: " + got["c"])
			}
			return nil
		}),
	)
	app := New(
		WithProvider(m),
		WithApprover(AutoApprover()),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithMCP(agent.MCPServerConfig{Name: "el", URL: elicitingMCPServer(t)}),
	)
	defer app.CloseMCP()
	asks := NewAskUserHandler()
	app.SetAskUserHandler(asks)
	reqs, unsubscribe := asks.Subscribe("el-1")
	defer unsubscribe()

	type answered struct {
		req    *AskUserRequest
		answer string
	}
	var log []answered
	answers := []string{`{"age":"x"}`, `{"name":"Ada","age":36}`, "是"}
	go func() {
		for _, a := range answers {
			select {
			case req := <-reqs:
				log = append(log, answered{req, a})
				req.Respond(a)
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()

	for ev := range app.RunSession(context.Background(), "el-1", "注册并确认") {
		if ev.Type == EventError {
			t.Fatalf("运行出错: %v", ev.Error)
		}
	}
	m.AssertDone(t)
	if len(log) != 3 {
		t.Fatalf("应提问 3 次（一次无效后重问 + 确认），实际 %d", len(log))
	}
	first, retry, confirm := log[0].req, log[1].req, log[2].req
	if first.Payload["kind"] != "mcp_elicitation" || first.Payload["server"] != "el" || first.Payload["mode"] != "form" ||
		first.Payload["schema"] == nil || first.ToolUseID != "s" || first.SessionID != "el-1" {
		t.Fatalf("提问载荷: %+v", first)
	}
	if !strings.Contains(first.Question, "注册信息") || !strings.Contains(first.Question, "name（姓名）") {
		t.Fatalf("提问文本应带消息与字段: %q", first.Question)
	}
	if retry.Payload["error"] == nil || !strings.Contains(retry.Question, "上次的回答无效") {
		t.Fatalf("回答不符合表单时应附上原因重新提问: %+v", retry)
	}
	if confirm.ToolUseID != "c" {
		t.Fatalf("第二次提问应归属 confirm 调用: %+v", confirm)
	}
}

// 回答解析：JSON 对象接受、action 拒绝/取消、空回答取消、url 模式任意文本同意、
// 单字段表单按类型转换、多字段表单要求 JSON。
func TestB7_ParseElicitAnswer(t *testing.T) {
	form := mcp.Elicitation{Mode: "form", Schema: map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "number"},
	}}}
	single := mcp.Elicitation{Mode: "form", Schema: map[string]any{"type": "object", "properties": map[string]any{
		"n": map[string]any{"type": "integer"},
	}}}
	url := mcp.Elicitation{Mode: "url", URL: "https://x"}
	for _, tc := range []struct {
		e       mcp.Elicitation
		answer  string
		action  string
		content string
		problem bool
	}{
		{form, `{"a":"x","b":2}`, mcp.ElicitAccept, `{"a":"x","b":2}`, false},
		{form, `{"action":"accept","content":{"a":"y"}}`, mcp.ElicitAccept, `{"a":"y"}`, false},
		{form, `{"action":"decline"}`, mcp.ElicitDecline, `null`, false},
		{form, `  `, mcp.ElicitCancel, `null`, false},
		{form, `随便写写`, "", "", true},
		{form, `{"b":"不是数字"}`, "", "", true},
		{single, `42`, mcp.ElicitAccept, `{"n":42}`, false},
		{single, `四十二`, "", "", true},
		{url, `好的`, mcp.ElicitAccept, `null`, false},
		{url, `{"action":"cancel"}`, mcp.ElicitCancel, `null`, false},
	} {
		resp, problem := parseElicitAnswer(tc.e, tc.answer)
		if (problem != "") != tc.problem {
			t.Fatalf("%q: problem=%q", tc.answer, problem)
		}
		if tc.problem {
			continue
		}
		content, _ := json.Marshal(resp.Content)
		if resp.Action != tc.action || string(content) != tc.content {
			t.Fatalf("%q: %s %s", tc.answer, resp.Action, content)
		}
	}
}

// 没有提问处理器或不在会话里时拒绝（不阻塞）。
func TestB7_ElicitWithoutSessionDeclines(t *testing.T) {
	resp, err := ElicitViaAskUser(context.Background(), mcp.Elicitation{Mode: "form"})
	if err != nil || resp.Action != mcp.ElicitDecline {
		t.Fatalf("应拒绝: %+v %v", resp, err)
	}
}
