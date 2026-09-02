// assert.go 断言注册表与内置确定性原语。
//
// 设计约定（docs/benchmark-research.md §2.4）：
//   - 内置原语只放确定性的（字符串/结构/轨迹/资源上限）——LLM 判分
//     走根包门面的 Judge 接口（B1），不混进断言注册表
//   - RegisterAssert 是轻逃生舱（一等公民函数），不是插件系统——
//     OpenAI evals「自定义要继承类写两个方法」的教训
//   - 断言评估自身出错（regex 编译失败/Value 类型不对）不算 fail，
//     算 case 损坏 → excluded 不进分母（τ-bench 标注错误的教训）
package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// AssertFunc 断言实现。返回 Verdict；返回 error 表示断言自身无法评估
// （Value 类型不对、正则编译失败等 case 损坏，非 agent 失败）。
type AssertFunc func(ctx context.Context, c Case, out TargetOutput, a Assert) (Verdict, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]AssertFunc{}
)

// RegisterAssert 注册自定义断言类型（领域判分逻辑的逃生舱）。
// 覆盖同名内置类型是允许的（宿主替换语义），并发安全。
func RegisterAssert(typ string, fn AssertFunc) {
	if typ == "" || fn == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[typ] = fn
}

// AssertTypes 返回已注册的断言类型清单（给报错信息附可用清单用——
// 对齐内置工具「打错名报错附可用清单」的既有做法）。
func AssertTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}

func lookupAssert(typ string) (AssertFunc, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	fn, ok := registry[typ]
	return fn, ok
}

func init() {
	builtin := map[string]AssertFunc{
		"contains":       assertContains,
		"contains-any":   assertContainsAny,
		"equals":         assertEquals,
		"starts-with":    assertStartsWith,
		"regex":          assertRegex,
		"is-json":        assertIsJSON,
		"tool-used":      assertToolUsed,
		"tool-sequence":  assertToolSequence,
		"max-latency-ms": assertMaxLatency,
		"max-tokens":     assertMaxTokens,
	}
	for typ, fn := range builtin {
		RegisterAssert(typ, fn)
	}
}

// applyNegate 统一施加取反语义：Pass 反转、Score 取补、Reason 加前缀。
func applyNegate(v Verdict, negate bool) Verdict {
	if !negate {
		return v
	}
	v.Pass = !v.Pass
	v.Score = 1 - v.Score
	if v.Reason != "" {
		v.Reason = "(取反) " + v.Reason
	}
	return v
}

func passVerdict(score float64, reason string) Verdict {
	return Verdict{Pass: score >= 1, Score: score, Reason: reason}
}

// strValue 把断言 Value 规整成 string（string 直接用；数字/布尔转字符串）。
func strValue(a Assert) (string, error) {
	switch v := a.Value.(type) {
	case nil:
		return "", fmt.Errorf("Value 不能为空")
	case string:
		return v, nil
	case bool, float64, int, int64, json.Number:
		return fmt.Sprintf("%v", v), nil
	default:
		return "", fmt.Errorf("Value 类型 %T 不被该断言支持", a.Value)
	}
}

// strListValue 把断言 Value 规整成 []string（单条字符串也算单元素列表）。
func strListValue(a Assert) ([]string, error) {
	switch v := a.Value.(type) {
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("Value[%d] 类型 %T 不是字符串", i, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("Value 类型 %T 不被该断言支持（要 string 或字符串数组）", a.Value)
	}
}

func numValue(a Assert) (float64, error) {
	switch v := a.Value.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case json.Number:
		return v.Float64()
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("Value 类型 %T 不是数字", a.Value)
	}
}

// ---------- 内置原语 ----------

