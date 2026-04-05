// Package thinking 实现 Extended Thinking 支持。
//
// 对齐 Claude Code 的 thinking 配置和 thinking block 处理。
// 支持 adaptive/enabled/disabled 三种模式和 ultrathink budget。
package thinking

import (
	"context"
	"strings"
)

// Mode 是 thinking 的运行模式。
type Mode string

const (
	// ModeAdaptive 自适应模式：根据任务复杂度自动决定是否使用 thinking。
	ModeAdaptive Mode = "adaptive"

	// ModeEnabled 启用模式：始终使用 thinking。
	ModeEnabled Mode = "enabled"

	// ModeDisabled 禁用模式：不使用 thinking。
	ModeDisabled Mode = "disabled"
)

// UltrathinkThreshold 是启用 ultrathink 的最低 budget。
const UltrathinkThreshold = 10000

// DefaultBudget 是默认的 thinking budget (tokens)。
const DefaultBudget = 5000

// Config 是 Extended Thinking 的配置。
type Config struct {
	// Mode 是 thinking 模式。默认 ModeAdaptive。
	Mode Mode `json:"mode"`

	// BudgetTokens 是 thinking 的 token 预算。
	// 超过 UltrathinkThreshold 时启用 ultrathink。
	// 默认 5000。
	BudgetTokens int `json:"budget_tokens"`

	// SlowModel 是否使用更便宜的模型做思考。
	// 对齐 Claude Code：使用 Haiku 等便宜模型进行推理过程思考，
	// 不消耗 Opus/Sonnet 的配额。
	SlowModel bool `json:"slow_model"`

	// SlowModelID 指定用于思考的模型 ID。
	// 当 SlowModel 为 true 时使用。
	SlowModelID string `json:"slow_model_id,omitempty"`
}

// DefaultConfig 返回默认的 thinking 配置。
func DefaultConfig() Config {
	return Config{
		Mode:         ModeAdaptive,
		BudgetTokens: DefaultBudget,
	}
}

// IsEnabled 返回 thinking 是否启用。
func (c Config) IsEnabled() bool {
	return c.Mode != ModeDisabled
}

// IsUltrathink 返回是否启用 ultrathink 模式。
func (c Config) IsUltrathink() bool {
	return c.BudgetTokens >= UltrathinkThreshold
}

// ThinkingBlock 表示 LLM 返回的 thinking 内容块。
type ThinkingBlock struct {
	// Text 是 thinking 的文本内容。
	Text string `json:"text"`

	// Tokens 是 thinking 使用的 token 数。
	Tokens int `json:"tokens,omitempty"`
}

// ToAPIParam 将 Config 转换为 API 请求参数。
// 返回 nil 表示不发送 thinking 参数。
func (c Config) ToAPIParam() map[string]any {
	if c.Mode == ModeDisabled {
		return nil
	}

	param := map[string]any{
		"type": "enabled",
	}

	if c.BudgetTokens > 0 {
		param["budget_tokens"] = c.BudgetTokens
	}

	return param
}

// ShouldUseThinking 根据模式和上下文决定是否使用 thinking。
// toolCount: 本轮可用的工具数量。
// contextTokens: 当前上下文的 token 数。
func (c Config) ShouldUseThinking(toolCount int, contextTokens int) bool {
	switch c.Mode {
	case ModeEnabled:
		return true
	case ModeDisabled:
		return false
	case ModeAdaptive:
		// 自适应逻辑：工具多或上下文大时启用 thinking。
		if toolCount > 5 {
			return true
		}
		if contextTokens > 50000 {
			return true
		}
		return false
	default:
		return false
	}
}

// AdjustBudget 根据上下文动态调整 thinking budget。
// 返回调整后的 budget tokens。
func AdjustBudget(baseBudget int, contextTokens int, contextWindow int) int {
	if baseBudget <= 0 {
		baseBudget = DefaultBudget
	}

	// 上下文接近满时减少 thinking budget。
	usageRatio := float64(contextTokens) / float64(contextWindow)
	if usageRatio > 0.8 {
		// 上下文使用超过 80% 时，将 budget 减半。
		return baseBudget / 2
	}
	if usageRatio > 0.9 {
		// 超过 90% 时，将 budget 降到最低。
		return 1000
	}

	return baseBudget
}

