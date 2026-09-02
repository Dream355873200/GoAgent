// junit.go JUnit XML 输出（B1）：把报告转成 CI 系统通吃的 junit.xml
// 格式——GitHub Actions / Jenkins / GitLab 都内建解析和展示。
//
// 映射口径：
//   - 一个 (target, case) 聚合 = 一个 <testcase>（name = "target/case_id"）
//   - AllPass = 通过；否则 <failure>（带 pass 率与失败断言摘要）
//   - Valid=0（全 infra/excluded）= <error>（基建问题不是 agent 失败，
//     CI 里要可视觉区分——error 不算红）
//   - <testsuite> 带 pass/fail/error 计数与耗时汇总
package benchmark

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

// junitXML 是 <testsuites> 根（多 target 时仍是一个 suite——case 才是
// 测试单位，target 是矩阵轴不是套件边界）。
type junitXML struct {
	XMLName xml.Name     `xml:"testsuites"`
	Name    string       `xml:"name,attr"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      string      `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	TestCases []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// JUnitXML 把报告写成 JUnit XML（单 testsuite，case 级聚合口径）。
func (r *Report) JUnitXML(w io.Writer) error {
	suite := junitSuite{
		Name:      "goagent-benchmark",
		Timestamp: r.StartedAt.UTC().Format(time.RFC3339),
	}
	if d := r.FinishedAt.Sub(r.StartedAt); d > 0 {
		suite.Time = fmt.Sprintf("%.3f", d.Seconds())
	}
	for _, s := range r.Summaries() {
		tc := junitCase{
			Name:      s.CaseID,
			ClassName: "benchmark." + s.Target,
			Time:      fmt.Sprintf("%.3f", s.MeanLatencyMs/1000),
		}
		suite.Tests++
		switch {
		case s.Valid == 0:
			suite.Errors++
			tc.Error = &junitMessage{
				Type:    "error",
				Message: "无法判定（全部 trial 为 infra_error/excluded）",
				Body:    fmt.Sprintf("trials=%d infra=%d excluded=%d", s.Trials, s.Infra, s.Excluded),
			}
		case s.AllPass:
			// 通过：无子元素
		default:
			suite.Failures++
			tc.Failure = &junitMessage{
				Type:    "failure",
				Message: fmt.Sprintf("pass %d/%d (mean_score=%.3f)", s.Passed, s.Valid, s.MeanScore),
				Body:    failureBody(r, s),
			}
		}
		suite.TestCases = append(suite.TestCases, tc)
	}
	doc := junitXML{Name: "goagent-benchmark", Suites: []junitSuite{suite}}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return enc.Flush()
}

// failureBody 从该 (target, case) 的 trial 行里挑第一条失败 trial 的
// 断言明细——CI 报告里点开能看到「挂的是哪条断言、为什么」。
func failureBody(r *Report, s CaseSummary) string {
	var b strings.Builder
	for _, row := range r.Rows {
		if row.Target != s.Target || row.CaseID != s.CaseID || row.Status != StatusFail {
			continue
		}
		for _, av := range row.Asserts {
			if !av.Pass {
				fmt.Fprintf(&b, "[%s] %s\n", av.Type, av.Reason)
			}
		}
		break // 一条失败 trial 足够定位；全部展开会淹没 CI 界面
	}
	return strings.TrimRight(b.String(), "\n")
}
