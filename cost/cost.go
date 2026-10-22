// Package cost 实现 API 调用成本追踪。
//
// 支持按模型、按会话追踪 token 使用量和对应的 USD 成本。
// 内置 Anthropic 模型定价表（可自定义扩展）。
//
// 对齐 Claude Code 的成本追踪逻辑。
package cost

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
)

// ModelPricing 定义模型的定价信息。
type ModelPricing struct {
	// InputPerMillion 是每百万输入 token 的 USD 价格。
	InputPerMillion float64
	// OutputPerMillion 是每百万输出 token 的 USD 价格。
	OutputPerMillion float64
	// CacheReadPerMillion 是每百万缓存读取 token 的 USD 价格。
	CacheReadPerMillion float64
	// CacheWritePerMillion 是每百万缓存写入 token 的 USD 价格。
	CacheWritePerMillion float64
}

// 内置 Anthropic 模型定价（2025 年价格）。
var defaultPricing = map[string]ModelPricing{
	"claude-opus-4-6": {
		InputPerMillion:      15.0,
		OutputPerMillion:     75.0,
		CacheReadPerMillion:  1.5,
		CacheWritePerMillion: 18.75,
	},
	"claude-sonnet-4-6": {
		InputPerMillion:      3.0,
		OutputPerMillion:     15.0,
		CacheReadPerMillion:  0.3,
		CacheWritePerMillion: 3.75,
	},
	"claude-haiku-4-5": {
		InputPerMillion:      0.8,
		OutputPerMillion:     4.0,
		CacheReadPerMillion:  0.08,
		CacheWritePerMillion: 1.0,
	},
	// 兼容旧版模型 ID。
	"claude-3-5-sonnet-20241022": {
		InputPerMillion:  3.0,
		OutputPerMillion: 15.0,
	},
	"claude-3-5-haiku-20241022": {
		InputPerMillion:  0.8,
		OutputPerMillion: 4.0,
	},
}

