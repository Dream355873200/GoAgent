// Package agent 实现子 agent 系统。
//
// 子 agent 是独立运行的 agent 循环实例，拥有自己的系统提示、
// 工具集和最大轮次限制。主 agent 可以通过工具调用启动子 agent。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/permission"
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

	// Permission 是工具的权限级别（零值为只读）。子 agent 在某次 agent 运行
	// 之中被调用时，工具调用按这个级别过该运行的检查链。
	Permission permission.Level
	// Serial 为 true 时，该工具的调用不与同一轮的其他工具调用并行执行
	// （默认并行）。
	Serial bool
}

// RunResult 是子 agent 运行的结果。
type RunResult struct {
	// FinalText 是子 agent 的最终文本输出。
	FinalText string

	// Messages 是子 agent 的完整消息历史。
	Messages []message.Message

	// TurnCount 是子 agent 运行的轮次数（模型响应次数）。
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

func (r *Runner) report(p Progress) {
	if r.onProgress == nil {
		return
	}
	r.progMu.Lock()
	defer r.progMu.Unlock()
	r.onProgress(p)
}

// Run 执行子 agent 并返回结果。
//
// 子 agent 与主循环使用同一个循环实现：独立的消息历史与系统提示，模型响应
// 走流式，同一轮的工具调用按各自的并发声明并行执行（并发有上限），工具
// panic 被恢复为错误结果，上下文接近窗口上限时自动压缩，输出被 max_tokens
// 截断时自动续写。达到最大轮次不算失败，返回已有的结果。
//
// 在某次 agent 运行之中调用时（ctx 来自该运行的工具执行），子 agent 的工具
// 调用继承该运行的检查链——plan 模式、中间件、hooks 与权限门，按
// ToolDef.Permission 检查——以及它的观测出口；不在任何运行之内时不做检查。
// Definition.Provider 与 NewRunner 的默认 provider 都为空时，沿用所在运行的
// provider。
func (r *Runner) Run(ctx context.Context, def Definition, input string) (*RunResult, error) {
	prov := def.Provider
	if prov == nil {
		prov = r.defaultProvider
	}
	if prov == nil {
		if s := loop.ScopeFrom(ctx); s != nil {
			prov = s.Provider
		}
	}
	if prov == nil {
		return nil, fmt.Errorf("子 agent %q 没有可用的 provider", def.Name)
	}

	maxTurns := def.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	tools := make([]loop.ToolEntry, 0, len(def.Tools))
	for _, t := range def.Tools {
		tools = append(tools, loop.ToolEntry{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Permission:  t.Permission,
			Concurrent:  !t.Serial,
			ExecuteFn:   t.Execute,
		})
	}

	// 微压缩裁剪过期的工具结果，自动摘要在 0.8 水位触发——长任务的子 agent
	// 不会因上下文撑满而失败。
	comp := compaction.NewManager(compaction.Config{
		AutoCompactThreshold: 0.8,
		Layers:               []compaction.Layer{compaction.LayerMicro, compaction.LayerAuto},
	})
	comp.SetProvider(prov)

	l := loop.NewChild(ctx, loop.Config{
		Provider:     prov,
		Tools:        tools,
		SystemPrompt: def.SystemPrompt,
		MaxTurns:     maxTurns,
		Model:        def.Model,
		MaxTokens:    def.MaxTokens,
		Compaction:   comp,
	})

	var (
		usage          provider.Usage
		turn           = 1
		toolUses       int
		anyOutput      bool
		lastErr        error
		text, thinking strings.Builder
		turnText       strings.Builder // 本轮助手文本，首个工具启动时作为活动描述上报
		turnReported   bool
	)
	progress := func(activity string) {
		r.report(Progress{Turn: turn, ToolUses: toolUses, Tokens: usage.InputTokens + usage.OutputTokens, Activity: activity})
	}
	flush := func() {
		if thinking.Len() > 0 {
			r.emit(AgentEvent{Type: "thinking", Text: thinking.String()})
			thinking.Reset()
		}
		if s := text.String(); strings.TrimSpace(s) != "" {
			r.emit(AgentEvent{Type: "text", Text: s})
		}
		text.Reset()
	}

	for ev := range l.Run(ctx, input) {
		switch ev.Type {
		case loop.EvtThinking:
			anyOutput = true
			thinking.WriteString(ev.Thinking)
		case loop.EvtTextDelta:
			anyOutput = true
			text.WriteString(ev.Text)
			turnText.WriteString(ev.Text)
		case loop.EvtToolStart:
			anyOutput = true
			if !turnReported {
				turnReported = true
				if t := strings.TrimSpace(turnText.String()); t != "" {
					progress(firstLine(t))
				}
			}
			flush()
			toolUses++
			r.emit(AgentEvent{Type: "tool_start", Tool: ev.ToolName, Input: preview(string(ev.ToolInput), 800)})
			progress(ToolActivity(ev.ToolName, ev.ToolInput))
		case loop.EvtToolDone:
			r.emit(AgentEvent{Type: "tool_done", Tool: ev.ToolName, Result: preview(ev.ToolResult, 2000)})
		case loop.EvtUsageUpdate:
			if ev.Usage != nil {
				usage.InputTokens += ev.Usage.InputTokens
				usage.OutputTokens += ev.Usage.OutputTokens
				usage.CacheReadTokens += ev.Usage.CacheReadTokens
				usage.CacheCreateTokens += ev.Usage.CacheCreateTokens
			}
		case loop.EvtTurnComplete:
			flush()
			turn++
			turnText.Reset()
			turnReported = false
		case loop.EvtError:
			flush()
			lastErr = ev.Error
			r.emit(AgentEvent{Type: "error", Text: ev.Error.Error()})
		case loop.EvtSubAgentProgress:
			// 子 agent 工具里再派生的子 agent：进度转发到所在运行的事件流。
			loop.EmitFromTool(ctx, ev)
		case loop.EvtProgress:
			// 工具经 Context.Progress 上报的状态行同样转发；循环自身的状态
			// 行（限流重试等）只属于子 agent，不转发。
			if strings.HasPrefix(ev.StatusKey, "tool-progress") {
				loop.EmitFromTool(ctx, ev)
			}
		}
	}
	flush()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msgs := l.FinalMessages()
	if lastErr != nil && !errors.Is(lastErr, loop.ErrMaxTurns) {
		return nil, fmt.Errorf("子 agent %q 运行失败: %w", def.Name, lastErr)
	}
	if lastErr == nil && !anyOutput {
		return nil, fmt.Errorf("子 agent %q 的模型没有返回任何内容", def.Name)
	}

	finalText := ""
	turns := 0
	for _, m := range msgs {
		if m.Role == message.RoleAssistant {
			turns++
		}
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == message.RoleAssistant {
		finalText = message.ExtractText(msgs[n-1])
	}
	return &RunResult{
		FinalText: finalText,
		Messages:  msgs,
		TurnCount: turns,
		Usage:     usage,
	}, nil
}

// preview 截断长文本（轨迹与事件里的入参/结果预览），不切坏多字节字符。
func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
