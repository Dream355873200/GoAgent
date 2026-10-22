// Package goagent is an AI Agent application framework.
//
// It encapsulates production-grade mechanisms (context compression, permission
// management, memory, streaming tool execution) derived from the Claude Code
// architecture, so that users only need to define tools and business logic.
//
// Quick start:
//
//	app := goagent.New(
//	    goagent.WithProvider(goagent.Anthropic(apiKey)),
//	    goagent.WithSystemPrompt("You are a helpful assistant"),
//	)
//	app.Tool("greet", goagent.ToolDef{
//	    Description: "Greet someone",
//	    Input:       GreetInput{},
//	    Permission:  goagent.ReadOnly,
//	    Execute:     func(ctx goagent.Context, in GreetInput) (string, error) { ... },
//	})
//	cli.Run(app) // 终端界面：github.com/Dream355873200/GoAgent/cli
package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Dream355873200/GoAgent/analytics"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/budget"
	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/cost"
	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/hooks"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/mcp"
	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/plan"
	"github.com/Dream355873200/GoAgent/prompts"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/reminder"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/sessionmem"
	"github.com/Dream355873200/GoAgent/statedir"
	"github.com/Dream355873200/GoAgent/sysprompt"
	"github.com/Dream355873200/GoAgent/task"
)

// App is the central object of a GoAgent application.
// Create one with New(), register tools, then run.
type App struct {
	mu          sync.RWMutex
	provider    provider.Provider
	fallback    provider.Provider
	logger      *slog.Logger // 库内日志（WithLogger 注入；默认 slog.Default()）
	tools       map[string]*registeredTool
	toolOrder   []string // insertion order
	middlewares []MiddlewareWithFilter
	config      appConfig

	// 内部子系统（由 New() 按需初始化）。
	taskStore   task.StoreInterface   // Task 系统存储（WithTaskTools 启用时初始化）
	planMgr     plan.StoreInterface   // Plan Mode 管理器（WithPlanTools 启用时初始化）
	hooksMgr    *hooks.Manager        // Hooks 管理器
	bgTaskMgr   *bgtask.Manager       // 内置后台任务管理器（自定义存储时为 nil）
	bgTaskStore bgtask.StoreInterface // 后台任务存储（内置 Manager 或宿主自定义）
	mcpClients  []*mcp.Client         // MCP 客户端列表

	// runtimes 是各会话跨运行常驻的状态：sessionID → *sessionRuntime（见
	// sessionruntime.go；CloseSession 释放）。
	runtimes sync.Map

	// Pipeline DAG 编排（可选）
	pipelineCfg *PipelineConfig // UsePipeline 保存的配置；每次 RunPipeline 新建运行实例

	// HTTP/WebSocket 处理器（可选）
	askUserHandler     *AskUserHandler     // 异步提问处理器
	planConfirmHandler *PlanConfirmHandler // 异步计划确认处理器
	interruptHandler   *InterruptHandler   // 中断处理器

	// 可观测性子系统
	costTracker      *cost.Tracker            // 成本追踪器
	analyticsTracker *analytics.Tracker       // 分析追踪器
	obsRegistry      *observer.SyncObservable // Observer 注册表

	// 全局权限模式的运行时覆写：-1 = 未覆写（沿用构造配置）。
	permModeOverride atomic.Int32

	// gates 登记所有进行中运行的权限门（见 acquireGate），使模式切换与
	// Guard 增删即时作用于它们——多个会话可以同时在跑。
	gatesMu sync.Mutex
	gates   map[*permission.Gate]*liveGate
}

// registeredTool wraps a user-defined tool with its generated schema.
type registeredTool struct {
	def         ToolDef
	inputSchema any // generated JSON Schema
}

// New creates a new App with the given options.
func New(opts ...Option) *App {
	app := &App{
		tools:  make(map[string]*registeredTool),
		config: defaultConfig(),
	}
	app.permModeOverride.Store(-1) // -1 = 未覆写，沿用构造配置
	for _, opt := range opts {
		opt.apply(&app.config)
	}
	app.provider = app.config.provider
	app.fallback = app.config.fallback

	// 库内日志：WithLogger 注入，默认 slog.Default()（宿主 slog.SetDefault 可全局接管）。
	app.logger = app.config.logger
	if app.logger == nil {
		app.logger = slog.Default()
	}

	// 从 ProviderConfig 自动创建 provider（如果未直接设置 provider）。
	if app.provider == nil && app.config.providerConfig != nil {
		app.provider = BuildProvider(*app.config.providerConfig)
	}

	// 初始化 Observer 注册表。
	app.obsRegistry = observer.NewSyncObservable()

	// 初始化成本追踪（WithCostTracking）。
	if app.config.costTracking {
		app.costTracker = cost.NewTracker()
		app.obsRegistry.Add(&cost.Observer{Tracker: app.costTracker})
	}

	// 初始化分析追踪（WithAnalytics）。
	if app.config.analyticsTracking {
		app.analyticsTracker = analytics.NewTracker()
		app.obsRegistry.Add(&analytics.Observer{Tracker: app.analyticsTracker})
	}

	// 注册用户传入的 Observer。
	for _, obs := range app.config.observers {
		app.obsRegistry.Add(obs)
	}

	// 初始化内部子系统。
	app.hooksMgr = hooks.NewManager()
	for _, h := range app.config.hooks {
		app.hooksMgr.Register(h)
	}

	// 后台任务系统（WithBgTaskTools 启用时才初始化）。
	if app.config.enableBgTaskTools {
		var store bgtask.StoreInterface
		if app.config.bgTaskStore != nil {
			store = app.config.bgTaskStore
			// 自定义存储不是内置 Manager 时，终态通知等依赖 Manager 的能力不可用。
			app.bgTaskMgr, _ = store.(*bgtask.Manager)
		} else {
			app.bgTaskMgr = bgtask.NewManager("")
			store = app.bgTaskMgr
		}
		app.bgTaskStore = store
		if bgTaskToolsProvider != nil {
			app.useToolsSkipDuplicates(bgTaskToolsProvider(store)...)
		}
		// 终态自动通知进主循环：任务完成/失败/终止 → steering queue
		// 车道注入（RunSession 结束后自动续跑，模型立即知晓结果并跟进，
		// 无需用户轮询催促；无活跃会话 ID 的任务由宿主自行处理）。
		// 空闲会话的通知暂存队列，下次 RunSession 消费。
		if app.config.steering != nil && app.bgTaskMgr != nil {
			mgr := app.bgTaskMgr
			hub := app.config.steering
			mgr.OnTaskDone = func(t bgtask.TaskState) {
				if t.SessionID == "" {
					return
				}
				hub.Enqueue(t.SessionID, reminder.Wrap(reminder.SourceHost, bgTaskDoneText(t)))
			}
		}
	}

	// Task 系统（WithTaskTools 启用时才初始化）。
	if app.config.enableTaskTools {
		if app.config.taskStore != nil {
			app.taskStore = app.config.taskStore
		} else {
			app.taskStore = task.NewStore()
		}
		if taskToolsProvider != nil {
			app.useToolsSkipDuplicates(taskToolsProvider(app.taskStore)...)
		}
	}

	// Issue 工具（WithIssueTools 启用时才注册）。
	// 与 task 同存储：WithTaskTools 同时启用时复用同一个 store（含会话分区），
	// 单独启用时自建。不复用则两个工具各写各的存储，issue 记录和任务列表对不上。
	if app.config.enableIssueTools {
		store := app.taskStore
		if store == nil {
			if app.config.taskStore != nil {
				store = app.config.taskStore
			} else {
				store = task.NewStore()
			}
		}
		if issueToolsProvider != nil {
			app.useToolsSkipDuplicates(issueToolsProvider(store)...)
		}
	}

	// Plan 系统（WithPlanTools 启用时才初始化）。
	if app.config.enablePlanTools {
		if app.config.planStore != nil {
			app.planMgr = app.config.planStore
		} else {
			app.planMgr = plan.NewManager(statedir.Join("", "plans"))
		}
		if planToolsProvider != nil {
			app.useToolsSkipDuplicates(planToolsProvider(app.planMgr)...)
		}
	}

	// AskUser 工具（WithAskTools 启用时才注册）。
	if app.config.enableAskTools && askToolsProvider != nil {
		app.useToolsSkipDuplicates(askToolsProvider()...)
	}

	// 自动注册内置工具（WithBuiltinTools）。
	if app.config.builtinTools {
		app.autoRegisterBuiltinTools()
	}

	// 注册子 agent 工具（WithSubAgents）：与 App.AgentTool 同一实现。
	if len(app.config.subAgentDefs) > 0 && app.provider == nil {
		app.logger.Warn("goagent: WithSubAgents 需要 provider，子 agent 工具未注册")
	}
	if app.provider != nil {
		for _, def := range app.config.subAgentDefs {
			app.useToolsSkipDuplicates(NamedTool{Name: "Agent_" + def.Name, Def: app.AgentTool(def)})
		}
	}

	// 注册 ToolKit 工具（WithToolKits）。
	for _, kit := range app.config.toolKits {
		app.useToolsSkipDuplicates(kit.Tools()...)
	}

	// 初始化 MCP 服务器并注册其工具。
	if len(app.config.mcpServers) > 0 {
		app.initMCP()
	}

	return app
}