// UsageRecord 记录单次 API 调用的 token 使用量。
type UsageRecord struct {
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CacheRead    int       `json:"cache_read,omitempty"`
	CacheWrite   int       `json:"cache_write,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	CostUSD      float64   `json:"cost_usd"`
}

// CostSummary 是成本追踪的汇总。
type CostSummary struct {
	TotalUSD    float64            `json:"total_usd"`
	TotalInput  int                `json:"total_input_tokens"`
	TotalOutput int                `json:"total_output_tokens"`
	TotalCache  int                `json:"total_cache_tokens"`
	ByModel     map[string]float64 `json:"by_model"`
	RecordCount int                `json:"record_count"`
}

// Tracker 追踪 API 调用成本。
type Tracker struct {
	mu      sync.Mutex
	records []UsageRecord
	pricing map[string]ModelPricing
}

// NewTracker 创建一个新的成本追踪器。
func NewTracker() *Tracker {
	// 复制默认定价表。
	pricing := make(map[string]ModelPricing, len(defaultPricing))
	for k, v := range defaultPricing {
		pricing[k] = v
	}
	return &Tracker{
		pricing: pricing,
	}
}

// SetPricing 设置或覆盖模型定价。
func (t *Tracker) SetPricing(model string, pricing ModelPricing) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pricing[model] = pricing
}

// Record 记录一次 API 调用的 token 使用。
func (t *Tracker) Record(model string, inputTokens, outputTokens, cacheRead, cacheWrite int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	costUSD := t.calculateCost(model, inputTokens, outputTokens, cacheRead, cacheWrite)

	t.records = append(t.records, UsageRecord{
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CacheRead:    cacheRead,
		CacheWrite:   cacheWrite,
		Timestamp:    time.Now(),
		CostUSD:      costUSD,
	})
}

// TotalCostUSD 返回总的 USD 成本。
func (t *Tracker) TotalCostUSD() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	total := 0.0
	for _, r := range t.records {
		total += r.CostUSD
	}
	return total
}

// Summary 返回成本追踪的汇总。
func (t *Tracker) Summary() CostSummary {
	t.mu.Lock()
	defer t.mu.Unlock()

	summary := CostSummary{
		ByModel:     make(map[string]float64),
		RecordCount: len(t.records),
	}

	for _, r := range t.records {
		summary.TotalUSD += r.CostUSD
		summary.TotalInput += r.InputTokens
		summary.TotalOutput += r.OutputTokens
		summary.TotalCache += r.CacheRead + r.CacheWrite
		summary.ByModel[r.Model] += r.CostUSD
	}

	return summary
}

// FormatSummary 返回人类可读的成本摘要。
func (t *Tracker) FormatSummary() string {
	s := t.Summary()
	result := fmt.Sprintf("总成本: $%.4f (%.2f%% 输入, %.2f%% 输出)\n",
		s.TotalUSD,
		safePercent(float64(s.TotalInput), float64(s.TotalInput+s.TotalOutput)),
		safePercent(float64(s.TotalOutput), float64(s.TotalInput+s.TotalOutput)),
	)
	result += fmt.Sprintf("Token 使用: %d 输入, %d 输出", s.TotalInput, s.TotalOutput)
	if s.TotalCache > 0 {
		result += fmt.Sprintf(", %d 缓存", s.TotalCache)
	}
	result += "\n"

	if len(s.ByModel) > 1 {
		result += "按模型:\n"
		for model, cost := range s.ByModel {
			result += fmt.Sprintf("  %s: $%.4f\n", model, cost)
		}
	}

	return result
}

// Reset 清除所有追踪记录。
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records = nil
}

// calculateCost 计算单次 API 调用的 USD 成本。未知模型记为 0（不再按
// Sonnet 价格猜测），宿主可用 SetPricing 补充定价。
func (t *Tracker) calculateCost(model string, inputTokens, outputTokens, cacheRead, cacheWrite int) float64 {
	pricing, ok := lookupPricing(t.pricing, model)
	if !ok {
		return 0
	}
	return pricing.cost(inputTokens, outputTokens, cacheRead, cacheWrite)
}

func (p ModelPricing) cost(inputTokens, outputTokens, cacheRead, cacheWrite int) float64 {
	c := float64(inputTokens) * p.InputPerMillion / 1_000_000
	c += float64(outputTokens) * p.OutputPerMillion / 1_000_000
	c += float64(cacheRead) * p.CacheReadPerMillion / 1_000_000
	c += float64(cacheWrite) * p.CacheWritePerMillion / 1_000_000
	return c
}

// LookupPricing 在内置定价表中查找模型定价，容忍 "-v1"、"-v1:0"、日期等
// 后缀（如 "claude-sonnet-4-6-v1" 命中 "claude-sonnet-4-6"）。
func LookupPricing(model string) (ModelPricing, bool) {
	return lookupPricing(defaultPricing, model)
}

// Estimate 按内置定价表估算一次调用的 USD 成本；未知模型返回 0。
// usage 的 InputTokens 不含缓存部分（见 provider.Usage 的口径约定）。
func Estimate(model string, usage provider.Usage) float64 {
	p, ok := LookupPricing(model)
	if !ok {
		return 0
	}
	return p.cost(usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreateTokens)
}

func lookupPricing(table map[string]ModelPricing, model string) (ModelPricing, bool) {
	if p, ok := table[model]; ok {
		return p, true
	}
	base := normalizeModelID(model)
	if p, ok := table[base]; ok {
		return p, true
	}
	return ModelPricing{}, false
}

// normalizeModelID 去掉云厂商与版本后缀：Bedrock 的 "-v1"、"-v1:0"，
// 以及 "-20251001" 这类日期后缀。
func normalizeModelID(model string) string {
	m := model
	if i := strings.LastIndex(m, ":"); i > 0 {
		m = m[:i]
	}
	m = strings.TrimSuffix(m, "-v1")
	m = strings.TrimSuffix(m, "-v2")
	if i := strings.LastIndex(m, "-"); i > 0 {
		if tail := m[i+1:]; len(tail) == 8 && strings.Trim(tail, "0123456789") == "" {
			m = m[:i]
		}
	}
	return m
}

func safePercent(part, total float64) float64 {
	if total == 0 {
		return 0
	}
	return part / total * 100
}
