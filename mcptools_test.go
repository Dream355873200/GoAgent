package goagent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Dream355873200/GoAgent/mcp"
)

func TestMCPToolDefKeepsSchemaAndPassesRawInput(t *testing.T) {
	remoteSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	}
	var got json.RawMessage
	app := New()
	app.Tool("mcp__fs__read", MCPToolDef(mcp.FrameworkTool{
		Name:        "mcp__fs__read",
		Description: "读文件",
		InputSchema: remoteSchema,
		Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			got = in
			return "ok", nil
		},
	}))
	app.Tool("Echo", ToolDef{
		Description: "回显",
		Input:       struct{ Msg string `json:"msg"` }{},
		Permission:  ReadOnly,
		Execute:     func(_ Context, in struct{ Msg string `json:"msg"` }) (string, error) { return in.Msg, nil },
	})

	var entrySchema any
	for _, e := range app.buildToolSet() {
		if e.Name == "mcp__fs__read" {
			entrySchema = e.InputSchema
		}
	}
	if !reflect.DeepEqual(entrySchema, remoteSchema) {
		t.Fatalf("显式 Schema 应原样下发，得到 %v", entrySchema)
	}

	tools, err := app.AgentTools("mcp__fs__read", "Echo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tools[0].InputSchema, remoteSchema) {
		t.Fatalf("子 agent 工具 schema 应与主 agent 一致: %v", tools[0].InputSchema)
	}
	if out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"path":"a.go"}`)); err != nil || out != "ok" || string(got) != `{"path":"a.go"}` {
		t.Fatalf("原始入参应透传: out=%q err=%v got=%s", out, err, got)
	}
	if out, err := tools[1].Execute(context.Background(), json.RawMessage(`{"msg":"hi"}`)); err != nil || out != "hi" {
		t.Fatalf("结构体入参工具: out=%q err=%v", out, err)
	}
	if _, err := app.AgentTools("Nope"); err == nil {
		t.Fatal("未注册工具应报错")
	}

	if p, ok := app.ToolPermission("Echo"); !ok || p != ReadOnly {
		t.Fatalf("ToolPermission(Echo) = %v %v", p, ok)
	}
	if p, ok := app.ToolPermission("mcp__fs__read"); !ok || p != Normal {
		t.Fatalf("MCP 工具应为 Normal: %v %v", p, ok)
	}
}
