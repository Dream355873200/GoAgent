package goagent

import (
	"os"

	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/anthropic"
	"github.com/Dream355873200/GoAgent/provider/openai"
)

// WithOpenAI 使用 OpenAI 兼容 API 作为 Provider。
// 支持 OpenAI、Ollama、OpenRouter、vLLM 等任何兼容 API。
//
// 未指定的参数自动从环境变量读取：
//   - model:   OPENAI_MODEL（默认 "qwen2.5:7b"）
//   - baseURL: OPENAI_BASE_URL（默认 "http://localhost:11434/v1"）
//   - apiKey:  OPENAI_API_KEY（默认 ""，即无需鉴权）
//
// 示例：
//
//	// 环境变量配置一切，最简启动
//	goagent.New(goagent.WithOpenAI())
//
//	// 也可直接用 ProviderConfig（更直观，推荐）
//	goagent.New(goagent.ProviderConfig{
//	    Model:  "gpt-4o",
//	    APIKey: "sk-...",
//	    BaseURL: "https://api.openai.com/v1",
//	})
func WithOpenAI() Option {
	model := os.Getenv("OPENAI_MODEL")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	apiKey := os.Getenv("OPENAI_API_KEY")
	if model == "" {
		model = "qwen2.5:7b"
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	return ProviderConfig{
		Type:    "openai",
		Model:   model,
		APIKey:  apiKey,
		BaseURL: baseURL,
	}
}

// WithAnthropic 使用 Anthropic Claude API 作为 Provider。
//
// 未指定的参数自动从环境变量读取：
//   - apiKey: ANTHROPIC_API_KEY
//   - model:  ANTHROPIC_MODEL（默认 "claude-sonnet-4-6-v1"）
//
// 示例：
//
//	// 环境变量配置一切
//	goagent.New(goagent.WithAnthropic())
//
//	// 也可直接用 ProviderConfig（更直观，推荐）
//	goagent.New(goagent.ProviderConfig{
//	    Type:   "anthropic",
//	    Model:  "claude-sonnet-4-6",
//	    APIKey: "sk-ant-...",
//	})
func WithAnthropic() Option {
	model := os.Getenv("ANTHROPIC_MODEL")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if model == "" {
		model = "claude-sonnet-4-6-v1"
	}
	return ProviderConfig{
		Type:   "anthropic",
		Model:  model,
		APIKey: apiKey,
	}
}

// ---------------------------------------------------------------------------
// 低层 Provider 工厂（需要精细控制时使用）
// ---------------------------------------------------------------------------

// AnthropicOption 是 Anthropic Provider 的配置选项（高级用法）。
type AnthropicOption = anthropic.Option

// Model 返回一个 anthropic.Option，设置模型 ID。
func Model(model string) AnthropicOption {
	return anthropic.WithModel(model)
}

// Anthropic 创建一个 Anthropic provider（大多数场景用 WithAnthropic 即可）。
func Anthropic(apiKey string, opts ...anthropic.Option) provider.Provider {
	return anthropic.New(apiKey, opts...)
}

// OpenAI 创建一个 OpenAI 兼容的 provider（大多数场景用 ProviderConfig 即可）。
func OpenAI(cfg openai.Config) provider.Provider {
	return openai.New(cfg)
}

// buildProvider 根据 ProviderConfig 创建对应的 provider 实例。
func buildProvider(cfg ProviderConfig) provider.Provider {
	typ := cfg.Type
	if typ == "" {
		typ = "openai"
	}

	switch typ {
	case "anthropic":
		model := cfg.Model
		if model == "" {
			model = "claude-sonnet-4-6-v1"
		}
		opts := []anthropic.Option{anthropic.WithModel(model)}
		if cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
		}
		return anthropic.New(cfg.APIKey, opts...)
	default: // "openai" 及其他兼容 API
		return openai.New(openai.Config{
			APIKey:          cfg.APIKey,
			BaseURL:         cfg.BaseURL,
			Model:           cfg.Model,
			ContextWindow:   cfg.ContextWindow,
			MaxOutputTokens: cfg.MaxOutputTokens,
		})
	}
}
