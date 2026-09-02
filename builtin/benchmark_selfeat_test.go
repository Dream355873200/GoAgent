// benchmark_selfeat_test.go 文件工具自食测试：goagent 的 benchmark
// 系统（根包门面 AgentTarget）驱动 builtin 文件工具（Write/Read/Edit）
// 的真实执行全链路——「用自己的 benchmark 测自己」的最小实例。
// 放在 builtin 包是因为根包测试不能 import builtin（import cycle）。
package builtin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/builtin"
	"github.com/Dream355873200/GoAgent/provider"
)

func TestBenchmarkFileToolsWriteRead(t *testing.T) {
	dir := t.TempDir()

	// 工厂形态：MockProvider 带响应游标（有状态），Repeat>1 时必须
	// 每 trial 现造，否则并发 trial 共享游标互相污染。
	target := goagent.NewAgentTargetFromFactory("file-tools", func() []goagent.Option {
		mock := &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c1", "Write", map[string]any{"file_path": "edit.txt", "content": "v1 content"})}},
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c2", "Read", map[string]any{"file_path": "edit.txt"})}},
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c3", "Edit", map[string]any{"file_path": "edit.txt", "old_string": "v1", "new_string": "v2"})}},
			{Text: "写入并读出确认，内容是 hello benchmark，已编辑"},
		}}
		return []goagent.Option{
			goagent.WithPermissionMode(goagent.PermissionBypass),
			goagent.WithProvider(mock),
			goagent.WithMaxTurns(6),
			goagent.WithSessionWorkDir(func(string) string { return dir }),
		}
	})
	target.UseTools(builtin.CoreTools()...)

	// 一条 case 覆盖三件事：写入内容正确（终产物断言）、工具序列
	// 完整（trajectory 断言）、总结包含关键信息（输出断言）。
	// Edit 前置校验（未 Read 过拒编辑）是 readstate.go 的核心行为，
	// Write→Read→Edit 序列同时锁定了这条合法路径。
	cases := []goagent.Case{
		{
			ID: "write-read-edit", Family: "file-tools",
			Input: "把 v1 content 写入 edit.txt，读出来确认，然后把 v1 改成 v2",
			Asserts: []goagent.Assert{
				{Type: "tool-sequence", Value: []any{"Write", "Read", "Edit"}},
				{Type: "contains", Value: "hello benchmark"},
				{Type: "tool-used", Value: "W*"},
			},
		},
	}

	r := &goagent.Runner{Targets: []goagent.Target{target}, Cases: cases, Repeat: 2}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Summaries() {
		if !s.AllPass {
			for _, row := range rep.Rows {
				t.Logf("row: %+v asserts=%+v", row, row.Asserts)
			}
			t.Errorf("case %s/%s 应全过: %+v", s.Target, s.CaseID, s)
		}
	}
	// 产物真实落盘（工具真实执行，不是 mock 空转）。
	if b, err := os.ReadFile(filepath.Join(dir, "edit.txt")); err != nil || string(b) != "v2 content" {
		t.Errorf("edit.txt 应编辑成功: content=%q err=%v", b, err)
	}
}
