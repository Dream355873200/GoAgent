// report.go 报告：per-trial 行落 JSONL、按 case 聚合、套件内容哈希、
// 新旧对比 diff（回归对比是第一公民——单次分数无意义，变化量才是）。
package benchmark

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

// Report 一次评测运行的完整结果。
type Report struct {
	// SuiteHash 套件内容哈希——报告必须携带（tau3 改分后新旧结果
	// 不可比的教训：没有哈希就无法判断两份报告是否可比）。
	SuiteHash string `json:"suite_hash"`

	// Targets 参与的 target 名单（行序）。
	Targets []string `json:"targets"`

	// Repeat 每 case 重复次数。
	Repeat int `json:"repeat"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Rows per-trial 结果（含 infra_error/excluded 行——完整留痕）。
	Rows []Result `json:"rows"`
}

// CaseSummary 单个 (target, case) 的聚合视图。
type CaseSummary struct {
	Target    string  `json:"target"`
	CaseID    string  `json:"case_id"`
	Trials    int     `json:"trials"`     // 实际 trial 行数
	Passed    int     `json:"passed"`     // pass 行数
	Failed    int     `json:"failed"`     // fail 行数
	Infra     int     `json:"infra"`      // infra_error 行数
	Excluded  int     `json:"excluded"`   // excluded 行数
	Valid     int     `json:"valid"`      // 进分母的行数 = passed+failed
	AllPass   bool    `json:"all_pass"`   // 全部有效 trial 都过（最严口径，pass^N 精神）
	PassRate  float64 `json:"pass_rate"`  // passed/valid（0 当 valid=0）
	MeanScore float64 `json:"mean_score"` // 有效 trial 的平均分
	// ScoreStdDev / ScoreCI95 分数的标准差与 95% 置信区间半宽
	//（mean ± CI95）。CI 用正态近似 1.96σ/√n——n<30 时偏窄，看
	// 方差本身比看 CI 更可靠（METR 的教训：误差棒能差 ±2 倍）。
	ScoreStdDev float64 `json:"score_stddev,omitempty"`
	ScoreCI95   float64 `json:"score_ci95,omitempty"`
	// MeanLatencyMs / LatencyStdDevMs 延迟统计（有效 trial）。
	MeanLatencyMs   float64 `json:"mean_latency_ms,omitempty"`
	LatencyStdDevMs float64 `json:"latency_stddev_ms,omitempty"`
}

// Summaries 按 (target, case) 聚合行结果。顺序按 target/case 排序。
func (r *Report) Summaries() []CaseSummary {
	type key struct{ target, caseID string }
	agg := map[key]*CaseSummary{}
	scores := map[key][]float64{}
	latencies := map[key][]float64{}
	var order []key
	for _, row := range r.Rows {
		k := key{row.Target, row.CaseID}
		s, ok := agg[k]
		if !ok {
			s = &CaseSummary{Target: row.Target, CaseID: row.CaseID}
			agg[k] = s
			order = append(order, k)
		}
		s.Trials++
		switch row.Status {
		case StatusPass:
			s.Passed++
		case StatusFail:
			s.Failed++
		case StatusInfraError:
			s.Infra++
		case StatusExcluded:
			s.Excluded++
		}
		if row.Status == StatusPass || row.Status == StatusFail {
			s.Valid++
			s.MeanScore += row.Score
			scores[k] = append(scores[k], row.Score)
			latencies[k] = append(latencies[k], float64(row.LatencyMs))
		}
	}
	out := make([]CaseSummary, 0, len(order))
	for _, k := range order {
		s := agg[k]
		if s.Valid > 0 {
			s.PassRate = float64(s.Passed) / float64(s.Valid)
			s.MeanScore /= float64(s.Valid)
			s.AllPass = s.Passed == s.Valid
			s.ScoreStdDev, s.ScoreCI95 = meanStdCI(scores[k])
			s.MeanLatencyMs, s.LatencyStdDevMs = meanStd(latencies[k])
		}
		out = append(out, *s)
	}
	return out
}

// meanStd 样本均值与（总体口径）标准差。空切片返回 (0,0)。
func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	varsq := 0.0
	for _, x := range xs {
		varsq += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(varsq / float64(len(xs)))
}

// meanStdCI 标准差与 95% CI 半宽（正态近似 1.96σ/√n）。
// n=1 时 CI 无意义返回 0（与 std 一致——单点无离散可言）。
func meanStdCI(xs []float64) (std, ci float64) {
	_, std = meanStd(xs)
	if len(xs) < 2 {
		return std, 0
	}
	return std, 1.96 * std / math.Sqrt(float64(len(xs)))
}

// SuiteStats 套件级统计（AllPass 口径：一个 case 全部有效 trial 都过才算过）。
// 返回（通过 case 数, 失败 case 数, 总 case 数），按 target 汇总。
func (r *Report) SuiteStats() (pass, fail, total int) {
	for _, s := range r.Summaries() {
		total++
		if s.Valid == 0 {
			continue // 全 infra/excluded 的 case 不进分母
		}
		if s.AllPass {
			pass++
		} else {
			fail++
		}
	}
	return pass, fail, total
}

// ---------- JSONL 落盘 ----------

// reportHeader JSONL 第一行（其余每行一个 Result）。
// 报告是行式流数据：追加友好、逐行可读，对齐会话持久化的做法。
type reportHeader struct {
	Kind       string    `json:"kind"` // "benchmark-report"
	SuiteHash  string    `json:"suite_hash"`
	Targets    []string  `json:"targets"`
	Repeat     int       `json:"repeat"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	RowCount   int       `json:"row_count"`
}