// bgTaskDoneText 后台任务终态通知正文（host 来源 system-reminder）。
func bgTaskDoneText(t bgtask.TaskState) string {
	desc := t.Description
	if t.Command != "" {
		desc = t.Command
	}
	switch t.Status {
	case bgtask.StatusCompleted:
		preview := t.Result
		if len(preview) > 300 {
			preview = preview[:300] + "…"
		}
		if preview == "" {
			return fmt.Sprintf("后台任务「%s」已完成（task_id=%s），完整输出可用 TaskOutput 工具获取。", desc, t.ID)
		}
		return fmt.Sprintf("后台任务「%s」已完成（task_id=%s）。结果摘要：\n%s\n完整输出可用 TaskOutput 工具获取。", desc, t.ID, preview)
	case bgtask.StatusKilled:
		return fmt.Sprintf("后台任务「%s」已终止（task_id=%s）。", desc, t.ID)
	default:
		return fmt.Sprintf("后台任务「%s」已失败（task_id=%s）：\n%s\n详情可用 TaskOutput 工具获取。", desc, t.ID, t.Error)
	}
}

// Tool registers a tool with the given name and definition.
// The ToolDef.Input struct is reflected into a JSON Schema automatically.
// Panics if a tool with the same name is already registered, or if Execute
// does not have the signature func(goagent.Context, T) (string, error).
func (a *App) Tool(name string, def ToolDef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.registerToolLocked(name, def)
}

// registerToolLocked 注册单个工具的内部实现，调用方必须已持有 a.mu。
// 抽出此函数是为了让 SetTools 在持锁状态下批量注册，避免重入 a.mu.Lock() 死锁。
func (a *App) registerToolLocked(name string, def ToolDef) {
	if _, exists := a.tools[name]; exists {
		panic(fmt.Sprintf("goagent: tool %q already registered", name))
	}
	if err := validateExecute(def); err != nil {
		panic(fmt.Sprintf("goagent: tool %q: %v", name, err))
	}

	a.tools[name] = &registeredTool{
		def:         def,
		inputSchema: inputSchemaOf(def),
	}
	a.toolOrder = append(a.toolOrder, name)
}

// UseTools registers multiple tools at once.
func (a *App) UseTools(tools ...NamedTool) {
	for _, t := range tools {
		a.Tool(t.Name, t.Def)
	}
}

// useToolsSkipDuplicates 供 New() 装配内置工具、ToolKit 时使用：与已注册
// 工具重名的跳过并告警（例如 WithBuiltinTools 与 WithToolKits(builtin.FileKit())
// 同时启用），而不是在构造期 panic。
func (a *App) useToolsSkipDuplicates(tools ...NamedTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range tools {
		if _, exists := a.tools[t.Name]; exists {
			a.logger.Warn("goagent: 工具重名，跳过后注册的同名工具", "tool", t.Name)
			continue
		}
		a.registerToolLocked(t.Name, t.Def)
	}
}

// NamedTool pairs a name with a ToolDef for batch registration.
type NamedTool struct {
	Name string
	Def  ToolDef
}

// Use adds middleware(s) to the app.
// Middleware is called in the order registered.
//
// Use WithTools() to restrict a middleware to specific tools:
//
//	app.Use(&RateLimitMiddleware{}, WithTools("Bash", "Write"))
func (a *App) Use(mw Middleware, opts ...MiddlewareOption) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cfg := &middlewareConfig{middleware: mw}
	for _, opt := range opts {
		opt.apply(cfg)
	}

	a.middlewares = append(a.middlewares, MiddlewareWithFilter{
		Middleware: cfg.middleware,
		Filter:     cfg.filter,
	})
}

// autoRegisterBuiltinTools 自动注册所有内置工具。
// 通过 builtinToolsProvider 延迟注入，避免 import cycle。
func (a *App) autoRegisterBuiltinTools() {
	if builtinToolsProvider != nil {
		a.useToolsSkipDuplicates(builtinToolsProvider()...)
	}
}

// ===== 全局包级变量（依赖注入模式）=====
// 以下全局变量用于打破 goagent → builtin → goagent 的 import cycle。
// builtin 包通过 init() 调用 Register*Provider 注册工具提供函数。
// 注意：多 App 实例共享这些全局变量，并发不安全。
// 未来可考虑用 interface-based registry 替代。

// builtinToolsProvider 是由 builtin 包注入的工具提供函数。
// 通过 init() 注册，避免 goagent→builtin→goagent 的循环依赖。
var builtinToolsProvider func() []NamedTool

// RegisterBuiltinToolsProvider 由 builtin 包调用以注册内置工具提供函数。
// 框架用户不需要调用此函数。
func RegisterBuiltinToolsProvider(fn func() []NamedTool) {
	builtinToolsProvider = fn
}

// taskToolsProvider 是由 builtin 包注入的 Task 工具提供函数。
var taskToolsProvider func(store task.StoreInterface) []NamedTool

// issueToolsProvider 是由 builtin 包注入的 Issue 工具提供函数。
var issueToolsProvider func(store task.StoreInterface) []NamedTool

// planToolsProvider 是由 builtin 包注入的 Plan 工具提供函数。
var planToolsProvider func(store plan.StoreInterface) []NamedTool

