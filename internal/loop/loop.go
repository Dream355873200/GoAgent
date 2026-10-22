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
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dream355873200/GoAgent/budget"
	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/cost"
	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/schema"
)

// DefaultMaxTurns 是 Config.MaxTurns 未设置（<=0）时的最大轮次。
const DefaultMaxTurns = 100

// ErrMaxTurns 表示循环因达到最大轮次而终止（随 EvtError 发出，用 errors.Is 判断）。
var ErrMaxTurns = errors.New("已达最大轮次")

// Config 包含循环运行所需的一切。
type Config struct {
	Provider        provider.Provider
	Fallback        provider.Provider // 可选的后备模型
	Tools           []ToolEntry
	Middlewares     []Middleware
	SystemPrompt    string
	MaxTurns        int    // <=0 时取 DefaultMaxTurns
	ReasoningEffort string // 思考强度档位（off/low/medium/high；空 = 不干预），随每次模型请求下发
	Compaction      *compaction.Manager
	Permission      *permission.Gate
	Memory          *memory.Manager
	Executor        *executor.Executor // nil 时使用默认配置的执行器
	Budget          *budget.Tracker    // token 预算追踪器
	Hooks           HooksRunner        // hooks 管理器（含 stop 扩展点，可选）
	PlanChecker     PlanChecker        // plan mode 工具过滤（可选）
	PostSampling    PostSamplingHook   // 采样后钩子（SessionMemory 等）
	InitialMessages []message.Message  // 历史消息（多轮对话恢复用）
	// Steering 插话源（可选）。非 nil 时每个工具批结束边界调用
	// DrainGuides 取走待注入消息，作为 user 消息追加进下一次模型
	// 请求——运行中的模型流不受影响。见 steering.go。
	Steering SteerSource
	// Observer 接收循环中的可观测性事件（Token 用量、工具执行、权限决策等）。
	// 可传入 observer.MultiObserver 广播给多个观察者。
	// 如果为 nil，则不发送任何可观测性事件。
	Observer observer.Observer
	// SessionID 是当前会话的标识符，用于 Observer 事件的上下文。
	// 如果为空字符串，Observer 事件中的 SessionID 字段为空。
	SessionID string
	// Logger 库内日志出口（nil = 不打日志）。关键事件（每轮摘要、
	// 限流重试、后备切换、错误现场）走 Info/Warn/Error 级，
	// 由宿主经 WithLogger 或 slog.Default() 注入。
	Logger *slog.Logger
	// Debug 测试模式：细节日志（逐轮请求参数、工具调用明细）额外
	// 输出。对齐「测试模式的中间信息应尽可能可获取」原则。
	// 细节日志同样走 Info 级输出（不依赖 slog Debug level），
	// 宿主无需调整 handler 级别。
	Debug bool
	// SuspendGate 挂起/恢复门闩（可选）。非 nil 时，每轮工具执行前
	// 检查是否处于挂起等待——审批等待/用户暂停场景由宿主经
	// gate.Wait 阻塞、gate.Resume 唤醒续跑（非取消语义）。
	// 挂起期间消息序列与执行位置保持完整（内存态）。
	SuspendGate *SuspendGate
	// OnMessage 在每条消息进入对话时、于循环 goroutine 内同步调用（可选）：
	// 用户输入、助手回复、工具结果、恢复提示、插话。它先于任何压缩改写
	// 发生，拿到的是消息的完整原貌，适合做逐条持久化。InitialMessages
	// 与压缩派生的消息不会触发。
	OnMessage func(m message.Message)
	// SkipInputValidation 关闭工具入参校验。默认（false）每次工具调用
	// 执行前按工具的 InputSchema 校验参数（类型、必填、枚举等），不合规
	// 的调用不执行，错误说明作为工具结果返回给模型以便修正。
	SkipInputValidation bool
	// Model 覆盖每次请求的模型 ID（空 = provider 当前的模型）。切换到后备
	// provider 后不再使用。
	Model string
	// MaxTokens 单次响应的输出上限（0 = 由 provider 决定）。
	MaxTokens int
	// Nested 表示这是在某次运行之内派生的子循环（NewChild 自动设置）：它不是
	// 一次会话，不向 Observer 发送 OnSessionStart / OnSessionEnd。
	Nested bool
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
	// RunPostToolUse 在工具执行后调用（含执行失败的情况，toolErr 非 nil）。
	RunPostToolUse(ctx context.Context, toolName string, input json.RawMessage, result string, toolErr error)
	// RunStop 在循环退出前调用。返回 block=true 时阻止退出。
	// stopReason 是本轮 LLM 的停止原因（引擎自愈已处理 StopMaxTokens，
	// 到这里的不会是截断）；lastText/messages 是退出现场，供内容级检查。
	RunStop(ctx context.Context, stopReason, lastText string, messages []message.Message) (block bool, message string, err error)
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
	// Class 副作用分级（executor.Class）。非 Unset 时优先于 Concurrent。
	Class     executor.Class
	ExecuteFn func(ctx context.Context, input json.RawMessage) (string, error)

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
	// ToolIsError EvtToolDone 时表示该工具以错误结束（结果文本为错误信息）
	ToolIsError bool
	Usage       *provider.Usage
	Error       error
	// StatusKey 非空时本事件是「状态行更新」而非追加消息：前端按 Key
	// 原地替换显示（如 429 重试的倒计时行）。Text 为空 = 清除该状态行。
	StatusKey string

	// 子 agent 进度字段（EvtSubAgentProgress；由工具经 EmitFromTool 发射）。
	AgentID       string
	AgentDesc     string
	AgentStatus   string
	AgentActivity string
	AgentToolUses int
	AgentTokens   int
}

// 事件类型（匹配公共 EventType）。
// 注意：公共面 11-14 已被 SubAgentProgress/AskUser/PlanConfirm/Interrupt
// 占用，本包新增类型必须显式对齐公共数值（toPublicEvent 数值直转）。
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
	// EvtSubAgentProgress = 11 对齐公共 EventSubAgentProgress：子 agent 进度
	//（工具执行期间经 EmitFromTool 发射，不由循环本身产生）。
	EvtSubAgentProgress = 11
	// EvtInterrupted = 15 对齐公共 EventInterrupted：用户主动终止。
	EvtInterrupted = 15
	// 公共 EventRetrieval = 16（检索事件不经过 loop，跳过该数值）。
	// EvtSteer = 17 对齐公共 EventSteer：guide 车道消息已注入模型上下文。
	EvtSteer = 17
	// 公共 EventQueueRun = 18（queue 车道消费由 RunSession 在 loop 外
	// 直接发射，不经过 loop；后续 loop 新增类型须跳过该数值）。
)

// maxStreamRetries 流中断锚点恢复的连续重试上限（熔断）。流正常结束即
// 清零；连续超限说明网络/provider 持续异常，果断报错给宿主而不是
// 无限重试。
const maxStreamRetries = 3

