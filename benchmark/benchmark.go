// Package benchmark 提供 GoAgent 的通用型评测（eval）底座。
//
// 定位（调研结论见 docs/benchmark-research.md）：
//   - 通用库：Case/Assert/Target/Report 是度量原语，golden 数据与领域
//     判分逻辑留在宿主（RegisterAssert 逃生舱）
//   - 判分打分制：断言返回 Verdict{Pass, Score 0-1, Reason} 而非 bool，
//     方差统计与后续 pass^k 指标都吃连续分
//   - 结果四态：pass / fail / infra_error / excluded——基建抖动不进
//     回归分母（τ-bench/SWE-bench 都是带病运行很久才补的分类）
//   - 重复运行是地基：Runner 原生 Repeat（LLM 目标建议 ≥4 次，
//     METR 实测误差棒 ±2 倍、τ-bench 官方要求 4+ trials）
//   - 断言默认不注入 agent 上下文（Target 只拿 Input 字段——
//     SWE-bench 污染教训：答案泄漏是物理问题）
//
// 与根包门面（benchmark.go）的关系遵循「子包实现 + 根包门面」约定：
// 本包不 import 根包，根包做类型别名 + AgentTarget 接线。
package benchmark

import (
	"context"
	"encoding/json"
)

// Case 是一条评测用例：给 Target 的输入 + 对产出的期望断言。
// 声明式数据（可 JSON 序列化进版本库可 diff）；断言字段只在评分阶段
// 消费，Target 实现只应读 Input（防答案泄漏）。
type Case struct {
	// ID 用例唯一标识（套件内必须唯一，报告与 diff 都以它为键）。
	ID string `json:"id"`

	// Family 任务族（METR 结构：family + 参数化变体，分析粒度到
	// family 才能摊薄单任务噪声）。空 = 自成一族。
	Family string `json:"family,omitempty"`

	// Input 喂给 Target 的输入（prompt / 任务描述）。
	Input string `json:"input"`

	// Asserts 期望断言（至少 1 条——不可判定的 case 是配置错误）。
	// 通过判定取 AND（SWE-bench F2P×P2P 语义：全过才算过）。
	Asserts []Assert `json:"asserts"`

	// Weight 用例权重（聚合时用，默认 1）。
	Weight float64 `json:"weight,omitempty"`

	// Tags 自由标签（如 "regression"、"smoke"），库只透传。
	Tags []string `json:"tags,omitempty"`

	// Metadata 自由元数据（难度、created_at、来源会话等），库只透传。
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentView 返回剥掉判分信息的 case 视图（只有 ID + Input + Family）。
//
// 答案泄漏防线（SWE-bench 教训，research.md §4.1）：断言字段默认不
// 注入 agent 上下文。当 Target 实现会把整个 Case 结构序列化进 prompt
// （如用户模拟器、RAG 注入）时，用这个视图代替原 Case——泄漏是
// 物理问题，靠「实现者自觉不读 Asserts」不够，提供不可泄漏的形态
// 才是硬保证。
func (c Case) AgentView() Case {
	return Case{
		ID:     c.ID,
		Family: c.Family,
		Input:  c.Input,
	}
}

// Assert 是一条期望断言。Type 在注册表中查实现，Value 由实现自行解释
// （contains 的子串、regex 的模式、max-latency-ms 的毫秒数……）。
type Assert struct {
	// Type 断言类型（内置原语或 RegisterAssert 注册的自定义类型）。
	Type string `json:"type"`

	// Value 期望值，各类型自行解释。省略时用类型默认语义
	// （如 is-json 只验证「输出可解析为 JSON」）。
	Value any `json:"value,omitempty"`

	// Weight 断言权重（case 分数 = 断言分加权平均，默认 1）。
	Weight float64 `json:"weight,omitempty"`

	// Negate 取反（对 contains 等正向断言取 not- 语义）。
	Negate bool `json:"negate,omitempty"`

	// Metric 命名指标标签——同 Metric 的断言分数可按名聚合。
	Metric string `json:"metric,omitempty"`
}

// Verdict 是一次判分的结果。打分制（0-1 连续分）而非布尔制——
// pass^k、方差、加权聚合都以连续分为底。
type Verdict struct {
	Pass   bool    `json:"pass"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
	Metric string  `json:"metric,omitempty"`
}

// ToolCall 记录一次工具调用（TrajectoryAssertion 的原料——
// GoAgent observer 原生就有，trajectory 断言无需后配 tracing）。
type ToolCall struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// TargetOutput 是 Target 一次运行的产出快照。断言在这个结构上评估。
type TargetOutput struct {
	// Output 最终文本产出（对话型目标 = 末轮文本；其他形态自行定义）。
	Output string `json:"output"`

	// ToolCalls 实际发生的工具调用序列（按时间序）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Turns agent 轮次数。
	Turns int `json:"turns"`

	// Tokens 累计 token 消耗（input+output）。
	Tokens int64 `json:"tokens"`

	// LatencyMs 运行耗时（毫秒）。
	LatencyMs int64 `json:"latency_ms"`

	// State 终态快照：工作区文件的相对路径 → 文本内容。终态断言族
	//（file-exists / state-equals 等）在这里评估。由 Target 实现负责
	// 采集（根包 AgentTarget 提供 CaptureStateFrom 选项）；空 = 该
	// target 不产生可快照的工作区（终态断言全挂，属配置错误——宿主
	// 应保证用终态断言的 case 配的 target 会采集状态）。
	State map[string]string `json:"state,omitempty"`
}

// Target 是被测对象抽象：同一套 Case 打到多个 Target 形成矩阵
// （promptfoo 矩阵模式）。Run 返回 error 一律视为 infra 错误
// （网络/供应商/装配失败）——不进 pass/fail 分母，Runner 自动重试；
// agent 任务失败的正确表达是「正常返回 + 断言不满足」，不是 error。
type Target interface {
	Name() string
	Run(ctx context.Context, c Case) (TargetOutput, error)
}

// TargetFunc 函数式 Target 适配器（快速接现有函数，不用定义类型）。
type TargetFunc func(ctx context.Context, c Case) (TargetOutput, error)

func (f TargetFunc) Name() string { return "target-func" }

func (f TargetFunc) Run(ctx context.Context, c Case) (TargetOutput, error) {
	return f(ctx, c)
}

// NamedTarget 给 TargetFunc 命名的包装（报告里 target 列用名字区分）。
func NamedTarget(name string, fn TargetFunc) Target {
	return &namedTarget{name: name, fn: fn}
}

type namedTarget struct {
	name string
	fn   TargetFunc
}

func (t *namedTarget) Name() string { return t.name }
func (t *namedTarget) Run(ctx context.Context, c Case) (TargetOutput, error) {
	return t.fn(ctx, c)
}