// contains：Output 包含 Value（字符串数组 = 全部包含才过）。
func assertContains(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	subs, err := strListValue(a)
	if err != nil {
		return Verdict{}, err
	}
	missing := make([]string, 0, len(subs))
	for _, s := range subs {
		if !strings.Contains(out.Output, s) {
			missing = append(missing, s)
		}
	}
	score := float64(len(subs)-len(missing)) / float64(len(subs))
	reason := fmt.Sprintf("包含 %d/%d 个期望片段", len(subs)-len(missing), len(subs))
	if len(missing) > 0 {
		reason = fmt.Sprintf("缺少片段 %q（%s）", missing, reason)
	}
	return applyNegate(passVerdict(score, reason), a.Negate), nil
}

// contains-any：Output 包含 Value 数组中任意一个。
func assertContainsAny(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	subs, err := strListValue(a)
	if err != nil {
		return Verdict{}, err
	}
	for _, s := range subs {
		if strings.Contains(out.Output, s) {
			return applyNegate(passVerdict(1, fmt.Sprintf("命中片段 %q", s)), a.Negate), nil
		}
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("未命中任何片段 %q", subs)), a.Negate), nil
}

// equals：Output 与 Value 精确相等。
func assertEquals(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, err := strValue(a)
	if err != nil {
		return Verdict{}, err
	}
	if out.Output == want {
		return applyNegate(passVerdict(1, "输出与期望完全一致"), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("输出与期望不一致（want %.80q, got %.80q）", want, out.Output)), a.Negate), nil
}

// starts-with：Output 以 Value 开头。
func assertStartsWith(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	prefix, err := strValue(a)
	if err != nil {
		return Verdict{}, err
	}
	if strings.HasPrefix(out.Output, prefix) {
		return applyNegate(passVerdict(1, fmt.Sprintf("以 %q 开头", prefix)), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("不以 %q 开头", prefix)), a.Negate), nil
}

// regex：Output 命中 Value 正则。
func assertRegex(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	pattern, err := strValue(a)
	if err != nil {
		return Verdict{}, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Verdict{}, fmt.Errorf("正则编译失败: %w", err)
	}
	if re.MatchString(out.Output) {
		return applyNegate(passVerdict(1, fmt.Sprintf("命中正则 %q", pattern)), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("未命中正则 %q", pattern)), a.Negate), nil
}

// is-json：Output 可解析为 JSON。Value 为字符串数组时，进一步要求
// 顶层 JSON 对象包含这些 key（轻量结构校验；完整 JSON Schema 留给宿主）。
func assertIsJSON(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	var parsed any
	if err := json.Unmarshal([]byte(out.Output), &parsed); err != nil {
		return applyNegate(passVerdict(0, fmt.Sprintf("不是合法 JSON: %v", err)), a.Negate), nil
	}
	var wantKeys []string
	if a.Value != nil {
		ks, err := strListValue(a)
		if err != nil {
			return Verdict{}, err
		}
		wantKeys = ks
	}
	if len(wantKeys) == 0 {
		return applyNegate(passVerdict(1, "输出是合法 JSON"), a.Negate), nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return applyNegate(passVerdict(0, "JSON 顶层不是对象，无法检查 key"), a.Negate), nil
	}
	missing := make([]string, 0)
	for _, k := range wantKeys {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return applyNegate(passVerdict(0, fmt.Sprintf("JSON 缺少 key %q", missing)), a.Negate), nil
	}
	return applyNegate(passVerdict(1, fmt.Sprintf("JSON 含全部期望 key %q", wantKeys)), a.Negate), nil
}