// bgTaskToolsProvider 是由 builtin 包注入的后台任务工具提供函数。
var bgTaskToolsProvider func(store bgtask.StoreInterface) []NamedTool

// askToolsProvider 是由 builtin 包注入的 AskUser 工具提供函数。
var askToolsProvider func() []NamedTool

// RegisterIssueToolsProvider 由 builtin 包调用以注册 Issue 工具提供函数。
// 延迟注入避免 import cycle（builtin import goagent）。
func RegisterIssueToolsProvider(fn func(store task.StoreInterface) []NamedTool) {
	issueToolsProvider = fn
}

// RegisterTaskToolsProvider 由 builtin 包调用以注册 Task 工具提供函数。
func RegisterTaskToolsProvider(fn func(store task.StoreInterface) []NamedTool) {
	taskToolsProvider = fn
}

// RegisterPlanToolsProvider 由 builtin 包调用以注册 Plan 工具提供函数。
func RegisterPlanToolsProvider(fn func(store plan.StoreInterface) []NamedTool) {
	planToolsProvider = fn
}

// RegisterBgTaskToolsProvider 由 builtin 包调用以注册后台任务工具提供函数。
func RegisterBgTaskToolsProvider(fn func(store bgtask.StoreInterface) []NamedTool) {
	bgTaskToolsProvider = fn
}

// RegisterAskToolsProvider 由 builtin 包调用以注册 AskUser 工具提供函数。
func RegisterAskToolsProvider(fn func() []NamedTool) {
	askToolsProvider = fn
}

// Run starts an agent session and returns a channel of events.
// This is the SDK/embedded mode — the caller consumes events from the channel.
// 每次调用创建新的独立会话。如需多轮对话请使用 RunSession 或 RunWithHistory。
func (a *App) Run(ctx context.Context, input string) <-chan Event {
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		a.run(ctx, input, nil, events, false)
	}()
	return events
}

// RunWithHistory 带历史消息运行 agent，支持多轮对话。
// 不依赖 SessionManager，由调用方自行管理消息的加载与持久化。
// 这是业务层自行管理对话记录的推荐方式。
//
// 调用方负责：
//   - 从自己的存储加载历史消息作为 history 参数
//   - 从事件流收集本轮产生的消息并自行持久化
//
// 示例：
//
//	// 业务层从 DB 加载历史。
//	history := loadMessagesFromDB(userID, sessionID)
//
//	// 运行 agent。
//	for ev := range app.RunWithHistory(ctx, history, "继续上次的工作") {
//	    // 收集事件，业务层自行存储。
//	    saveEventToDB(ev)
//	}
func (a *App) RunWithHistory(ctx context.Context, history []message.Message, input string) <-chan Event {
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		// 构建一个虚拟 session 来传递历史消息（不落盘：持久化由调用方负责）。
		sess := &session.Session{
			ID:       "ephemeral",
			Messages: history,
		}
		a.run(ctx, input, sess, events, false)
	}()
	return events
}

// RunWithSessionHistory 以真实会话身份运行，历史消息由宿主注入（不经 goagent
// 会话持久化——宿主有自己的存储时用这个，而非 RunWithHistory 的 "ephemeral"
// 虚拟会话）。会话 ID 真实化后，按 SessionID 路由的机制全部可用：
// steering guide 车道（SessionID ≠ "ephemeral" 才启用）、queue 车道（本轮结束
// 后依次消费排队消息自动续跑，语义与 RunSession 一致）、工具回调拿到的
// Context.SessionID 也是真实值。配置了会话管理器时，同一会话不能与
// RunSession / 另一次 RunWithSessionHistory 并发运行。
func (a *App) RunWithSessionHistory(ctx context.Context, sessionID string, history []message.Message, input string) <-chan Event {
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		if mgr := a.config.sessionManager; mgr != nil {
			if err := mgr.Acquire(sessionID); err != nil {
				events <- Event{Type: EventError, Error: err}
				return
			}
			defer mgr.Release(sessionID)
		}
		sess := &session.Session{ID: sessionID, Messages: history}
		a.run(ctx, input, sess, events, false)
		// queue 车道续跑：本轮结束后依次消费排队消息（同一会话、同一事件流），
		// 直到队列排空或 ctx 被取消（用户中断则停止消费，消息留在队列里）。
		if a.config.steering != nil {
			for ctx.Err() == nil {
				next, ok := a.config.steering.claimQueued(sessionID)
				if !ok {
					break
				}
				events <- Event{Type: EventQueueRun, Text: next}
				a.run(ctx, next, sess, events, false)
			}
		}
	}()
	return events
}

// RunSession 在指定会话中运行 agent，支持多轮对话。
// 自动加载历史消息并在结束后持久化新消息（可通过 WithAutoPersist(false) 禁用）。
// 需要先通过 WithSessionManager 配置会话管理器。
//
// 如果 sessionID 对应的会话不存在，自动创建。
// 同一个 sessionID 不能并发运行（会返回错误事件）。
//
// 示例：
//
//	// 第一轮
//	for ev := range app.RunSession(ctx, "sess-123", "帮我写个函数") { ... }
//
//	// 第二轮（自动带上第一轮的上下文）
//	for ev := range app.RunSession(ctx, "sess-123", "加上错误处理") { ... }
func (a *App) RunSession(ctx context.Context, sessionID string, input string) <-chan Event {
	events := make(chan Event, 64)
	go func() {
		defer close(events)

		mgr := a.config.sessionManager
		if mgr == nil {
			events <- Event{Type: EventError, Error: fmt.Errorf("未配置 SessionManager，请使用 WithSessionManager")}
			return
		}

		// 防止同一会话并发执行。
		if err := mgr.Acquire(sessionID); err != nil {
			events <- Event{Type: EventError, Error: err}
			return
		}
		defer mgr.Release(sessionID)

		// 获取或创建会话。
		sess, err := mgr.GetOrCreate(ctx, sessionID, nil)
		if err != nil {
			events <- Event{Type: EventError, Error: fmt.Errorf("获取会话失败: %w", err)}
			return
		}

		// 空输入 = 空闲唤醒：取队头作为本轮输入（后台任务在会话空闲时
		// 完成，通知只能等这里消费）；队列空则无事可做。
		if input == "" {
			if a.config.steering == nil {
				return
			}
			next, ok := a.config.steering.claimQueued(sessionID)
			if !ok {
				return
			}
			events <- Event{Type: EventQueueRun, Text: next}
			input = next
		}

		// 更新状态为运行中。
		_ = mgr.UpdateState(ctx, sessionID, sessionStateRunning)

		// 运行 agent，传入历史消息。
		a.run(ctx, input, sess, events, true)

		// queue 车道续跑：本轮结束后依次消费排队消息（同一会话、同一
		// 事件流），直到队列排空或 ctx 被取消（用户中断则停止消费，
		// 消息留在队列里等下次 RunSession）。每次续跑前发 EventQueueRun，
		// 前端把排队消息渲染为普通用户气泡并开新一轮。
		if a.config.steering != nil {
			for ctx.Err() == nil {
				next, ok := a.config.steering.claimQueued(sessionID)
				if !ok {
					break
				}
				events <- Event{Type: EventQueueRun, Text: next}
				a.run(ctx, next, sess, events, true)
			}
		}

		// 更新状态为空闲。
		_ = mgr.UpdateState(context.WithoutCancel(ctx), sessionID, sessionStateIdle)
	}()
	return events
}

