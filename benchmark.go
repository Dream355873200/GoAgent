// benchmark.go 根包门面（「子包实现 + 根包门面」约定，对齐 retrieval.go）。
//
// 用法（引擎回归层，mock provider 零成本）：
//
//	target := goagent.NewAgentTarget("files-v2",
//	    goagent.WithProvider(mock),
//	    goagent.WithBuiltinTools(),
//	    goagent.WithSessionWorkDir(func(string) string { return tmpDir }),
//	)
//	runner := &goagent.Runner{
//	    Targets: []goagent.Target{target},
//	    Cases: []goagent.Case{{
//	        ID: "write-read", Input: "把 hello 写入 greeting.txt 再读出来",
//	        Asserts: []goagent.Assert{
//	            {Type: "tool-sequence", Value: []string{"Write", "Read"}},
//	            {Type: "contains", Value: "hello"},
//	        },
//	    }},
//	}
//	report, err := runner.Run(ctx)
//
// 用法（能力评测层，B1：judge 断言 + pass^k + 统计）：
//
//	runner := &goagent.Runner{
//	    Targets: []goagent.Target{realModelTarget},
//	    Cases: []goagent.Case{{
//	        ID: "summary", Input: "为这段剧本写分镜摘要",
//	        Asserts: []goagent.Assert{
//	            {Type: "judge", Value: []string{
//	                "摘要覆盖全部主要情节",
//	                "每条镜头描述可视觉化（不出现抽象形容）",
//	            }},
//	        },
//	    }},
//	    Repeat: 4,                                  // pass^k 需要 k+ 个 trial
//	    Judge: &goagent.LLMJudge{Provider: judgeP}, // 建议与被测不同家模型
//	}
//	report, _ := runner.Run(ctx)
//	for _, pk := range report.PassK(2) { ... }     // 可靠性指标
//	for _, s := range report.Summaries() { ... }    // 方差/CI/延迟
//	report.JUnitXML(w)                              // CI 展示
//
// AgentTarget 语义：每次 Run 用 opts 现造一个 App（独立会话、独立
// 事件流），事件流转成 TargetOutput——工具轨迹/token/轮次天然采集，
// 无需后配 tracing。
package goagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/benchmark"
)

// ---------- 类型别名（根包门面） ----------

type (
	// Case 评测用例（输入 + 期望断言）。
	Case = benchmark.Case
	// Assert 单条期望断言。
	Assert = benchmark.Assert
	// Verdict 判分结果（打分制）。
	Verdict = benchmark.Verdict
	// ToolCall 工具调用记录（trajectory 断言的原料）。
	ToolCall = benchmark.ToolCall
	// TargetOutput Target 一次运行的产出快照。
	TargetOutput = benchmark.TargetOutput
	// Target 被测对象抽象（矩阵运行的轴）。
	Target = benchmark.Target
	// TargetFunc 函数式 Target 适配器。
	TargetFunc = benchmark.TargetFunc
	// Runner 评测执行器（Repeat/四态/infra 重试/并发）。
	Runner = benchmark.Runner
	// TrialResult 单 trial 结果（根包 Result 已被工具结果类型占用，故加前缀）。
	TrialResult = benchmark.Result
	// AssertVerdict 单断言判分记录。
	AssertVerdict = benchmark.AssertVerdict
	// Report 评测报告。
	Report = benchmark.Report
	// CaseSummary per-(target, case) 聚合。
	CaseSummary = benchmark.CaseSummary
	// Diff 新旧报告的 per-case 状态迁移。
	Diff = benchmark.Diff
	// Status 结果四态。
	Status = benchmark.Status
	// AssertFunc 自定义断言实现（注册表签名）。
	AssertFunc = benchmark.AssertFunc
	// Judge 内容判分器（B1：LLM-as-judge 通道）。
	Judge = benchmark.Judge
	// JudgeInput 单次判分的全部输入（含轨迹参考）。
	JudgeInput = benchmark.JudgeInput
	// JudgeFunc 函数式 Judge 适配器。
	JudgeFunc = benchmark.JudgeFunc
	// LLMJudge 用任意 provider 做裁判的默认实现。
	LLMJudge = benchmark.LLMJudge
	// PassKStat pass^k / pass@k 统计（Report.PassK 的行单位）。
	PassKStat = benchmark.PassKStat
)

