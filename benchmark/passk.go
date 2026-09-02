// passk.go pass^k / pass@k 指标（B1）：把「重复运行的通过计数」翻译
// 成可比的可靠性数字。
//
// 为什么需要两个（τ-bench 的核心观察）：
//   - pass@k（HumanEval 无偏估计）=「随机抽 k 次，至少一次过」的
//     概率。量的是能力上界——agent 碰运气能不能做到。
//   - pass^k（τ-bench）=「随机抽 k 次，全部都过」的概率。量的是
//     可靠性——agent 每次都做到的稳定度。生产 agent 要的是这个：
//     gpt-4o 在 τ-bench retail 上 pass^1≈60% 但 pass^8≈25%——
//     「大多数时候能过」和「每次都能过」之间隔着巨大的工程鸿沟。
//
// 只统计有效 trial（pass+fail）；infra_error/excluded 不进分母
// （与 CaseSummary 一致——基建抖动不算 agent 的失败）。
package benchmark

import "sort"

// PassKStat 单个 (target, case) 的 pass^k / pass@k 统计。
type PassKStat struct {
	Target   string  `json:"target"`
	CaseID   string  `json:"case_id"`
	N        int     `json:"n"`          // 有效 trial 数（pass+fail）
	Passed   int     `json:"passed"`     // 其中通过数
	PassPowK float64 `json:"pass_pow_k"` // pass^k：k 次全过的概率估计
	PassAtK  float64 `json:"pass_at_k"`  // pass@k：至少一次过的概率估计
}

// PassK 计算 k 取各值下的两类指标。k ≥ 2 才有意义（k=1 时两者都
// 等于 pass rate）；k 超过 N 时 pass^k 退化（样本不足）——此时
// PassPowK 用 N 次全过的概率近似并如实反映样本极限，注释见下。
// 返回结果按 target/case 排序（与 Summaries 一致的确定性顺序）。
func (r *Report) PassK(k int) []PassKStat {
	if k < 1 {
		k = 1
	}
	agg := map[string]*PassKStat{}
	var order []string
	for _, s := range r.Summaries() {
		key := s.Target + "/" + s.CaseID
		if s.Valid == 0 {
			continue // 全 infra/excluded：无法判定
		}
		st := &PassKStat{Target: s.Target, CaseID: s.CaseID, N: s.Valid, Passed: s.Passed}
		st.PassPowK = passPowK(s.Passed, s.Valid, k)
		st.PassAtK = passAtK(s.Passed, s.Valid, k)
		agg[key] = st
		order = append(order, key)
	}
	sort.Strings(order)
	out := make([]PassKStat, 0, len(order))
	for _, key := range order {
		out = append(out, *agg[key])
	}
	return out
}

// passPowK pass^k = C(c,k)/C(n,k)：从 n 个 trial 里随机抽 k 个，
// 全部是通过的概率。c<k 时显然为 0。k>n 时二项式系数为 0——用
// C(c,n)/C(n,n)（全部 trial 都过的频率）作为样本极限下的近似：
// 样本只有 n 个，「n 个全过」是「k>n 次全过」能给出的最好证据。
func passPowK(c, n, k int) float64 {
	if k <= n {
		return binom(c, k) / binom(n, k)
	}
	return binom(c, n) / binom(n, n)
}

// passAtK pass@k = 1 − C(n−c,k)/C(n,k)：随机抽 k 个至少一个通过
// 的无偏估计（HumanEval 论文的标准公式）。n−c<k 时为 1（失败数
// 不足以填满 k 个抽样，必然抽到至少一个通过）。
func passAtK(c, n, k int) float64 {
	fails := n - c
	if fails < k {
		return 1
	}
	return 1 - binom(fails, k)/binom(n, k)
}

// binom 组合数 C(n,k) 的 float64 乘积公式。n 是 trial 数（个位到
// 几百），连乘无精度问题。非法参数（k>n 或负数）返回 0。
func binom(n, k int) float64 {
	if k < 0 || n < 0 || k > n {
		return 0
	}
	k = min(k, n-k) // C(n,k) = C(n,n−k)，取小的那边乘得少
	// 逐项乘除保持中间值是整数（前 i 项的积必是 C(n,i) 的整数倍）。
	result := 1.0
	for i := 1; i <= k; i++ {
		result = result * float64(n-k+i) / float64(i)
	}
	return result
}