// Sessions 返回会话管理器（如果已配置）。
// 开发者可用此方法在 HTTP handler 中管理会话。
//
// 示例（Gin 路由）：
//
//	r.POST("/sessions", func(c *gin.Context) {
//	    id, _ := app.Sessions().Create(ctx, nil)
//	    c.JSON(200, gin.H{"session_id": id})
//	})
//
//	r.GET("/sessions", func(c *gin.Context) {
//	    list, _ := app.Sessions().List(ctx)
//	    c.JSON(200, list)
//	})
//
//	r.DELETE("/sessions/:id", func(c *gin.Context) {
//	    app.Sessions().Delete(ctx, c.Param("id"))
//	})
func (a *App) Sessions() *session.Manager {
	return a.config.sessionManager
}

// BgTasks 返回内置后台任务管理器。
// 未启用 WithBgTaskTools、或通过 WithBgTaskStore 使用自定义存储时返回 nil
// （自定义存储经 BgTaskStore 获取）。
func (a *App) BgTasks() *bgtask.Manager {
	return a.bgTaskMgr
}

// BgTaskStore 返回后台任务存储接口。
// 需先启用 WithBgTaskTools()，否则返回 nil。
func (a *App) BgTaskStore() bgtask.StoreInterface {
	return a.bgTaskStore
}

// TaskStore 返回 Task 存储接口。
// 需先启用 WithTaskTools()，否则返回 nil。
func (a *App) TaskStore() task.StoreInterface {
	return a.taskStore
}

// PlanStore 返回 Plan 存储接口。
// 需先启用 WithPlanTools()，否则返回 nil。
func (a *App) PlanStore() plan.StoreInterface {
	return a.planMgr
}

// SuspendGate 返回挂起/恢复门闩（需先启用 WithSuspend，否则 nil）。
//
// 宿主典型用法（审批等待场景）：
//
//	gate := app.SuspendGate()
//	// 发起挂起：下一次轮次边界循环阻塞，状态活在内存
//	go func() {
//	    if approved := waitForApproval(); approved {
//	        gate.Resume()     // 唤醒续跑
//	    } else {
//	        gate.Terminate()  // 终止
//	    }
//	}()
//
// 注意 WithSuspend() 作为 Option 无法把 gate 返回给宿主——正确姿势是
// New(WithSuspend()) 之后经本方法取引用。
func (a *App) SuspendGate() *loop.SuspendGate {
	return a.config.suspendGate
}

// Steering 返回插话通道（如果启用了 WithSteering）。
// 未启用返回 nil。
//
// 用法：
//
//	hub.Steer(sessionID, "插话内容")     // 活跃 run 的工具批边界注入
//	hub.Enqueue(sessionID, "排队内容")   // run 结束后自动续跑
//	hub.PendingQueued(sessionID)         // 查看排队积压
func (a *App) Steering() *SteeringHub {
	return a.config.steering
}

// Usage 返回成本追踪摘要（如果启用了 WithCostTracking）。
// 如果未启用，返回 nil。
func (a *App) Usage() *cost.CostSummary {
	if a.costTracker == nil {
		return nil
	}
	summary := a.costTracker.Summary()
	return &summary
}

// Analytics 返回使用分析摘要（如果启用了 WithAnalytics）。
// 如果未启用，返回 nil。
func (a *App) Analytics() analytics.Summary {
	if a.analyticsTracker == nil {
		return analytics.Summary{}
	}
	return a.analyticsTracker.GetSummary()
}

// ModelID 返回当前使用的模型 ID。
func (a *App) ModelID() string {
	a.mu.RLock()
	p := a.provider
	a.mu.RUnlock()
	if p == nil {
		return ""
	}
	return p.Capabilities().ModelID
}

// SetModel 动态切换当前使用的模型。
// 如果 provider 不支持运行时切换，返回 false。
func (a *App) SetModel(modelID string) bool {
	a.mu.RLock()
	p := a.provider
	a.mu.RUnlock()
	if switcher, ok := p.(provider.ModelSwitcher); ok {
		switcher.SetModel(modelID)
		return true
	}
	return false
}

// SetProvider 动态替换 LLM Provider。
// 线程安全，可在运行时调用（下次 Run 时生效）。
func (a *App) SetProvider(p provider.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
}

// SetSystemPrompt 动态替换 System Prompt。
// 线程安全，可在运行时调用（下次 Run 时生效）。
func (a *App) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.systemPrompt = prompt
}

// SetTools 动态替换全部工具（清空后重新注册）。
// 线程安全，可在运行时调用（下次 Run 时生效）。
func (a *App) SetTools(tools ...NamedTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = make(map[string]*registeredTool, len(tools))
	a.toolOrder = a.toolOrder[:0]
	for _, t := range tools {
		a.registerToolLocked(t.Name, t.Def)
	}
}

// ReplaceTool 覆盖注册单个工具（不存在则注册）。
// 典型用途：包装内置工具加应用层策略——例如在 Execute 外再包一层
// 输入拦截（领域禁令、审计、沙箱校验），保留原工具的描述与 Schema。
// 线程安全，可在运行时调用（下次 Run 时生效）。
func (a *App) ReplaceTool(name string, def ToolDef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.tools[name]; exists {
		a.tools[name] = &registeredTool{
			def:         def,
			inputSchema: inputSchemaOf(def),
		}
		return
	}
	a.registerToolLocked(name, def)
}

// ToolNames 返回已注册工具的名称列表（按注册顺序）。
func (a *App) ToolNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, len(a.toolOrder))
	copy(names, a.toolOrder)
	return names
}

// ToolDescription 返回指定工具的描述。
func (a *App) ToolDescription(name string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if rt, ok := a.tools[name]; ok {
		return rt.def.Description
	}
	return ""
}

// TaskSummaries 返回所有任务的摘要列表。
// 供 TUI 的 /tasks 命令使用。
func (a *App) TaskSummaries() []task.ListSummary {
	if a.taskStore == nil {
		return nil
	}
	return a.taskStore.ListSummaries()
}

// session 状态常量映射（避免在 goagent 包直接引用 session.State 枚举值）。
var (
	sessionStateRunning = session.StateRunning
	sessionStateIdle    = session.StateIdle
)

// Execute runs a single agent session synchronously and returns the final text result.
// Blocks until the agent completes or the context is canceled.
func (a *App) Execute(ctx context.Context, input string) (*ExecuteResult, error) {
	var finalText string
	var totalUsage provider.Usage
	var lastErr error

	for ev := range a.Run(ctx, input) {
		switch ev.Type {
		case EventTextDelta:
			finalText += ev.Text
		case EventUsageUpdate:
			if ev.Usage != nil {
				totalUsage.InputTokens += ev.Usage.InputTokens
				totalUsage.OutputTokens += ev.Usage.OutputTokens
				totalUsage.CacheReadTokens += ev.Usage.CacheReadTokens
				totalUsage.CacheCreateTokens += ev.Usage.CacheCreateTokens
			}
		case EventError:
			lastErr = ev.Error
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return &ExecuteResult{FinalText: finalText, TotalUsage: totalUsage}, nil
}

// ExecuteResult is the result of a synchronous Execute() call.
type ExecuteResult struct {
	FinalText  string
	TotalUsage provider.Usage
}

// RunHTTP starts an HTTP server with SSE streaming. Blocks until the server stops.
func (a *App) RunHTTP(addr string) error {
	return runHTTP(a, addr)
}

// SetApprover 替换审批者（WithApprover 的运行时形态），对之后开始的运行生效。
func (a *App) SetApprover(ap Approver) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.approver = ap
}

// SetSessionManager 替换会话管理器（WithSessionManager 的运行时形态）。应在
// 开始运行之前调用。
func (a *App) SetSessionManager(mgr *session.Manager) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.sessionManager = mgr
}