// maxOutputRecoveries 输出被 max_tokens 截断时注入续写提示的最大次数。
const maxOutputRecoveries = 3

// 合成 tool_result 的文案。
const (
	interruptedResultText = "工具执行被用户中断"
	missingResultText     = "该工具调用没有返回结果"
)

// loopState 是跨迭代携带的可变状态。
type loopState struct {
	// messages 是发给模型的视图：压缩只改写它。
	messages []message.Message
	// transcript 是完整对话记录，只追加：进入对话的每条消息与自动摘要的
	// 摘要记录（见 recordSummaries）。compaction.Project(transcript) 可还原
	// 视图。
	transcript                  []message.Message
	turnCount                   int
	maxOutputRecoveryCount      int
	thinkingOnlyRecoveryCount   int
	hasAttemptedReactiveCompact bool
	usingFallback               bool
	// lastInputTokens 记录上一轮请求实际占用的上下文 token 数（含缓存读写），
	// 用于替代客户端估算做 token 阻断判断。对齐 Claude Code 的
	// finalContextTokensFromLastResponse()。
	lastInputTokens int
	// toolStartTimes 记录每个工具调用的开始时间，用于计算执行时长。
	toolStartTimes map[string]time.Time
	// streamRetryCount 流中断锚点恢复的连续失败计数（熔断用）。
	// 流正常结束时清零——只对「连续中断」计数，偶发一次网络抖动
	// 不该让整轮报废。
	streamRetryCount int
}

// Loop 是核心 agent 状态机。
type Loop struct {
	cfg       Config
	toolIndex map[string]*ToolEntry
	// toolSchemas 是各工具用于入参校验的 schema（nil 表示不校验）。
	toolSchemas map[string]map[string]any
	// mu 保护 finalMessages：写方是 run goroutine（每轮末尾 + 结束
	// defer），读方是事件消费 goroutine（EventDone 时 / 逐条持久化
	// 时调 FinalMessages）——两个 goroutine 天然并发，没有锁就是
	// data race（-race 实测：EventDone 事件先于 defer 写到达消费者）。
	mu            sync.RWMutex
	finalMessages []message.Message // 完整对话记录（见 FinalMessages）
}

// New 创建一个新的 Loop。
func New(cfg Config) *Loop {
	if cfg.Executor == nil {
		logger := cfg.Logger
		cfg.Executor = executor.New(executor.Config{
			PanicHandler: func(tool string, r any, stack []byte) {
				if logger != nil {
					logger.Error("loop: 工具 panic 已恢复", "tool", tool, "panic", fmt.Sprint(r), "stack", string(stack))
				}
			},
		})
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	idx := make(map[string]*ToolEntry, len(cfg.Tools))
	for i := range cfg.Tools {
		idx[cfg.Tools[i].Name] = &cfg.Tools[i]
	}
	var schemas map[string]map[string]any
	if !cfg.SkipInputValidation {
		schemas = make(map[string]map[string]any, len(cfg.Tools))
		for _, t := range cfg.Tools {
			if s := schema.AsMap(t.InputSchema); s != nil {
				schemas[t.Name] = s
			}
		}
	}
	return &Loop{cfg: cfg, toolIndex: idx, toolSchemas: schemas}
}

// logInfo 输出关键事件日志（每轮摘要、限流重试、后备切换、错误现场）。
// Logger 未注入时静默跳过。
func (l *Loop) logInfo(format string, args ...any) {
	if l.cfg.Logger != nil {
		l.cfg.Logger.Info(fmt.Sprintf("loop: "+format, args...))
	}
}

// logWarn 输出可自愈的异常日志（限流、过载切换、截断恢复）。
func (l *Loop) logWarn(format string, args ...any) {
	if l.cfg.Logger != nil {
		l.cfg.Logger.Warn(fmt.Sprintf("loop: "+format, args...))
	}
}

// logError 输出导致循环终止的错误日志。
func (l *Loop) logError(format string, args ...any) {
	if l.cfg.Logger != nil {
		l.cfg.Logger.Error(fmt.Sprintf("loop: "+format, args...))
	}
}

// logDebug 输出测试模式细节日志（逐轮请求参数、工具调用明细）。
// 仅在 Config.Debug（WithDebugMode）开启时输出——级别仍走 Info，
// 宿主无需调整 slog handler 级别。
func (l *Loop) logDebug(format string, args ...any) {
	if l.cfg.Logger != nil && l.cfg.Debug {
		l.cfg.Logger.Info(fmt.Sprintf("loop: "+format, args...))
	}
}

// Run 启动 agent 循环并返回一个事件通道。传给工具执行的 ctx 携带本循环的
// 作用域（见 Scope）：工具里派生的子循环经 NewChild 继承它。
func (l *Loop) Run(ctx context.Context, input string) <-chan Event {
	out := make(chan Event, 64)
	sink := &toolSink{out: out, done: make(chan struct{})}
	ctx = WithScope(context.WithValue(ctx, toolSinkKey{}, sink), l.scope())
	go func() {
		defer close(out)
		defer sink.close() // 先于 close(out)：迟到的工具事件被丢弃而非向已关闭通道发送
		l.run(ctx, input, out)
	}()
	return out
}

// toolSinkKey 工具事件通道的 context 键。
type toolSinkKey struct{}

// toolSink 让执行中的工具向本次 run 的事件流追加事件。工具可能在循环
// 结束后仍在运行（中断、后台化），关闭后的发射静默丢弃。
type toolSink struct {
	mu     sync.RWMutex
	closed bool
	out    chan<- Event
	done   chan struct{}
}

func (s *toolSink) emit(ev Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.out <- ev:
	case <-s.done:
	}
}

