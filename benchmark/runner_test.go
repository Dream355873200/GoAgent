// runner/report 单测：四态落定、Repeat、infra 重试不进分母、
// 套件校验、JSONL 往返、Diff 状态迁移、SuiteHash 稳定性。
package benchmark_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Dream355873200/GoAgent/benchmark"
)

// echoTarget 恒定输出（mock 目标，引擎回归层形态）。
func echoTarget(name, output string) benchmark.Target {
	return benchmark.NamedTarget(name, func(_ context.Context, _ benchmark.Case) (benchmark.TargetOutput, error) {
		return benchmark.TargetOutput{Output: output}, nil
	})
}

// flakyTarget 前 n 次 infra 错误，之后正常输出。
// calls 用原子计数：Runner 并发跑 trial，闭包计数器必须并发安全。
func flakyTarget(name, output string, fails int) (benchmark.Target, *atomic.Int64) {
	var calls atomic.Int64
	return benchmark.NamedTarget(name, func(_ context.Context, _ benchmark.Case) (benchmark.TargetOutput, error) {
		if calls.Add(1) <= int64(fails) {
			return benchmark.TargetOutput{}, errors.New("connection refused")
		}
		return benchmark.TargetOutput{Output: output}, nil
	}), &calls
}

func basicCase(id, want string) benchmark.Case {
	return benchmark.Case{
		ID: id, Input: "hi",
		Asserts: []benchmark.Assert{{Type: "contains", Value: want}},
	}
}

func TestRunnerFourStates(t *testing.T) {
	ok := echoTarget("ok", "hello world")
	bad := echoTarget("bad", "goodbye")
	// 前 2 次 infra，默认重试 2 次 → 第 3 次成功。
	flaky, calls := flakyTarget("flaky", "hello world", 2)

	cases := []benchmark.Case{
		basicCase("c-pass", "hello"),
		{ // 断言自身坏（regex 编译失败）→ excluded 不进分母
			ID: "c-excluded", Input: "x",
			Asserts: []benchmark.Assert{{Type: "regex", Value: "("}},
		},
	}

	r := &benchmark.Runner{Targets: []benchmark.Target{ok, bad, flaky}, Cases: cases, Repeat: 2}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Rows) != 12 { // 3 targets × 2 cases × 2 trials
		t.Fatalf("应有 12 行, got %d", len(rep.Rows))
	}
	pass, fail, total := rep.SuiteStats()
	// Summaries 共 3 targets × 2 cases = 6；坏 case c-excluded 的三个
	// summary 全部 excluded 不进分母；进分母 3 个：ok/c-pass pass、
	// bad/c-pass fail、flaky/c-pass（重试后）pass。
	if total != 6 {
		t.Errorf("summary 总数应 6, got %d", total)
	}
	if pass != 2 || fail != 1 {
		t.Errorf("pass=%d fail=%d, want 2/1", pass, fail)
	}
	// flaky 全部调用 = 4 次成功（2 case × 2 trial）+ 2 次失败被重试消化。
	if n := calls.Load(); n != 6 {
		t.Errorf("flaky 调用数 = %d, want 6（4 成功 + 2 infra 重试）", n)
	}

	// 状态清点：不应有 infra_error 行落盘（重试成功）。
	for _, row := range rep.Rows {
		if row.Status == benchmark.StatusInfraError {
			t.Errorf("重试成功后不应有 infra_error 行: %+v", row)
		}
	}
	// excluded 行保留（留痕）但标记。
	excluded := 0
	for _, row := range rep.Rows {
		if row.Status == benchmark.StatusExcluded {
			excluded++
			if row.Note == "" {
				t.Errorf("excluded 行应带原因 Note")
			}
		}
	}
	if excluded != 6 { // 3 targets × 2 trials × 1 坏 case
		t.Errorf("excluded 行应 6, got %d", excluded)
	}
}

func TestRunnerInfraErrorAfterRetries(t *testing.T) {
	// 恒定失败：重试耗尽后落 infra_error，不进分母。
	dead := benchmark.NamedTarget("dead", func(_ context.Context, _ benchmark.Case) (benchmark.TargetOutput, error) {
		return benchmark.TargetOutput{}, errors.New("api key invalid")
	})
	r := &benchmark.Runner{Targets: []benchmark.Target{dead}, Cases: []benchmark.Case{basicCase("c", "x")}, Repeat: 1}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows[0].Status != benchmark.StatusInfraError {
		t.Errorf("重试耗尽应落 infra_error, got %s", rep.Rows[0].Status)
	}
	if rep.Rows[0].Error == "" {
		t.Errorf("infra_error 行应带错误信息")
	}
	pass, fail, total := rep.SuiteStats()
	if pass != 0 || fail != 0 || total != 1 {
		t.Errorf("全 infra 的 case 不进分母: pass=%d fail=%d total=%d", pass, fail, total)
	}
}

