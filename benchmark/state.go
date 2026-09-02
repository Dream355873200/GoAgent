// state.go 终态断言族（B2）：对 TargetOutput.State（工作区文件快照）
// 的断言——评「副作用对不对」，与输出断言（评「说了什么」）互补。
//
// 为什么需要（τ-bench × SWE-bench 交叉结论，research.md §2.1）：
// 「对话内容对」和「副作用对」是独立通道——agent 可以总结写得漂亮
// 但文件没写、写错位置、顺手改坏了别的东西。只看输出的断言全是
// 纸糊的。
//
// 断言族四条：
//   - file-exists / file-not-exists：存在性。not-exists 是
//     PreserveAssertion 语义（SWE-bench PASS_TO_PASS：不该动的东西
//     没动）的基础形态。
//   - file-contains：单文件内容包含期望片段。
//   - state-equals：期望终态快照精确比对（GoldReplayer 派生，不
//     手写——SABER 教训：手写期望态是标注错误重灾区）。多余文件
//     也算挂（agent 顺手留下的垃圾终态就是脏的）。
//
// State 的键是相对工作区根的路径（'/' 分隔，声明式 JSON 友好），
// 值是文件文本内容。二进制产物不进快照——宿主有需求走
// RegisterAssert 自定义断言。
package benchmark

import (
	"context"
	"fmt"
	"strings"
)

// fileExistsValue 规整 file-exists / file-not-exists 的 Value：
// 单路径字符串或路径数组。
func fileExistsValue(a Assert) ([]string, error) {
	return strListValue(a)
}

// assertFileExists file-exists：State 里全部期望路径存在。
func assertFileExists(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, err := fileExistsValue(a)
	if err != nil {
		return Verdict{}, err
	}
	missing := make([]string, 0, len(want))
	for _, p := range want {
		if _, ok := out.State[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		score := float64(len(want)-len(missing)) / float64(len(want))
		return applyNegate(passVerdict(score, fmt.Sprintf("缺少文件 %q", missing)), a.Negate), nil
	}
	return applyNegate(passVerdict(1, fmt.Sprintf("全部文件存在 %q", want)), a.Negate), nil
}

// assertFileNotExists file-not-exists：期望路径全部不存在（Preserve
// 语义——「不该出现的东西没出现」，如 agent 不该碰的答案文件、
// 临时垃圾）。
func assertFileNotExists(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, err := fileExistsValue(a)
	if err != nil {
		return Verdict{}, err
	}
	found := make([]string, 0, len(want))
	for _, p := range want {
		if _, ok := out.State[p]; ok {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return applyNegate(passVerdict(0, fmt.Sprintf("不该存在的文件出现了 %q", found)), a.Negate), nil
	}
	return applyNegate(passVerdict(1, "期望不存在的文件全部不存在"), a.Negate), nil
}

// fileContainsInput file-contains 的 Value：{"path": "...", "contains": "..."}
// 或该结构的数组。
type fileContainsInput struct {
	Path     string
	Contains string
}

func fileContainsValue(a Assert) ([]fileContainsInput, error) {
	one := func(v any) (fileContainsInput, error) {
		m, ok := v.(map[string]any)
		if !ok {
			return fileContainsInput{}, fmt.Errorf("Value 项类型 %T 不是 {path, contains} 对象", v)
		}
		p, _ := m["path"].(string)
		sub, _ := m["contains"].(string)
		if p == "" {
			return fileContainsInput{}, fmt.Errorf("file-contains 缺少 path 字段")
		}
		return fileContainsInput{Path: p, Contains: sub}, nil
	}
	switch v := a.Value.(type) {
	case map[string]any:
		in, err := one(v)
		if err != nil {
			return nil, err
		}
		return []fileContainsInput{in}, nil
	case []any:
		out := make([]fileContainsInput, 0, len(v))
		for i, item := range v {
			in, err := one(item)
			if err != nil {
				return nil, fmt.Errorf("Value[%d]: %w", i, err)
			}
			out = append(out, in)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("Value 类型 %T 不被 file-contains 支持（要 {path, contains} 或其数组）", a.Value)
	}
}

// assertFileContains file-contains：指定文件包含期望片段（文件不存在
// 也算挂——缺文件比内容不符更根本）。
func assertFileContains(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	wants, err := fileContainsValue(a)
	if err != nil {
		return Verdict{}, err
	}
	missed := make([]string, 0, len(wants))
	for _, w := range wants {
		content, ok := out.State[w.Path]
		switch {
		case !ok:
			missed = append(missed, fmt.Sprintf("%s: 文件不存在", w.Path))
		case !strings.Contains(content, w.Contains):
			missed = append(missed, fmt.Sprintf("%s: 不包含 %q", w.Path, w.Contains))
		}
	}
	if len(missed) > 0 {
		score := float64(len(wants)-len(missed)) / float64(len(wants))
		return applyNegate(passVerdict(score, fmt.Sprintf("未满足 %v", missed)), a.Negate), nil
	}
	return applyNegate(passVerdict(1, fmt.Sprintf("%d 个文件内容全部命中", len(wants))), a.Negate), nil
}

// assertStateEquals state-equals：State 与期望快照（Value =
// map[path]content）完全一致——缺失、内容不同、多余文件都算挂。
// 期望快照应经 GoldReplayer 回放派生，不手写。
func assertStateEquals(_ context.Context, _ Case, out TargetOutput, a Assert) (Verdict, error) {
	want, ok := a.Value.(map[string]any)
	if !ok {
		return Verdict{}, fmt.Errorf("Value 类型 %T 不是 {path: content} 快照对象", a.Value)
	}
	var missing, diff, extra []string
	for p, wv := range want {
		wc, _ := wv.(string)
		got, exists := out.State[p]
		switch {
		case !exists:
			missing = append(missing, p)
		case got != wc:
			diff = append(diff, p)
		}
	}
	for p := range out.State {
		if _, ok := want[p]; !ok {
			extra = append(extra, p)
		}
	}
	if len(missing) == 0 && len(diff) == 0 && len(extra) == 0 {
		return applyNegate(passVerdict(1, fmt.Sprintf("终态快照一致（%d 个文件）", len(want))), a.Negate), nil
	}
	// 部分分：命中文件数 / max(期望, 实际)。终态比对的分母取大边
	// ——extra 也算偏离，不能只按期望数算（agent 留一堆垃圾文件还
	// 拿高分是错误激励）。
	total := len(want)
	if len(out.State) > total {
		total = len(out.State)
	}
	score := float64(len(want)-len(missing)-len(diff)) / float64(total)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("缺失 %q", missing))
	}
	if len(diff) > 0 {
		parts = append(parts, fmt.Sprintf("内容不同 %q", diff))
	}
	if len(extra) > 0 {
		parts = append(parts, fmt.Sprintf("多余 %q", extra))
	}
	return applyNegate(passVerdict(score, "终态不一致："+strings.Join(parts, "；")), a.Negate), nil
}

func init() {
	builtin := map[string]AssertFunc{
		"file-exists":     assertFileExists,
		"file-not-exists": assertFileNotExists,
		"file-contains":   assertFileContains,
		"state-equals":    assertStateEquals,
	}
	for typ, fn := range builtin {
		RegisterAssert(typ, fn)
	}
}
