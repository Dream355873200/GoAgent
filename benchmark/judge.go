// judge.go LLM 判分通道（B1）：Judge 接口 + LLMJudge 实现 + "judge"
// 断言类型接线。
//
// 定位（docs/benchmark-research.md §2/§6 B1）：
//   - 确定性断言评「坏没坏」（格式/路径/成本），judge 评「好不好」
//     （内容质量）——两条通道互补，共用同一套 Verdict/Report/Diff
//   - judge 是概率的、可被 gaming 的（METR 教训）：它给的分数也要
//     重复运行看方差，且生产配置应有「人工抽检校准 judge」环节
//   - rubric 形态学 closedqa（OpenAI evals 验证过的做法）：判定准则
//     显式成文，judge 只依据准则判断——不写 rubric 让 judge「看着
//     办」是分差噪声的最大来源
//
// 接线方式：Judge 实例经 context 注入（WithJudge），"judge" 断言
// handler 从 ctx 取——保持 AssertFunc 纯函数签名，注册表不持状态。
package benchmark

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// ---------- Judge 契约 ----------

// JudgeInput 单次判分的全部输入。ToolCalls 让 judge 可参考执行轨迹
// （「有没有真做」和「做对了没」是两个维度——只看最终输出会漏掉
// 「碰巧答对但过程全错」的 run）。
type JudgeInput struct {
	CaseID    string     // 所属 case（日志/调试用）
	Input     string     // 原始任务输入
	Output    string     // agent 最终输出
	ToolCalls []ToolCall // 工具调用轨迹（可选参考）
	Criterion string     // 本次判定的判定准则（rubric 单条）
}

// Judge 内容判分器。实现必须是无偏好中立即严厉的——LLMJudge 的
// 默认 prompt 明确要求「宁可 FAIL 不可放水」。
type Judge interface {
	Judge(ctx context.Context, in JudgeInput) (Verdict, error)
}

// JudgeFunc 函数式适配器。
type JudgeFunc func(ctx context.Context, in JudgeInput) (Verdict, error)

func (f JudgeFunc) Judge(ctx context.Context, in JudgeInput) (Verdict, error) {
	return f(ctx, in)
}

// ---------- context 注入（对齐 SandboxFromContext 的既有约定） ----------

type judgeCtxKey struct{}

// WithJudge 把 Judge 注入 context（Runner.Run 自动做）。
func WithJudge(ctx context.Context, j Judge) context.Context {
	return context.WithValue(ctx, judgeCtxKey{}, j)
}

// JudgeFromContext 取注入的 Judge（未注入返回 nil）。
func JudgeFromContext(ctx context.Context) Judge {
	if j, ok := ctx.Value(judgeCtxKey{}).(Judge); ok {
		return j
	}
	return nil
}

// ---------- LLMJudge ----------

// LLMJudge 用任意 provider.Provider 做裁判。provider 是唯一的模型
// 抽象边界——宿主配 Anthropic/OpenAI/mock 都行，judge 与被测 agent
// 可以不同模型（推荐：judge 用与被测不同家的模型，降低同源偏好）。
type LLMJudge struct {
	Provider provider.Provider

	// Model 模型覆盖（走 provider.ModelSwitcher 或 Request.Model，
	// 留空用 provider 默认）。
	Model string

	// MaxTokens 单次判分的输出上限（默认 1024——理由不需要长）。
	MaxTokens int

	// Retries judge 输出格式损坏（解析不出 PASS/FAIL）时的重试次数
	// （默认 1）。LLM 偶发不守格式是常态，不是错误。
	Retries int
}

// judgeSystem 是判分 system prompt。三条纪律来自 closedqa 的实测
// 经验：只依据准则（禁止脑补标准）、宁可失败不可放水、先判断后
// 结论（理由在前结论在后——但输出格式约定结论在首行，所以 prompt
// 里要求「先在心里判断完再动笔」）。
const judgeSystem = `你是严格的评测裁判。你的任务是判断「待评输出」是否满足给定的「判定准则」。

规则：
1. 只依据判定准则判断。准则没提到的方面一律不评价，哪怕你认为是缺陷。
2. 拿不准就判 FAIL。判 PASS 需要准则被明确满足，不需要你脑补合理化。
3. 先在心里完成完整判断，再输出。

输出格式（严格遵守）：
第一行只能是 PASS 或 FAIL（大写，无其他字符）。
第二行起用不超过 120 字说明理由，指明依据准则的哪一条。`

