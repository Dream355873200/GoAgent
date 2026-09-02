// benchmark 子包断言原语单测：各原语的通过/失败/取反/坏 case 路径。
package benchmark_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/benchmark"
)

func evalOne(t *testing.T, a benchmark.Assert, out benchmark.TargetOutput) (benchmark.AssertVerdict, error) {
	t.Helper()
	c := benchmark.Case{ID: "t", Input: "x", Asserts: []benchmark.Assert{a}}
	_, _, verdicts, err := benchmark.EvaluateCase(context.Background(), c, out)
	if err != nil {
		return benchmark.AssertVerdict{}, err
	}
	return verdicts[0], nil
}

func TestAssertContains(t *testing.T) {
	out := benchmark.TargetOutput{Output: "东京的时区是 UTC+9"}
	v, err := evalOne(t, benchmark.Assert{Type: "contains", Value: "UTC+9"}, out)
	if err != nil || !v.Pass || v.Score != 1 {
		t.Errorf("contains 命中应满分: v=%+v err=%v", v, err)
	}
	// 数组 = 全部包含，部分命中给部分分。
	v, _ = evalOne(t, benchmark.Assert{Type: "contains", Value: []any{"UTC+9", "不存在"}}, out)
	if v.Pass || v.Score != 0.5 {
		t.Errorf("部分命中应半分不通过: %+v", v)
	}
	// 取反。
	v, _ = evalOne(t, benchmark.Assert{Type: "contains", Value: "不存在", Negate: true}, out)
	if !v.Pass {
		t.Errorf("取反后应通过: %+v", v)
	}
	// 坏 case：Value 为 nil。
	if _, err := evalOne(t, benchmark.Assert{Type: "contains"}, out); err == nil {
		t.Errorf("Value 为空应报错（case 损坏）")
	}
}

