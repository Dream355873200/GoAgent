// benchmark_app_test.go 根包自食测试：AgentTarget 驱动 mock provider
// → 自建工具全链路，断言 trajectory / 输出 / infra 语义。
// 文件工具（builtin 包）的自食测试在 builtin/benchmark_selfeat_test.go
// ——根包测试不能 import builtin（import cycle）。
package goagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

func TestBenchmarkAgentTargetToolFlow(t *testing.T) {
	dir := t.TempDir()

	// 工厂形态：MockProvider 带响应游标（有状态），Repeat>1 时必须
	// 每 trial 现造，否则并发 trial 共享游标互相污染。
	target := NewAgentTargetFromFactory("tools", func() []Option {
		mock := &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c1", "save", map[string]any{"path": "note.txt", "content": "hello benchmark"})}},
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c2", "load", map[string]any{"path": "note.txt"})}},
			{Text: "文件内容是 hello benchmark"},
		}}
		return []Option{
			WithProvider(mock),
			WithMaxTurns(5),
			WithSessionWorkDir(func(string) string { return dir }),
		}
	})
	target.UseTools(
		InferTool("save", "写文件", func(ctx context.Context, in struct {
			Path    string `json:"path" desc:"路径"`
			Content string `json:"content" desc:"内容"`
		}) (string, error) {
			return "ok", os.WriteFile(filepath.Join(WorkDirFromContext(ctx), in.Path), []byte(in.Content), 0o644)
		}),
		InferTool("load", "读文件", func(ctx context.Context, in struct {
			Path string `json:"path" desc:"路径"`
		}) (string, error) {
			b, err := os.ReadFile(filepath.Join(WorkDirFromContext(ctx), in.Path))
			return string(b), err
		}),
	)

	cases := []Case{
		{
			ID: "save-load-roundtrip", Family: "tools",
			Input: "把 hello benchmark 写入 note.txt，再读出来确认",
			Asserts: []Assert{
				{Type: "tool-sequence", Value: []any{"save", "load"}},
				{Type: "contains", Value: "hello benchmark"},
				{Type: "tool-used", Value: "s*"},
			},
		},
	}

	r := &Runner{Targets: []Target{target}, Cases: cases, Repeat: 2}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pass, fail, _ := rep.SuiteStats()
	if pass != 1 || fail != 0 {
		for _, row := range rep.Rows {
			t.Logf("row: %+v", row)
		}
		t.Fatalf("自食用例应全过: pass=%d fail=%d", pass, fail)
	}
	// 产物真的落盘了（工具真实执行，不是 mock 空转）。
	if b, err := os.ReadFile(filepath.Join(dir, "note.txt")); err != nil || string(b) != "hello benchmark" {
		t.Errorf("文件应真实写入: content=%q err=%v", b, err)
	}
}