// MemoryDir 返回 WithMemoryDir 配置的记忆目录（未配置时为空）。
func (a *App) MemoryDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config.memoryDir
}

// SetAskUserHandler 设置异步提问处理器。
// 应在 RunHTTP 之前调用。
//
// 注意：此 handler 仅用于 HTTP/WebSocket 模式的 SSE 事件转发。
// 如需在 CLI 模式使用 AskUser 工具，请单独调用：
//
//	app.SetAskUserHandler(h)
//	builtin.SetAskUserCallback(h.Ask)
func (a *App) SetAskUserHandler(h *AskUserHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.askUserHandler = h
}

// SetPlanConfirmHandler 设置异步计划确认处理器。
// 应在 RunHTTP 之前调用。
func (a *App) SetPlanConfirmHandler(h *PlanConfirmHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planConfirmHandler = h
}

// SetInterruptHandler 设置中断处理器。
// 应在 RunHTTP 之前调用。
func (a *App) SetInterruptHandler(h *InterruptHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interruptHandler = h
}

// SetPermissionMode 运行时切换全局权限模式。立即生效：
//   - 进行中的运行：直接改写其权限门的模式（模式来自会话级设置的运行除外，
//     见 SetSessionPermissionMode、WithSessionPermissionMode）；
//   - 后续运行：构造权限门时采用此覆写（高于构造时的 WithPermissionMode）。
//
// 典型用途是宿主在 UI 上提供模式切换（如 规划/自动编辑/全部放行）。多个
// 用户共用一个 App 时，只想改变某个会话的模式应使用 SetSessionPermissionMode。
func (a *App) SetPermissionMode(mode PermissionModeOption) {
	a.permModeOverride.Store(int32(mode))
	a.eachLiveGate(func(g *permission.Gate, lg *liveGate) {
		if !lg.sessionMode {
			g.SetMode(permission.PermissionMode(mode))
		}
	})
}

// CurrentPermissionMode 返回当前生效的权限模式：运行时覆写优先，否则回退
// 构造配置；两者都未设置时为 PermissionDefault。
func (a *App) CurrentPermissionMode() PermissionModeOption {
	if m := a.permModeOverride.Load(); m >= 0 {
		return PermissionModeOption(m)
	}
	if a.config.permissionMode != nil {
		return PermissionModeOption(*a.config.permissionMode)
	}
	return PermissionDefault
}

// AddPermissionGuard 运行期注册一条权限策略 Guard（单调：可拒不可放）。
// 立即对所有进行中运行的权限门生效（从各自的下一次工具调用起），并记入
// 构造配置——后续运行构造新权限门时延续。同名覆盖。
func (a *App) AddPermissionGuard(gd permission.Guard) {
	a.mu.Lock()
	guards := make([]permission.Guard, 0, len(a.config.permissionGuards)+1)
	replaced := false
	for _, old := range a.config.permissionGuards {
		if old.Name == gd.Name {
			old = gd // 同名覆盖
			replaced = true
		}
		guards = append(guards, old)
	}
	if !replaced {
		guards = append(guards, gd)
	}
	a.config.permissionGuards = guards
	a.mu.Unlock()
	a.eachLiveGate(func(g *permission.Gate, _ *liveGate) { g.AddGuard(gd) })
}

// RemovePermissionGuard 运行期注销一条权限策略 Guard。
func (a *App) RemovePermissionGuard(name string) {
	a.mu.Lock()
	kept := make([]permission.Guard, 0, len(a.config.permissionGuards))
	for _, gd := range a.config.permissionGuards {
		if gd.Name != name {
			kept = append(kept, gd)
		}
	}
	// 换成新切片而不是就地删除：进行中的 run 持有旧配置的浅拷贝，就地
	// 删除会改写它正在遍历的底层数组。
	a.config.permissionGuards = kept
	a.mu.Unlock()
	a.eachLiveGate(func(g *permission.Gate, _ *liveGate) { g.RemoveGuard(name) })
}

// SetThinkingEffort 运行时切换思考强度档位（off/low/medium/high）。
// 自下一次 run 起随每次模型请求下发；进行中的轮次不回溯。
func (a *App) SetThinkingEffort(effort string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.thinkingEffort = effort
}

// CurrentThinkingEffort 返回当前思考强度档位（空 = 不干预）。
func (a *App) CurrentThinkingEffort() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config.thinkingEffort
}

