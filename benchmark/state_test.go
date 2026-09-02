// state_test.go 终态断言族 + case 入库验证测试。
package benchmark

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// evalOne 直接查注册表跑单条断言（内部测试形态——assert_test.go 的
// 同名助手在 benchmark_test 包，签名不同不冲突）。
func evalOne(a Assert, out TargetOutput) (Verdict, error) {
	fn, ok := lookupAssert(a.Type)
	if !ok {
		return Verdict{}, fmt.Errorf("未知断言类型 %q", a.Type)
	}
	return fn(context.Background(), Case{}, out, a)
}

func stAssert(typ string, value any) Assert {
	return Assert{Type: typ, Value: value}
}

func TestFileExists(t *testing.T) {
	out := TargetOutput{State: map[string]string{"a.txt": "x", "sub/b.txt": "y"}}

	if v, err := evalOne(stAssert("file-exists", "a.txt"), out); err != nil || !v.Pass {
		t.Errorf("存在的文件应过: %+v err=%v", v, err)
	}
	// 数组形态 + 部分命中给部分分。
	v, _ := evalOne(stAssert("file-exists", []any{"a.txt", "missing.txt"}), out)
	if v.Pass || v.Score != 0.5 {
		t.Errorf("1/2 存在应 Score=0.5: %+v", v)
	}
	// 嵌套路径用 '/' 键。
	if v, _ := evalOne(stAssert("file-exists", "sub/b.txt"), out); !v.Pass {
		t.Errorf("嵌套路径应过: %+v", v)
	}
	// 空 State（target 没采集终态）→ 全挂。
	if v, _ := evalOne(stAssert("file-exists", "a.txt"), TargetOutput{}); v.Pass {
		t.Errorf("空 State 应挂: %+v", v)
	}
}

func TestFileNotExists(t *testing.T) {
	out := TargetOutput{State: map[string]string{"secret.txt": "答案"}}
	if v, _ := evalOne(stAssert("file-not-exists", "secret.txt"), out); v.Pass {
		t.Errorf("不该存在的文件出现了应挂: %+v", v)
	}
	if v, _ := evalOne(stAssert("file-not-exists", []any{"gone.txt", "other.txt"}), out); !v.Pass {
		t.Errorf("不存在应过: %+v", v)
	}
}

func TestFileContains(t *testing.T) {
	out := TargetOutput{State: map[string]string{"note.txt": "hello benchmark"}}
	ok := stAssert("file-contains", map[string]any{"path": "note.txt", "contains": "benchmark"})
	if v, err := evalOne(ok, out); err != nil || !v.Pass {
		t.Errorf("内容命中应过: %+v err=%v", v, err)
	}
	// 文件不存在比内容不符更根本，两者都挂。
	missFile := stAssert("file-contains", map[string]any{"path": "no.txt", "contains": "x"})
	if v, _ := evalOne(missFile, out); v.Pass || !strings.Contains(v.Reason, "文件不存在") {
		t.Errorf("文件不存在应挂且 reason 指明: %+v", v)
	}
	missContent := stAssert("file-contains", map[string]any{"path": "note.txt", "contains": "zzz"})
	if v, _ := evalOne(missContent, out); v.Pass || !strings.Contains(v.Reason, "不包含") {
		t.Errorf("内容不符应挂且 reason 指明: %+v", v)
	}
	// 数组形态。
	multi := stAssert("file-contains", []any{
		map[string]any{"path": "note.txt", "contains": "hello"},
		map[string]any{"path": "note.txt", "contains": "zzz"},
	})
	if v, _ := evalOne(multi, out); v.Pass || v.Score != 0.5 {
		t.Errorf("数组 1/2 命中应 Score=0.5: %+v", v)
	}
	// 坏 case：缺 path 字段 → error（case 损坏）。
	if _, err := evalOne(stAssert("file-contains", map[string]any{"contains": "x"}), out); err == nil {
		t.Errorf("缺 path 应返回 error")
	}
}

