package goagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/session"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// WithMCP：服务端的进度通知显示为工具状态行；工具列表变更同步进注册表。
func TestB4_WithMCPProgressAndListChanged(t *testing.T) {
	obj := map[string]any{"type": "object"}
	server := sdk.NewServer(&sdk.Implementation{Name: "b4", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "slow", InputSchema: obj}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Progress: 1, Total: 1, Message: "step"})
		time.Sleep(100 * time.Millisecond) // 让进度先于响应被分发（见 mcp/sdk_test.go）
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil
	})
	server.AddTool(&sdk.Tool{Name: "old", InputSchema: obj}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "old"}}}, nil
	})
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer srv.Close()

	app := New(
		WithProvider(&toolThenText{tool: "mcp__srv__slow"}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithMCP(agent.MCPServerConfig{Name: "srv", URL: srv.URL}),
		WithApprover(AutoApprover()),
	)
	defer app.CloseMCP()
	names := app.ToolNames()
	if !slices.Contains(names, "mcp__srv__slow") || !slices.Contains(names, "mcp__srv__old") {
		t.Fatalf("应注册服务器的工具, got %v", names)
	}

	var progress []string
	for ev := range app.RunSession(context.Background(), "b4", "go") {
		if ev.Type == EventProgress && strings.HasPrefix(ev.StatusKey, "tool-progress") && ev.Text != "" {
			progress = append(progress, ev.Text)
		}
	}
	if len(progress) != 1 || progress[0] != "[1/1] step" {
		t.Fatalf("服务端进度应显示为工具状态行, got %v", progress)
	}

	server.AddTool(&sdk.Tool{Name: "new", InputSchema: obj}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "new"}}}, nil
	})
	server.RemoveTools("old")
	deadline := time.Now().Add(10 * time.Second)
	for {
		names = app.ToolNames()
		if slices.Contains(names, "mcp__srv__new") && !slices.Contains(names, "mcp__srv__old") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("工具列表变更应同步进注册表, got %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(names, "mcp__srv__slow") {
		t.Fatal("未变化的工具应保留")
	}
}
