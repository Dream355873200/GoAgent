// Package agent 实现子 agent 支持。
package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// AgentDefinition 定义一个子 agent 的配置。
// 对齐 Claude Code 的 AgentDefinition。
type AgentDefinition struct {
	// AgentType 是 agent 的类型标识（如 "general-purpose", "research"）。
	AgentType string
	// Description 是 agent 的描述。
	Description string
	// Prompt 是 agent 的系统提示词。
	Prompt string
	// Tools 允许此 agent 使用的工具列表（空表示使用所有工具）。
	Tools []string
	// DisallowedTools 禁止此 agent 使用的工具列表。
	DisallowedTools []string
	// Model 使用的模型（空表示继承父 agent）。
	Model string
	// MaxTurns 最大轮次限制。
	MaxTurns int
	// Background 是否在后台运行。
	Background bool
	// Isolation 隔离模式（worktree/remote）。
	Isolation string
	// MCPServers MCP 服务器配置。
	MCPServers []MCPServerConfig
	// Hooks Hooks 配置。
	Hooks map[string]any
	// Skills 技能列表。
	Skills []string
	// InitialPrompt 初始提示。
	InitialPrompt string
	// Memory 记忆范围（user/project/local）。
	Memory string
	// Source Agent 来源（builtin/plugin/user）。
	Source string
}

// MCPServerConfig MCP 服务器配置。
type MCPServerConfig struct {
	// Name 服务器名称。
	Name string
	// Command 启动命令（stdio 模式使用）。
	Command string
	// Args 启动参数（stdio 模式使用）。
	Args []string
	// Env 环境变量（stdio 模式使用）。
	Env map[string]string
	// Transport 传输方式："stdio" 或 "http"。
	Transport string
	// URL HTTP URL（http 模式使用）。
	URL string
	// Auth 认证信息（http 模式使用）。
	Auth string
}

// SubAgentConfig 配置子 agent 运行。
type SubAgentConfig struct {
	// Definition 是子 agent 的定义。
	Definition *AgentDefinition
	// Provider 用于运行子 agent 的提供者。
	Provider provider.Provider
	// ParentProvider 父 agent 的提供者（用于共享 prompt cache）。
	ParentProvider provider.Provider
	// SystemPrompt 父 agent 的系统提示词。
	SystemPrompt string
	// Tools 父 agent 注册的工具列表。
	Tools []ToolEntry
	// MaxTurns 最大运行轮次（0 表示使用 agent 定义的限制）。
	MaxTurns int
	// AbortSignal 取消信号。
	AbortSignal context.Context
	// CustomInstructions 自定义指令。
	CustomInstructions string
}

// ToolEntry 工具条目。
type ToolEntry struct {
	Name        string
	Description string
	InputSchema any
	Permission  PermissionLevel
	ExecuteFn   func(ctx context.Context, input []byte) (string, error)
}

// PermissionLevel 权限级别。
type PermissionLevel int

const (
	PermissionLevelReadOnly PermissionLevel = iota
	PermissionLevelNormal
	PermissionLevelRequireApproval
	PermissionLevelDangerous
)

// SubAgentResult 子 agent 运行结果。
type SubAgentResult struct {
	// Messages 运行过程中产生的消息。
	Messages []message.Message
	// Summary 最终摘要（最后一条助手消息的文本）。
	Summary string
	// TotalUsage 总的 token 使用量。
	TotalUsage provider.Usage
	// TurnCount 运行的总轮次。
	TurnCount int
}

// SubAgent 是一个子 agent 接口。
// 对齐 Claude Code 的 runAgent 机制。
type SubAgent interface {
	// Run 运行子 agent 并返回结果。
	Run(ctx context.Context, cfg SubAgentConfig, initialPrompt string) (*SubAgentResult, error)

	// RunStream 流式运行子 agent。
	RunStream(ctx context.Context, cfg SubAgentConfig, initialPrompt string) (<-chan StreamEvent, error)
}

