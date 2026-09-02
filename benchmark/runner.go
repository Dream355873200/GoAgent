// runner.go 评测执行器：矩阵（targets × cases × trials）运行、四态
// 结果、infra 自动重试、并发控制。
package benchmark

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Status 结果四态。infra_error（基建抖动）与 excluded（case 损坏）
// 不进 pass/fail 分母——否则基建抖动会淹没真实回归。
type Status string

const (
	StatusPass       Status = "pass"
	StatusFail       Status = "fail"
	StatusInfraError Status = "infra_error"
	StatusExcluded   Status = "excluded"
)

// Result 一次 trial 的完整结果（Report 的行单位）。
type Result struct {
	Target    string          `json:"target"`
	CaseID    string          `json:"case_id"`
	Trial     int             `json:"trial"` // 0 起
	Status    Status          `json:"status"`
	Score     float64         `json:"score"`
	Output    string          `json:"output,omitempty"`
	ToolCalls []ToolCall      `json:"tool_calls,omitempty"`
	Turns     int             `json:"turns"`
	Tokens    int64           `json:"tokens"`
	LatencyMs int64           `json:"latency_ms"`
	Asserts   []AssertVerdict `json:"asserts,omitempty"`
	Error     string          `json:"error,omitempty"` // infra_error 的错误信息
	Note      string          `json:"note,omitempty"`  // excluded 的原因
}