// run is the internal entry point that wires up all subsystems and starts the agent loop.
// sess 为 nil 时创建无状态会话；非 nil 时使用已有历史消息。persist 为 true
// 时（RunSession）新进入对话的消息逐条写入会话存储。
func (a *App) run(ctx context.Context, input string, sess *session.Session, out chan<- Event, persist bool) {
	// 提取 SessionID（sess 可能为 nil，如 pipeline supervisor、RunWithHistory
	// 等场景）。会话级 resolver（工作目录/项目上下文/提示词/工具可见性）都
	// 以此为键，故在锁内读配置前先取。
	var sessionID string
	if sess != nil {
		sessionID = sess.ID
	}
	a.mu.RLock()
	cfg := a.config
	prov := a.provider
	fallback := a.fallback
	askHandler := a.askUserHandler
	tools := a.buildToolSet()
	// 会话级工具可见性：谓词不过滤的会话保持全量（默认）。不同会话可
	// 携带不同装配档位——工具注册表仍是进程级一份。
	if cfg.sessionToolFilterFn != nil && sessionID != "" && len(tools) > 0 {
		filtered := tools[:0:0]
		for _, e := range tools {
			if cfg.sessionToolFilterFn(sessionID, e.Name) {
				filtered = append(filtered, e)
			}
		}
		tools = filtered
	}
	// 会话级动态描述：ToolDef.SessionDescription 非空结果覆盖静态描述。
	if sessionID != "" {
		for i := range tools {
			if rt := a.tools[tools[i].Name]; rt != nil && rt.def.SessionDescription != nil {
				if d := rt.def.SessionDescription(sessionID); d != "" {
					tools[i].Description = d
				}
			}
		}
	}
	mws := make([]MiddlewareWithFilter, len(a.middlewares))
	copy(mws, a.middlewares)
	a.mu.RUnlock()

	// 子系统初始化。
	rt := a.sessionRuntime(sessionID)

	// 1. Compaction Manager（对齐 Claude Code 流程）。按会话常驻：自动摘要的
	// 熔断状态需要跨运行保留，按运行新建会让熔断每条用户消息都被重置。
	// 提示词目录按会话解析：压缩 / YOLO 分类与系统提示词同一档位，缺文件
	// 各自回退嵌入默认值——提示词组可只覆盖其中几段。
	promptDir := effectivePromptDir(cfg, sessionID)
	compCfg := compaction.Config{
		AutoCompactThreshold: cfg.compaction.AutoCompactThreshold,
		MaxResultSize:        cfg.compaction.MaxResultSize,
		PostCompact:          cfg.postCompact,
	}
	if promptDir != "" {
		compCfg.PromptFile = filepath.Join(promptDir, prompts.Compact)
	}
	compMgr := rt.compactionManager(compCfg)
	// 设置 Provider 以支持新的 Compact() 方法（对齐 Claude Code 流程）。
	if prov != nil {
		compMgr.SetProvider(prov)
	}

	// 2. Permission Gate：模式、会话共享的"始终允许"授权、规则、Guard、编辑类
	// 工具与可选分类器（见 newGate）。登记为进行中，运行期间的模式切换与
	// Guard 增删即时生效。
	permGate, releaseGate := a.acquireGate(rt)
	defer releaseGate()

	// 3. Memory Manager。项目上下文 = 全局文件 + 会话级追加（不同会话
	// 可携带不同领域规范，单进程多档位并存）。
	projectCtx := cfg.projectContextFiles
	if cfg.sessionProjectCtxFn != nil && sessionID != "" {
		projectCtx = append(append([]string{}, projectCtx...), cfg.sessionProjectCtxFn(sessionID)...)
	}
	memMgr := memory.NewManager(memory.Config{
		MemoryDir:      cfg.memoryDir,
		ProjectContext: projectCtx,
	})

	// 4. Executor。工具 panic 被执行器恢复为错误结果，这里只负责记录堆栈。
	logger := a.logger
	exec := executor.New(executor.Config{
		MaxConcurrency: cfg.maxConcurrency,
		PanicHandler: func(tool string, r any, stack []byte) {
			logger.Error("goagent: 工具 panic 已恢复", "tool", tool, "panic", fmt.Sprint(r), "stack", string(stack))
		},
	})

	// 5. Budget Tracker。
	budgetTracker := budget.NewTracker(budget.Config{
		TotalBudget: cfg.tokenBudget,
	})

	// 6. SessionMemory（会话内定期记忆提取）。
	var sessMem *sessionmem.SessionMemory
	if cfg.sessionMemoryCfg != nil && prov != nil {
		sessMem = sessionmem.New(prov, *cfg.sessionMemoryCfg)
	}

	// 提取 SessionID 已上移到 run 入口（会话级 resolver 依赖）。

	// 会话工作目录由 WithSessionWorkDir 的解析函数按会话提供——单进程多会话
	// 各自扎根不同目录（Bash 的 cmd.Dir、相对路径解析均以此为基准），
	// 未配置时为空 = 沿用进程 cwd（旧行为）。
	workDir := ""
	if cfg.sessionWorkDirFn != nil && sessionID != "" {
		workDir = cfg.sessionWorkDirFn(sessionID)
	}

	// 沙箱：WithSandbox 配置时每次 run 创建独立会话（Enter 失败发错误事件
	// 干净返回，防 BaseDir 配错崩宿主）。沙箱根非空时覆盖会话工作目录——
	// 内置工具（Bash/文件读写/git）全部落到沙箱内。见 sandbox.go 优先级规则。
	if cfg.sandbox != nil {
		sbSess, err := cfg.sandbox.Enter(ctx, sessionID, cfg.sandboxPolicy)
		if err != nil {
			out <- Event{Type: EventError, Error: fmt.Errorf("goagent: 沙箱初始化失败: %w", err)}
			return
		}
		defer func() {
			if err := sbSess.Close(); err != nil {
				a.logger.Warn("sandbox: 关闭沙箱会话失败", "error", err)
			}
		}()
		ctx = WithSandboxSession(ctx, sbSess)
		if r := sbSess.Root(); r != "" {
			workDir = r
		}
	}

	// 7. 动态 System Prompt 组装。必须在 workDir 定型之后——环境信息段
	// 要告知模型当前工作目录（见 buildSystemPrompt）。
	systemPrompt := a.buildSystemPrompt(cfg, memMgr, workDir, sessionID)

	// 会话上下文注入：SessionID + 工作目录经 context value 流经整个
	// 执行链（loop → executor → 工具的 goagent.Context 字段）。
	// 工作目录由 WithSessionWorkDir 的解析函数按会话提供——单进程多会话
	// 各自扎根不同目录（Bash 的 cmd.Dir、相对路径解析均以此为基准），
	// 未配置时为空 = 沿用进程 cwd（旧行为）。
	ctx = WithSessionContext(ctx, sessionID, workDir)

	// RAG 形态 1（WithRetrieval）：run 前置固定流程——retrieve→rerank→
	// 拼进用户输入，agent 无感知。检索失败 fail-closed（见 WithRetrieval）。
	if cfg.retrieval != nil {
		var err error
		input, err = a.injectRetrieval(ctx, cfg.retrieval, input, out)
		if err != nil {
			out <- Event{Type: EventError, Error: err}
			return
		}
	}

	// 构建 loop 配置。
	// Observer 合并 ctx 边界注入链（observer.IntoContext）：benchmark 等
	// 单次 run 临时采集的 observer 与 App 级 observer 同时收到事件。
	hooksRunner := &hooksRunnerAdapter{mgr: a.hooksMgr}
	loopMws := toLoopMiddlewares(mws)
	loopCfg := loop.Config{
		Provider:     prov,
		Fallback:     fallback,
		Tools:        tools,
		Middlewares:  loopMws,
		SystemPrompt: systemPrompt,
		MaxTurns:     cfg.maxTurns,
		Compaction:   compMgr,

		ReasoningEffort: cfg.thinkingEffort,
		Permission:      permGate,
		// Memory 不下传：memMgr 的上下文段已由 buildSystemPrompt 组装进
		// systemPrompt，循环再追加一遍会让项目上下文重复出现。
		Executor:    exec,
		Budget:      budgetTracker,
		Hooks:       hooksRunner,
		Observer:    observer.ResolveObserver(ctx, a.obsRegistry.Observer()),
		SessionID:   sessionID,
		Logger:      a.logger,
		Debug:       cfg.debug,
		SuspendGate: cfg.suspendGate,

		SkipInputValidation: cfg.skipInputValidation,
	}

	// PlanChecker 仅在 Plan 系统启用时注入。
	if a.planMgr != nil {
		loopCfg.PlanChecker = &planCheckerAdapter{mgr: a.planMgr}
	}

	// 本次运行的作用域（检查链与观测出口）由循环随 ctx 交给每一次工具执行，
	// 在运行中派生的子运行（create_pipeline、run_team、子 agent）经
	// loop.NewChild 继承。会话运行时与提问处理器同样随 ctx 下发：工具的
	// Store/Load 读写本会话的键值存储，内置 AskUser 工具按会话路由提问。
	ctx = withSessionRuntime(ctx, rt)
	ctx = withAskUserHandler(ctx, askHandler)

	// 插话通道：仅真实会话（sess 非空且非 ephemeral）接线。beginRun
	// 打开 guide 车道，run 结束（含 error/interrupt 退出）时 endRun
	// 关闭并把未消费的插话降级进 queue 车道——插话永不丢失。
	if cfg.steering != nil && sess != nil && sessionID != "" && sessionID != "ephemeral" {
		loopCfg.Steering = steerSourceAdapter{hub: cfg.steering, sessionID: sessionID}
		cfg.steering.beginRun(sessionID)
		defer cfg.steering.endRun(sessionID)
	}

	// 注册 PostSampling hook（SessionMemory）。
	if sessMem != nil {
		loopCfg.PostSampling = &sessionMemHookAdapter{sm: sessMem}
	}

	// 如果有 session，传入历史消息。
	// 先修复孤立的 tool_use（中断/异常退出残留）：assistant 消息带
	// tool_calls 却没有配对的 tool 消息，上游 API 会整请求 400 拒绝——
	// 会话从此每一轮都报错。修复后本次运行的落盘序列也是对齐的。
	if sess != nil && len(sess.Messages) > 0 {
		sess.Messages = session.EnsureToolResultPairing(sess.Messages)
		loopCfg.InitialMessages = sess.Messages
	}

	// 持久化：每条消息进入对话时由循环同步回调、逐条追加——先于任何压缩
	// 改写，写入的是完整原貌；进程被杀时已发生的步骤全部保留。自动摘要以
	// 摘要记录的形式同样经这里落盘，重新加载时由 compaction.Project 直接还原
	// 模型视图；其他派生消息不进入对话记录。
	// 用户中断后的收尾消息同样要落盘，所以写入不随运行 ctx 取消。
	if persist && sess != nil && cfg.sessionManager != nil && (cfg.autoPersist == nil || *cfg.autoPersist) {
		storeCtx := context.WithoutCancel(ctx)
		mgr := cfg.sessionManager
		id := sess.ID
		loopCfg.OnMessage = func(m message.Message) {
			if err := mgr.AppendMessage(storeCtx, id, m); err != nil {
				logger.Warn("goagent: 会话消息落盘失败", "session", id, "error", err)
			}
		}
	}

	agentLoop := loop.New(loopCfg)
	for ev := range agentLoop.Run(ctx, input) {
		pubEv := toPublicEvent(ev)
		// EventDone 时附带完整对话记录（含摘要记录），供业务层持久化。
		if pubEv.Type == EventDone {
			pubEv.Messages = agentLoop.FinalMessages()
		}
		if pubEv.Type == EventTurnComplete && cfg.transcriptSink != nil {
			cfg.transcriptSink(sessionID, agentLoop.FinalMessages())
		}
		out <- pubEv
	}
	finalMsgs := agentLoop.FinalMessages()
	if cfg.transcriptSink != nil && len(finalMsgs) > 0 {
		cfg.transcriptSink(sessionID, finalMsgs)
	}

	// 会话对象推进到本轮结束时的上下文：同一事件流里的 queue 车道续跑
	// （RunWithSessionHistory / RunSession 复用同一个 sess）必须看到上一轮
	// 的对话，否则排队消息开跑时模型对刚才发生的事一无所知。
	if sess != nil && len(finalMsgs) > 0 {
		sess.Messages = finalMsgs
	}
}

