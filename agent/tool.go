// Package agent — 子 agent 工具定义。
//
// AgentToolDef 创建一个 LLM 可调用的工具，用于启动子 agent。
// 主 agent 可以通过此工具将复杂任务委托给子 agent。
//
// 对齐 Claude Code 的 Agent 工具。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// AgentToolInput 是 Agent 工具的输入结构。
type AgentToolInput struct {
	// Prompt 是发送给子 agent 的提示。
	Prompt string `json:"prompt"`
}

// AgentToolDef 创建一个启动子 agent 的工具定义。
// 返回工具名称、描述、输入 schema 和执行函数。
func AgentToolDef(runner *Runner, def Definition) (string, string, any, func(context.Context, json.RawMessage) (string, error)) {
	name := "Agent_" + def.Name

	description := fmt.Sprintf("启动子 agent '%s'。%s", def.Name, def.Description)

	inputSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "发送给子 agent 的提示/任务描述",
			},
		},
		"required": []string{"prompt"},
	}

	executeFn := func(ctx context.Context, input json.RawMessage) (string, error) {
		var toolInput AgentToolInput
		if err := json.Unmarshal(input, &toolInput); err != nil {
			return "", fmt.Errorf("解析 Agent 工具输入失败: %w", err)
		}

		result, err := runner.Run(ctx, def, toolInput.Prompt)
		if err != nil {
			return "", fmt.Errorf("子 agent '%s' 执行失败: %w", def.Name, err)
		}

		return result.FinalText, nil
	}

	return name, description, inputSchema, executeFn
}

// RegisterAgentTools 将多个子 agent 定义注册为工具。
// 返回可供主 agent 使用的工具定义列表。
type AgentToolEntry struct {
	Name        string
	Description string
	InputSchema any
	Execute     func(context.Context, json.RawMessage) (string, error)
}

// CreateAgentTools 从定义列表创建子 agent 工具。
func CreateAgentTools(runner *Runner, defs []Definition) []AgentToolEntry {
	entries := make([]AgentToolEntry, len(defs))
	for i, def := range defs {
		name, desc, schema, execFn := AgentToolDef(runner, def)
		entries[i] = AgentToolEntry{
			Name:        name,
			Description: desc,
			InputSchema: schema,
			Execute:     execFn,
		}
	}
	return entries
}