// ThinkingProvider 思考内容提供者接口。
// 用于支持自定义思考策略，如使用不同模型做思考。
type ThinkingProvider interface {
	// GetThinking 生成思考内容。
	// 返回一个流式通道，内容通过该通道发送。
	GetThinking(ctx context.Context, prompt string) (<-chan ThinkingChunk, error)

	// GetModel 返回用于思考的模型 ID。
	GetModel() string
}

// ThinkingChunk 思考内容块。
type ThinkingChunk struct {
	// Text 是思考文本。
	Text string
	// Tokens 是到目前为止使用的 token 数。
	Tokens int
	// IsFinal 表示是否完成。
	IsFinal bool
}

// DefaultThinkingProvider 默认思考提供者。
// 使用主模型进行思考。
type DefaultThinkingProvider struct {
	modelID string
}

// NewDefaultThinkingProvider 创建默认思考提供者。
func NewDefaultThinkingProvider(modelID string) *DefaultThinkingProvider {
	return &DefaultThinkingProvider{modelID: modelID}
}

// GetThinking 实现 ThinkingProvider 接口。
func (p *DefaultThinkingProvider) GetThinking(ctx context.Context, prompt string) (<-chan ThinkingChunk, error) {
	// 默认实现返回空通道，子类应重写此方法
	ch := make(chan ThinkingChunk, 1)
	close(ch)
	return ch, nil
}

// GetModel 返回模型 ID。
func (p *DefaultThinkingProvider) GetModel() string {
	return p.modelID
}

// SlowThinkingProvider 使用便宜模型做思考的提供者。
type SlowThinkingProvider struct {
	fastModelID string // 主模型
	slowModelID string // 思考用模型
}

// NewSlowThinkingProvider 创建慢速思考提供者。
func NewSlowThinkingProvider(fastModelID, slowModelID string) *SlowThinkingProvider {
	return &SlowThinkingProvider{
		fastModelID: fastModelID,
		slowModelID: slowModelID,
	}
}

// GetModel 返回思考用模型 ID。
func (p *SlowThinkingProvider) GetModel() string {
	return p.slowModelID
}

// GetThinking 使用便宜模型生成思考。
func (p *SlowThinkingProvider) GetThinking(ctx context.Context, prompt string) (<-chan ThinkingChunk, error) {
	// TODO: 实现使用 slowModelID 调用 API 的逻辑
	ch := make(chan ThinkingChunk, 1)
	close(ch)
	return ch, nil
}

// ThinkingSummarizer 思考摘要器。
// 将长思考内容压缩为短摘要。
type ThinkingSummarizer interface {
	// Summarize 生成思考摘要。
	Summarize(ctx context.Context, thinking string) (string, error)
}

// DefaultThinkingSummarizer 默认思考摘要器。
type DefaultThinkingSummarizer struct{}

// Summarize 实现 ThinkingSummarizer 接口。
func (s *DefaultThinkingSummarizer) Summarize(ctx context.Context, thinking string) (string, error) {
	// 默认实现返回原文，子类应重写此方法
	return thinking, nil
}

// ThinkingStats 思考统计信息。
type ThinkingStats struct {
	// TokensUsed 使用的 token 数。
	TokensUsed int
	// ThinkingText 思考内容。
	ThinkingText string
	// IsTruncated 是否被截断。
	IsTruncated bool
}

// CollectThinking 从流中收集思考内容。
func CollectThinking(ch <-chan ThinkingChunk) *ThinkingStats {
	stats := &ThinkingStats{}
	var builder strings.Builder
	for chunk := range ch {
		builder.WriteString(chunk.Text)
		stats.TokensUsed = chunk.Tokens
		if chunk.IsFinal {
			break
		}
	}
	stats.ThinkingText = builder.String()
	return stats
}