// 结果四态。
const (
	StatusPass       = benchmark.StatusPass
	StatusFail       = benchmark.StatusFail
	StatusInfraError = benchmark.StatusInfraError
	StatusExcluded   = benchmark.StatusExcluded
)

// RegisterAssert 注册自定义断言类型（领域判分逃生舱）。
var RegisterAssert = benchmark.RegisterAssert

// AssertTypes 已注册断言类型清单。
var AssertTypes = benchmark.AssertTypes

// SuiteHash 套件内容哈希（报告可比性的凭据）。
var SuiteHash = benchmarkSuiteHash

// DiffReports 对比两份报告（回归对比第一公民）。
var DiffReports = benchmark.DiffReports

// LoadReport 读取 JSONL 报告。
var LoadReport = benchmark.LoadReport

// ReadJSONLReport 从 reader 解析报告。
var ReadJSONLReport = benchmark.ReadJSONL

// WithJudge 把 Judge 注入 context（Runner.Run 自动做，宿主单独调用
// EvaluateCase 时手动注入）。
var WithJudge = benchmark.WithJudge

// JudgeFromContext 取注入的 Judge（未注入返回 nil）。
var JudgeFromContext = benchmark.JudgeFromContext

// ValidateCase case 入库前双端验证（gold 全过 + bad 至少挂一条，
// 否则是坏 case 不该进套件）。
var ValidateCase = benchmark.ValidateCase

// DeriveState GoldReplayer：在隔离目录回放参考工具调用序列，派生
// 期望终态快照（SABER 教训：期望终态不手写）。见 gold.go。
// （定义在根包本体，此处仅文档索引。）

var benchmarkSuiteHash = benchmark.SuiteHash

// NamedTarget 给 TargetFunc 命名（报告按名字区分 target）。
var NamedTarget = benchmark.NamedTarget

// ---------- AgentTarget：把 App 变成被测对象 ----------

// AgentTarget 用一组 Option 装配 App 作为被测对象。每次 Run 现造
// App：会话独立（benchmark trial 互不污染——环境隔离防线的进程内
// 部分；文件级隔离配合 WithSessionWorkDir / WithSandbox 使用）。
//
// 注意：评测 trial 通常无人值守，Normal 级工具的审批交互
// （EventNeedApproval）没人应答会让 run 挂起。需要跑写类工具的
// 宿主应显式配 WithPermissionMode(PermissionBypass) 或
// WithApprover（自动审批策略由宿主自己决定——库不默认放开权限）。
//
// 注意：opts 里的**有状态组件是共享的**。静态配置（system prompt、
// 工具定义）共享无害；但带内部游标的 provider（如 MockProvider）
// 会在 Repeat>1 时跨 trial 污染——这类组件用 NewAgentTargetFromFactory
// 每 trial 现造。
//
// error 语义：App 事件流里出现 EventError 即视为 infra 错误返回
// （provider 挂了/装配坏了），由 Runner 重试且不进分母。agent 任务
// 失败（正常完成但断言不满足）不走 error。
type AgentTarget struct {
	name string
	opts []Option
	// optsFn 非空时优先（工厂形态：每 Run 现造 opts）。
	optsFn func() []Option
	// extraTools Run 内 New 之后注入的工具（opts 之外的工具面，
	// 典型用法：宿主自建 InferTool 工具直接挂 target）。
	extraTools []NamedTool
	// stateDirFn 非空时 Run 结束后快照该目录为 TargetOutput.State
	//（终态断言的采集端）。
	stateDirFn func() string
	mu         sync.Mutex
}

// NewAgentTarget 创建 agent 目标。opts 完整描述被测 agent 的装配
// （provider / 工具 / system prompt / 沙箱……），改 prompt 对比 =
// 两个不同 opts 的 target 进同一 Runner 矩阵。
func NewAgentTarget(name string, opts ...Option) *AgentTarget {
	return &AgentTarget{name: name, opts: opts}
}

