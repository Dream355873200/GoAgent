// judge_test.go LLM 判分通道测试：mock provider 驱动 LLMJudge 解析
// 逻辑 + "judge" 断言类型经 Runner 的全链路 + judge 故障语义。
package benchmark

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

func TestLLMJudgePassAndFail(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantPass    bool
	}{
		{"标准PASS", "PASS\n输出明确问候了用户。", true},
		{"标准FAIL", "FAIL\n输出未包含任何问候。", false},
		{"带前缀思考", "好的，让我看看。\nPASS: 准则第 1 条满足。", true},
		{"小写", "pass\n理由", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &LLMJudge{Provider: provider.NewMockProvider(tc.reply)}
			v, err := j.Judge(context.Background(), JudgeInput{
				Input: "写一句问候", Output: "你好世界", Criterion: "输出包含问候",
			})
			if err != nil {
				t.Fatal(err)
			}
			if v.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (reason: %s)", v.Pass, tc.wantPass, v.Reason)
			}
			// 分数是 0/1 二值（judge 是判定不是打分——连续分交给
			// 多准则比例）。
			if want := 0.0; tc.wantPass {
				want = 1
			} else if v.Score != want {
				t.Errorf("Score = %v, want %v", v.Score, want)
			}
		})
	}
}

func TestLLMJudgeBadFormatRetry(t *testing.T) {
	// 第一次响应格式损坏，重试一次拿到合法输出。
	j := &LLMJudge{
		Provider: provider.NewMockProvider("嗯，这个输出看起来还行。", "PASS\n第二次响应符合格式。"),
		Retries:  1,
	}
	v, err := j.Judge(context.Background(), JudgeInput{Criterion: "任意"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Pass {
		t.Errorf("重试后应判 PASS, got fail (reason: %s)", v.Reason)
	}
}

func TestLLMJudgeBadFormatExhausted(t *testing.T) {
	// 重试后仍解析不出 → error（judge 通道故障，不是 agent 失败）。
	j := &LLMJudge{
		Provider: provider.NewMockProvider("完全不知所云"),
		Retries:  1,
	}
	_, err := j.Judge(context.Background(), JudgeInput{Criterion: "任意"})
	if err == nil {
		t.Fatal("格式损坏重试后仍失败应返回 error")
	}
	if !strings.Contains(err.Error(), "PASS/FAIL") {
		t.Errorf("错误信息应说明解析失败: %v", err)
	}
}

// echoTarget 直接返回固定输出（judge 链路测试用——被测的不是 agent）。
func echoTarget(name, output string) Target {
	return NamedTarget(name, func(ctx context.Context, c Case) (TargetOutput, error) {
		return TargetOutput{Output: output}, nil
	})
}

func TestJudgeAssertFullPipeline(t *testing.T) {
	// 全链路：case 带 judge 断言 → Runner 注入 Judge → 判分 → 落态。
	newRunner := func(judgeReply string, extraAsserts ...Assert) *Runner {
		return &Runner{
			Targets: []Target{echoTarget("t", "这是一段生成的分镜描述")},
			Cases: []Case{{
				ID: "quality", Input: "生成一句分镜描述",
				Asserts: append([]Assert{{Type: "judge", Value: "输出是具体的视觉描述而非抽象概括"}}, extraAsserts...),
			}},
			Judge: &LLMJudge{Provider: provider.NewMockProvider(judgeReply)},
		}
	}

	t.Run("judge判过则case过", func(t *testing.T) {
		rep, err := newRunner("PASS\n输出足够具体。").Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Rows[0].Status != StatusPass {
			t.Errorf("judge PASS 应落 pass, got %s (note=%q)", rep.Rows[0].Status, rep.Rows[0].Note)
		}
	})
	t.Run("judge判挂则case挂", func(t *testing.T) {
		rep, err := newRunner("FAIL\n输出过于抽象。").Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Rows[0].Status != StatusFail {
			t.Errorf("judge FAIL 应落 fail, got %s", rep.Rows[0].Status)
		}
	})
	t.Run("judge与确定性断言AND", func(t *testing.T) {
		// judge 过但 contains 挂 → 整体 fail（两条通道互补、判定取 AND）。
		rep, err := newRunner("PASS\nok", Assert{Type: "contains", Value: "不存在的片段"}).Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Rows[0].Status != StatusFail {
			t.Errorf("确定性断言挂应整体 fail, got %s", rep.Rows[0].Status)
		}
	})
}

func TestJudgeAssertCriteriaList(t *testing.T) {
	// 多准则：JudgeFunc 无状态可并发（MockProvider 游标非并发安全，
	// 这里不用它）。两条准则一过一挂 → Score=0.5、Pass=false。
	j := JudgeFunc(func(ctx context.Context, in JudgeInput) (Verdict, error) {
		if strings.Contains(in.Criterion, "具体") {
			return Verdict{Pass: true, Score: 1, Reason: "PASS\n具体性满足"}, nil
		}
		return Verdict{Pass: false, Score: 0, Reason: "FAIL\n长度不足"}, nil
	})
	rep, err := (&Runner{
		Targets: []Target{echoTarget("t", "输出")},
		Cases: []Case{{
			ID: "multi", Input: "生成",
			Asserts: []Assert{{Type: "judge", Value: []any{"输出要具体", "输出要有一定长度"}}},
		}},
		Judge: j,
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := rep.Rows[0]
	if row.Status != StatusFail {
		t.Errorf("一挂一过应整体 fail, got %s", row.Status)
	}
	if row.Score != 0.5 {
		t.Errorf("Score = %v, want 0.5", row.Score)
	}
	if !strings.Contains(row.Asserts[0].Reason, "1/2") {
		t.Errorf("reason 应含命中比例: %s", row.Asserts[0].Reason)
	}
}

func TestJudgeMissingConfigRejected(t *testing.T) {
	// 用了 judge 断言但没配 Judge：validate 整单拒跑（配置错误）。
	_, err := (&Runner{
		Targets: []Target{echoTarget("t", "x")},
		Cases:   []Case{{ID: "c", Input: "i", Asserts: []Assert{{Type: "judge", Value: "r"}}}},
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Judge") {
		t.Fatalf("应报缺 Judge 配置错误, got %v", err)
	}
}

func TestJudgeFailureExcludesCase(t *testing.T) {
	// judge 自身故障（provider 挂）→ excluded 不进分母，不是 fail。
	j := JudgeFunc(func(ctx context.Context, in JudgeInput) (Verdict, error) {
		return Verdict{}, errors.New("网络超时")
	})
	rep, err := (&Runner{
		Targets: []Target{echoTarget("t", "x")},
		Cases:   []Case{{ID: "c", Input: "i", Asserts: []Assert{{Type: "judge", Value: "r"}}}},
		Judge:   j,
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows[0].Status != StatusExcluded {
		t.Errorf("judge 故障应落 excluded, got %s (note=%q)", rep.Rows[0].Status, rep.Rows[0].Note)
	}
}