func TestAssertEqualsAndStartsWith(t *testing.T) {
	out := benchmark.TargetOutput{Output: "hello world"}
	if v, _ := evalOne(t, benchmark.Assert{Type: "equals", Value: "hello world"}, out); !v.Pass {
		t.Errorf("equals 相等应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "equals", Value: "hello"}, out); v.Pass {
		t.Errorf("equals 子串不应过（严格相等）")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "starts-with", Value: "hello"}, out); !v.Pass {
		t.Errorf("starts-with 应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "starts-with", Value: "world"}, out); v.Pass {
		t.Errorf("非前缀不应过")
	}
}

func TestAssertRegex(t *testing.T) {
	out := benchmark.TargetOutput{Output: "耗时 123ms 完成"}
	if v, _ := evalOne(t, benchmark.Assert{Type: "regex", Value: `\d+ms`}, out); !v.Pass {
		t.Errorf("regex 命中应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "regex", Value: `\d+秒`}, out); v.Pass {
		t.Errorf("regex 未命中不应过")
	}
	// 编译失败 = case 损坏（error 而非 fail）。
	if _, err := evalOne(t, benchmark.Assert{Type: "regex", Value: "("}, out); err == nil {
		t.Errorf("坏正则应报 case 损坏错误")
	}
}

func TestAssertIsJSON(t *testing.T) {
	ok := benchmark.TargetOutput{Output: `{"name":"go","stars":100}`}
	if v, _ := evalOne(t, benchmark.Assert{Type: "is-json"}, ok); !v.Pass {
		t.Errorf("合法 JSON 应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "is-json", Value: []any{"name", "stars"}}, ok); !v.Pass {
		t.Errorf("含全部 key 应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "is-json", Value: []any{"forks"}}, ok); v.Pass {
		t.Errorf("缺 key 不应过")
	}
	bad := benchmark.TargetOutput{Output: "not json"}
	if v, _ := evalOne(t, benchmark.Assert{Type: "is-json"}, bad); v.Pass {
		t.Errorf("非 JSON 不应过")
	}
	// 顶层数组时 key 校验 = case 无法判定？不，是明确不通过（结构不符）。
	arr := benchmark.TargetOutput{Output: `[1,2,3]`}
	if v, _ := evalOne(t, benchmark.Assert{Type: "is-json", Value: []any{"a"}}, arr); v.Pass {
		t.Errorf("顶层数组缺对象 key 不应过")
	}
}

func TestAssertTrajectory(t *testing.T) {
	out := benchmark.TargetOutput{
		Output: "done",
		ToolCalls: []benchmark.ToolCall{
			{Name: "Write", Input: []byte(`{"file_path":"a.txt"}`)},
			{Name: "Read", Input: []byte(`{"file_path":"a.txt"}`)},
			{Name: "Read", Input: []byte(`{"file_path":"b.txt"}`)},
		},
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-used", Value: []any{"Write", "Read"}}, out); !v.Pass {
		t.Errorf("tool-used 全部存在应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-used", Value: "Edit"}, out); v.Pass {
		t.Errorf("不存在工具不应过")
	}
	// 通配：W* 匹配 Write。
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-used", Value: "W*"}, out); !v.Pass {
		t.Errorf("通配 W* 应匹配 Write")
	}
	// 序列完全一致（含顺序与完整性）。
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-sequence", Value: []any{"Write", "Read", "Read"}}, out); !v.Pass {
		t.Errorf("序列一致应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-sequence", Value: []any{"Write", "Read"}}, out); v.Pass {
		t.Errorf("缺一次 Read 的序列不应过（完整性）")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "tool-sequence", Value: []any{"Read", "Write"}}, out); v.Pass {
		t.Errorf("顺序颠倒不应过")
	}
}

func TestAssertResourceLimits(t *testing.T) {
	out := benchmark.TargetOutput{LatencyMs: 500, Tokens: 1200}
	if v, _ := evalOne(t, benchmark.Assert{Type: "max-latency-ms", Value: float64(1000)}, out); !v.Pass {
		t.Errorf("耗时在上限内应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "max-latency-ms", Value: float64(100)}, out); v.Pass {
		t.Errorf("耗时超限不应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "max-tokens", Value: float64(2000)}, out); !v.Pass {
		t.Errorf("token 在上限内应过")
	}
	if v, _ := evalOne(t, benchmark.Assert{Type: "max-tokens", Value: float64(1000)}, out); v.Pass {
		t.Errorf("token 超限不应过")
	}
}

func TestRegisterAssertCustom(t *testing.T) {
	benchmark.RegisterAssert("contains-both", func(_ context.Context, _ benchmark.Case, out benchmark.TargetOutput, a benchmark.Assert) (benchmark.Verdict, error) {
		want, _ := a.Value.([]any)
		aPart, bPart := want[0].(string), want[1].(string)
		hasA, hasB := strings.Contains(out.Output, aPart), strings.Contains(out.Output, bPart)
		score := 0.0
		if hasA {
			score += 0.5
		}
		if hasB {
			score += 0.5
		}
		return benchmark.Verdict{Pass: hasA && hasB, Score: score, Reason: "自定义双包含"}, nil
	})
	out := benchmark.TargetOutput{Output: "alpha only"}
	v, err := evalOne(t, benchmark.Assert{Type: "contains-both", Value: []any{"alpha", "beta"}}, out)
	if err != nil || v.Pass || v.Score != 0.5 {
		t.Errorf("自定义断言半分: v=%+v err=%v", v, err)
	}
	// 类型清单应包含注册的类型。
	found := false
	for _, typ := range benchmark.AssertTypes() {
		if typ == "contains-both" {
			found = true
		}
	}
	if !found {
		t.Errorf("AssertTypes 应含自定义类型")
	}
}

func TestUnknownAssertType(t *testing.T) {
	_, err := evalOne(t, benchmark.Assert{Type: "no-such-type"}, benchmark.TargetOutput{})
	if err == nil || !strings.Contains(err.Error(), "可用") {
		t.Errorf("未知类型报错应附可用清单: %v", err)
	}
}
