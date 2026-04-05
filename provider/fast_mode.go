// Package provider 定义 provider 接口。
package provider

import (
	"context"
)

// FastModeConfig 快速模式配置。
type FastModeConfig struct {
	// Enabled 是否启用快速模式。
	Enabled bool
	// MaxTokens 最大输出 tokens（快速模式限制）。
	MaxTokens int
	// Model 快速模式使用的模型。
	Model string
	// SkipTools 是否跳过工具调用。
	SkipTools bool
}

// DefaultFastModeConfig 返回默认快速模式配置。
func DefaultFastModeConfig() FastModeConfig {
	return FastModeConfig{
		Enabled:   false,
		MaxTokens: 1024,
		Model:     "claude-haiku-4-5-20251001",
		SkipTools: false,
	}
}

// FastModeProvider 快速模式包装器。
// 对齐 Claude Code 的 fast mode 机制。
type FastModeProvider struct {
	provider Provider
	config   FastModeConfig
	fallback Provider
}

// NewFastModeProvider 创建快速模式包装器。
func NewFastModeProvider(provider Provider, config FastModeConfig) *FastModeProvider {
	if config.Model == "" {
		config.Model = DefaultFastModeConfig().Model
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 1024
	}
	return &FastModeProvider{
		provider: provider,
		config:   config,
	}
}

// Stream 实现 Provider 接口的 Stream 方法。
// 在快速模式下使用简化配置。
func (f *FastModeProvider) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	if !f.config.Enabled {
		return f.provider.Stream(ctx, req)
	}

	// 创建快速模式请求
	fastReq := *req
	if fastReq.MaxTokens == 0 || fastReq.MaxTokens > f.config.MaxTokens {
		fastReq.MaxTokens = f.config.MaxTokens
	}
	if f.config.Model != "" {
		fastReq.Model = f.config.Model
	}

	// 如果启用 SkipTools，移除工具
	if f.config.SkipTools {
		fastReq.Tools = nil
	}

	return f.provider.Stream(ctx, &fastReq)
}

// Complete 实现 Provider 接口的 Complete 方法。
func (f *FastModeProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	if !f.config.Enabled {
		return f.provider.Complete(ctx, req)
	}

	// 创建快速模式请求
	fastReq := *req
	if fastReq.MaxTokens == 0 || fastReq.MaxTokens > f.config.MaxTokens {
		fastReq.MaxTokens = f.config.MaxTokens
	}
	if f.config.Model != "" {
		fastReq.Model = f.config.Model
	}

	if f.config.SkipTools {
		fastReq.Tools = nil
	}

	return f.provider.Complete(ctx, &fastReq)
}

// Capabilities 返回提供商能力。
func (f *FastModeProvider) Capabilities() Capabilities {
	return f.provider.Capabilities()
}

// SetModel 设置模型（透传到实际 provider）。
func (f *FastModeProvider) SetModel(modelID string) {
	if switcher, ok := f.provider.(ModelSwitcher); ok {
		switcher.SetModel(modelID)
	}
}

// Enable 启用快速模式。
func (f *FastModeProvider) Enable() {
	f.config.Enabled = true
}

// Disable 禁用快速模式。
func (f *FastModeProvider) Disable() {
	f.config.Enabled = false
}

// IsEnabled 返回快速模式是否启用。
func (f *FastModeProvider) IsEnabled() bool {
	return f.config.Enabled
}

// IsFastEligible 判断请求是否符合快速模式条件。
func IsFastEligible(req *Request) bool {
	// 简单请求适合快速模式：
	// - 没有工具调用
	// - 消息较少
	// - 没有系统提示词
	if len(req.Tools) > 0 {
		return false
	}
	if len(req.Messages) > 5 {
		return false
	}
	if req.SystemPrompt != "" && len(req.SystemPrompt) > 500 {
		return false
	}
	return true
}