// tool-used：Value（字符串或数组）里的工具全部被调用过（存在性，
// 不看顺序）。Value 支持通配后缀 *（对齐权限 matcher 的习惯）。
func assertToolUsed(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, err := strListValue(a)
	if err != nil {
		return Verdict{}, err
	}
	var patterns []*regexp.Regexp
	for _, w := range want {
		if strings.HasSuffix(w, "*") {
			re, err := regexp.Compile("^" + regexp.QuoteMeta(strings.TrimSuffix(w, "*")) + ".*$")
			if err != nil {
				return Verdict{}, fmt.Errorf("通配模式 %q 编译失败: %w", w, err)
			}
			patterns = append(patterns, re)
		}
	}
	missing := make([]string, 0, len(want))
	for _, w := range want {
		if !toolCalled(out.ToolCalls, w, patterns) {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		score := float64(len(want)-len(missing)) / float64(len(want))
		return applyNegate(passVerdict(score, fmt.Sprintf("未调用工具 %q", missing)), a.Negate), nil
	}
	return applyNegate(passVerdict(1, fmt.Sprintf("调用了全部期望工具 %q", want)), a.Negate), nil
}

func toolCalled(calls []ToolCall, name string, patterns []*regexp.Regexp) bool {
	for _, c := range calls {
		if c.Name == name {
			return true
		}
	}
	for _, re := range patterns {
		for _, c := range calls {
			if re.MatchString(c.Name) {
				return true
			}
		}
	}
	return false
}

// tool-sequence：工具调用序列与 Value（字符串数组）完全一致
// （含顺序与完整性——比 tool-used 严格，用于锁定执行路径）。
func assertToolSequence(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, err := strListValue(a)
	if err != nil {
		return Verdict{}, err
	}
	got := make([]string, len(out.ToolCalls))
	for i, c := range out.ToolCalls {
		got[i] = c.Name
	}
	if strings.Join(got, "→") == strings.Join(want, "→") {
		return applyNegate(passVerdict(1, fmt.Sprintf("工具序列一致: %v", got)), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("工具序列不一致（want %v, got %v）", want, got)), a.Negate), nil
}

// max-latency-ms：LatencyMs 不超过 Value（资源上限类断言）。
func assertMaxLatency(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	max, err := numValue(a)
	if err != nil {
		return Verdict{}, err
	}
	if float64(out.LatencyMs) <= max {
		return applyNegate(passVerdict(1, fmt.Sprintf("耗时 %dms ≤ 上限 %vms", out.LatencyMs, max)), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("耗时 %dms 超上限 %vms", out.LatencyMs, max)), a.Negate), nil
}

// max-tokens：Tokens 不超过 Value。
func assertMaxTokens(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	max, err := numValue(a)
	if err != nil {
		return Verdict{}, err
	}
	if float64(out.Tokens) <= max {
		return applyNegate(passVerdict(1, fmt.Sprintf("token %d ≤ 上限 %v", out.Tokens, max)), a.Negate), nil
	}
	return applyNegate(passVerdict(0, fmt.Sprintf("token %d 超上限 %v", out.Tokens, max)), a.Negate), nil
}

// 通过判定取 AND；Score 为加权平均。返回的 error 表示断言自身无法
// 评估（case 损坏 → 调用方应记 excluded 而非 fail）。
// EvaluateCase 在一个 TargetOutput 上评估 case 的全部断言（导出版本——
// 宿主可不跑 Runner 单独判分某个输出，如人工抽检场景）。
func EvaluateCase(ctx context.Context, c Case, out TargetOutput) (bool, float64, []AssertVerdict, error) {
	verdicts := make([]AssertVerdict, 0, len(c.Asserts))
	totalW, sum, allPass := 0.0, 0.0, true
	for _, a := range c.Asserts {
		fn, ok := lookupAssert(a.Type)
		if !ok {
			return false, 0, verdicts, fmt.Errorf("未知断言类型 %q（可用: %v）", a.Type, AssertTypes())
		}
		v, err := fn(ctx, c, out, a)
		if err != nil {
			return false, 0, verdicts, fmt.Errorf("断言 %q 无法评估: %w", a.Type, err)
		}
		w := a.Weight
		if w <= 0 {
			w = 1
		}
		totalW += w
		sum += v.Score * w
		if !v.Pass {
			allPass = false
		}
		verdicts = append(verdicts, AssertVerdict{
			Type: a.Type, Pass: v.Pass, Score: v.Score, Reason: v.Reason,
		})
	}
	score := 0.0
	if totalW > 0 {
		score = sum / totalW
	}
	return allPass, score, verdicts, nil
}
