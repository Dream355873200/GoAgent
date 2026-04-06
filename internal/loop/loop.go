// Package loop 实现核心 agent 状态机。
//
// 这是框架的核心，源自 Claude Code 的 query.ts。
// 每次迭代：预处理 → 压缩 → 调用 API → 流式消费+工具执行 →
// 检查退出/恢复 → 后处理。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dream355873200/GoAgent/budget"
	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

// Config 包含循环运行所需的一切。
type Config struct {
	Provider        provider.Provider
	Fallback        provider.Provider // 可选的后备模型
	Tools           []ToolEntry
	Middlewares     []Middleware
	SystemPrompt    string
	MaxTurns        int
	Compaction      *compaction.Manager
	Permission      *permission.Gate
	Memory          *memory.Manager
	Executor        *executor.Executor
	StopHooks       *StopHookRunner   // stop hook 运行器
	Budget          *budget.Tracker   // token 预算追踪器
	Hooks           HooksRunner       // 外部 hooks 管理器（可选）
	PlanChecker     PlanChecker       // plan mode 工具过滤（可选）
	PostSampling    PostSamplingHook  // 采样后钩子（SessionMemory 等）
	InitialMessages []message.Message // 历史消息（多轮对话恢复用）
	// Observer 接收循环中的可观测性事件（Token 用量、工具执行、权限决策等）。
	// 可传入 observer.MultiObserver 广播给多个观察者。
	// 如果为 nil，则不发送任何可观测性事件。
	Observer observer.Observer
	// SessionID 是当前会话的标识符，用于 Observer 事件的上下文。
	// 如果为空字符串，Observer 事件中的 SessionID 字段为空。
	SessionID string
}

// PlanChecker 是 plan mode 的接口。
// 由 goagent.go 层将 plan.Manager 适配为此接口。
type PlanChecker interface {
	// IsActive 返回是否处于 plan mode。
	IsActive() bool
	// IsToolAllowed 检查指定权限级别的工具是否在 plan mode 下允许。
	// level 是 permission.Level 值：0=ReadOnly, 1=Normal, 2=RequireApproval, 3=Dangerous。
	IsToolAllowed(level permission.Level) bool
}

// HooksRunner 是外部 hooks 系统的接口。
// 由 goagent.go 层将 hooks.Manager 适配为此接口。
type HooksRunner interface {
	// RunPreToolUse 在工具执行前调用。返回 block=true 时阻止执行。
	RunPreToolUse(ctx context.Context, toolName string, input json.RawMessage) (block bool, message string, err error)
	// RunPostToolUse 在工具执行后调用。
	RunPostToolUse(ctx context.Context, toolName string, input json.RawMessage, result string, toolErr error)
	// RunStop 在循环退出前调用。返回 block=true 时阻止退出。
	RunStop(ctx context.Context) (block bool, message string, err error)
}

// PostSamplingHook 是采样后（API 响应+工具执行后）调用的钩子。
// 对齐 Claude Code 的 postSamplingHooks 机制。
// 主要用于 SessionMemory 的定期提取。
type PostSamplingHook interface {
	// AfterSampling 在每轮 API 响应+工具执行后调用。
	// 实现应非阻塞（如需异步操作应在内部启动 goroutine）。
	AfterSampling(ctx context.Context, messages []message.Message)
}

// ToolEntry 是循环看到的已注册工具。
type ToolEntry struct {
	Name        string
	Description string
	InputSchema any
	Permission  permission.Level
	Concurrent  bool
	ExecuteFn   func(ctx context.Context, input json.RawMessage) (string, error)

	// InterruptBehavior 中断行为："cancel" 或 "block"。默认 "cancel"。
	InterruptBehavior  string
	MaxResultSizeChars int
}

// Middleware 是循环内部的中间件接口。
type Middleware interface {
	BeforeTool(ctx context.Context, toolName string, input json.RawMessage) *Decision
	AfterTool(ctx context.Context, toolName string, result string, err error)
}

// Decision 来自中间件的决定。
type Decision struct {
	Allow  bool
	Reason string
}

