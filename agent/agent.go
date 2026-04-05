// Package agent 实现子 agent 系统。
//
// 子 agent 是独立运行的 agent 循环实例，拥有自己的系统提示、
// 工具集和最大轮次限制。主 agent 可以通过工具调用启动子 agent。
//
// 对齐 Claude Code 的子 agent 架构。
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropic-community/goagent/message"
	"github.com/anthropic-community/goagent/provider"
)

// Definition 定义一个子 agent。
type Definition struct {
	// Name 是子 agent 的名称。
	Name string

	// Description 是展示给 LLM 的描述，用于决定何时启动此 agent。
	Description string

	// SystemPrompt 是子 agent 的系统提示。
	SystemPrompt string

	// Tools 是子 agent 可用的工具集。
	Tools []ToolDef

	// MaxTurns 是子 agent 的最大轮次。默认 10。
	MaxTurns int

	// Model 是子 agent 使用的模型（可选，默认使用主 agent 的模型）。
	Model string

	// Provider 是子 agent 使用的 provider（可选）。
	Provider provider.Provider
}

// ToolDef 是子 agent 工具的简化定义。
type ToolDef struct {
	Name        string
	Description string
	InputSchema any
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
}

// RunResult 是子 agent 运行的结果。
type RunResult struct {
	// FinalText 是子 agent 的最终文本输出。
	FinalText string

	// Messages 是子 agent 的完整消息历史。
	Messages []message.Message

	// TurnCount 是子 agent 运行的轮次数。
	TurnCount int

	// Usage 是子 agent 消耗的 token 统计。
	Usage provider.Usage
}

// Runner 执行子 agent。
type Runner struct {
	// defaultProvider 是没有指定 provider 时使用的默认 provider。
	defaultProvider provider.Provider
}

// NewRunner 创建一个新的子 agent 运行器。
func NewRunner(defaultProvider provider.Provider) *Runner {
	return &Runner{defaultProvider: defaultProvider}
}

// Run 执行子 agent 并返回结果。
// 这是一个隔离的 agent 循环，不共享主循环的消息历史。
func (r *Runner) Run(ctx context.Context, def Definition, input string) (*RunResult, error) {
	prov := def.Provider
	if prov == nil {
		prov = r.defaultProvider
	}
	if prov == nil {
		return nil, fmt.Errorf("子 agent %q 没有可用的 provider", def.Name)
	}

	maxTurns := def.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	// 构建工具定义。
	toolDefs := make([]provider.ToolDefinition, len(def.Tools))
	toolIndex := make(map[string]*ToolDef, len(def.Tools))
	for i, t := range def.Tools {
		toolDefs[i] = provider.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		td := def.Tools[i]
		toolIndex[t.Name] = &td
	}

	// 初始化消息。
	messages := []message.Message{message.NewUserMessage(input)}
	var totalUsage provider.Usage
	turnCount := 0

	// 子 agent 循环。
	for turnCount < maxTurns {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// 调用 API。
		resp, err := prov.Complete(ctx, &provider.Request{
			Messages:     messages,
			SystemPrompt: def.SystemPrompt,
			Tools:        toolDefs,
			MaxTokens:    4096,
		})
		if err != nil {
			return nil, fmt.Errorf("子 agent %q API 调用失败: %w", def.Name, err)
		}

		// 累计 token 使用。
		totalUsage.InputTokens += resp.Usage.InputTokens
		totalUsage.OutputTokens += resp.Usage.OutputTokens

		// 添加助手消息。
		messages = append(messages, resp.Message)

		// 提取工具调用。
		toolCalls := message.ExtractToolCalls(resp.Message)
		if len(toolCalls) == 0 {
			// 无工具调用，子 agent 完成。
			return &RunResult{
				FinalText: message.ExtractText(resp.Message),
				Messages:  messages,
				TurnCount: turnCount + 1,
				Usage:     totalUsage,
			}, nil
		}

		// 执行工具调用。
		for _, tc := range toolCalls {
			tool := toolIndex[tc.Name]
			if tool == nil {
				messages = append(messages, message.NewToolResultMessage(
					tc.ID,
					fmt.Sprintf("未知工具: %s", tc.Name),
					true,
				))
				continue
			}

			result, toolErr := tool.Execute(ctx, tc.Input)
			isError := toolErr != nil
			if isError {
				result = "错误: " + toolErr.Error()
			}
			messages = append(messages, message.NewToolResultMessage(tc.ID, result, isError))
		}

		turnCount++
	}

	// 达到最大轮次。
	finalText := ""
	if last := messages[len(messages)-1]; last.Role == message.RoleAssistant {
		finalText = message.ExtractText(last)
	}

	return &RunResult{
		FinalText: finalText,
		Messages:  messages,
		TurnCount: turnCount,
		Usage:     totalUsage,
	}, nil
}