func (s *toolSink) close() {
	close(s.done) // 先解除阻塞中的发射，再拿写锁
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// EmitFromTool 从工具执行 ctx 向所属 run 的事件流发射事件；ctx 不来自
// 循环（或 run 已结束）时返回 false。
func EmitFromTool(ctx context.Context, ev Event) bool {
	s, ok := ctx.Value(toolSinkKey{}).(*toolSink)
	if !ok || s == nil {
		return false
	}
	s.emit(ev)
	return true
}

// FinalMessages 返回完整的对话记录：InitialMessages、本次运行中进入对话的
// 每条消息，以及自动摘要追加的摘要记录——压缩不改写它。把它作为下一次运行
// 的 InitialMessages 传回，循环用 compaction.Project 还原模型视图，不必再
// 请求模型摘要一遍。运行期间每轮结束时同步更新；完整结果在 Run() 返回的
// channel 消费完毕后可用。
func (l *Loop) FinalMessages() []message.Message {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.finalMessages
}

// setFinalMessages 由 run goroutine 同步最新消息列表（锁内拷贝 slice
// header）。已发布的元素不会再被就地改写：循环只追加，压缩层按
// copy-on-write 生成新切片，所以读方拿到的旧 header 仍然安全。
func (l *Loop) setFinalMessages(msgs []message.Message) {
	l.mu.Lock()
	l.finalMessages = msgs
	l.mu.Unlock()
}

// appendMessage 把一条消息加入对话（视图与对话记录）、分配 UID，并同步回调
// OnMessage。只有经由这里（或 recordSummaries）进入的消息才是对话记录的
// 一部分；压缩等派生出的其他消息没有 UID，也不会触发回调。
func (l *Loop) appendMessage(state *loopState, m message.Message) {
	if m.UID == "" {
		m.UID = message.NewUID()
	}
	state.messages = append(state.messages, m)
	state.transcript = append(state.transcript, m)
	if l.cfg.OnMessage != nil {
		l.cfg.OnMessage(m)
	}
}

// setView 换上压缩后的视图，并把其中新产生的摘要（还没有 UID 的
// IsCompactSummary 消息）作为摘要记录追加进对话记录——同样分配 UID、
// 回调 OnMessage。重新加载时 compaction.Project 据此还原视图。
func (l *Loop) setView(state *loopState, view []message.Message) {
	for i := range view {
		if !view[i].IsCompactSummary || view[i].UID != "" {
			continue
		}
		rec := view[i]
		rec.UID = message.NewUID()
		view[i] = rec
		state.transcript = append(state.transcript, rec)
		if l.cfg.OnMessage != nil {
			l.cfg.OnMessage(rec)
		}
	}
	state.messages = view
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

	state := &loopState{
		messages:       compaction.Project(l.cfg.InitialMessages),
		transcript:     make([]message.Message, 0, len(l.cfg.InitialMessages)+1),
		toolStartTimes: make(map[string]time.Time),
	}
	state.transcript = append(state.transcript, l.cfg.InitialMessages...)
	l.appendMessage(state, message.NewUserMessage(input))

	// 通知 Observer 会话开始（子循环不是一次会话，不通知）。
	if l.cfg.Observer != nil && !l.cfg.Nested {
		l.cfg.Observer.OnSessionStart(ctx, l.cfg.SessionID)
	}

	// 循环结束时保存最终消息列表，并通知 Observer 会话结束。
	// 运行期间每轮迭代末尾也同步到 finalMessages（见 run 循环体），
	// 供外部在事件流消费中途调用 FinalMessages() 做逐条即时持久化。
	defer func() {
		l.setFinalMessages(state.transcript)
		if l.cfg.Observer != nil && !l.cfg.Nested {
			l.cfg.Observer.OnSessionEnd(ctx, l.cfg.SessionID, state.turnCount)
		}
	}()
	l.setFinalMessages(state.transcript)

	activeProvider := l.cfg.Provider

	// nil provider 守卫：此前直接在 Capabilities() 上 panic（nil pointer），
	// 且发生在 goroutine 里无人 recover——整个进程崩掉。给出可诊断的错误事件。
	if activeProvider == nil {
		l.logError("provider 未配置（WithProvider / ProviderConfig 均未设置），循环退出")
		out <- Event{Type: EvtError, Error: fmt.Errorf("loop: provider 未配置（WithProvider / ProviderConfig 均未设置）")}
		return
	}

	l.logInfo("启动 session=%s model=%s tools=%d initial_msgs=%d max_turns=%d debug=%v",
		l.cfg.SessionID, activeProvider.Capabilities().ModelID, len(l.cfg.Tools),
		len(state.messages), l.cfg.MaxTurns, l.cfg.Debug)

	// 工具定义缓存 — 一次性构建，跨迭代复用。
	// 对齐 Claude Code 的 toolUseContext.options.tools（不在循环内重建）。
	toolDefs := buildToolDefs(l.cfg.Tools, activeProvider.Capabilities())

	// 等待 memory 加载完成，构建 system prompt。
	<-memDone
	systemPrompt := l.cfg.SystemPrompt
	if memSuffix != "" {
		systemPrompt += "\n\n" + memSuffix
	}

	// ====== 主循环 ======
mainLoop:
	for {
		// 检查最大轮次。
		if state.turnCount >= l.cfg.MaxTurns {
			l.logError("已达最大轮次 (%d)，循环终止", l.cfg.MaxTurns)
			err := fmt.Errorf("%w (%d)", ErrMaxTurns, l.cfg.MaxTurns)
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, err)
			}
			out <- Event{Type: EvtError, Error: err}
			return
		}

		// 挂起检查点：宿主经 SuspendGate 发起挂起（非取消）时，循环在此
		// 阻塞等待唤醒。挂起期间内存态（消息序列/执行位置）完整保留，
		// 唤醒后从本行继续——「挂起点恢复」而非重跑。
		if l.cfg.SuspendGate != nil && l.cfg.SuspendGate.IsWaiting() {
			l.logInfo("循环挂起中，等待唤醒 · turn=%d msgs=%d", state.turnCount+1, len(state.messages))
			out <- Event{Type: EvtProgress, StatusKey: "suspend", Text: "已挂起，等待外部信号"}
			resumed, suspendErr := l.cfg.SuspendGate.Wait(ctx)
			if suspendErr != nil {
				// Terminate：终止路径（区别于终止 ctx cancel 的第三种退出）。
				l.logWarn("挂起被终止: %v", suspendErr)
				out <- Event{Type: EvtProgress, StatusKey: "suspend", Text: ""}
				out <- Event{Type: EvtError, Error: suspendErr}
				return
			}
			if !resumed {
				// ctx 取消（用户终止）：走统一的中断路径。
				out <- Event{Type: EvtProgress, StatusKey: "suspend", Text: ""}
				continue // 下一轮循环顶部的 ctx 检查会处理终止
			}
			l.logInfo("挂起唤醒，继续执行 · turn=%d", state.turnCount+1)
			out <- Event{Type: EvtProgress, StatusKey: "suspend", Text: ""} // 清除状态行
		}

		// 检查上下文取消。
		if ctx.Err() != nil {
			// 用户主动终止（/interrupt 路由到 ctx cancel）与超时/其他错误
			// 分开：Canceled 走一等事件，前端渲染「已停止」而非报错。
			if errors.Is(ctx.Err(), context.Canceled) {
				l.logInfo("用户中断，循环终止")
				out <- Event{Type: EvtInterrupted, Text: "用户中断"}
				return
			}
			l.logError("上下文已取消，循环终止: %v", ctx.Err())
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, ctx.Err())
			}
			out <- Event{Type: EvtError, Error: ctx.Err()}
			return
		}

		caps := activeProvider.Capabilities()

		// ① 预处理：四层压缩。
		contextWindow := 200_000 // provider 未声明时的默认值
		if caps.ContextWindow > 0 {
			contextWindow = caps.ContextWindow
		}
		if l.cfg.Compaction != nil {
			compacted, freed := l.cfg.Compaction.Apply(ctx, state.messages, contextWindow)
			if freed > 0 {
				l.setView(state, compacted)
				l.setFinalMessages(state.transcript)
				out <- Event{Type: EvtCompaction, Text: fmt.Sprintf("已压缩，释放 ~%d tokens", freed)}
				if l.cfg.Observer != nil {
					l.cfg.Observer.OnCompaction(ctx, freed, "auto_compact")
				}
			}
		}

		// ② Token 阻断检查。
		// 优先使用上一轮 API 返回的实际上下文占用（非首轮时可用），
		// 对齐 Claude Code 的 tokenCountWithEstimation()。
		var totalTokens int
		if state.lastInputTokens > 0 {
			totalTokens = state.lastInputTokens
		} else {
			totalTokens = message.EstimateMessagesTokens(state.messages)
		}
		hardLimit := contextWindow - 3000
		if totalTokens > hardLimit && l.cfg.Compaction == nil {
			err := fmt.Errorf("上下文已耗尽: %d tokens > %d 限制", totalTokens, hardLimit)
			l.logError("%v", err)
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, err)
			}
			out <- Event{Type: EvtError, Error: err}
			return
		}

		// ③ 构建请求。SystemPrompt 总是带上：切到后备模型时，不支持
		// SystemBlocks 的实现（如 OpenAI 兼容）只读这个字段。
		// 发送前统一修复 tool_use/tool_result 配对：压缩、中断、存储损坏
		// 都可能留下缺口，缺口会让上游 API 整请求 400。
		req := &provider.Request{
			Messages:        message.SanitizeForRequest(state.messages),
			SystemPrompt:    systemPrompt,
			MaxTokens:       l.cfg.MaxTokens,
			ReasoningEffort: l.cfg.ReasoningEffort,
		}
		if !state.usingFallback {
			req.Model = l.cfg.Model
		}
		if caps.SupportsTools {
			req.Tools = toolDefs
		}
		if caps.SupportsCaching {
			req.SystemBlocks = []provider.SystemBlock{{
				Text:         systemPrompt,
				CacheControl: &provider.CacheControl{Type: "ephemeral"},
			}}
		}

		l.logDebug("turn=%d 请求: msgs=%d tools=%d input_tokens_est=%d",
			state.turnCount+1, len(req.Messages), len(req.Tools), totalTokens)

		// LLM 调用级 observer（可选接口）：Stream 建立前触发 OnLLMStart。
		llmInfo := &observer.LLMCallInfo{
			Model:          caps.ModelID,
			Turn:           state.turnCount + 1,
			NumMessages:    len(req.Messages),
			NumTools:       len(req.Tools),
			MaxTokens:      req.MaxTokens,
			InputTokensEst: totalTokens,
		}
		if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
			o.OnLLMStart(ctx, llmInfo)
		}
		llmStart := time.Now()

		// ④ 调用 API（流式）。
		stream, err := activeProvider.Stream(ctx, req)
		if err != nil {
			var interrupted bool
			stream, err, interrupted = l.retryRateLimit(ctx, activeProvider, req, err, state, out)
			if interrupted {
				return
			}
		}

		if err != nil {
			// 处理过载 → 后备。
			var overloadErr *provider.OverloadError
			if errors.As(err, &overloadErr) && l.cfg.Fallback != nil && !state.usingFallback {
				activeProvider = l.cfg.Fallback
				state.usingFallback = true
				l.logWarn("主模型过载，切换到后备模型 %s · turn=%d",
					activeProvider.Capabilities().ModelID, state.turnCount+1)
				out <- Event{Type: EvtProgress, Text: "主模型过载，切换到后备模型"}
				continue
			}
			// 处理 prompt 过长 → 响应式压缩。
			if l.tryReactiveCompact(ctx, state, err, contextWindow) {
				continue
			}

			// 流请求本身被取消（用户中断在 Stream 建立阶段传导）。
			if errors.Is(err, context.Canceled) {
				l.logInfo("用户中断（请求阶段），循环终止")
				if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
					o.OnLLMError(ctx, llmInfo, err)
				}
				out <- Event{Type: EvtInterrupted, Text: "用户中断"}
				return
			}

			l.logError("API 错误，循环终止: %v", err)
			if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
				o.OnLLMError(ctx, llmInfo, err)
			}
			out <- Event{Type: EvtError, Error: fmt.Errorf("API 错误: %w", err)}
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, fmt.Errorf("API 错误: %w", err))
			}
			return
		}

		// ⑤ 消费流 + 流式工具执行。
		// 所有工具（包括需要权限的）都进入 StreamingExecutor，权限检查
		// 通过 PreCheck 闭包在执行前完成。流式期间 Poll 到的结果与轮末
		// Wait 的结果合并使用——每个结果只会从两者之一返回一次。
		streamingExec := executor.NewStreamingExecutor(l.cfg.Executor)
		var assistantText string
		var thinkingText string
		var toolCalls []message.ToolCall
		var stopReason provider.StopReason
		var turnUsage *provider.Usage
		var completeMsg *message.Message
		var results []executor.ToolResult

		for ev := range stream {
			switch ev.Type {
			case provider.EventTextDelta:
				assistantText += ev.Text
				out <- Event{Type: EvtTextDelta, Text: ev.Text}

			case provider.EventThinkingDelta:
				thinkingText += ev.Thinking
				out <- Event{Type: EvtThinking, Thinking: ev.Thinking}

			case provider.EventToolUseStart:
				if ev.ToolCall != nil {
					l.startTool(ctx, state, streamingExec, *ev.ToolCall, out)
					toolCalls = append(toolCalls, *ev.ToolCall)
				}

			case provider.EventUsage:
				turnUsage = provider.MergeUsage(turnUsage, ev.Usage)

			case provider.EventMessageComplete:
				if ev.StopReason != "" {
					stopReason = ev.StopReason
				}
				if ev.Message != nil {
					completeMsg = ev.Message
				}

			case provider.EventError:
				// 流被取消（用户中断传导到 provider 层）同样走一等事件。
				if errors.Is(ev.Error, context.Canceled) || ctx.Err() != nil {
					l.logInfo("用户中断（流中），循环终止")
					if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
						o.OnLLMError(ctx, llmInfo, ev.Error)
					}
					l.finishInterrupted(ctx, state, assistantText, toolCalls, results, streamingExec, false, out)
					return
				}

				// 流刚建立就因上下文过长被拒（还没有任何内容）：响应式压缩后重试。
				if assistantText == "" && len(toolCalls) == 0 && l.tryReactiveCompact(ctx, state, ev.Error, contextWindow) {
					continue mainLoop
				}

				// 流中断锚点恢复（网络断/SSE 断）：不整轮报错。已到达的
				// 部分内容固化为完整消息（部分正文 + 已派发工具的真实结果
				// + 兜底合成结果），消息序列保持 API 对齐——这就是「最后
				// 安全锚点」；注入续跑提示后重开流。熔断：连续中断超过
				// 上限才放弃（streamRetryCount 在流正常结束时清零）。
				if state.streamRetryCount >= maxStreamRetries {
					l.logError("流中断且连续 %d 次恢复失败，循环终止: %v", maxStreamRetries, ev.Error)
					if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
						o.OnLLMError(ctx, llmInfo, ev.Error)
					}
					out <- Event{Type: EvtError, Error: ev.Error}
					if l.cfg.Observer != nil {
						l.cfg.Observer.OnError(ctx, ev.Error)
					}
					return
				}
				state.streamRetryCount++
				l.logWarn("流中断，锚点恢复重试 %d/%d: %v", state.streamRetryCount, maxStreamRetries, ev.Error)
				out <- Event{Type: EvtProgress, Text: fmt.Sprintf("流中断，正在恢复 (%d/%d)", state.streamRetryCount, maxStreamRetries)}

				// 已派发工具的真实结果必须保留（副作用已发生）：等待跑完并上报。
				late := append(streamingExec.Poll(), streamingExec.Wait(ctx)...)
				l.reportToolResults(ctx, state, late, out)
				results = append(results, late...)

				if assistantText != "" || len(toolCalls) > 0 {
					// 部分思考块丢弃：不完整且无签名回传价值，锚点只需正文
					// 与工具调用。正文按「已流出的原样」固化——模型续跑时
					// 能看到自己停在哪。
					l.appendAnchor(state, assistantText, toolCalls, results, "流中断，该工具调用未收到结果")
					l.appendMessage(state, message.NewMetaMessage(
						"Your previous response was interrupted by a network error mid-stream. "+
							"The partial content above is preserved as-is. Continue seamlessly from "+
							"where it stopped — no apology, no recap, no repetition.",
					))
					l.setFinalMessages(state.transcript)
				}
				// 无任何部分内容（流刚建立就断）：状态无需修复，直接重开流。
				// 注意 continue mainLoop：此处处于流消费的 range 循环内，
				// 裸 continue 只会结束本流（被当作正常结束），必须重开
				// 的是主循环的模型请求。
				continue mainLoop
			}

			// 轮询已完成的流式工具。
			if polled := streamingExec.Poll(); len(polled) > 0 {
				l.reportToolResults(ctx, state, polled, out)
				results = append(results, polled...)
			}
		}

		// 非增量 provider：流里只有一条完整消息、没有任何增量事件（例如把
		// 非流式接口包装成 Stream 的实现）时，从完整消息补出文本与思考，工具
		// 调用照常派发。
		if completeMsg != nil && assistantText == "" && thinkingText == "" && len(toolCalls) == 0 && ctx.Err() == nil {
			for _, b := range completeMsg.Content {
				switch b.Type {
				case "text":
					assistantText += b.Text
					out <- Event{Type: EvtTextDelta, Text: b.Text}
				case "thinking":
					thinkingText += b.Thinking
					out <- Event{Type: EvtThinking, Thinking: b.Thinking}
				}
			}
			for _, tc := range message.ExtractToolCalls(*completeMsg) {
				l.startTool(ctx, state, streamingExec, tc, out)
				toolCalls = append(toolCalls, tc)
			}
		}

		// 流正常结束：连续中断计数清零（熔断只针对连续失败）。
		state.streamRetryCount = 0

		// 用量：一次请求只上报一次合并后的终值。
		if turnUsage != nil {
			l.recordUsage(ctx, state, activeProvider, turnUsage, out)
		}

		// 流消费完毕：LLM 调用级 observer 触发 OnLLMDone（本轮响应完整落地）。
		if o, ok := l.cfg.Observer.(observer.LLMObserver); ok {
			o.OnLLMDone(ctx, llmInfo, &observer.LLMResult{
				Model:         caps.ModelID,
				Turn:          state.turnCount + 1,
				Duration:      time.Since(llmStart),
				StopReason:    string(stopReason),
				OutputTextLen: len(assistantText),
				ThinkingLen:   len(thinkingText),
				NumToolCalls:  len(toolCalls),
				Usage:         turnUsage,
			})
		}

		// ⑥ 中止检查 — 已执行的工具记入历史，未完成的合成结果，维持 API 消息对齐。
		if ctx.Err() != nil {
			if l.cfg.Observer != nil {
				l.cfg.Observer.OnError(ctx, ctx.Err())
			}
			l.finishInterrupted(ctx, state, assistantText, toolCalls, results, streamingExec, false, out)
			return
		}

		// 构建助手消息。
		l.appendMessage(state, buildAssistantMessage(thinkingText, assistantText, toolCalls, completeMsg))

		// 同步到 finalMessages：assistant 消息（思考+工具调用）一完成就落盘，
		// 不等工具执行完——工具跑几十秒（analyze/build）期间前端重连/重开
		// 项目就能看到本轮的思考和正在执行的工具调用。
		l.setFinalMessages(state.transcript)

		// ⑦ 无工具调用 → 退出判断。
		if len(toolCalls) == 0 {
			// 输出被 max_tokens 截断：注入续写提示（最多 maxOutputRecoveries 次）。
			// 不做"提升 max_tokens 重试"：多数 OpenAI 兼容后端在未显式传
			// max_tokens 时使用服务端默认值，显式传一个更大的值可能直接 400。
			if stopReason == provider.StopMaxTokens && state.maxOutputRecoveryCount < maxOutputRecoveries {
				l.appendMessage(state, message.NewMetaMessage(
					"Output token limit hit. Resume directly — no apology, "+
						"no recap. Pick up mid-thought. Break remaining work into smaller pieces.",
				))
				state.maxOutputRecoveryCount++
				l.logWarn("输出被截断(stop=max_tokens)，注入继续提示 %d/%d", state.maxOutputRecoveryCount, maxOutputRecoveries)
				out <- Event{Type: EvtProgress, Text: fmt.Sprintf("输出被截断，恢复尝试 %d/%d", state.maxOutputRecoveryCount, maxOutputRecoveries)}
				continue
			}

			// 推理型模型（DeepSeek-R1/V4 等）可能整段回复只进思考通道：
			// content 为空、无工具调用。此时历史里是一条只有 thinking 块的
			// 助手消息，用户侧表现为「什么都不输出直接停止」。注入恢复提示，
			// 要求模型在正文通道重新给出回复（最多 2 次，避免死循环）。
			if assistantText == "" && thinkingText != "" {
				if state.thinkingOnlyRecoveryCount < 2 {
					state.thinkingOnlyRecoveryCount++
					l.appendMessage(state, message.NewMetaMessage(
						"Your previous reply contained only internal reasoning with no visible response. "+
							"Re-state your answer in the main response channel as regular text "+
							"(do not use the thinking channel alone).",
					))
					l.logWarn("回复只有思考无正文，注入正文重述提示 %d/2", state.thinkingOnlyRecoveryCount)
					out <- Event{Type: EvtProgress, Text: "回复只有思考无正文，要求正文重述"}
					continue
				}
			}

			// Stop Hooks（统一扩展点：hooks 包 EventStop）。
			if l.cfg.Hooks != nil {
				block, msg, err := l.cfg.Hooks.RunStop(ctx, string(stopReason), assistantText, state.messages)
				if err != nil {
					out <- Event{Type: EvtError, Error: fmt.Errorf("stop hook 错误: %w", err)}
					return
				}
				if block {
					l.appendMessage(state, message.NewMetaMessage(msg))
					l.logWarn("stop hook 阻止退出: %s", msg)
					out <- Event{Type: EvtProgress, Text: fmt.Sprintf("stop hook 阻止退出: %s", msg)}
					continue
				}
			}

			// 正常完成。
			l.logInfo("完成: turns=%d msgs=%d stop=%s text=%dB thinking=%dB",
				state.turnCount, len(state.messages), stopReason, len(assistantText), len(thinkingText))
			l.setFinalMessages(state.transcript)
			out <- Event{Type: EvtDone}
			return
		}

		// ⑧ 有工具调用 → 等待所有工具完成。
		remaining := streamingExec.Wait(ctx)
		l.reportToolResults(ctx, state, remaining, out)
		results = append(results, remaining...)

		if ctx.Err() != nil {
			// 等待期间被中断：助手消息已在历史里，只需补齐结果。
			l.finishInterrupted(ctx, state, assistantText, toolCalls, results, streamingExec, true, out)
			return
		}

		ordered := orderResults(toolCalls, results, missingResultText)
		inputs := make(map[string]json.RawMessage, len(toolCalls))
		for _, tc := range toolCalls {
			inputs[tc.ID] = tc.Input
		}

		// 执行后处理：结果截断 → 中间件 AfterTool → Hooks PostToolUse。
		for i := range ordered {
			r := &ordered[i]
			tool := l.toolIndex[r.Name]
			if tool == nil {
				continue
			}
			r.Content = truncateResult(r.Content, tool.MaxResultSizeChars)
			var toolErr error
			if r.IsError {
				toolErr = errors.New(r.Content)
			}
			for _, mw := range l.cfg.Middlewares {
				mw.AfterTool(ctx, r.Name, r.Content, toolErr)
			}
			if l.cfg.Hooks != nil {
				l.cfg.Hooks.RunPostToolUse(ctx, r.Name, inputs[r.ToolUseID], r.Content, toolErr)
			}
		}

		// 将工具结果作为消息追加（按调用顺序）。
		for _, r := range ordered {
			l.appendMessage(state, message.NewToolResultMessage(r.ToolUseID, r.Content, r.IsError))
		}

		// 工具批结束边界：注入 guide 车道的插话消息。选此边界是因为
		// 助手消息与工具结果此刻已成对落定，消息序列处于一致状态；
		// 插话作为 user 消息追加后随下一次模型请求进入上下文——
		// 运行中的模型流不受影响。run 结束时仍未消费的插话由
		// SteeringHub.endRun 降级进 queue 车道，不会丢失。
		if l.cfg.Steering != nil && l.cfg.SessionID != "" {
			for _, g := range l.cfg.Steering.DrainGuides(l.cfg.SessionID) {
				l.appendMessage(state, message.NewUserMessage(g))
				out <- Event{Type: EvtSteer, Text: g}
			}
		}

		// ⑨ 后处理。
		state.turnCount++
		state.hasAttemptedReactiveCompact = false
		l.logInfo("turn=%d 完成: tools=%d context_tokens=%d msgs=%d",
			state.turnCount, len(ordered), state.lastInputTokens, len(state.messages))

		// 同步到 finalMessages：外部（goagent.run）在事件流消费中途调用
		// FinalMessages() 做逐条即时持久化，进程被杀时已完成的轮次全保留。
		l.setFinalMessages(state.transcript)

		// 增加压缩管理器的轮次计数。
		if l.cfg.Compaction != nil {
			l.cfg.Compaction.IncrementTurn()
		}

		// PostSampling hook（SessionMemory 等）。
		if l.cfg.PostSampling != nil {
			l.cfg.PostSampling.AfterSampling(ctx, state.messages)
		}

		// Token 预算检查（仅在设置了总预算时生效）。
		if l.cfg.Budget != nil {
			result := l.cfg.Budget.Check()
			if result.ShouldStop() {
				l.logWarn("token 预算停止: %s %s", result.Status, result.Reason)
				out <- Event{Type: EvtProgress, StatusKey: "budget", Text: fmt.Sprintf("token 预算已停止本次运行（%s）：%s", result.Status, result.Reason)}
				out <- Event{Type: EvtDone}
				return
			}
		}

		out <- Event{Type: EvtTurnComplete}
		// continue → 下一次迭代
	}
}

