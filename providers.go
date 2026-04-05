package goagent

import (
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/anthropic"
	"github.com/Dream355873200/GoAgent/provider/openai"
)

// Anthropic creates an Anthropic provider with the given API key.
// This is a convenience function for the most common setup.
//
// Options:
//   - Use anthropic.WithModel("claude-opus-4-6-v1") to change the model.
//
// Example:
//
//	goagent.WithProvider(goagent.Anthropic(apiKey))
func Anthropic(apiKey string, opts ...anthropic.Option) provider.Provider {
	return anthropic.New(apiKey, opts...)
}

// Model returns an anthropic.Option that sets the model ID.
// Convenience wrapper so users don't need to import the anthropic package.
func Model(model string) anthropic.Option {
	return anthropic.WithModel(model)
}

// OpenAI 创建一个 OpenAI 兼容的 provider。
// 支持 OpenAI、Ollama、OpenRouter、vLLM 等任何兼容 API。
//
// 示例：
//
//	// 连接本地 Ollama
//	goagent.WithProvider(goagent.OpenAI(openai.Config{
//	    BaseURL: "http://localhost:11434/v1",
//	    Model:   "qwen2.5:7b",
//	}))
//
//	// 连接 OpenRouter
//	goagent.WithProvider(goagent.OpenAI(openai.Config{
//	    APIKey:  "sk-or-...",
//	    BaseURL: "https://openrouter.ai/api/v1",
//	    Model:   "anthropic/claude-3.5-sonnet",
//	}))
func OpenAI(cfg openai.Config) provider.Provider {
	return openai.New(cfg)
}
