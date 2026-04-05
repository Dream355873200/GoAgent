// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"context"
	"fmt"

	"github.com/anthropic-community/goagent/message"
	"github.com/anthropic-community/goagent/provider"
)

// ForkedAgentConfig 配置 forked agent。
type ForkedAgentConfig struct {
	// Provider 用于运行 forked agent 的提供者。
	Provider provider.Provider
	// SystemPrompt 系统提示词（forked agent 使用相同的）。
	SystemPrompt string
	// MaxTokens 最大输出 tokens。
	MaxTokens int
	// MaxTurns 最大轮次（默认为 1）。
	MaxTurns int
	// Tools 工具定义列表。
	Tools []ToolDefinition
	// CustomInstructions 自定义指令。
	CustomInstructions string
}

// ToolDefinition 工具定义。
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema any
}

// ForkedAgentResult forked agent 执行结果。
type ForkedAgentResult struct {
	// Messages 生成的响应消息。
	Messages []message.Message
	// Summary 从响应中提取的摘要文本。
	Summary string
	// TotalUsage 总的 token 使用量。
	TotalUsage provider.Usage
}

// RunForkedAgent 运行一个 forked agent。
// 对齐 Claude Code 的 runForkedAgent。
//
// Forked agent 是隔离的 agent loop，运行指定的 turns 数，
// 通常用于需要完整 agent 能力（工具、streaming 等）的任务，
// 如压缩摘要生成。
//
// 特点：
// - 运行独立的 agent loop，与父 agent 隔离
// - 通常只运行 1 turn（maxTurns=1）
// - 可选使用父 agent 的 system prompt 和 tools
func RunForkedAgent(ctx context.Context, cfg ForkedAgentConfig, prompt string) (*ForkedAgentResult, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("RunForkedAgent: 需要 Provider")
	}

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 1
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}

	// 构建请求消息
	reqMessages := []message.Message{
		message.NewUserMessage(prompt),
	}

	// 添加系统提示词（如果提供）
	systemPrompt := cfg.SystemPrompt
	if cfg.CustomInstructions != "" {
		systemPrompt += "\n\n" + cfg.CustomInstructions
	}

	// 构建工具定义
	var toolDefs []provider.ToolDefinition
	if len(cfg.Tools) > 0 {
		toolDefs = make([]provider.ToolDefinition, len(cfg.Tools))
		for i, t := range cfg.Tools {
			toolDefs[i] = provider.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			}
		}
	}

	// 调用 provider 的 Complete
	req := &provider.Request{
		Messages:     reqMessages,
		SystemPrompt: systemPrompt,
		Tools:        toolDefs,
		MaxTokens:    cfg.MaxTokens,
	}

	resp, err := cfg.Provider.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("RunForkedAgent: LLM 调用失败: %w", err)
	}

	// 提取摘要文本
	summary := message.ExtractText(resp.Message)

	return &ForkedAgentResult{
		Messages:   []message.Message{resp.Message},
		Summary:    summary,
		TotalUsage: resp.Usage,
	}, nil
}

// RunForkedAgentStream 流式运行 forked agent。
// 返回一个 channel，接收响应文本片段。
func RunForkedAgentStream(ctx context.Context, cfg ForkedAgentConfig, prompt string) (<-chan string, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("RunForkedAgentStream: 需要 Provider")
	}

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 1
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}

	// 构建请求消息
	reqMessages := []message.Message{
		message.NewUserMessage(prompt),
	}

	// 添加系统提示词
	systemPrompt := cfg.SystemPrompt
	if cfg.CustomInstructions != "" {
		systemPrompt += "\n\n" + cfg.CustomInstructions
	}

	// 构建工具定义
	var toolDefs []provider.ToolDefinition
	if len(cfg.Tools) > 0 {
		toolDefs = make([]provider.ToolDefinition, len(cfg.Tools))
		for i, t := range cfg.Tools {
			toolDefs[i] = provider.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			}
		}
	}

	req := &provider.Request{
		Messages:     reqMessages,
		SystemPrompt: systemPrompt,
		Tools:        toolDefs,
		MaxTokens:    cfg.MaxTokens,
	}

	stream, err := cfg.Provider.Stream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("RunForkedAgentStream: 流式调用失败: %w", err)
	}

	out := make(chan string, 1)

	go func() {
		defer close(out)
		for ev := range stream {
			if ev.Text != "" {
				out <- ev.Text
			}
		}
	}()

	return out, nil
}

// ForkedAgent 是一个轻量级的 forked agent 接口。
// 用于需要完整 agent 能力（工具、streaming 等）但需要隔离执行的任务。
//
// 对齐 Claude Code 的 runForkedAgent 机制。
type ForkedAgent interface {
	// Run 运行 forked agent 并返回结果。
	Run(ctx context.Context, prompt string) (*ForkedAgentResult, error)

	// RunStream 流式运行 forked agent，返回文本片段 channel。
	RunStream(ctx context.Context, prompt string) (<-chan string, error)
}

// DefaultForkedAgent 默认的 ForkedAgent 实现。
type DefaultForkedAgent struct {
	cfg ForkedAgentConfig
}

// NewDefaultForkedAgent 创建默认的 ForkedAgent。
func NewDefaultForkedAgent(cfg ForkedAgentConfig) *DefaultForkedAgent {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 1
	}
	return &DefaultForkedAgent{cfg: cfg}
}

func (a *DefaultForkedAgent) Run(ctx context.Context, prompt string) (*ForkedAgentResult, error) {
	return RunForkedAgent(ctx, a.cfg, prompt)
}

func (a *DefaultForkedAgent) RunStream(ctx context.Context, prompt string) (<-chan string, error) {
	return RunForkedAgentStream(ctx, a.cfg, prompt)
}

// ForkedAgentFromProvider 创建一个使用指定 provider 的 ForkedAgent。
func ForkedAgentFromProvider(prov provider.Provider, systemPrompt string) *DefaultForkedAgent {
	return NewDefaultForkedAgent(ForkedAgentConfig{
		Provider:     prov,
		SystemPrompt: systemPrompt,
		MaxTurns:     1,
		MaxTokens:    4096,
	})
}