// buildSystemPrompt 使用 sysprompt.Builder 动态组装系统提示词。
// workDir 是本次 run 的工作目录（会话目录 > 沙箱根；空 = 进程 cwd），
// 注入环境信息段告知模型——不告知会导致模型猜路径（如 /workspace）
// 并误用当前平台不存在的命令（如 Windows cmd 下跑 pwd）。
func (a *App) buildSystemPrompt(cfg appConfig, memMgr *memory.Manager, workDir, sessionID string) string {
	builder := sysprompt.NewBuilder()
	promptDir := effectivePromptDir(cfg, sessionID)

	if cfg.systemPrompt != "" {
		// 传统模式：用户提供的纯字符串
		builder.AddBasePrompt(cfg.systemPrompt)
	} else {
		// 默认：加载 prompt 体系（promptDir 优先，否则嵌入默认值）
		builder.AddSection("identity", resolvePrompt(promptDir, prompts.Identity), nil)
		builder.AddSection("doing_tasks", resolvePrompt(promptDir, prompts.DoingTasks), nil)
		builder.AddSection("actions", resolvePrompt(promptDir, prompts.Actions), nil)
		builder.AddSection("using_tools", resolvePrompt(promptDir, prompts.UsingTools), nil)
		builder.AddSection("tone_style", resolvePrompt(promptDir, prompts.ToneStyle), nil)
		builder.AddSection("output_efficiency", resolvePrompt(promptDir, prompts.OutputEfficiency), nil)
		builder.AddSection("reminder", resolvePrompt(promptDir, prompts.Reminder), nil)
	}

	// 角色卡（WithSessionRoleCard）：团队/多身份会话的持久身份层，紧随
	// 基础身份段——「你是谁、会什么、能用什么工具、向谁汇报」。
	if cfg.sessionRoleCardFn != nil && sessionID != "" {
		if card := cfg.sessionRoleCardFn(sessionID); card != "" {
			builder.AddSection("role_card", card, nil)
		}
	}

	// 环境信息（平台/Shell/工作目录）：无条件注入——工作目录是模型的
	// 寻址基准（对齐 Claude Code 的 Working directory 行）。会话目录未
	// 提供（未配 WithSessionWorkDir）时回退进程 cwd。
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	builder.SetProjectDir(workDir)
	builder.AddEnvironmentInfo()

	// Git 状态（WithGitContext 启用时才注入）。仓库根从工作目录探测——
	// 多项目会话场景下进程 cwd 与会话目录不同，git 上下文必须对齐后者。
	if cfg.enableGitStatus {
		root := sysprompt.DetectGitRootIn(workDir)
		if root != "" {
			builder.SetGitRoot(root)
		}
		builder.AddGitStatus()
	}

	// 当前日期。
	builder.AddCurrentDate()

	// Memory 注入（CLAUDE.md 内容）。
	if memMgr != nil {
		if suffix := memMgr.BuildSystemPromptSuffix(); suffix != "" {
			builder.AddMemory(suffix)
		}
	}

	text, _ := builder.Build()
	return text
}

// effectivePromptDir 会话级提示词目录：解析非空时覆盖全局 promptDir（不同
// 会话可并行携带不同提示词档位；空结果保持全局值）。
func effectivePromptDir(cfg appConfig, sessionID string) string {
	if cfg.sessionPromptDirFn != nil && sessionID != "" {
		if d := cfg.sessionPromptDirFn(sessionID); d != "" {
			return d
		}
	}
	return cfg.promptDir
}

// resolvePrompt 按 promptDir > 嵌入默认值 的优先级解析 prompt。
func resolvePrompt(promptDir, embedName string) string {
	if promptDir != "" {
		if data, err := os.ReadFile(filepath.Join(promptDir, embedName)); err == nil {
			return string(data)
		}
	}
	return prompts.MustLoad(embedName)
}

// GetSystemPrompt 返回当前的完整 system prompt（供用户查看/调试）。
// 注意：不含 run 级动态信息（会话工作目录）——实际 run 时会按会话注入。
func (a *App) GetSystemPrompt() string {
	return a.buildSystemPrompt(a.config, nil, "", "")
}

