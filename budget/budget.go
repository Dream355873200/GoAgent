// Package budget 实现 token 预算追踪和 diminishing returns 检测。
//
// 对齐 Claude Code 的 BudgetTracker + checkTokenBudget() 逻辑。
// 支持 per-turn token 预算、总预算、diminishing returns 提前终止。
package budget

import (
	"fmt"
	"sync"
)

// Config 是预算追踪器的配置。
type Config struct {
	// TotalBudget 是整个会话的 token 总预算。0 表示无限制。
	TotalBudget int

	// MinOutputPerTurn 是每轮最少输出 token 数。
	// 连续多轮低于此值触发 diminishing returns。默认 50。
	MinOutputPerTurn int

	// MaxDiminishingTurns 是允许的连续 diminishing returns 轮数。
	// 超过此值后会话提前终止。默认 3。
	MaxDiminishingTurns int
}

// DefaultConfig 返回默认预算配置。
func DefaultConfig() Config {
	return Config{
		TotalBudget:         0,
		MinOutputPerTurn:    50,
		MaxDiminishingTurns: 3,
	}
}

// TurnUsage 记录单轮的 token 使用量。
type TurnUsage struct {
	Turn         int `json:"turn"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Status 是预算检查的结果。
type Status int

const (
	// StatusOK 预算充足，可以继续。
	StatusOK Status = iota
	// StatusWarning 预算接近耗尽（剩余 < 20%）。
	StatusWarning
	// StatusExhausted 预算已耗尽，应停止。
	StatusExhausted
	// StatusDiminishing 检测到 diminishing returns，建议停止。
	StatusDiminishing
)

// String 返回状态名称。
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarning:
		return "warning"
	case StatusExhausted:
		return "exhausted"
	case StatusDiminishing:
		return "diminishing"
	default:
		return "unknown"
	}
}

// CheckResult 是预算检查的详细结果。
type CheckResult struct {
	// Status 是检查状态。
	Status Status
	// Reason 是状态原因的描述。
	Reason string
	// Remaining 是剩余 token 预算。-1 表示无限制。
	Remaining int
	// TotalConsumed 是已消耗的总 token 数。
	TotalConsumed int
}

// ShouldStop 返回是否应该停止 agent 循环。
func (r CheckResult) ShouldStop() bool {
	return r.Status == StatusExhausted || r.Status == StatusDiminishing
}

// Tracker 追踪 token 预算使用情况。
type Tracker struct {
	mu     sync.Mutex
	config Config

	// 每轮使用量记录。
	turns []TurnUsage

	// 总消耗。
	totalInput  int
	totalOutput int

	// diminishing returns 计数器。
	consecutiveLowOutput int
}

// NewTracker 创建一个新的预算追踪器。
func NewTracker(cfg Config) *Tracker {
	if cfg.MinOutputPerTurn <= 0 {
		cfg.MinOutputPerTurn = DefaultConfig().MinOutputPerTurn
	}
	if cfg.MaxDiminishingTurns <= 0 {
		cfg.MaxDiminishingTurns = DefaultConfig().MaxDiminishingTurns
	}
	return &Tracker{config: cfg}
}

// RecordUsage 记录一轮的 token 使用量。
func (t *Tracker) RecordUsage(turn, inputTokens, outputTokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.turns = append(t.turns, TurnUsage{
		Turn:         turn,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	})
	t.totalInput += inputTokens
	t.totalOutput += outputTokens

	// 更新 diminishing returns 计数。
	if outputTokens < t.config.MinOutputPerTurn {
		t.consecutiveLowOutput++
	} else {
		t.consecutiveLowOutput = 0
	}
}

// Check 检查当前预算状态。
func (t *Tracker) Check() CheckResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	totalConsumed := t.totalInput + t.totalOutput

	// 检查 diminishing returns。
	if t.consecutiveLowOutput >= t.config.MaxDiminishingTurns {
		return CheckResult{
			Status:        StatusDiminishing,
			Reason:        fmt.Sprintf("连续 %d 轮输出低于 %d tokens", t.consecutiveLowOutput, t.config.MinOutputPerTurn),
			Remaining:     t.remaining(totalConsumed),
			TotalConsumed: totalConsumed,
		}
	}

	// 无总预算限制时始终 OK。
	if t.config.TotalBudget <= 0 {
		return CheckResult{
			Status:        StatusOK,
			Remaining:     -1,
			TotalConsumed: totalConsumed,
		}
	}

	remaining := t.config.TotalBudget - totalConsumed

	// 预算已耗尽。
	if remaining <= 0 {
		return CheckResult{
			Status:        StatusExhausted,
			Reason:        fmt.Sprintf("token 预算已耗尽: %d / %d", totalConsumed, t.config.TotalBudget),
			Remaining:     0,
			TotalConsumed: totalConsumed,
		}
	}

	// 预算接近耗尽（< 20%）。
	warningThreshold := t.config.TotalBudget / 5
	if remaining < warningThreshold {
		return CheckResult{
			Status:        StatusWarning,
			Reason:        fmt.Sprintf("token 预算即将耗尽: 剩余 %d / %d", remaining, t.config.TotalBudget),
			Remaining:     remaining,
			TotalConsumed: totalConsumed,
		}
	}

	return CheckResult{
		Status:        StatusOK,
		Remaining:     remaining,
		TotalConsumed: totalConsumed,
	}
}

// remaining 计算剩余预算（内部方法，不加锁）。
func (t *Tracker) remaining(consumed int) int {
	if t.config.TotalBudget <= 0 {
		return -1
	}
	r := t.config.TotalBudget - consumed
	if r < 0 {
		return 0
	}
	return r
}

// TotalConsumed 返回已消耗的总 token 数。
func (t *Tracker) TotalConsumed() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalInput + t.totalOutput
}

// Turns 返回所有轮次的使用记录。
func (t *Tracker) Turns() []TurnUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]TurnUsage, len(t.turns))
	copy(result, t.turns)
	return result
}

// Reset 清除所有追踪数据。
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turns = nil
	t.totalInput = 0
	t.totalOutput = 0
	t.consecutiveLowOutput = 0
}