// buildToolDefs 把注册的工具转成请求里的工具定义。支持缓存时给最后一个
// 工具打 cache_control（Anthropic 缓存从头到该标记为止的前缀）。
func buildToolDefs(tools []ToolEntry, caps provider.Capabilities) []provider.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]provider.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	if caps.SupportsCaching {
		defs[len(defs)-1].CacheControl = &provider.CacheControl{Type: "ephemeral"}
	}
	return defs
}

// startTool 处理流中解析出的一个 tool_use：发事件、通知 Observer，并交给
// StreamingExecutor（未知工具直接得到错误结果）。
func (l *Loop) startTool(ctx context.Context, state *loopState, se *executor.StreamingExecutor, tc message.ToolCall, out chan<- Event) {
	l.logDebug("tool_start: %s id=%s input_len=%d", tc.Name, tc.ID, len(tc.Input))
	out <- Event{Type: EvtToolStart, ToolName: tc.Name, ToolInput: tc.Input, ToolUseID: tc.ID}

	// Observer: 通知工具开始执行（ctx 携带调用 ID，
	// 宿主经 observer.ToolCallIDFromContext 做 span 关联）。
	if l.cfg.Observer != nil {
		l.cfg.Observer.OnToolStart(observer.WithToolCallID(ctx, tc.ID), tc.Name, tc.Input)
	}
	state.toolStartTimes[tc.ID] = time.Now()

	tool := l.toolIndex[tc.Name]
	if tool == nil {
		name := tc.Name
		se.Add(ctx, executor.ToolCall{
			ID:    tc.ID,
			Name:  tc.Name,
			Input: tc.Input,
			Class: executor.ClassReadOnly,
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", fmt.Errorf("未知工具 %q", name)
			},
		})
		return
	}

	// 入参不符合 schema 的调用不执行（也不进入审批）：错误说明作为工具结果
	// 交回模型修正。此前缺必填字段的调用会以零值静默执行。
	if s := l.toolSchemas[tc.Name]; s != nil {
		if err := schema.Validate(s, tc.Input); err != nil {
			msg := fmt.Sprintf("工具 %q 的参数不符合其入参 schema，未执行：\n%v\n请修正参数后重试。", tc.Name, err)
			se.Add(ctx, executor.ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: tc.Input,
				Class: executor.ClassReadOnly,
				Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
					return "", errors.New(msg)
				},
			})
			return
		}
	}

	// 更新 transcript 供 YOLO 分类器使用。
	if l.cfg.Permission != nil {
		l.cfg.Permission.SetTranscript(state.messages)
	}
	se.Add(ctx, executor.ToolCall{
		ID:         tc.ID,
		Name:       tc.Name,
		Input:      tc.Input,
		Class:      tool.Class,
		Concurrent: tool.Concurrent,
		Execute:    tool.ExecuteFn,
		PreCheck:   l.buildPreCheck(tool, tc.Name, tc.Input, state.messages),
	})
}