// Event 由循环发射给外层。
type Event struct {
	Type       int
	Text       string
	Thinking   string // thinking 内容增量
	ToolName   string
	ToolInput  json.RawMessage
	ToolUseID  string // 对齐 Claude Code ToolUseBlock.id
	ToolResult string
	Usage      *provider.Usage
	Error      error
}

// 事件类型（匹配公共 EventType）。
const (
	EvtTextDelta    = 0
	EvtThinking     = 1
	EvtToolStart    = 2
	EvtToolDone     = 3
	EvtNeedApproval = 4
	EvtUsageUpdate  = 5
	EvtTurnComplete = 6
	EvtDone         = 7
	EvtError        = 8
	EvtProgress     = 9
	EvtCompaction   = 10
)

// Transition 描述循环继续到下一次迭代的原因。
type Transition int

const (
	TransNextTurn                Transition = iota // 正常：工具调用已处理
	TransCollapseDrainRetry                        // 上下文折叠释放了空间
	TransReactiveCompactRetry                      // 413 后的响应式压缩
	TransMaxOutputEscalate                         // 8k → 64k 重试
	TransMaxOutputRecovery                         // 注入继续提示
	TransStopHookBlocking                          // stop hook 需要修正
	TransTokenBudgetContinuation                   // token 预算未耗尽
)

// loopState 是跨迭代携带的可变状态。
type loopState struct {
	messages                    []message.Message
	turnCount                   int
	transition                  *Transition
	maxOutputRecoveryCount      int
	maxOutputTokensOverride     int
	hasAttemptedReactiveCompact bool
	usingFallback               bool
	// lastInputTokens 记录上一轮 API 返回的实际 input tokens，
	// 用于替代客户端估算做 token 阻断判断。对齐 Claude Code 的
	// finalContextTokensFromLastResponse()。
	lastInputTokens int
	// toolStartTimes 记录每个工具调用的开始时间，用于计算执行时长。
	toolStartTimes map[string]time.Time
}

// Loop 是核心 agent 状态机。
type Loop struct {
	cfg           Config
	toolIndex     map[string]*ToolEntry
	finalMessages []message.Message // 循环结束后的完整消息列表
}

// New 创建一个新的 Loop。
func New(cfg Config) *Loop {
	idx := make(map[string]*ToolEntry, len(cfg.Tools))
	for i := range cfg.Tools {
		idx[cfg.Tools[i].Name] = &cfg.Tools[i]
	}
	return &Loop{cfg: cfg, toolIndex: idx}
}

// Run 启动 agent 循环并返回一个事件通道。
func (l *Loop) Run(ctx context.Context, input string) <-chan Event {
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		l.run(ctx, input, out)
	}()
	return out
}

// FinalMessages 返回循环结束后的完整消息列表。
// 必须在 Run() 返回的 channel 消费完毕后调用。
// 用于会话持久化。
func (l *Loop) FinalMessages() []message.Message {
	return l.finalMessages
}