func TestBenchmarkAgentTargetInfraError(t *testing.T) {
	// 无 provider 装配：loop 发 EventError → AgentTarget 返回 infra 错误
	// → Runner 记 infra_error 不进分母（不发 panic 崩测试进程——
	// nil provider 守卫的行为也被 benchmark 层覆盖）。
	target := NewAgentTarget("no-provider", WithMaxTurns(2))
	r := &Runner{
		Targets:      []Target{target},
		Cases:        []Case{{ID: "c", Input: "hi", Asserts: []Assert{{Type: "contains", Value: "x"}}}},
		InfraRetries: 0,
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows[0].Status != StatusInfraError {
		t.Errorf("无 provider 应落 infra_error, got %s", rep.Rows[0].Status)
	}
}

func TestBenchmarkMatrixCompareTargets(t *testing.T) {
	// 矩阵对比：同一 case 打两个 target（好 agent vs 坏 agent），
	// Diff 报告状态迁移——「改 prompt 前后」的最小形态。
	dir := t.TempDir()
	mkTarget := func(name string, finalText string) Target {
		mock := &provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall(
				"c1", "save", map[string]any{"path": "a.txt", "content": "data"})}},
			{Text: finalText},
		}}
		tgt := NewAgentTarget(name,
			WithProvider(mock),
			WithMaxTurns(3),
			WithSessionWorkDir(func(string) string { return dir }),
		)
		tgt.UseTools(InferTool("save", "写文件", func(ctx context.Context, in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}) (string, error) {
			return "ok", os.WriteFile(filepath.Join(WorkDirFromContext(ctx), in.Path), []byte(in.Content), 0o644)
		}))
		return tgt
	}
	cases := []Case{{
		ID: "report-content", Input: "把 data 写入 a.txt",
		Asserts: []Assert{{Type: "contains", Value: "data"}},
	}}

	good := mkTarget("good", "已写入 data")
	bad := mkTarget("bad", "写坏了")
	r := &Runner{Targets: []Target{good, bad}, Cases: cases}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sums := rep.Summaries()
	if len(sums) != 2 {
		t.Fatalf("应有 2 个 summary, got %d", len(sums))
	}
	byTarget := map[string]CaseSummary{}
	for _, s := range sums {
		byTarget[s.Target] = s
	}
	if !byTarget["good"].AllPass {
		t.Errorf("good 应全过: %+v", byTarget["good"])
	}
	if byTarget["bad"].AllPass || byTarget["bad"].Passed != 0 {
		t.Errorf("bad 应全挂: %+v", byTarget["bad"])
	}

	// 自身 diff 应零迁移（回归对比第一公民的最小验证）。
	d := DiffReports(rep, rep)
	if len(d.Regressed) != 0 || len(d.Fixed) != 0 {
		t.Errorf("相同报告 diff 应零迁移: %+v", d)
	}
}

func TestBenchmarkFacadeAliases(t *testing.T) {
	// 门面别名完整性：根包 API 面与子包一致可用。
	if _, err := LoadReport(filepath.Join(t.TempDir(), "nonexistent.jsonl")); err == nil {
		t.Errorf("读不存在报告应报错")
	}
	c := Case{ID: "x", Input: "i", Asserts: []Assert{{Type: "contains", Value: "v"}}}
	if SuiteHash(c) == "" {
		t.Errorf("SuiteHash 不应为空")
	}
	// 自定义断言经根包注册表。
	RegisterAssert("root-custom", func(_ context.Context, _ Case, _ TargetOutput, a Assert) (Verdict, error) {
		return Verdict{Pass: true, Score: 1, Reason: "root custom"}, nil
	})
	found := false
	for _, typ := range AssertTypes() {
		if typ == "root-custom" {
			found = true
		}
	}
	if !found {
		t.Errorf("根包 RegisterAssert 应注册进同一注册表")
	}
}

func TestBenchmarkJudgeFacadePipeline(t *testing.T) {
	// B1 门面自食：LLMJudge + judge 断言 + pass^k + junit 全走根包别名。
	// mock 裁判恒 PASS；被测 target 4 trial 全过 → pass^2 = 1。
	// Concurrency=1：MockProvider 游标非并发安全，judge 与 agent 共享
	// mock 时必须串行（真实 provider 是 HTTP 调用，无此约束）。
	agent := NewAgentTargetFromFactory("chatty", func() []Option {
		return []Option{WithProvider(provider.NewMockProvider("分镜摘要：主角穿过雨巷。"))}
	})
	r := &Runner{
		Targets: []Target{agent},
		Cases: []Case{{
			ID: "quality", Input: "写一句分镜描述",
			Asserts: []Assert{{Type: "judge", Value: "输出是具体可拍摄的镜头描述"}},
		}},
		Repeat:      4,
		Concurrency: 1,
		Judge:       &LLMJudge{Provider: provider.NewMockProvider("PASS\n输出具体。")},
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rep.Rows {
		if row.Status != StatusPass {
			t.Errorf("row %d 应 pass: %+v", row.Trial, row)
		}
	}
	stats := rep.PassK(2)
	if len(stats) != 1 || stats[0].PassPowK != 1 {
		t.Errorf("4/4 全过 pass^2 应为 1: %+v", stats)
	}
	var sb strings.Builder
	if err := rep.JUnitXML(&sb); err != nil || !strings.Contains(sb.String(), "tests=\"1\"") {
		t.Errorf("junit 输出异常: err=%v xml=%s", err, sb.String())
	}
}