// StreamEvent 流式事件。
type StreamEvent struct {
	// Type 事件类型。
	Type EventType
	// Text 文本增量。
	Text string
	// Thinking thinking 内容增量。
	Thinking string
	// ToolName 工具名称。
	ToolName string
	// ToolResult 工具结果。
	ToolResult string
	// Done 是否完成。
	Done bool
	// Error 错误。
	Error error
}

// EventType 事件类型。
type EventType int

const (
	EventTypeTextDelta EventType = iota
	EventTypeThinkingDelta
	EventTypeToolStart
	EventTypeToolDone
	EventTypeUsage
	EventTypeDone
)

// DefaultSubAgent 默认的 SubAgent 实现。
type DefaultSubAgent struct{}

// NewDefaultSubAgent 创建默认的 SubAgent。
func NewDefaultSubAgent() *DefaultSubAgent {
	return &DefaultSubAgent{}
}

// Run 运行子 agent。
// 对齐 Claude Code 的 runAgent。
func (a *DefaultSubAgent) Run(ctx context.Context, cfg SubAgentConfig, initialPrompt string) (*SubAgentResult, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("SubAgent.Run: 需要 Provider")
	}

	// 确定使用的工具
	tools := resolveTools(cfg)

	// 确定使用的模型
	model := cfg.Definition.Model
	if model == "" || model == "inherit" {
		model = cfg.Provider.Capabilities().ModelID
	}

	// 确定最大轮次
	maxTurns := cfg.MaxTurns
	if maxTurns == 0 && cfg.Definition.MaxTurns > 0 {
		maxTurns = cfg.Definition.MaxTurns
	}
	if maxTurns == 0 {
		maxTurns = 10 // 默认最大轮次
	}

	// 构建初始消息
	messages := []message.Message{
		message.NewUserMessage(initialPrompt),
	}

	// 构建系统提示词
	systemPrompt := cfg.SystemPrompt
	if cfg.Definition.Prompt != "" {
		systemPrompt = cfg.Definition.Prompt
	}
	if cfg.CustomInstructions != "" {
		systemPrompt += "\n\n" + cfg.CustomInstructions
	}

	var totalUsage provider.Usage
	var lastAssistantText string
	turnCount := 0

	// 运行 agent loop
	for turnCount < maxTurns {
		// 检查取消
		select {
		case <-ctx.Done():
			return &SubAgentResult{
				Messages:   messages,
				Summary:    lastAssistantText,
				TotalUsage: totalUsage,
				TurnCount:  turnCount,
			}, ctx.Err()
		default:
		}

		// 调用 API
		req := &provider.Request{
			Messages:     messages,
			SystemPrompt: systemPrompt,
			Tools:        tools,
			MaxTokens:    4096,
			Model:        model,
		}

		resp, err := cfg.Provider.Complete(ctx, req)
		if err != nil {
			return &SubAgentResult{
				Messages:   messages,
				Summary:    lastAssistantText,
				TotalUsage: totalUsage,
				TurnCount:  turnCount,
			}, fmt.Errorf("SubAgent.Run: API 调用失败: %w", err)
		}

		// 累加 usage
		if resp.Usage.InputTokens > 0 {
			totalUsage.InputTokens += resp.Usage.InputTokens
		}
		if resp.Usage.OutputTokens > 0 {
			totalUsage.OutputTokens += resp.Usage.OutputTokens
		}

		// 添加助手消息
		messages = append(messages, resp.Message)

		// 检查是否应该停止
		lastAssistantText = message.ExtractText(resp.Message)
		hasToolCalls := hasToolCalls(resp.Message)

		if !hasToolCalls {
			// 没有工具调用，正常结束
			break
		}

		turnCount++

		// 如果是后台 agent，在首轮后结束
		if cfg.Definition.Background && turnCount >= 1 {
			break
		}
	}

	return &SubAgentResult{
		Messages:   messages,
		Summary:    lastAssistantText,
		TotalUsage: totalUsage,
		TurnCount:  turnCount,
	}, nil
}