// retryRateLimit 处理 429：等待后重试同一模型（换模型无济于事——同一账号
// 同一限额）。等待时长优先采用服务端的 Retry-After。状态行原地更新倒计时。
// interrupted 为 true 表示等待期间被用户中断且已发出终止事件。
func (l *Loop) retryRateLimit(ctx context.Context, p provider.Provider, req *provider.Request, err error, state *loopState, out chan<- Event) (stream <-chan provider.StreamEvent, outErr error, interrupted bool) {
	var rateLimitErr *provider.RateLimitError
	if !errors.As(err, &rateLimitErr) {
		return nil, err, false
	}
	const statusKey = "rate-limit"
	const maxRateRetries = 10
	for attempt := 1; attempt <= maxRateRetries; attempt++ {
		brief := truncateErr(rateLimitErr.Error(), 60)
		// TPM 窗口 60s：首次 15s 起步（部分配额可能已释放），逐次递增至 60s。
		wait := time.Duration(15*attempt) * time.Second
		if wait > 60*time.Second {
			wait = 60 * time.Second
		}
		if rateLimitErr.RetryAfter > 0 {
			wait = min(rateLimitErr.RetryAfter, 5*time.Minute)
		}
		l.logWarn("429 限流: %s · 等待 %s 后重试 (attempt %d/%d) · turn=%d",
			brief, wait, attempt, maxRateRetries, state.turnCount+1)
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			remain := int(time.Until(deadline).Seconds()) + 1
			out <- Event{Type: EvtProgress, StatusKey: statusKey, Text: fmt.Sprintf(
				"✻ 429 %s · Retrying in %ds · attempt %d/%d",
				brief, remain, attempt, maxRateRetries)}
			step := time.Until(deadline)
			if step > time.Second {
				step = time.Second
			}
			select {
			case <-ctx.Done():
				out <- Event{Type: EvtProgress, StatusKey: statusKey, Text: ""}
				if errors.Is(ctx.Err(), context.Canceled) {
					l.logInfo("429 等待中用户中断")
					out <- Event{Type: EvtInterrupted, Text: "用户中断"}
				} else {
					l.logError("429 等待中被取消: %v", ctx.Err())
					out <- Event{Type: EvtError, Error: fmt.Errorf("API 错误: %w", err)}
				}
				return nil, nil, true
			case <-time.After(step):
			}
		}
		stream, err = p.Stream(ctx, req)
		if err == nil {
			l.logInfo("429 限流恢复: attempt %d 后成功 · turn=%d", attempt, state.turnCount+1)
			out <- Event{Type: EvtProgress, StatusKey: statusKey, Text: ""}
			return stream, nil, false
		}
		if !errors.As(err, &rateLimitErr) {
			out <- Event{Type: EvtProgress, StatusKey: statusKey, Text: ""}
			return nil, err, false // 变成别的错误了（过载/断网）：走常规处理
		}
	}
	l.logError("429 重试 %d 次后仍失败: %v", maxRateRetries, err)
	out <- Event{Type: EvtProgress, StatusKey: statusKey, Text: ""}
	return nil, err, false
}