// WriteJSONL 把报告写成 JSONL（第一行 header，后续每行一个 Result）。
func (r *Report) WriteJSONL(w io.Writer) error {
	enc := json.NewEncoder(w)
	hdr := reportHeader{
		Kind: "benchmark-report", SuiteHash: r.SuiteHash, Targets: r.Targets,
		Repeat: r.Repeat, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		RowCount: len(r.Rows),
	}
	if err := enc.Encode(hdr); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

// SaveReport 落盘报告到路径。
func (r *Report) SaveReport(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return r.WriteJSONL(f)
}

// LoadReport 读取 JSONL 报告。
func LoadReport(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadJSONL(f)
}

// ReadJSONL 从 reader 解析 JSONL 报告。
func ReadJSONL(r io.Reader) (*Report, error) {
	dec := json.NewDecoder(r)
	var hdr reportHeader
	if err := dec.Decode(&hdr); err != nil {
		return nil, fmt.Errorf("benchmark: 报告缺少 header 行: %w", err)
	}
	if hdr.Kind != "benchmark-report" {
		return nil, fmt.Errorf("benchmark: 非法报告 header kind=%q", hdr.Kind)
	}
	rep := &Report{
		SuiteHash: hdr.SuiteHash, Targets: hdr.Targets, Repeat: hdr.Repeat,
		StartedAt: hdr.StartedAt, FinishedAt: hdr.FinishedAt,
	}
	for {
		var row Result
		if err := dec.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("benchmark: 结果行解析失败: %w", err)
		}
		rep.Rows = append(rep.Rows, row)
	}
	if hdr.RowCount > 0 && len(rep.Rows) != hdr.RowCount {
		return nil, fmt.Errorf("benchmark: 报告行数不符（header 声明 %d, 实际 %d）——文件截断？", hdr.RowCount, len(rep.Rows))
	}
	return rep, nil
}

// ---------- 套件哈希 ----------

// SuiteHash 对套件做内容哈希（case 声明规范化序列化后 sha256）。
// case 按 ID 排序后哈希——顺序不影响可比性（Runner 并发跑，case
// 顺序本就不该影响结果）。报告携带此哈希；diff 两份报告前先比对——
// 哈希不同则结果不可比（tau3 改分后新旧结果不可比的教训）。
func SuiteHash(cases ...Case) string {
	// json.Marshal 对 map 按 key 排序，序列化稳定。
	type suite struct {
		Kind  string `json:"kind"`
		Cases []Case `json:"cases"`
	}
	sorted := make([]Case, len(cases))
	copy(sorted, cases)
	sort.Slice(sorted, func(i, k int) bool { return sorted[i].ID < sorted[k].ID })
	b, err := json.Marshal(suite{Kind: "benchmark-suite", Cases: sorted})
	if err != nil {
		// Case 全是 JSON 兼容字段，Marshal 不应失败；兜底常量哈希。
		return "unhashable"
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// ---------- Diff ----------

// Diff 两份报告的 per-case 状态迁移。旧→新（a = 基线，b = 新版本）。
type Diff struct {
	// Fixed fail→pass（修好的）
	Fixed []string `json:"fixed,omitempty"`
	// Regressed pass→fail（回归——最需要人看的清单）
	Regressed []string `json:"regressed,omitempty"`
	// Unstable 涉及 infra/excluded 的迁移（基建或 case 损坏，
	// 先修基建再谈回归）
	Unstable []string `json:"unstable,omitempty"`
	// SuiteMismatch 两份报告套件哈希不同时为 true——此时整个 diff
	// 仅供参考（case 集变了，对比口径不成立）。
	SuiteMismatch bool `json:"suite_mismatch,omitempty"`
}

// String 人类可读摘要。
func (d Diff) String() string {
	if d.SuiteMismatch {
		return fmt.Sprintf("diff: 套件哈希不同，对比口径不成立（fixed=%d regressed=%d unstable=%d 仅参考）",
			len(d.Fixed), len(d.Regressed), len(d.Unstable))
	}
	return fmt.Sprintf("diff: fixed=%d regressed=%d unstable=%d", len(d.Fixed), len(d.Regressed), len(d.Unstable))
}

// DiffReports 对比两份报告（AllPass 口径聚合成 case 级三态：
// pass / fail / indeterminate——有效 trial 为 0 的 case 无法判定）。
// 键为 "target/case_id"。
func DiffReports(a, b *Report) Diff {
	var d Diff
	if a == nil || b == nil {
		d.Unstable = append(d.Unstable, "(nil report)")
		return d
	}
	if a.SuiteHash != b.SuiteHash {
		d.SuiteMismatch = true
	}
	state := func(r *Report) map[string]string {
		m := map[string]string{}
		for _, s := range r.Summaries() {
			k := s.Target + "/" + s.CaseID
			switch {
			case s.Valid == 0:
				m[k] = "indeterminate"
			case s.AllPass:
				m[k] = "pass"
			default:
				m[k] = "fail"
			}
		}
		return m
	}
	oldS, newS := state(a), state(b)
	for k, nv := range newS {
		ov, ok := oldS[k]
		if !ok {
			continue // 新增 case，无基线
		}
		if ov == nv {
			continue
		}
		switch {
		case ov == "pass" && nv == "fail":
			d.Regressed = append(d.Regressed, k)
		case ov == "fail" && nv == "pass":
			d.Fixed = append(d.Fixed, k)
		default:
			// 涉及 indeterminate（infra/excluded 吞掉全部 trial）的迁移。
			d.Unstable = append(d.Unstable, fmt.Sprintf("%s: %s→%s", k, ov, nv))
		}
	}
	// 删掉的 case（旧有新无）不算回归，但值得提示。
	for k := range oldS {
		if _, ok := newS[k]; !ok {
			d.Unstable = append(d.Unstable, fmt.Sprintf("%s: 已从套件移除", k))
		}
	}
	return d
}