func (l *Loop) run(ctx context.Context, input string, out chan<- Event) {
	// P3: Memory Prefetch 异步化。
	// 对齐 Claude Code 的 startRelevantMemoryPrefetch()：
	// 在循环入口异步启动，与后续初始化/压缩并行进行。
	var memSuffix string
	memDone := make(chan struct{})
	go func() {
		defer close(memDone)
		if l.cfg.Memory != nil {
			memSuffix = l.cfg.Memory.BuildSystemPromptSuffix()
		}
	}()

	// 初始化状态。
	var initialMessages []message.Message
	if len(l.cfg.InitialMessages) > 0 {
		// 多轮对话：使用历史消息 + 新的用户输入。
		initialMessages = make([]message.Message, len(l.cfg.InitialMessages))
		copy(initialMessages, l.cfg.InitialMessages)
		initialMessages = append(initialMessages, message.NewUserMessage(input))
	} else {
		// 新会话：只有用户输入。
		initialMessages = []message.Message{message.NewUserMessage(input)}
	}

	state := &loopState{
		messages:       initialMessages,
		toolStartTimes: make(map[string]time.Time),
	}

	// 通知 Observer 会话开始。
	if l.cfg.Observer != nil {
		l.cfg.Observer.OnSessionStart(ctx, l.cfg.SessionID)
	}

	// 循环结束时保存最终消息列表，并通知 Observer 会话结束。
	defer func() {
		l.finalMessages = state.messages
		if l.cfg.Observer != nil {
			l.cfg.Observer.OnSessionEnd(ctx, l.cfg.SessionID, state.turnCount)
		}
	}()

	activeProvider := l.cfg.Provider

	// P1a: 工具定义缓存 — 一次性构建，跨迭代复用。
	// 对齐 Claude Code 的 toolUseContext.options.tools（不在循环内重建）。
	caps := activeProvider.Capabilities()
	var toolDefs []provider.ToolDefinition
	if caps.SupportsTools {
		toolDefs = make([]provider.ToolDefinition, 0, len(l.cfg.Tools))
		for _, t := range l.cfg.Tools {
			toolDefs = append(toolDefs, provider.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
		// Prompt Caching: 给最后一个 tool 加 cache_control 标记。
		// Anthropic 缓存从头到该标记为止的全部前缀（system + tools）。
		if caps.SupportsCaching && len(toolDefs) > 0 {
			toolDefs[len(toolDefs)-1].CacheControl = &provider.CacheControl{Type: "ephemeral"}
		}
	}

	// 创建 Withholder 和预处理器。
	withholder := NewWithholder()

	// P3: 等待 memory 加载完成，构建 system prompt。
	<-memDone
	systemPrompt := l.cfg.SystemPrompt
	if memSuffix != "" {
		systemPrompt += "\n\n" + memSuffix
	}

	// ====== 主循环 ======
	for {
		// 检查最大轮次。
		if state.turnCount >= l.cfg.MaxTurns {
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, fmt.Errorf("已达最大轮次 (%d)", l.cfg.MaxTurns))
			}
			out <- Event{Type: EvtError, Error: fmt.Errorf("已达最大轮次 (%d)", l.cfg.MaxTurns)}
			return
		}

		// 检查上下文取消。
		if ctx.Err() != nil {
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, ctx.Err())
			}
			out <- Event{Type: EvtError, Error: ctx.Err()}
			return
		}

		// ① 预处理：四层压缩。
		contextWindow := 200_000 // 默认值，应来自 provider 能力
		if caps := activeProvider.Capabilities(); caps.ContextWindow > 0 {
			contextWindow = caps.ContextWindow
		}

		// P2: 压缩只对 boundary 后消息运行，对齐 Claude Code 的
		// getMessagesAfterCompactBoundary()。
		if l.cfg.Compaction != nil {
			compacted, freed := l.cfg.Compaction.Apply(ctx, state.messages, contextWindow)
			if freed > 0 {
				state.messages = compacted
				out <- Event{Type: EvtCompaction, Text: fmt.Sprintf("已压缩，释放 ~%d tokens", freed)}
				// Observer: 通知压缩事件。
				if l.cfg.Observer != nil {
					l.cfg.Observer.OnCompaction(ctx, freed, "auto_compact")
				}
			}
		}

		// ② Token 阻断检查。
		// P1b: 优先使用 API 返回的实际 input tokens（非首轮时可用），
		// 对齐 Claude Code 的 tokenCountWithEstimation()。
		var totalTokens int
		if state.lastInputTokens > 0 {
			// 非首轮：使用上一轮 API 返回的实际值（更准确）。
			totalTokens = state.lastInputTokens
		} else {
			// 首轮：无 API 数据，用客户端估算。
			totalTokens = message.EstimateMessagesTokens(state.messages)
		}
		hardLimit := contextWindow - 3000
		if totalTokens > hardLimit && l.cfg.Compaction == nil {
			err := fmt.Errorf("上下文已耗尽: %d tokens > %d 限制", totalTokens, hardLimit)
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, err)
			}
			out <- Event{Type: EvtError, Error: err}
			return
		}

		// ③ 工具定义已在循环外缓存（P1a），此处直接使用 toolDefs。

		// ④ 调用 API（流式）。
		maxTokens := 4096
		if state.maxOutputTokensOverride > 0 {
			maxTokens = state.maxOutputTokensOverride
		}

		// 构建请求：优先使用 SystemBlocks（带 cache_control），否则回退到 SystemPrompt。
		req := &provider.Request{
			Messages:  state.messages,
			Tools:     toolDefs,
			MaxTokens: maxTokens,
		}
		if caps.SupportsCaching {
			// 将 system prompt 包装为 SystemBlocks，最后一个 block 标记缓存。
			// 如果有 tools，tools 的最后一个已标记缓存，system prompt 不需要重复标记。
			// 如果没有 tools，system prompt 最后一个 block 标记缓存。
			req.SystemBlocks = []provider.SystemBlock{
				{
					Text:         systemPrompt,
					CacheControl: cacheControlForSystemPrompt(caps.SupportsCaching, len(toolDefs)),
				},
			}
		} else {
			req.SystemPrompt = systemPrompt
		}

		stream, err := activeProvider.Stream(ctx, req)

		if err != nil {
			// 处理过载 → 后备。
			var overloadErr *provider.OverloadError
			if errors.As(err, &overloadErr) && l.cfg.Fallback != nil && !state.usingFallback {
				activeProvider = l.cfg.Fallback
				state.usingFallback = true
				out <- Event{Type: EvtProgress, Text: "主模型过载，切换到后备模型"}
				continue
			}
			// 处理 prompt 过长 → 响应式压缩。
			var ptlErr *provider.PromptTooLongError
			if errors.As(err, &ptlErr) && l.cfg.Compaction != nil && !state.hasAttemptedReactiveCompact {
				recovered, ok := l.cfg.Compaction.HandleOverflow(ctx, state.messages, contextWindow)
				if ok {
					state.messages = recovered
					state.hasAttemptedReactiveCompact = true
					t := TransReactiveCompactRetry
					state.transition = &t
					continue
				}
			}

			out <- Event{Type: EvtError, Error: fmt.Errorf("API 错误: %w", err)}
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, fmt.Errorf("API 错误: %w", err))
			}
			return
		}

		// ⑤ 消费流 + 流式工具执行。
		// P0: 所有工具（包括需要权限的）都进入 StreamingExecutor，
		// 对齐 Claude Code 的 StreamingToolExecutor.addTool()。
		// 权限检查通过 PreCheck 闭包在执行前完成。
		streamingExec := executor.NewStreamingExecutor(l.cfg.Executor)
		var assistantText string
		var thinkingText string
		var toolCalls []message.ToolCall
		var stopReason provider.StopReason

		// 重置 withholder。
		withholder.Clear()

		for ev := range stream {
			// 检查是否需要暂扣此事件。
			if reason := withholder.ShouldWithhold(ev); reason != WithholdNone {
				withholder.Withhold(ev, reason)
				continue
			}

			switch ev.Type {
			case provider.EventTextDelta:
				assistantText += ev.Text
				out <- Event{Type: EvtTextDelta, Text: ev.Text}

			case provider.EventThinkingDelta:
				thinkingText += ev.Thinking
				out <- Event{Type: EvtThinking, Thinking: ev.Thinking}

			case provider.EventToolUseStart:
				if ev.ToolCall != nil {
					toolCalls = append(toolCalls, *ev.ToolCall)
					out <- Event{Type: EvtToolStart, ToolName: ev.ToolCall.Name, ToolInput: ev.ToolCall.Input, ToolUseID: ev.ToolCall.ID}

					// Observer: 通知工具开始执行。
					if l.cfg.Observer != nil {
						l.cfg.Observer.OnToolStart(ctx, ev.ToolCall.Name, ev.ToolCall.Input)
					}
					// 记录开始时间用于计算执行时长。
					state.toolStartTimes[ev.ToolCall.ID] = time.Now()

					// P0: 为所有工具构建 ToolCall（含 PreCheck 闭包），
					// 统一交给 StreamingExecutor 管理。
					tc := *ev.ToolCall
					tool := l.toolIndex[tc.Name]
					if tool == nil {
						// 未知工具 — 直接生成错误结果。
						streamingExec.Add(ctx, executor.ToolCall{
							ID:         tc.ID,
							Name:       tc.Name,
							Input:      tc.Input,
							Concurrent: true,
							Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
								return fmt.Sprintf("错误: 未知工具 %q", tc.Name), fmt.Errorf("未知工具")
							},
						})
					} else {
						concurrent := tool.Concurrent

						// 构建 PreCheck 闭包：包含完整的权限检查链。
						// 对齐 Claude Code StreamingToolExecutor.executeTool() 的内置权限检查。
						// 更新 transcript 供 YOLO 分类器使用。
						if l.cfg.Permission != nil {
							l.cfg.Permission.SetTranscript(state.messages)
						}
						preCheck := l.buildPreCheck(tool, tc.Name, tc.Input)

						streamingExec.Add(ctx, executor.ToolCall{
							ID:         tc.ID,
							Name:       tc.Name,
							Input:      tc.Input,
							Concurrent: concurrent,
							Execute:    tool.ExecuteFn,
							PreCheck:   preCheck,
						})
					}
				}

			case provider.EventUsage:
				if ev.Usage != nil {
					out <- Event{Type: EvtUsageUpdate, Usage: ev.Usage}
					// P1b: 记录 API 返回的实际 input tokens。
					if ev.Usage.InputTokens > 0 {
						state.lastInputTokens = ev.Usage.InputTokens
					}
					// 记录到 budget tracker。
					if l.cfg.Budget != nil {
						l.cfg.Budget.RecordUsage(state.turnCount, ev.Usage.InputTokens, ev.Usage.OutputTokens)
					}
					// Observer: 通知 Token 用量。
					if l.cfg.Observer != nil {
						costUSD := estimateCostUSD(activeProvider.Capabilities().ModelID, ev.Usage)
						l.cfg.Observer.OnTokenUsage(ctx, activeProvider.Capabilities().ModelID, ev.Usage, costUSD)
					}
				}

			case provider.EventMessageComplete:
				if ev.StopReason != "" {
					stopReason = ev.StopReason
				}

			case provider.EventError:
				out <- Event{Type: EvtError, Error: ev.Error}
				if l.cfg.Observer != nil {
					l.cfg.Observer.OnError(ctx, ev.Error)
				}
				return
			}

			// 轮询已完成的流式工具。
			for _, result := range streamingExec.Poll() {
				out <- Event{Type: EvtToolDone, ToolName: result.Name, ToolResult: result.Content, ToolUseID: result.ToolUseID}
				// Observer: 通知工具执行完成。
				if l.cfg.Observer != nil {
					var duration time.Duration
					if startTime, ok := state.toolStartTimes[result.ToolUseID]; ok {
						duration = time.Since(startTime)
						delete(state.toolStartTimes, result.ToolUseID)
					}
					if result.IsError {
						l.cfg.Observer.OnToolError(ctx, result.Name, nil, fmt.Errorf("%s", result.Content), duration)
					} else {
						l.cfg.Observer.OnToolDone(ctx, result.Name, nil, result.Content, duration)
					}
				}
			}
		}

		// ⑥ 中止检查 — 生成合成 tool_result 维持 API 消息对齐。
		if ctx.Err() != nil {
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, ctx.Err())
			}
			streamingExec.Discard()
			// 为未完成的工具调用生成合成结果。
			if len(toolCalls) > 0 {
				syntheticMsg := message.Message{Role: message.RoleAssistant}
				if assistantText != "" {
					syntheticMsg.Content = append(syntheticMsg.Content, message.ContentBlock{
						Type: "text",
						Text: assistantText,
					})
				}
				for _, tc := range toolCalls {
					syntheticMsg.Content = append(syntheticMsg.Content, message.ContentBlock{
						Type:      "tool_use",
						ToolUseID: tc.ID,
						ToolName:  tc.Name,
						Input:     tc.Input,
					})
				}
				state.messages = append(state.messages, syntheticMsg)
				// 为每个 tool_use 添加合成 tool_result。
				for _, tc := range toolCalls {
					state.messages = append(state.messages,
						message.NewToolResultMessage(tc.ID, "工具执行被用户中断", true),
					)
				}
			}
			out <- Event{Type: EvtError, Error: ctx.Err()}
			return
		}

		// 构建助手消息。
		assistantMsg := message.Message{
			Role: message.RoleAssistant,
		}
		// Thinking 内容放在最前面（对应 Claude API 的 thinking block 顺序）。
		if thinkingText != "" {
			assistantMsg.Content = append(assistantMsg.Content, message.ContentBlock{
				Type:     "thinking",
				Thinking: thinkingText,
			})
		}
		if assistantText != "" {
			assistantMsg.Content = append(assistantMsg.Content, message.ContentBlock{
				Type: "text",
				Text: assistantText,
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
		state.messages = append(state.messages, assistantMsg)

		// ⑦ 无工具调用 → 退出判断。
		if len(toolCalls) == 0 {
			// 检查 max_output_tokens 恢复。
			if stopReason == provider.StopMaxTokens {
				// 第一次尝试：提升 4k → 64k。
				if state.maxOutputTokensOverride == 0 {
					state.maxOutputTokensOverride = 64_000
					t := TransMaxOutputEscalate
					state.transition = &t
					out <- Event{Type: EvtProgress, Text: "输出被截断，提升最大 tokens"}
					continue
				}
				// 第二次尝试：注入继续提示（最多 3 次）。
				if state.maxOutputRecoveryCount < 3 {
					recoveryMsg := message.NewMetaMessage(
						"Output token limit hit. Resume directly — no apology, " +
							"no recap. Pick up mid-thought. Break remaining work into smaller pieces.",
					)
					state.messages = append(state.messages, recoveryMsg)
					state.maxOutputRecoveryCount++
					t := TransMaxOutputRecovery
					state.transition = &t
					out <- Event{Type: EvtProgress, Text: fmt.Sprintf("输出被截断，恢复尝试 %d/3", state.maxOutputRecoveryCount)}
					continue
				}
			}

			// 运行 Stop Hooks。
			if l.cfg.StopHooks != nil && l.cfg.StopHooks.HasHooks() {
				hookResult := l.cfg.StopHooks.RunStopHooks(ctx, state.messages, assistantText)
				if hookResult != nil && hookResult.Block {
					// Hook 阻止了退出，注入修正消息继续循环。
					correctionMsg := message.NewMetaMessage(hookResult.Message)
					state.messages = append(state.messages, correctionMsg)
					t := TransStopHookBlocking
					state.transition = &t
					out <- Event{Type: EvtProgress, Text: fmt.Sprintf("stop hook 阻止退出: %s", hookResult.Reason)}
					continue
				}
			}

			// 外部 Hooks RunStop（与 StopHooks 互补，覆盖更通用的退出拦截场景）。
			if l.cfg.Hooks != nil {
				block, msg, err := l.cfg.Hooks.RunStop(ctx)
				if err != nil {
					out <- Event{Type: EvtError, Error: fmt.Errorf("stop hook 错误: %w", err)}
					return
				}
				if block {
					correctionMsg := message.NewMetaMessage(msg)
					state.messages = append(state.messages, correctionMsg)
					t := TransStopHookBlocking
					state.transition = &t
					out <- Event{Type: EvtProgress, Text: fmt.Sprintf("外部 hook 阻止退出: %s", msg)}
					continue
				}
			}

			// 正常完成。
			out <- Event{Type: EvtDone}
			return
		}

		// ⑧ 有工具调用 → 等待所有工具完成。
		// P0: 所有工具（含权限检查）已在步骤⑤统一交给 StreamingExecutor，
		// 此处只需等待剩余结果。对齐 Claude Code query.ts 的
		// streamingToolExecutor.getRemainingResults() 路径。
		remaining := streamingExec.Wait(ctx)
		for _, result := range remaining {
			out <- Event{Type: EvtToolDone, ToolName: result.Name, ToolResult: result.Content, ToolUseID: result.ToolUseID}
			// Observer: 通知工具执行完成。
			if l.cfg.Observer != nil {
				var duration time.Duration
				if startTime, ok := state.toolStartTimes[result.ToolUseID]; ok {
					duration = time.Since(startTime)
					delete(state.toolStartTimes, result.ToolUseID)
				}
				if result.IsError {
					l.cfg.Observer.OnToolError(ctx, result.Name, nil, fmt.Errorf("%s", result.Content), duration)
				} else {
					l.cfg.Observer.OnToolDone(ctx, result.Name, nil, result.Content, duration)
				}
			}
		}

		// 收集所有结果。
		allResults := append(streamingExec.Poll(), remaining...)

		// 执行后处理：中间件 AfterTool + Hooks PostToolUse。
		for _, result := range allResults {
			tool := l.toolIndex[result.Name]
			if tool == nil || result.IsError {
				continue
			}
			// 结果截断（MaxResultSizeChars）。
			if tool.MaxResultSizeChars > 0 && len(result.Content) > tool.MaxResultSizeChars {
				result.Content = result.Content[:tool.MaxResultSizeChars] + "\n... (结果被截断)"
			}
			// 中间件 AfterTool。
			for _, mw := range l.cfg.Middlewares {
				mw.AfterTool(ctx, result.Name, result.Content, nil)
			}
			// Hooks PostToolUse。
			if l.cfg.Hooks != nil {
				// 找到对应的 toolCall 以获取 input。
				for _, tc := range toolCalls {
					if tc.ID == result.ToolUseID {
						l.cfg.Hooks.RunPostToolUse(ctx, tc.Name, tc.Input, result.Content, nil)
						break
					}
				}
			}
		}

		// 将工具结果作为消息追加。
		for _, result := range allResults {
			state.messages = append(state.messages,
				message.NewToolResultMessage(result.ToolUseID, result.Content, result.IsError),
			)
		}

		// ⑨ 后处理。
		state.turnCount++
		state.hasAttemptedReactiveCompact = false
		t := TransNextTurn
		state.transition = &t

		// 增加压缩管理器的轮次计数。
		if l.cfg.Compaction != nil {
			l.cfg.Compaction.IncrementTurn()
		}

		// PostSampling hook（SessionMemory 等）。
		if l.cfg.PostSampling != nil {
			l.cfg.PostSampling.AfterSampling(ctx, state.messages)
		}

		// Token 预算检查。
		if l.cfg.Budget != nil {
			result := l.cfg.Budget.Check()
			if result.ShouldStop() {
				out <- Event{Type: EvtProgress, Text: fmt.Sprintf("token 预算状态: %s", result.Status)}
				out <- Event{Type: EvtDone}
				return
			}
		}

		out <- Event{Type: EvtTurnComplete}
		// continue → 下一次迭代
	}
}

// buildPreCheck 构建工具的 PreCheck 闭包，包含完整的权限检查链。
// 对齐 Claude Code 的 StreamingToolExecutor.executeTool() 内置权限逻辑。
// 检查链：ValidateInput → CheckPermissions → PlanChecker → Permission.Gate → Middlewares → Hooks。
// 返回 nil 表示工具不需要预检查（如 ReadOnly 级别且无额外约束）。
func (l *Loop) buildPreCheck(tool *ToolEntry, toolName string, toolInput json.RawMessage) executor.PreCheckFn {
	// ReadOnly 工具且无额外检查 → 跳过预检查，最大化并发。
	if tool.Permission == permission.LevelReadOnly &&
		l.cfg.PlanChecker == nil &&
		l.cfg.Hooks == nil &&
		len(l.cfg.Middlewares) == 0 {
		return nil
	}

	return func(ctx context.Context) (string, bool) {
		// 1. Plan mode 工具过滤。
		if l.cfg.PlanChecker != nil && l.cfg.PlanChecker.IsActive() {
			if !l.cfg.PlanChecker.IsToolAllowed(tool.Permission) {
				return fmt.Sprintf("Plan mode 下禁止使用工具 %q（仅允许只读工具和 ExitPlanMode）", toolName), true
			}
		}

		// 2. Permission Gate 检查（可能阻塞等待用户审批）。
		if l.cfg.Permission != nil {
			allowed, reason := l.cfg.Permission.Check(toolName, string(toolInput), tool.Permission)
			// Observer: 通知权限决策。
			if l.cfg.Observer != nil {
				if allowed {
					l.cfg.Observer.OnPermissionGranted(ctx, toolName, tool.Permission.String())
				} else {
					l.cfg.Observer.OnPermissionDenied(ctx, toolName, tool.Permission.String(), reason)
				}
			}
			if !allowed {
				return fmt.Sprintf("权限被拒绝: %s", reason), true
			}
		}

		// 3. 中间件 BeforeTool。
		for _, mw := range l.cfg.Middlewares {
			if d := mw.BeforeTool(ctx, toolName, toolInput); d != nil && !d.Allow {
				return fmt.Sprintf("被中间件阻止: %s", d.Reason), true
			}
		}

		// 4. Hooks PreToolUse。
		if l.cfg.Hooks != nil {
			block, msg, err := l.cfg.Hooks.RunPreToolUse(ctx, toolName, toolInput)
			if err != nil {
				return fmt.Sprintf("hook 错误: %s", err.Error()), true
			}
			if block {
				return fmt.Sprintf("被 hook 阻止: %s", msg), true
			}
		}

		return "", false
	}
}

// 内置模型定价表（与 cost/cost.go 保持同步）。
// 用于在 Observer.OnTokenUsage 事件中提供 costUSD 参数。
var defaultPricingUSD = map[string]struct {
	inputPerMillion  float64
	outputPerMillion float64
}{
	"claude-opus-4-6":   {15.0, 75.0},
	"claude-sonnet-4-6": {3.0, 15.0},
	"claude-haiku-4-5":  {0.8, 4.0},
}

// estimateCostUSD 根据 Token 使用量估算 USD 成本。
// 这是一个近似计算，与 cost.Tracker.calculateCost 逻辑一致。
// 用于 Observer.OnTokenUsage 事件的 costUSD 参数。
func estimateCostUSD(modelID string, usage *provider.Usage) float64 {
	if usage == nil {
		return 0
	}
	pricing, ok := defaultPricingUSD[modelID]
	if !ok {
		// 未知模型使用 sonnet 定价。
		pricing = defaultPricingUSD["claude-sonnet-4-6"]
	}
	cost := float64(usage.InputTokens)*pricing.inputPerMillion/1_000_000 +
		float64(usage.OutputTokens)*pricing.outputPerMillion/1_000_000
	return cost
}

// cacheControlForSystemPrompt 决定 system prompt block 是否需要 cache_control。
// 策略：如果有 tools，tools 的最后一个已经标记了缓存断点，
// system prompt 无需额外标记（Anthropic 缓存按前缀匹配，tools 的标记已覆盖 system prompt）。
// 如果没有 tools，system prompt 需要自己标记。
func cacheControlForSystemPrompt(supportsCaching bool, toolCount int) *provider.CacheControl {
	if !supportsCaching {
		return nil
	}
	if toolCount > 0 {
		// tools 最后一个已标记，system prompt 不需要。
		// 但为了最大化缓存命中（system prompt 和 tools 分别缓存），也标记上。
		// Anthropic 最多支持 4 个 cache breakpoint，这里用掉 2 个（system + tools）是合理的。
		return &provider.CacheControl{Type: "ephemeral"}
	}
	// 无 tools 场景，system prompt 是唯一可缓存前缀。
	return &provider.CacheControl{Type: "ephemeral"}
}