// tryReactiveCompact 在上下文过长错误时尝试一次响应式压缩，成功返回 true。
func (l *Loop) tryReactiveCompact(ctx context.Context, state *loopState, err error, contextWindow int) bool {
	var ptlErr *provider.PromptTooLongError
	if !errors.As(err, &ptlErr) || l.cfg.Compaction == nil || state.hasAttemptedReactiveCompact {
		return false
	}
	recovered, ok := l.cfg.Compaction.HandleOverflow(ctx, state.messages, contextWindow)
	if !ok {
		return false
	}
	l.logWarn("上下文过长，响应式压缩后重试")
	l.setView(state, recovered)
	state.hasAttemptedReactiveCompact = true
	l.setFinalMessages(state.transcript)
	return true
}

// recordUsage 上报一次请求的用量：事件流、上下文占用、预算与 Observer。
func (l *Loop) recordUsage(ctx context.Context, state *loopState, p provider.Provider, u *provider.Usage, out chan<- Event) {
	out <- Event{Type: EvtUsageUpdate, Usage: u}
	if n := u.ContextTokens(); n > 0 {
		state.lastInputTokens = n
	}
	if l.cfg.Budget != nil {
		l.cfg.Budget.RecordUsage(state.turnCount, u.InputTokens, u.OutputTokens)
	}
	if l.cfg.Observer != nil {
		model := p.Capabilities().ModelID
		l.cfg.Observer.OnTokenUsage(ctx, model, u, cost.Estimate(model, *u))
	}
}