// RunStream 流式运行子 agent。
func (a *DefaultSubAgent) RunStream(ctx context.Context, cfg SubAgentConfig, initialPrompt string) (<-chan StreamEvent, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("SubAgent.RunStream: 需要 Provider")
	}

	// 确定使用的工具
	tools := resolveTools(cfg)

	// 确定使用的模型
	model := cfg.Definition.Model
	if model == "" || model == "inherit" {
		model = cfg.Provider.Capabilities().ModelID
	}

	// 构建初始消息
	messages := []message.Message{
		message.NewUserMessage(initialPrompt),
	}

	// 构建系统提示词
	systemPrompt := cfg.SystemPrompt
	if cfg.Definition.Prompt != "" {
		systemPrompt = cfg.Definition.Prompt
	}
	if cfg.CustomInstructions != "" {
		systemPrompt += "\n\n" + cfg.CustomInstructions
	}

	out := make(chan StreamEvent, 64)

	go func() {
		defer close(out)

		// 确定最大轮次
		maxTurns := cfg.MaxTurns
		if maxTurns == 0 && cfg.Definition.MaxTurns > 0 {
			maxTurns = cfg.Definition.MaxTurns
		}
		if maxTurns == 0 {
			maxTurns = 10
		}

		var totalUsage provider.Usage
		turnCount := 0

		for turnCount < maxTurns {
			// 检查取消
			select {
			case <-ctx.Done():
				out <- StreamEvent{Type: EventTypeDone, Error: ctx.Err()}
				return
			default:
			}

			// 调用流式 API
			req := &provider.Request{
				Messages:     messages,
				SystemPrompt: systemPrompt,
				Tools:        tools,
				MaxTokens:    4096,
				Model:        model,
			}

			stream, err := cfg.Provider.Stream(ctx, req)
			if err != nil {
				out <- StreamEvent{Type: EventTypeDone, Error: err}
				return
			}

			var currentText string
			var currentThinking string
			var toolCalls []message.ToolCall

			for ev := range stream {
				switch ev.Type {
				case provider.EventTextDelta:
					currentText += ev.Text
					out <- StreamEvent{Type: EventTypeTextDelta, Text: ev.Text}

				case provider.EventThinkingDelta:
					currentThinking += ev.Thinking
					out <- StreamEvent{Type: EventTypeThinkingDelta, Thinking: ev.Thinking}

				case provider.EventToolUseStart:
					if ev.ToolCall != nil {
						toolCalls = append(toolCalls, *ev.ToolCall)
						out <- StreamEvent{Type: EventTypeToolStart, ToolName: ev.ToolCall.Name}
					}

				case provider.EventUsage:
					if ev.Usage != nil {
						if ev.Usage.InputTokens > 0 {
							totalUsage.InputTokens += ev.Usage.InputTokens
						}
						if ev.Usage.OutputTokens > 0 {
							totalUsage.OutputTokens += ev.Usage.OutputTokens
						}
						out <- StreamEvent{Type: EventTypeUsage}
					}
				}
			}

			// 构建助手消息
			assistantMsg := message.Message{
				Role: message.RoleAssistant,
			}
			if currentThinking != "" {
				assistantMsg.Content = append(assistantMsg.Content, message.ContentBlock{
					Type:     "thinking",
					Thinking: currentThinking,
				})
			}
			if currentText != "" {
				assistantMsg.Content = append(assistantMsg.Content, message.ContentBlock{
					Type: "text",
					Text: currentText,
				})
			}
			for _, tc := range toolCalls {
				assistantMsg.Content = append(assistantMsg.Content, message.ContentBlock{
					Type:      "tool_use",
					ToolUseID: tc.ID,
					ToolName:  tc.Name,
					Input:     tc.Input,
				})
			}

			messages = append(messages, assistantMsg)

			// 检查是否应该停止
			hasToolCalls := hasToolCalls(assistantMsg)
			if !hasToolCalls {
				out <- StreamEvent{Type: EventTypeDone}
				return
			}

			turnCount++

			// 如果是后台 agent，在首轮后结束
			if cfg.Definition.Background && turnCount >= 1 {
				out <- StreamEvent{Type: EventTypeDone}
				return
			}
		}

		out <- StreamEvent{Type: EventTypeDone}
	}()

	return out, nil
}