func TestRunnerSuiteValidation(t *testing.T) {
	r := &benchmark.Runner{Targets: []benchmark.Target{echoTarget("t", "x")}}
	if _, err := r.Run(context.Background()); err == nil {
		t.Errorf("空 Cases 应报错")
	}
	r.Cases = []benchmark.Case{basicCase("", "x")}
	if _, err := r.Run(context.Background()); err == nil || err.Error() == "" {
		t.Errorf("缺 ID 应报错")
	}
	r.Cases = []benchmark.Case{
		basicCase("dup", "x"),
		basicCase("dup", "x"),
	}
	if _, err := r.Run(context.Background()); err == nil {
		t.Errorf("重复 ID 应报错")
	}
	r.Cases = []benchmark.Case{{ID: "no-assert", Input: "x"}}
	if _, err := r.Run(context.Background()); err == nil {
		t.Errorf("无断言 case 应报错（不可判定）")
	}
	r.Cases = []benchmark.Case{{ID: "bad-type", Input: "x",
		Asserts: []benchmark.Assert{{Type: "no-such"}}}}
	if _, err := r.Run(context.Background()); err == nil {
		t.Errorf("未知断言类型应报错")
	}
}

func TestReportJSONLRoundtrip(t *testing.T) {
	r := &benchmark.Runner{
		Targets: []benchmark.Target{echoTarget("t", "hello")},
		Cases:   []benchmark.Case{basicCase("c", "hello")},
		Repeat:  2,
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.jsonl")
	if err := rep.SaveReport(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := benchmark.LoadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SuiteHash != rep.SuiteHash || len(loaded.Rows) != len(rep.Rows) {
		t.Errorf("往返应保真: hash %q vs %q, rows %d vs %d",
			loaded.SuiteHash, rep.SuiteHash, len(loaded.Rows), len(rep.Rows))
	}
	if loaded.Repeat != 2 {
		t.Errorf("Repeat 应保留: %d", loaded.Repeat)
	}
	// 聚合在加载副本上应等价。
	p1, f1, _ := rep.SuiteStats()
	p2, f2, _ := loaded.SuiteStats()
	if p1 != p2 || f1 != f2 {
		t.Errorf("加载后聚合应等价: (%d,%d) vs (%d,%d)", p1, f1, p2, f2)
	}
}

func TestSuiteHashStableAndSensitive(t *testing.T) {
	a := []benchmark.Case{basicCase("c1", "x"), basicCase("c2", "y")}
	b := []benchmark.Case{basicCase("c2", "y"), basicCase("c1", "x")} // 顺序无关
	if benchmark.SuiteHash(a...) != benchmark.SuiteHash(b...) {
		t.Errorf("case 顺序不应影响哈希")
	}
	// Metadata map 序列化稳定（encoding/json 按 key 排序）。
	c1 := basicCase("c1", "x")
	c1.Metadata = map[string]any{"z": 1, "a": 2}
	c2 := basicCase("c1", "x")
	c2.Metadata = map[string]any{"a": 2, "z": 1}
	if benchmark.SuiteHash(c1) != benchmark.SuiteHash(c2) {
		t.Errorf("map 字段顺序不应影响哈希")
	}
	if benchmark.SuiteHash(c1) == benchmark.SuiteHash(basicCase("c1", "changed")) {
		t.Errorf("内容变化应改变哈希")
	}
}

func TestDiffReports(t *testing.T) {
	mk := func(target string, pass bool) *benchmark.Report {
		status := benchmark.StatusFail
		if pass {
			status = benchmark.StatusPass
		}
		return &benchmark.Report{
			SuiteHash: "same",
			Rows: []benchmark.Result{
				{Target: target, CaseID: "c1", Trial: 0, Status: status, Score: 1},
				{Target: target, CaseID: "c2", Trial: 0, Status: benchmark.StatusPass, Score: 1},
				{Target: target, CaseID: "c3", Trial: 0, Status: benchmark.StatusFail, Score: 0},
			},
		}
	}
	old := mk("t", true)
	newRep := mk("t", false) // c1: pass→fail

	d := benchmark.DiffReports(old, newRep)
	if len(d.Regressed) != 1 || d.Regressed[0] != "t/c1" {
		t.Errorf("c1 应判回归: %+v", d.Regressed)
	}
	if len(d.Fixed) != 0 || len(d.Unstable) != 0 {
		t.Errorf("不应有修复/不稳定项: %+v %+v", d.Fixed, d.Unstable)
	}

	// 反向：fail→pass = Fixed。
	d2 := benchmark.DiffReports(newRep, old)
	if len(d2.Fixed) != 1 || d2.Fixed[0] != "t/c1" {
		t.Errorf("反向 c1 应判修复: %+v", d2.Fixed)
	}

	// 套件哈希不同 → SuiteMismatch。
	old.SuiteHash = "other"
	d3 := benchmark.DiffReports(old, newRep)
	if !d3.SuiteMismatch {
		t.Errorf("哈希不同应标记 SuiteMismatch")
	}

	// infra 吞掉全部 trial → indeterminate → Unstable。
	old.SuiteHash = "same"
	infra := &benchmark.Report{
		SuiteHash: "same",
		Rows:      []benchmark.Result{{Target: "t", CaseID: "c1", Trial: 0, Status: benchmark.StatusInfraError}},
	}
	d4 := benchmark.DiffReports(old, infra)
	if len(d4.Unstable) == 0 {
		t.Errorf("pass→infra 应进 Unstable: %+v", d4)
	}
}