// finishInterrupted 处理用户中断（或 ctx 到期）：收集已完成的工具结果、
// 不再启动排队中的工具，把本轮已发生的内容（正文、工具调用及其真实或
// 合成的结果）记入历史，保证同会话可以继续对话。assistantAppended 为
// true 表示助手消息已在历史中，只需补齐结果。
func (l *Loop) finishInterrupted(ctx context.Context, state *loopState, text string, calls []message.ToolCall, results []executor.ToolResult, se *executor.StreamingExecutor, assistantAppended bool, out chan<- Event) {
	late := append(se.Poll(), se.Wait(ctx)...)
	l.reportToolResults(ctx, state, late, out)
	results = append(results, late...)

	if assistantAppended {
		for _, r := range orderResults(calls, results, interruptedResultText) {
			l.appendMessage(state, message.NewToolResultMessage(r.ToolUseID, r.Content, r.IsError))
		}
	} else if text != "" || len(calls) > 0 {
		l.appendAnchor(state, text, calls, results, interruptedResultText)
	}
	l.setFinalMessages(state.transcript)

	// 用户主动终止走一等事件；DeadlineExceeded 等仍走错误通道。ctx 本身
	// 未结束时是 provider 上报了取消（用户中断传导到流），同样视为中断。
	cause := ctx.Err()
	if cause == nil || errors.Is(cause, context.Canceled) {
		out <- Event{Type: EvtInterrupted, Text: "用户中断"}
	} else {
		out <- Event{Type: EvtError, Error: cause}
	}
}