// resolveTools 解析要使用的工具。
func resolveTools(cfg SubAgentConfig) []provider.ToolDefinition {
	if len(cfg.Tools) == 0 {
		return nil
	}

	// 如果 agent 指定了允许的工具列表，只包含这些工具
	if len(cfg.Definition.Tools) > 0 {
		allowedSet := make(map[string]bool)
		for _, t := range cfg.Definition.Tools {
			allowedSet[t] = true
		}
		// 如果 agent 指定了禁止的工具列表，排除这些工具
		if len(cfg.Definition.DisallowedTools) > 0 {
			for _, t := range cfg.Definition.DisallowedTools {
				allowedSet[t] = false
			}
		}
		result := make([]provider.ToolDefinition, 0)
		for _, t := range cfg.Tools {
			if allowedSet[t.Name] {
				result = append(result, provider.ToolDefinition{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				})
			}
		}
		return result
	}

	// 如果 agent 没有指定允许的工具列表，使用所有工具（排除禁止的）
	if len(cfg.Definition.DisallowedTools) > 0 {
		disallowedSet := make(map[string]bool)
		for _, t := range cfg.Definition.DisallowedTools {
			disallowedSet[t] = true
		}
		result := make([]provider.ToolDefinition, 0)
		for _, t := range cfg.Tools {
			if !disallowedSet[t.Name] {
				result = append(result, provider.ToolDefinition{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				})
			}
		}
		return result
	}

	// 全部工具
	result := make([]provider.ToolDefinition, len(cfg.Tools))
	for i, t := range cfg.Tools {
		result[i] = provider.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return result
}

// hasToolCalls 检查消息是否包含工具调用。
func hasToolCalls(msg message.Message) bool {
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// BuiltInAgents 内置的 Agent 定义。
// 对齐 Claude Code 的 builtInAgents。
var BuiltInAgents = map[string]*AgentDefinition{
	"general-purpose": {
		AgentType:   "general-purpose",
		Description: "通用助手，可以帮助你完成各种任务",
		Prompt:      "你是一个通用的 AI 助手。",
		MaxTurns:    0, // 无限制
		Background:  false,
	},
}

// LoadAgentsFromDir 从目录加载 Agent 定义。
// 对齐 Claude Code 的 loadAgentsDir。
func LoadAgentsFromDir(dir string) (map[string]*AgentDefinition, error) {
	// TODO: 实现从目录加载 agent 定义
	// 目前返回内置 agents
	result := make(map[string]*AgentDefinition)
	for k, v := range BuiltInAgents {
		result[k] = v
	}
	return result, nil
}

// GetAgent 获取指定类型的 Agent 定义。
func GetAgent(agentType string) *AgentDefinition {
	if def, ok := BuiltInAgents[agentType]; ok {
		return def
	}
	return nil
}

// SubAgentManager 管理子 agents。
type SubAgentManager struct {
	mu     sync.RWMutex
	agents map[string]*AgentDefinition
	active map[string]*SubAgentResult // 当前运行的 agents
	lock   sync.Mutex
}

// NewSubAgentManager 创建 SubAgentManager。
func NewSubAgentManager() *SubAgentManager {
	return &SubAgentManager{
		agents: make(map[string]*AgentDefinition),
		active: make(map[string]*SubAgentResult),
	}
}

// RegisterAgent 注册一个 agent 定义。
func (m *SubAgentManager) RegisterAgent(def *AgentDefinition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents[def.AgentType] = def
}

// GetAgent 获取 agent 定义。
func (m *SubAgentManager) GetAgent(agentType string) *AgentDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agents[agentType]
}

// ListAgents 列出所有注册的 agent。
func (m *SubAgentManager) ListAgents() []*AgentDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*AgentDefinition, 0, len(m.agents))
	for _, def := range m.agents {
		result = append(result, def)
	}
	return result
}
