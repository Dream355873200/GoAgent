// passk_test.go pass^k / pass@k 数学验证与聚合口径测试。
package benchmark

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBinom(t *testing.T) {
	cases := []struct {
		n, k int
		want float64
	}{
		{5, 2, 10},   // C(5,2)
		{10, 3, 120}, // C(10,3)
		{4, 0, 1},    // C(n,0)=1
		{7, 7, 1},    // C(n,n)=1
		{3, 5, 0},    // k>n 非法
		{0, 0, 1},
		{20, 10, 184756}, // 大数仍精确（乘积公式中间值是整数）
	}
	for _, c := range cases {
		if got := binom(c.n, c.k); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("C(%d,%d) = %v, want %v", c.n, c.k, got, c.want)
		}
	}
}

func TestPassPowKAndAtK(t *testing.T) {
	// c=5, n=10：pass rate 0.5。
	// pass^2 = C(5,2)/C(10,2) = 10/45 ≈ 0.2222
	if got := passPowK(5, 10, 2); math.Abs(got-10.0/45.0) > 1e-12 {
		t.Errorf("pass^2 = %v, want %v", got, 10.0/45.0)
	}
	// pass@2 = 1 − C(5,2)/C(10,2) ≈ 0.7778（至少一次过）
	if got := passAtK(5, 10, 2); math.Abs(got-(1-10.0/45.0)) > 1e-12 {
		t.Errorf("pass@2 = %v, want %v", got, 1-10.0/45.0)
	}
	// k=1：两者都退化为 pass rate。
	if got := passPowK(5, 10, 1); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("pass^1 = %v, want 0.5", got)
	}
	if got := passAtK(5, 10, 1); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("pass@1 = %v, want 0.5", got)
	}
	// 失败数不足以填满 k 个抽样 → pass@k = 1（必然抽到通过）。
	if got := passAtK(9, 10, 2); got != 1 {
		t.Errorf("fails=1 < k=2 时 pass@2 应为 1, got %v", got)
	}
	// 全过 → pass^k = 1（任何 k）。
	if got := passPowK(10, 10, 4); got != 1 {
		t.Errorf("全过 pass^4 应为 1, got %v", got)
	}
	// 全挂 → 都是 0。
	if got := passPowK(0, 10, 3); got != 0 {
		t.Errorf("全挂 pass^3 应为 0, got %v", got)
	}
	// k 超样本：n=3 全过，k=5 → 退化为 3 次全过的证据（=1）。
	if got := passPowK(3, 3, 5); got != 1 {
		t.Errorf("k>n 且全过应为 1, got %v", got)
	}
	// k 超样本：n=3 过 2，k=5 → 退化为 C(2,3)/C(3,3)=0（3 次没全过）。
	if got := passPowK(2, 3, 5); got != 0 {
		t.Errorf("k>n 且未全过应为 0, got %v", got)
	}
}

// mkReport 手造报告（passk/junit 测试共用的构造器）。
func mkReport(rows ...Result) *Report {
	return &Report{
		SuiteHash:  "h",
		Targets:    []string{"t"},
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
		Rows:       rows,
	}
}

func TestReportPassKAggregation(t *testing.T) {
	// t/case1：6 valid（4 过 2 挂）+ 2 infra（不进分母）。
	// t/case2：全 infra（Valid=0，不产出统计）。
	var rows []Result
	for i := 0; i < 4; i++ {
		rows = append(rows, Result{Target: "t", CaseID: "case1", Trial: i, Status: StatusPass, Score: 1})
	}
	for i := 0; i < 2; i++ {
		rows = append(rows, Result{Target: "t", CaseID: "case1", Trial: 4 + i, Status: StatusFail, Score: 0})
	}
	rows = append(rows,
		Result{Target: "t", CaseID: "case1", Trial: 6, Status: StatusInfraError, Error: "网络"},
		Result{Target: "t", CaseID: "case1", Trial: 7, Status: StatusExcluded, Note: "case 损坏"},
		Result{Target: "t", CaseID: "case2", Trial: 0, Status: StatusInfraError, Error: "挂了"},
	)
	rep := mkReport(rows...)

	stats := rep.PassK(2)
	if len(stats) != 1 {
		t.Fatalf("全 infra 的 case 不应产出统计, got %d 条", len(stats))
	}
	st := stats[0]
	if st.N != 6 || st.Passed != 4 {
		t.Errorf("N = %d Passed = %d, want 6/4（infra 不进分母）", st.N, st.Passed)
	}
	// pass^2 = C(4,2)/C(6,2) = 6/15 = 0.4
	if math.Abs(st.PassPowK-6.0/15.0) > 1e-12 {
		t.Errorf("pass^2 = %v, want 0.4", st.PassPowK)
	}
	// pass@2 = 1 − C(2,2)/C(6,2) = 1 − 1/15 ≈ 0.9333
	if math.Abs(st.PassAtK-(1-1.0/15.0)) > 1e-12 {
		t.Errorf("pass@2 = %v, want %v", st.PassAtK, 1-1.0/15.0)
	}
}

