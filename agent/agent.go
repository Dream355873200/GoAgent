// Package agent 实现子 agent 系统。
//
// 子 agent 是独立运行的 agent 循环实例，拥有自己的系统提示、
// 工具集和最大轮次限制。主 agent 可以通过工具调用启动子 agent。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
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

	// Model 是子 agent 使用的模型（可选，默认使用 provider 当前模型）。
	Model string

	// MaxTokens 单次响应输出上限（可选）。0 = 不指定，交给 provider 决定
	// （与主循环一致；推理模型的思考 token 也计入输出，硬限过小会截断）。
	MaxTokens int

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

// Progress 是子 agent 运行中的进度快照（每次工具启动、每轮结束时上报）。
type Progress struct {
	// Turn 是当前轮次（从 1 开始）。
	Turn int
	// ToolUses 是累计工具调用次数。
	ToolUses int
	// Tokens 是累计 token（输入+输出）。
	Tokens int
	// Activity 是一行活动描述（如 "Read main.go"、"思考: …"）。
	Activity string
}

// AgentEvent 是子 agent 运行的细粒度事件（宿主据此记录完整轨迹，
// 渲染只读工作过程时间线——对齐主 agent 的会话回放）。
type AgentEvent struct {
	Type   string // text | thinking | tool_start | tool_done | error
	Text   string // text/thinking/error 内容
	Tool   string // tool_start/tool_done 的工具名
	Input  string // tool_start 的入参 JSON（截断）
	Result string // tool_done 的结果文本（截断）
}

// Runner 执行子 agent。
type Runner struct {
	// defaultProvider 是没有指定 provider 时使用的默认 provider。
	defaultProvider provider.Provider

	// onProgress 进度回调（可选）；Runner 内部加锁串行调用。
	onProgress func(Progress)

	// onEvent 细粒度事件回调（可选）；与 onProgress 同锁串行调用。
	onEvent func(AgentEvent)

	progMu sync.Mutex // 串行化进度/事件回调
}

// NewRunner 创建一个新的子 agent 运行器。
func NewRunner(defaultProvider provider.Provider) *Runner {
	return &Runner{defaultProvider: defaultProvider}
}

// OnProgress 设置进度回调，返回 Runner 本身便于链式调用。
func (r *Runner) OnProgress(fn func(Progress)) *Runner {
	r.onProgress = fn
	return r
}

// OnEvent 设置细粒度事件回调（文本/思考/工具调用/错误），返回 Runner 链式调用。
func (r *Runner) OnEvent(fn func(AgentEvent)) *Runner {
	r.onEvent = fn
	return r
}

// emit 细粒度事件出口（progMu 与进度上报同锁，保证轨迹顺序稳定）。
func (r *Runner) emit(ev AgentEvent) {
	if r.onEvent == nil {
		return
	}
	r.progMu.Lock()
	defer r.progMu.Unlock()
	r.onEvent(ev)
}

// Run 执行子 agent 并返回结果。
// 这是一个隔离的 agent 循环，不共享主循环的消息历史。模型响应走流式
// （长生成不受响应头超时限制），同一轮的多个工具调用并行执行。
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

	toolUses := 0
	report := func(activity string) {
		if r.onProgress == nil {
			return
		}
		r.progMu.Lock()
		defer r.progMu.Unlock()
		r.onProgress(Progress{
			Turn:     turnCount + 1,
			ToolUses: toolUses,
			Tokens:   totalUsage.InputTokens + totalUsage.OutputTokens,
			Activity: activity,
		})
	}

	// 子 agent 循环。
	for turnCount < maxTurns {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// 调用 API。
		resp, err := streamTurn(ctx, prov, &provider.Request{
			Messages:     messages,
			SystemPrompt: def.SystemPrompt,
			Tools:        toolDefs,
			Model:        def.Model,
			MaxTokens:    def.MaxTokens,
		})
		if err != nil {
			return nil, fmt.Errorf("子 agent %q API 调用失败: %w", def.Name, err)
		}

		// 累计 token 使用。
		r.progMu.Lock()
		totalUsage.InputTokens += resp.Usage.InputTokens
		totalUsage.OutputTokens += resp.Usage.OutputTokens
		r.progMu.Unlock()

		// 添加助手消息。
		messages = append(messages, resp.Message)

		// 提取工具调用。
		toolCalls := message.ExtractToolCalls(resp.Message)
		if len(toolCalls) == 0 {
			// 无工具调用，子 agent 完成。
			r.emit(AgentEvent{Type: "text", Text: message.ExtractText(resp.Message)})
			return &RunResult{
				FinalText: message.ExtractText(resp.Message),
				Messages:  messages,
				TurnCount: turnCount + 1,
				Usage:     totalUsage,
			}, nil
		}
		if text := strings.TrimSpace(message.ExtractText(resp.Message)); text != "" {
			report(firstLine(text))
		}
		// 轨迹：本条的思考与文本（assistant 消息随流式到达后落定）
		for _, c := range resp.Message.Content {
			switch c.Type {
			case "thinking":
				if c.Thinking != "" {
					r.emit(AgentEvent{Type: "thinking", Text: c.Thinking})
				}
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					r.emit(AgentEvent{Type: "text", Text: c.Text})
				}
			}
		}
		for _, tc := range toolCalls {
			r.emit(AgentEvent{Type: "tool_start", Tool: tc.Name, Input: previewRunese(string(tc.Input), 800)})
		}

		// 并行执行本轮工具调用，结果按调用顺序回填。
		results := make([]message.Message, len(toolCalls))
		var wg sync.WaitGroup
		for i, tc := range toolCalls {
			tool := toolIndex[tc.Name]
			if tool == nil {
				results[i] = message.NewToolResultMessage(tc.ID, fmt.Sprintf("未知工具: %s", tc.Name), true)
				continue
			}
			r.progMu.Lock()
			toolUses++
			r.progMu.Unlock()
			report(ToolActivity(tc.Name, tc.Input))
			wg.Add(1)
			go func(i int, tc message.ToolCall) {
				defer wg.Done()
				result, toolErr := tool.Execute(ctx, tc.Input)
				isError := toolErr != nil
				if isError {
					result = "错误: " + toolErr.Error()
				}
				r.emit(AgentEvent{Type: "tool_done", Tool: tc.Name, Result: previewRunese(result, 2000)})
				results[i] = message.NewToolResultMessage(tc.ID, result, isError)
			}(i, tc)
		}
		wg.Wait()
		messages = append(messages, results...)

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

// previewRunese 截断长文本（轨迹与事件里的入参/结果预览）。
func previewRunese(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
