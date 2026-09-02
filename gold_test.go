// gold_test.go GoldReplayer + 终态采集端到端测试：DeriveState 派生
// 期望快照 → AgentTarget.CaptureStateFrom 采集真实终态 → state-equals
// 断言判分 → ValidateCase 入库验证，全链路走真实文件工具。
package goagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

// goldTools 测试用的写文件工具面（与 builtin.Write 等价的最小实现，
// 避免测试依赖审批模式）。
func goldTools() []NamedTool {
	return []NamedTool{
		InferTool("save", "写文件", func(ctx context.Context, in struct {
			Path    string `json:"path" desc:"路径"`
			Content string `json:"content" desc:"内容"`
		}) (string, error) {
			full := filepath.Join(WorkDirFromContext(ctx), in.Path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			return "ok", os.WriteFile(full, []byte(in.Content), 0o644)
		}),
	}
}

func TestDeriveStateReplay(t *testing.T) {
	dir := t.TempDir()
	calls := []ToolCall{
		{Name: "save", Input: []byte(`{"path":"a.txt","content":"alpha"}`)},
		{Name: "save", Input: []byte(`{"path":"sub/b.txt","content":"beta"}`)},
	}
	state, err := DeriveState(context.Background(), dir, goldTools(), calls)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.txt": "alpha", "sub/b.txt": "beta"}
	if len(state) != len(want) {
		t.Fatalf("快照应含 %d 个文件, got %d: %v", len(want), len(state), state)
	}
	for p, c := range want {
		if state[p] != c {
			t.Errorf("state[%s] = %q, want %q", p, state[p], c)
		}
	}
	// 文件真实落盘（工具真实执行，不是 mock 空转）。
	if b, err := os.ReadFile(filepath.Join(dir, "sub/b.txt")); err != nil || string(b) != "beta" {
		t.Errorf("b.txt 应落盘: %q err=%v", b, err)
	}
}

func TestDeriveStateGoldTrajectoryBroken(t *testing.T) {
	// gold 轨迹引用不存在的工具 → error（轨迹/装配坏了，派生物不可信）。
	calls := []ToolCall{{Name: "nope", Input: []byte(`{}`)}}
	if _, err := DeriveState(context.Background(), t.TempDir(), goldTools(), calls); err == nil {
		t.Errorf("未知工具应报错")
	}
}

func TestGoldReplayerEndToEnd(t *testing.T) {
	// 全链路：gold 回放派生期望快照 → 真实 agent 跑同一任务 →
	// state-equals 判分 → ValidateCase 双端验证。
	base := t.TempDir()
	goldDir := filepath.Join(base, "gold")
	os.MkdirAll(goldDir, 0o755)

	goldCalls := []ToolCall{
		{Name: "save", Input: []byte(`{"path":"note.txt","content":"hello benchmark"}`)},
	}
	wantState, err := DeriveState(context.Background(), goldDir, goldTools(), goldCalls)
	if err != nil {
		t.Fatal(err)
	}
	// DeriveState 返回 map[string]string，转 Value 兼容的 map[string]any。
	wantAny := map[string]any{}
	for p, c := range wantState {
		wantAny[p] = c
	}

	// 被测 agent：mock provider 指令走同样的写文件动作（不同路径顺序
	// 无关——state-equals 只比终态，等价路径全通过）。
	trialDir := filepath.Join(base, "trial")
	os.MkdirAll(trialDir, 0o755)
	target := NewAgentTargetFromFactory("files", func() []Option {
		return []Option{
			WithProvider(provider.NewMockProvider()),
			// WithSessionWorkDir 让 save 工具的相对路径落 trialDir。
			WithSessionWorkDir(func(string) string { return trialDir }),
			WithMaxTurns(2),
		}
	})
	target.UseTools(goldTools()...)
	target.CaptureStateFrom(func() string { return trialDir })

	c := Case{
		ID: "write-note", Input: "把 hello benchmark 写入 note.txt",
		Asserts: []Assert{
			{Type: "state-equals", Value: wantAny},
			{Type: "file-not-exists", Value: "secret.txt"}, // Preserve：不留垃圾
		},
	}
	// mock provider 无响应指令时 agent 不动文件 → 空 State。
	// 用 gold 输出做 gold 端、空输出做 bad 端验证 case 合格。
	goldOut := TargetOutput{State: wantState}
	badOut := TargetOutput{State: map[string]string{}}
	if err := ValidateCase(c, goldOut, badOut); err != nil {
		t.Fatalf("case 应合格: %v", err)
	}

	rep, err := (&Runner{
		Targets: []Target{target},
		Cases:   []Case{c},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// mock provider 默认响应不含工具调用 → agent 没写文件 → 终态空 →
	// state-equals 挂（agent 任务失败的正确表达，不是 infra）。
	row := rep.Rows[0]
	if row.Status != StatusFail {
		t.Errorf("未产出终态应判 fail, got %s (note=%q)", row.Status, row.Note)
	}
	if row.Status != StatusInfraError {
		// 空终态判挂——正是「只看输出会漏掉文件没写」的终态断言价值。
		t.Logf("终态断言生效：mock agent 未写文件被判 %s（终态通道独立于输出通道）", row.Status)
	}
}