// SessionSystemPrompt 按会话解析的 system prompt（会话提示词目录 + 会话
// 工作目录；供宿主预览某会话生效的提示词）。不含沙箱根与 memory 段。
func (a *App) SessionSystemPrompt(sessionID string) string {
	cfg := a.config
	workDir := ""
	if cfg.sessionWorkDirFn != nil && sessionID != "" {
		workDir = cfg.sessionWorkDirFn(sessionID)
	}
	return a.buildSystemPrompt(cfg, nil, workDir, sessionID)
}

// buildToolSet converts registered tools into the format needed by the loop.
func (a *App) buildToolSet() []loop.ToolEntry {
	entries := make([]loop.ToolEntry, 0, len(a.toolOrder))
	for _, name := range a.toolOrder {
		rt := a.tools[name]

		entry := loop.ToolEntry{
			Name:               name,
			Description:        rt.def.Description,
			InputSchema:        rt.inputSchema,
			Permission:         permission.Level(rt.def.Permission),
			Concurrent:         rt.def.Concurrent,
			Class:              rt.def.Effect,
			MaxResultSizeChars: rt.def.MaxResultSizeChars,
			InterruptBehavior:  rt.def.InterruptMode,
			ExecuteFn: func(ctx context.Context, input json.RawMessage) (string, error) {
				return rt.def.call(ctx, input)
			},
		}

		entries = append(entries, entry)
	}
	return entries
}

// toLoopMiddlewares adapts public MiddlewareWithFilter list to internal loop middleware.
func toLoopMiddlewares(mws []MiddlewareWithFilter) []loop.Middleware {
	result := make([]loop.Middleware, 0, len(mws))
	for _, mw := range mws {
		result = append(result, &middlewareAdapter{inner: mw})
	}
	return result
}

// middlewareAdapter wraps a public MiddlewareWithFilter into a loop.Middleware.
type middlewareAdapter struct {
	inner MiddlewareWithFilter
}

func (a *middlewareAdapter) BeforeTool(ctx context.Context, toolName string, input json.RawMessage) *loop.Decision {
	// Check if this middleware applies to the current tool
	if !a.inner.matches(toolName) {
		return nil // skip this middleware
	}

	c := newContextFromStd(ctx)
	d := a.inner.Middleware.BeforeTool(c, toolName, input)
	if d == nil {
		return nil
	}
	return &loop.Decision{
		Allow:  d.Allow,
		Reason: d.Reason,
	}
}

func (a *middlewareAdapter) AfterTool(ctx context.Context, toolName string, result string, err error) {
	if !a.inner.matches(toolName) {
		return
	}
	c := newContextFromStd(ctx)
	a.inner.AfterTool(c, toolName, &Result{Content: result, IsError: err != nil}, err)
}

// toPublicEvent converts an internal loop event to a public Event.
func toPublicEvent(ev loop.Event) Event {
	return Event{
		Type:        EventType(ev.Type),
		Text:        ev.Text,
		Thinking:    ev.Thinking,
		ToolName:    ev.ToolName,
		ToolInput:   ev.ToolInput,
		ToolUseID:   ev.ToolUseID,
		ToolResult:  ev.ToolResult,
		ToolIsError: ev.ToolIsError,
		StatusKey:   ev.StatusKey,
		Usage:       ev.Usage,
		Error:       ev.Error,

		AgentID:       ev.AgentID,
		AgentDesc:     ev.AgentDesc,
		AgentStatus:   ev.AgentStatus,
		AgentActivity: ev.AgentActivity,
		AgentToolUses: ev.AgentToolUses,
		AgentTokens:   ev.AgentTokens,
	}
}

// approverAdapter wraps a public goagent.Approver into a permission.Approver.
// 宿主审批器实现了 ContextApprover 时，审批在工具调用的 ctx 下进行（ctx 带
// 会话 ID，取消即应拒绝）。
type approverAdapter struct {
	inner Approver
}

func (a *approverAdapter) Approve(toolName string, input string, level permission.Level) (bool, bool) {
	return a.inner.Approve(toolName, input, Permission(level))
}

func (a *approverAdapter) ApproveContext(ctx context.Context, toolName string, input string, level permission.Level) (bool, bool) {
	if ca, ok := a.inner.(ContextApprover); ok {
		return ca.ApproveWithContext(ctx, toolName, input, Permission(level))
	}
	return a.inner.Approve(toolName, input, Permission(level))
}

// wrapApprover wraps a public Approver into a permission.Approver.
// Returns nil if the input is nil (permission.NewGate handles nil safely).
func wrapApprover(a Approver) permission.Approver {
	if a == nil {
		return nil
	}
	return &approverAdapter{inner: a}
}

// hooksRunnerAdapter 将 hooks.Manager 适配为 loop.HooksRunner 接口。
// 这层适配是必要的：internal/loop 不能直接 import hooks 包（避免循环依赖），
// 所以通过接口解耦，由 goagent.go 这层做桥接。
type hooksRunnerAdapter struct {
	mgr *hooks.Manager
}

func (h *hooksRunnerAdapter) RunPreToolUse(ctx context.Context, toolName string, input json.RawMessage) (block bool, message string, err error) {
	result, err := h.mgr.RunPreToolUse(ctx, toolName, input, SessionIDFromContext(ctx))
	if err != nil {
		return false, "", err
	}
	if result != nil && result.Block {
		return true, result.Message, nil
	}
	return false, "", nil
}

func (h *hooksRunnerAdapter) RunPostToolUse(ctx context.Context, toolName string, input json.RawMessage, result string, toolErr error) {
	// PostToolUse 不阻塞，忽略错误（仅日志用途）。
	_ = h.mgr.RunPostToolUse(ctx, toolName, input, result, toolErr, SessionIDFromContext(ctx))
}

func (h *hooksRunnerAdapter) RunStop(ctx context.Context, stopReason, lastText string, messages []message.Message) (block bool, message string, err error) {
	result, err := h.mgr.RunStop(ctx, SessionIDFromContext(ctx), stopReason, lastText, messages)
	if err != nil {
		return false, "", err
	}
	if result != nil && result.Block {
		return true, result.Message, nil
	}
	return false, "", nil
}

// planCheckerAdapter 将 plan.StoreInterface 适配为 loop.PlanChecker 接口。
// plan.StoreInterface.IsToolAllowed 接收 string 权限名，
// loop.PlanChecker.IsToolAllowed 接收 permission.Level（int），
// 此适配器做 Level → string 的映射。
type planCheckerAdapter struct {
	mgr plan.StoreInterface
}

func (p *planCheckerAdapter) IsActive() bool {
	return p.mgr.IsActive()
}

func (p *planCheckerAdapter) IsToolAllowed(level permission.Level) bool {
	// plan.Manager.IsToolAllowed 使用字符串权限名。
	var permStr string
	switch level {
	case permission.LevelReadOnly:
		permStr = "ReadOnly"
	default:
		permStr = "Write" // Normal/RequireApproval/Dangerous 都视为写操作
	}
	return p.mgr.IsToolAllowed(permStr)
}

// sessionMemHookAdapter 将 sessionmem.SessionMemory 适配为 loop.PostSamplingHook 接口。
type sessionMemHookAdapter struct {
	sm *sessionmem.SessionMemory
}

func (s *sessionMemHookAdapter) AfterSampling(ctx context.Context, messages []message.Message) {
	s.sm.MaybeExtract(ctx, messages)
}