func TestSummariesStats(t *testing.T) {
	// 方差/CI/延迟统计：scores [1, 0, 0.5, 0.5] → mean 0.5, std 0.3536；
	// latencies [100, 200, 300, 400] → mean 250, std 111.8。
	rep := mkReport(
		Result{Target: "t", CaseID: "c", Status: StatusPass, Score: 1, LatencyMs: 100},
		Result{Target: "t", CaseID: "c", Status: StatusFail, Score: 0, LatencyMs: 200},
		Result{Target: "t", CaseID: "c", Status: StatusFail, Score: 0.5, LatencyMs: 300},
		Result{Target: "t", CaseID: "c", Status: StatusFail, Score: 0.5, LatencyMs: 400},
	)
	s := rep.Summaries()[0]
	if math.Abs(s.MeanScore-0.5) > 1e-12 {
		t.Errorf("MeanScore = %v, want 0.5", s.MeanScore)
	}
	if math.Abs(s.ScoreStdDev-0.35355) > 1e-4 {
		t.Errorf("ScoreStdDev = %v, want ≈0.3536", s.ScoreStdDev)
	}
	// CI95 = 1.96σ/√4 = 1.96 × 0.35355 / 2 ≈ 0.3465
	if math.Abs(s.ScoreCI95-1.96*0.35355/2) > 1e-4 {
		t.Errorf("ScoreCI95 = %v, want ≈0.3465", s.ScoreCI95)
	}
	if math.Abs(s.MeanLatencyMs-250) > 1e-9 {
		t.Errorf("MeanLatencyMs = %v, want 250", s.MeanLatencyMs)
	}
	if math.Abs(s.LatencyStdDevMs-111.803) > 1e-2 {
		t.Errorf("LatencyStdDevMs = %v, want ≈111.8", s.LatencyStdDevMs)
	}

	// 单 trial：无离散，std/CI 均为 0。
	s1 := mkReport(Result{Target: "t", CaseID: "c", Status: StatusPass, Score: 1, LatencyMs: 50}).Summaries()[0]
	if s1.ScoreStdDev != 0 || s1.ScoreCI95 != 0 || s1.LatencyStdDevMs != 0 {
		t.Errorf("单 trial 离散度应为 0: %+v", s1)
	}
}

func TestJUnitXMLOutput(t *testing.T) {
	rep := mkReport(
		Result{Target: "good", CaseID: "case-pass", Status: StatusPass, Score: 1, LatencyMs: 100},
		Result{
			Target: "bad", CaseID: "case-fail", Status: StatusFail, Score: 0,
			Asserts: []AssertVerdict{{Type: "contains", Pass: false, Score: 0, Reason: "缺少片段 \"hello\""}},
		},
		Result{Target: "flaky", CaseID: "case-err", Status: StatusInfraError, Error: "超时"},
	)
	var sb strings.Builder
	if err := rep.JUnitXML(&sb); err != nil {
		t.Fatal(err)
	}
	xmlOut := sb.String()

	// 结构断言：三个 testcase、一个 failure（含断言明细）、一个 error。
	for _, want := range []string{
		"<testsuites", `name="case-pass"`, `name="case-fail"`, `name="case-err"`,
		"<failure", "<error", "缺少片段",
		`tests="3"`, `failures="1"`, `errors="1"`,
	} {
		if !strings.Contains(xmlOut, want) {
			t.Errorf("junit.xml 缺少 %q:\n%s", want, xmlOut)
		}
	}
	// 通过的 case 不应有 failure 子元素（只有名字出现）。
	if strings.Contains(xmlOut, `name="case-pass"><failure`) {
		t.Errorf("通过的 case 不应带 failure")
	}

	// 合法性：能被标准 XML 解析器回读。
	var back struct {
		XMLName xml.Name     `xml:"testsuites"`
		Suites  []junitSuite `xml:"testsuite"`
	}
	if err := xml.Unmarshal([]byte(xmlOut), &back); err != nil {
		t.Fatalf("junit.xml 不是合法 XML: %v", err)
	}
	if len(back.Suites) != 1 || back.Suites[0].Tests != 3 {
		t.Errorf("回读结构不符: %+v", back.Suites)
	}
}