// appendAnchor 把未完整结束的一轮固化进历史：助手消息（已流出的正文 +
// 工具调用）加每个调用的结果——有真实结果用真实结果，没有的用 missing
// 合成错误结果，维持 tool_use/tool_result 对齐。
func (l *Loop) appendAnchor(state *loopState, text string, calls []message.ToolCall, results []executor.ToolResult, missing string) {
	anchor := message.Message{Role: message.RoleAssistant}
	if text != "" {
		anchor.Content = append(anchor.Content, message.ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range calls {
		anchor.Content = append(anchor.Content, message.ContentBlock{
			Type:      "tool_use",
			ToolUseID: tc.ID,
			ToolName:  tc.Name,
			Input:     tc.Input,
		})
	}
	l.appendMessage(state, anchor)
	for _, r := range orderResults(calls, results, missing) {
		l.appendMessage(state, message.NewToolResultMessage(r.ToolUseID, r.Content, r.IsError))
	}
}

// buildAssistantMessage 组装本轮的助手消息。provider 在完成事件里给出了
// thinking/redacted_thinking 块（带签名）时原样采用——Anthropic 要求回传
// 时附带签名；否则用流式累积的思考文本。
func buildAssistantMessage(thinking, text string, calls []message.ToolCall, complete *message.Message) message.Message {
	m := message.Message{Role: message.RoleAssistant}
	var thinkingBlocks []message.ContentBlock
	if complete != nil {
		for _, b := range complete.Content {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				thinkingBlocks = append(thinkingBlocks, b)
			}
		}
	}
	if len(thinkingBlocks) > 0 {
		m.Content = append(m.Content, thinkingBlocks...)
	} else if thinking != "" {
		m.Content = append(m.Content, message.ContentBlock{Type: "thinking", Thinking: thinking})
	}
	if text != "" {
		m.Content = append(m.Content, message.ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range calls {
		m.Content = append(m.Content, message.ContentBlock{
			Type:      "tool_use",
			ToolUseID: tc.ID,
			ToolName:  tc.Name,
			Input:     tc.Input,
		})
	}
	return m
}

// orderResults 按工具调用顺序排列结果，每个调用恰好一个结果；缺失的
// 合成为 missing 错误结果。
func orderResults(calls []message.ToolCall, results []executor.ToolResult, missing string) []executor.ToolResult {
	byID := make(map[string]executor.ToolResult, len(results))
	for _, r := range results {
		if _, dup := byID[r.ToolUseID]; !dup {
			byID[r.ToolUseID] = r
		}
	}
	ordered := make([]executor.ToolResult, 0, len(calls))
	for _, tc := range calls {
		if r, ok := byID[tc.ID]; ok {
			ordered = append(ordered, r)
			continue
		}
		ordered = append(ordered, executor.ToolResult{ToolUseID: tc.ID, Name: tc.Name, Content: missing, IsError: true})
	}
	return ordered
}

// truncateResult 把超过 limit 字节的结果截断到 UTF-8 字符边界并注明。
func truncateResult(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n... (结果被截断：原始 %d 字节，保留前 %d 字节)", len(s), cut)
}

// reportToolResults 上报工具结果：事件流 + Observer + 开始时间清理。
// 流式消费中 Poll 的增量上报、轮末 Wait 的收尾上报与流中断恢复的
// 收集上报共用此路径。
func (l *Loop) reportToolResults(ctx context.Context, state *loopState, results []executor.ToolResult, out chan<- Event) {
	for _, result := range results {
		l.logDebug("tool_done: %s id=%s err=%v content_len=%d", result.Name, result.ToolUseID, result.IsError, len(result.Content))
		out <- Event{Type: EvtToolDone, ToolName: result.Name, ToolResult: result.Content, ToolUseID: result.ToolUseID, ToolIsError: result.IsError}
		var duration time.Duration
		if startTime, ok := state.toolStartTimes[result.ToolUseID]; ok {
			duration = time.Since(startTime)
			delete(state.toolStartTimes, result.ToolUseID)
		}
		if l.cfg.Observer == nil {
			continue
		}
		if result.IsError {
			l.cfg.Observer.OnToolError(observer.WithToolCallID(ctx, result.ToolUseID), result.Name, nil, fmt.Errorf("%s", result.Content), duration)
		} else {
			l.cfg.Observer.OnToolDone(observer.WithToolCallID(ctx, result.ToolUseID), result.Name, nil, result.Content, duration)
		}
	}
}

// buildPreCheck 构建工具的 PreCheck 闭包，包含完整的权限检查链。
// 对齐 Claude Code 的 StreamingToolExecutor.executeTool() 内置权限逻辑。
// 检查链：PlanChecker → Permission.Gate → Middlewares → Hooks。
// 返回 nil 表示没有任何检查需要做。
//
// transcript 是发起本次调用时的对话记录，随 ctx 交给权限门的分类器：同一个
// 权限门可能被多个并发循环共用，共享的 SetTranscript 记录不一定属于本循环。
func (l *Loop) buildPreCheck(tool *ToolEntry, toolName string, toolInput json.RawMessage, transcript []message.Message) executor.PreCheckFn {
	if l.cfg.PlanChecker == nil && l.cfg.Permission == nil && l.cfg.Hooks == nil && len(l.cfg.Middlewares) == 0 {
		return nil
	}

	return func(ctx context.Context) (string, bool) {
		// 1. Plan mode 工具过滤。
		if l.cfg.PlanChecker != nil && l.cfg.PlanChecker.IsActive() {
			if !l.cfg.PlanChecker.IsToolAllowed(tool.Permission) {
				return fmt.Sprintf("Plan mode 下禁止使用工具 %q（仅允许只读工具和 ExitPlanMode）", toolName), true
			}
		}

		// 2. 中间件 BeforeTool（先于用户审批：能被策略拒绝的调用不该打扰用户）。
		for _, mw := range l.cfg.Middlewares {
			if d := mw.BeforeTool(ctx, toolName, toolInput); d != nil && !d.Allow {
				return fmt.Sprintf("被中间件阻止: %s", d.Reason), true
			}
		}

		// 3. Hooks PreToolUse。
		if l.cfg.Hooks != nil {
			block, msg, err := l.cfg.Hooks.RunPreToolUse(ctx, toolName, toolInput)
			if err != nil {
				return fmt.Sprintf("hook 错误: %s", err.Error()), true
			}
			if block {
				return fmt.Sprintf("被 hook 阻止: %s", msg), true
			}
		}

		// 4. Permission Gate 检查（可能阻塞等待用户审批；ctx 取消即拒绝）。
		if l.cfg.Permission != nil {
			allowed, reason := l.cfg.Permission.CheckContext(permission.WithTranscript(ctx, transcript), toolName, string(toolInput), tool.Permission)
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

		return "", false
	}
}

// truncateErr 截断错误信息用于进度展示（429 的 message 带 request id 很长）。
func truncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 找个 UTF-8 安全的截断点
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}
