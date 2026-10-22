package goagent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

type shellIn struct {
	Command string `json:"command"`
}

func countingShell(executed *atomic.Int32) ToolDef {
	return ToolDef{
		Description: "执行命令",
		Input:       shellIn{},
		Permission:  Normal,
		Execute: func(ctx Context, in shellIn) (string, error) {
			executed.Add(1)
			return "ok", nil
		},
	}
}

func runWithMode(t *testing.T, mode PermissionModeOption, responses []provider.MockResponse, opts ...Option) (executed int32) {
	t.Helper()
	var n atomic.Int32
	prov := &provider.MockProvider{Responses: responses}
	app := New(append([]Option{WithProvider(prov), WithPermissionMode(mode)}, opts...)...)
	app.Tool("shell", countingShell(&n))
	drainEvents(app.Run(context.Background(), "go"))
	return n.Load()
}

func shellCall(cmd string) provider.MockResponse {
	return provider.MockResponse{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("c1", "shell", map[string]any{"command": cmd})}}
}

// auto 模式：分类器默认装配，每个非只读调用都经它判定（MockProvider 的
// Complete 依次给出判定结果）。
func TestFix_AutoModeConsultsClassifier(t *testing.T) {
	allow := runWithMode(t, PermissionAuto, []provider.MockResponse{
		shellCall("ls"),
		{Text: "<block>no</block>"},
		{Text: "done"},
	})
	if allow != 1 {
		t.Fatalf("分类器放行后工具应执行一次，实际 %d", allow)
	}

	block := runWithMode(t, PermissionAuto, []provider.MockResponse{
		shellCall("ls"),
		{Text: "<block>yes</block><reason>危险</reason>"},
		{Text: "<block>yes</block><reason>危险</reason>"},
		{Text: "done"},
	})
	if block != 0 {
		t.Fatalf("分类器判为危险时工具不应执行，实际 %d", block)
	}
}

// WithYoloClassifier(false)：不装配分类器；auto 模式下的调用改为询问审批者
// （这里没有审批者，因此被拒绝），也不产生分类请求。
func TestFix_YoloClassifierCanBeDisabled(t *testing.T) {
	n := runWithMode(t, PermissionAuto, []provider.MockResponse{
		shellCall("ls"),
		{Text: "done"},
	}, WithYoloClassifier(false))
	if n != 0 {
		t.Fatalf("没有分类器也没有审批者时应拒绝，实际执行 %d 次", n)
	}
}

// bypass 模式：普通命令直接执行、不调用分类器；高风险命令先经分类器。
func TestFix_BypassClassifiesOnlyHighRiskCommands(t *testing.T) {
	plain := runWithMode(t, PermissionBypass, []provider.MockResponse{
		shellCall("go build ./..."),
		{Text: "done"},
	})
	if plain != 1 {
		t.Fatalf("普通命令应直接执行（且不消耗分类请求），实际 %d", plain)
	}

	allowed := runWithMode(t, PermissionBypass, []provider.MockResponse{
		shellCall("rm -rf build"),
		{Text: "<block>no</block>"},
		{Text: "done"},
	})
	if allowed != 1 {
		t.Fatalf("分类器放行的高风险命令应执行，实际 %d", allowed)
	}

	blocked := runWithMode(t, PermissionBypass, []provider.MockResponse{
		shellCall("rm -rf build"),
		{Text: "<block>yes</block>"},
		{Text: "<block>yes</block>"},
		{Text: "done"},
	})
	if blocked != 0 {
		t.Fatalf("分类器判为危险的高风险命令不应执行，实际 %d", blocked)
	}
}

func TestFix_AutoModeHTTPString(t *testing.T) {
	if m, ok := permissionModeFromString("auto"); !ok || m != PermissionAuto {
		t.Fatalf("auto 应解析为 PermissionAuto: %v %v", m, ok)
	}
	if s := permissionModeString(PermissionAuto); s != "auto" {
		t.Fatalf("PermissionAuto 应输出 auto，实际 %q", s)
	}
}