// NewAgentTargetFromFactory 工厂形态：每次 Run 调用 factory 现造
// 全套 opts。有状态 provider（MockProvider 的响应游标、计数器等）
// 必须∈工厂内新建，否则 Repeat>1 时并发 trial 共享游标互相污染
// （benchmark 环境隔离纪律：trial 之间除外部世界外零共享）。
func NewAgentTargetFromFactory(name string, factory func() []Option) *AgentTarget {
	return &AgentTarget{name: name, optsFn: factory}
}

// Name 实现 Target。
func (t *AgentTarget) Name() string { return t.name }

// UseTools 给 target 附加工具（在 opts 装配之外追加，对全部 trial 生效——
// 工具定义是静态的，共享无害；有状态的才需要工厂形态）。并发安全。
func (t *AgentTarget) UseTools(tools ...NamedTool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.extraTools = append(t.extraTools, tools...)
}

// CaptureStateFrom 让 target 在每次 Run 结束后快照 dirFn() 返回的
// 目录为 TargetOutput.State（终态断言 file-exists / state-equals 的
// 采集端）。dirFn 每 trial 调用一次——配 WithSessionWorkDir 时传
// 同一个解析函数即可（trial 用工厂形态隔离目录时尤为重要）。
//
// 快照失败（目录不存在等）记入 Output 尾部不中断：终态断言会在
// 空 State 上正常判挂（缺文件语义），比把整个 trial 变 infra_error
// 更符合「agent 没产出终态 = 任务失败」的语义。
func (t *AgentTarget) CaptureStateFrom(dirFn func() string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stateDirFn = dirFn
}

// Run 实现 Target：现造 App 跑 case.Input，事件流聚合为 TargetOutput。
func (t *AgentTarget) Run(ctx context.Context, c Case) (TargetOutput, error) {
	t.mu.Lock()
	opts, extra := t.opts, t.extraTools
	if t.optsFn != nil {
		opts = t.optsFn()
	}
	stateDirFn := t.stateDirFn
	t.mu.Unlock()
	app := New(opts...)
	if len(extra) > 0 {
		app.UseTools(extra...)
	}

	start := time.Now()
	var out TargetOutput
	var b strings.Builder
	// RunWithHistory（空历史）而非 Run：Run 传 sess=nil → sessionID 空
	// → WithSessionWorkDir 静默失效（工具的相对路径解析退回进程 cwd）。
	// ephemeral 会话让每 trial 的 workDir 解析正常激活。
	for ev := range app.RunWithHistory(ctx, nil, c.Input) {
		switch ev.Type {
		case EventTextDelta:
			b.WriteString(ev.Text)
		case EventToolStart:
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				Name:  ev.ToolName,
				Input: ev.ToolInput,
			})
		case EventUsageUpdate:
			if ev.Usage != nil {
				out.Tokens += int64(ev.Usage.InputTokens) + int64(ev.Usage.OutputTokens)
			}
		case EventTurnComplete:
			out.Turns++
		case EventError:
			// infra 语义：provider/装配/引擎错误。注意事件流不会就此
			// 停止，但 benchmark 不需要残缺产出——直接返回。
			return out, fmt.Errorf("agent run 出错: %w", ev.Error)
		}
	}
	out.Output = b.String()
	out.LatencyMs = time.Since(start).Milliseconds()
	// 终态采集：run 结束后快照工作区（stateDirFn 由 CaptureStateFrom
	// 配置）。失败不中断——终态断言在空 State 上正常判挂（agent 没
	// 产出终态 = 任务失败，不是基建错误）。
	if stateDirFn != nil {
		if dir := stateDirFn(); dir != "" {
			if state, err := SnapshotDir(dir); err == nil {
				out.State = state
			} else {
				out.Output += fmt.Sprintf("\n[benchmark] 终态快照失败: %v", err)
			}
		}
	}
	return out, nil
}