func TestStateEquals(t *testing.T) {
	want := map[string]any{"a.txt": "1", "b.txt": "2"}

	// 完全一致 → 过。
	out := TargetOutput{State: map[string]string{"a.txt": "1", "b.txt": "2"}}
	if v, err := evalOne(stAssert("state-equals", want), out); err != nil || !v.Pass {
		t.Errorf("快照一致应过: %+v err=%v", v, err)
	}

	// 缺失 + 内容不同 + 多余三类差异都识别。
	out = TargetOutput{State: map[string]string{"a.txt": "wrong", "extra.txt": "垃圾"}}
	v, _ := evalOne(stAssert("state-equals", want), out)
	if v.Pass {
		t.Fatalf("三类差异应挂: %+v", v)
	}
	for _, frag := range []string{"缺失", "内容不同", "多余"} {
		if !strings.Contains(v.Reason, frag) {
			t.Errorf("reason 应含 %q: %s", frag, v.Reason)
		}
	}
	// 分母取大边：期望 2 实际 3，命中 0 → 0 分。
	if v.Score != 0 {
		t.Errorf("全空命中且多余文件 Score 应 0: %+v", v)
	}

	// 多余文件即使期望全命中也拉分（agent 留垃圾终态是脏的）。
	out = TargetOutput{State: map[string]string{"a.txt": "1", "b.txt": "2", "junk.txt": "x"}}
	if v, _ := evalOne(stAssert("state-equals", want), out); v.Pass {
		t.Errorf("多余文件应挂: %+v", v)
	}

	// 坏 case：Value 不是快照对象 → error。
	if _, err := evalOne(stAssert("state-equals", "not-a-map"), out); err == nil {
		t.Errorf("非 map Value 应返回 error")
	}
}

func TestAgentViewStripsAsserts(t *testing.T) {
	c := Case{
		ID: "c", Family: "f", Input: "任务",
		Asserts:  []Assert{{Type: "contains", Value: "答案"}},
		Tags:     []string{"regression"},
		Metadata: map[string]any{"hidden": true},
	}
	view := c.AgentView()
	if len(view.Asserts) != 0 || view.Tags != nil || view.Metadata != nil {
		t.Errorf("AgentView 应剥掉判分信息: %+v", view)
	}
	if view.ID != "c" || view.Input != "任务" || view.Family != "f" {
		t.Errorf("AgentView 应保留 ID/Input/Family: %+v", view)
	}
	// 原 case 不受影响（值拷贝语义）。
	if len(c.Asserts) != 1 {
		t.Errorf("原 case 被改坏: %+v", c)
	}
}

func TestValidateCase(t *testing.T) {
	c := Case{
		ID:    "c",
		Input: "把 hello 写入 greeting.txt",
		Asserts: []Assert{
			{Type: "contains", Value: "hello"},
			{Type: "file-exists", Value: "greeting.txt"},
		},
	}
	goodOut := TargetOutput{Output: "已写入 hello", State: map[string]string{"greeting.txt": "hello"}}

	// 合格 case：gold 过、bad 挂。
	badOut := TargetOutput{Output: "我拒绝", State: map[string]string{}}
	if err := ValidateCase(c, goodOut, badOut); err != nil {
		t.Errorf("合格 case 不应报错: %v", err)
	}

	// gold 端挂：断言无法判定成功 → 坏 case。
	wrongGold := TargetOutput{Output: "没有 hello", State: map[string]string{}}
	if err := ValidateCase(c, wrongGold, badOut); err == nil || !strings.Contains(err.Error(), "gold") {
		t.Errorf("gold 挂应报错: %v", err)
	}

	// bad 端全过：恒真断言 → 坏 case。
	immuneBad := TargetOutput{Output: "已写入 hello", State: map[string]string{"greeting.txt": "hello"}}
	if err := ValidateCase(c, goodOut, immuneBad); err == nil || !strings.Contains(err.Error(), "恒真") {
		t.Errorf("bad 全过应报错: %v", err)
	}

	// case 损坏（坏正则）：error 应包装传播。
	broken := Case{ID: "b", Input: "i", Asserts: []Assert{{Type: "regex", Value: "("}}}
	if err := ValidateCase(broken, goodOut, badOut); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Errorf("损坏 case 应报错: %v", err)
	}
}