// AssertVerdict 单条断言的判分记录（结果可追溯：哪个断言挂了一目了然）。
type AssertVerdict struct {
	Type   string  `json:"type"`
	Pass   bool    `json:"pass"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
}

// Runner 评测执行器。零值不可用，至少配 Targets/Cases。
type Runner struct {
	// Targets 被测对象（多个则形成对比矩阵）。
	Targets []Target

	// Cases 评测套件。
	Cases []Case

	// Repeat 每 case 重复次数（默认 1）。
	// LLM 目标建议 ≥4：METR 实测误差棒 ±2 倍、τ-bench 官方要求
	// 4+ trials。mock 目标（引擎回归层）1 次即可。
	Repeat int

	// InfraRetries infra_error 自动重试次数（默认 2，负数 = 关闭）。
	// 重试仍失败才把 infra_error 记入报告（保留行但不进分母）。
	InfraRetries int

	// Concurrency 并发 trial 数（默认 4）。
	Concurrency int

	// Timeout 单次 Target.Run 的超时（0 = 不限）。超时计 infra_error。
	Timeout time.Duration

	// Judge LLM 判分器（B1）。case 含 "judge" 断言时必须配置，否则
	// validate 阶段整单拒跑。注入方式：Run 内部自动 WithJudge 进 ctx。
	Judge Judge
}

// Run 执行全部 target × case × trial 组合。套件校验失败（ID 重复/
// 无断言）直接返回 error——坏套件不该被跑（「case 必须可判定」）。
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	rep := &Report{
		StartedAt: time.Now().UTC(),
		Repeat:    r.repeat(),
	}
	for _, t := range r.Targets {
		rep.Targets = append(rep.Targets, t.Name())
	}
	rep.SuiteHash = SuiteHash(r.Cases...)

	// judge 断言的判分通道：整 run 注入一次（handler 从 ctx 取）。
	if r.Judge != nil {
		ctx = WithJudge(ctx, r.Judge)
	}

	type job struct {
		target Target
		c      Case
		trial  int
	}
	jobs := make([]job, 0, len(r.Targets)*len(r.Cases)*r.repeat())
	for _, t := range r.Targets {
		for _, c := range r.Cases {
			for i := 0; i < r.repeat(); i++ {
				jobs = append(jobs, job{target: t, c: c, trial: i})
			}
		}
	}

	sem := make(chan struct{}, r.concurrency())
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := r.runOne(ctx, j.target, j.c, j.trial)
			mu.Lock()
			rep.Rows = append(rep.Rows, res)
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	// 确定性输出：行按 target/case/trial 排序（并发完成顺序无关）。
	sort.Slice(rep.Rows, func(i, k int) bool {
		a, b := rep.Rows[i], rep.Rows[k]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.CaseID != b.CaseID {
			return a.CaseID < b.CaseID
		}
		return a.Trial < b.Trial
	})
	rep.FinishedAt = time.Now().UTC()
	return rep, nil
}

func (r *Runner) repeat() int {
	if r.Repeat < 1 {
		return 1
	}
	return r.Repeat
}

func (r *Runner) concurrency() int {
	if r.Concurrency < 1 {
		return 4
	}
	return r.Concurrency
}

// infraRetries 零值 = 默认 2（与 Repeat 的零值语义一致：
// 未设置给默认，显式负数关闭）。
func (r *Runner) infraRetries() int {
	if r.InfraRetries < 0 {
		return 0
	}
	if r.InfraRetries == 0 {
		return 2
	}
	return r.InfraRetries
}

// validate 套件校验：目标/用例非空、ID 唯一、断言类型可解析。
// 配置错误与运行时排除分开——前者整单拒跑，后者记 excluded。
func (r *Runner) validate() error {
	if len(r.Targets) == 0 {
		return fmt.Errorf("benchmark: 未配置 Target")
	}
	if len(r.Cases) == 0 {
		return fmt.Errorf("benchmark: 未配置 Case")
	}
	seen := map[string]bool{}
	for _, t := range r.Targets {
		if t.Name() == "" {
			return fmt.Errorf("benchmark: Target 名字不能为空")
		}
	}
	for i, c := range r.Cases {
		if c.ID == "" {
			return fmt.Errorf("benchmark: Case[%d] 缺少 ID", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("benchmark: Case ID %q 重复", c.ID)
		}
		seen[c.ID] = true
		if len(c.Asserts) == 0 {
			return fmt.Errorf("benchmark: Case %q 无断言（不可判定的 case 是配置错误）", c.ID)
		}
		for _, a := range c.Asserts {
			if _, ok := lookupAssert(a.Type); !ok {
				return fmt.Errorf("benchmark: Case %q 断言类型 %q 未注册（可用: %v）", c.ID, a.Type, AssertTypes())
			}
			if a.Type == "judge" && r.Judge == nil {
				return fmt.Errorf("benchmark: Case %q 用了 judge 断言但 Runner 未配置 Judge", c.ID)
			}
		}
	}
	return nil
}

// runOne 跑一个 trial：infra 重试 → 断言评估 → 四态落定。
func (r *Runner) runOne(ctx context.Context, t Target, c Case, trial int) Result {
	res := Result{Target: t.Name(), CaseID: c.ID, Trial: trial}

	var out TargetOutput
	var err error
	attempts := r.infraRetries() + 1
	for i := 0; i < attempts; i++ {
		runCtx := ctx
		var cancel context.CancelFunc
		if r.Timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		}
		out, err = t.Run(runCtx, c)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			// 整个 run 被外部取消：不重试，直接落 infra_error。
			break
		}
	}
	if err != nil {
		// Target 返回 error 一律是 infra 语义（网络/供应商/装配失败），
		// agent 任务失败应表达为「正常返回 + 断言不满足」。
		res.Status = StatusInfraError
		res.Error = err.Error()
		return res
	}

	res.Output = out.Output
	res.ToolCalls = out.ToolCalls
	res.Turns = out.Turns
	res.Tokens = out.Tokens
	res.LatencyMs = out.LatencyMs

	pass, score, verdicts, aerr := EvaluateCase(ctx, c, out)
	if aerr != nil {
		// 断言自身无法评估 = case 损坏 → excluded（不进分母）。
		res.Status = StatusExcluded
		res.Note = aerr.Error()
		return res
	}
	res.Score = score
	res.Asserts = verdicts
	if pass {
		res.Status = StatusPass
	} else {
		res.Status = StatusFail
	}
	return res
}