func (j *LLMJudge) judge(ctx context.Context, in JudgeInput) (Verdict, error) {
	var b strings.Builder
	b.WriteString("【任务输入】\n")
	b.WriteString(in.Input)
	b.WriteString("\n\n【待评输出】\n")
	b.WriteString(in.Output)
	if len(in.ToolCalls) > 0 {
		names := make([]string, len(in.ToolCalls))
		for i, c := range in.ToolCalls {
			names[i] = c.Name
		}
		b.WriteString("\n\n【执行轨迹（工具调用序列，仅供参考）】\n")
		b.WriteString(strings.Join(names, " → "))
	}
	b.WriteString("\n\n【判定准则】\n")
	b.WriteString(in.Criterion)
	b.WriteString("\n\n请判断待评输出是否满足判定准则。")

	req := &provider.Request{
		Messages:  []message.Message{message.NewUserMessage(b.String())},
		MaxTokens: j.maxTokens(),
	}
	if j.Model != "" {
		req.Model = j.Model
	}

	resp, err := j.Provider.Complete(ctx, req)
	if err != nil {
		return Verdict{}, fmt.Errorf("judge 调用失败: %w", err)
	}
	v, perr := parseJudgeVerdict(messageText(resp.Message))
	if perr != nil {
		return Verdict{}, perr
	}
	return v, nil
}

func (j *LLMJudge) maxTokens() int {
	if j.MaxTokens > 0 {
		return j.MaxTokens
	}
	return 1024
}

// messageText 拼接消息的全部文本块（thinking 块不算——裁判理由以
// 正文为准）。
func messageText(m message.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// Judge 实现 Judge 接口：调 LLM → 解析 PASS/FAIL。格式损坏自动重试。
func (j *LLMJudge) Judge(ctx context.Context, in JudgeInput) (Verdict, error) {
	attempts := j.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		v, err := j.judge(ctx, in)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return Verdict{}, lastErr
}

// parseJudgeVerdict 解析 judge 输出：首行 PASS/FAIL（允许空白与前导
// 思考文本中先扫到合法行）。结论在首行 + 逐行扫描兜底——两层容错
// 比要求模型输出 JSON 稳（promptfoo 实测：结构化输出抖动率高于
// 首行标记）。
func parseJudgeVerdict(s string) (Verdict, error) {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.ToUpper(sc.Text()))
		switch {
		case line == "PASS", strings.HasPrefix(line, "PASS ") || strings.HasPrefix(line, "PASS，") || strings.HasPrefix(line, "PASS:"):
			return Verdict{Pass: true, Score: 1, Reason: strings.TrimSpace(s)}, nil
		case line == "FAIL", strings.HasPrefix(line, "FAIL ") || strings.HasPrefix(line, "FAIL，") || strings.HasPrefix(line, "FAIL:"):
			return Verdict{Pass: false, Score: 0, Reason: strings.TrimSpace(s)}, nil
		}
	}
	return Verdict{}, fmt.Errorf("judge 输出解析不出 PASS/FAIL（前 200 字: %.200s）", s)
}

// ---------- "judge" 断言类型 ----------

// assertJudge "judge" 断言：Value 为 rubric 文本（string）或判定准则
// 数组（[]string——每条独立判，全过才过，Score 按命中比例）。多条
// 准则并发判（独立 LLM 调用，无顺序依赖）。
// 需要 Runner 配置了 Judge（或宿主手动 WithJudge 注入 ctx）。
//
// judge 自身故障（网络挂/格式损坏重试后仍失败）返回 error → case
// 落 excluded 不进分母：judge 坏了不该惩罚被测 agent（对齐「断言
// 无法评估 ≠ agent 失败」的既有语义）。
func assertJudge(ctx context.Context, c Case, out TargetOutput, a Assert) (Verdict, error) {
	j := JudgeFromContext(ctx)
	if j == nil {
		return Verdict{}, fmt.Errorf("case 用了 judge 断言但未配置 Judge（Runner.Judge 或 WithJudge 注入）")
	}
	criteria, err := strListValue(a)
	if err != nil {
		return Verdict{}, err
	}

	results := make([]Verdict, len(criteria))
	var wg sync.WaitGroup
	for i, crit := range criteria {
		wg.Add(1)
		go func(i int, crit string) {
			defer wg.Done()
			v, err := j.Judge(ctx, JudgeInput{
				CaseID: c.ID, Input: c.Input, Output: out.Output,
				ToolCalls: out.ToolCalls, Criterion: crit,
			})
			if err == nil {
				results[i] = v
				return
			}
			// err 无法直接带回（slice 元素是值）——用零值 Verdict +
			// reason 标记，下面统一处理。
			results[i] = Verdict{Pass: false, Score: 0, Reason: "判分失败: " + err.Error()}
		}(i, crit)
	}
	wg.Wait()

	// 任一准则判分失败 = judge 通道故障 → 整条断言无法评估。
	for _, r := range results {
		if strings.HasPrefix(r.Reason, "判分失败: ") {
			return Verdict{}, fmt.Errorf("%s", r.Reason)
		}
	}
	passed := 0
	var reasons []string
	for _, r := range results {
		if r.Pass {
			passed++
		} else if r.Reason != "" {
			reasons = append(reasons, firstLine(r.Reason))
		}
	}
	score := float64(passed) / float64(len(criteria))
	reason := fmt.Sprintf("judge: 满足 %d/%d 条准则", passed, len(criteria))
	if len(reasons) > 0 {
		reason += "；未过: " + strings.Join(reasons, " / ")
	}
	return applyNegate(passVerdict(score, reason), a.Negate), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func init() {
	RegisterAssert("judge", assertJudge)
}
