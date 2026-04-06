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
//	app.RunCLI()
package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Dream355873200/GoAgent/agent"
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
	"github.com/Dream355873200/GoAgent/schema"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/sessionmem"
	"github.com/Dream355873200/GoAgent/sysprompt"
	"github.com/Dream355873200/GoAgent/task"
)

// App is the central object of a GoAgent application.
// Create one with New(), register tools, then run.
type App struct {
	mu          sync.RWMutex
	provider    provider.Provider
	fallback    provider.Provider
	tools       map[string]*registeredTool
	toolOrder   []string // insertion order
	middlewares []MiddlewareWithFilter
	groups      []*ToolGroup
	config      appConfig

	// 内部子系统（由 New() 按需初始化）。
	taskStore  task.StoreInterface // Task 系统存储（WithTaskTools 启用时初始化）
	planMgr    plan.StoreInterface // Plan Mode 管理器（WithPlanTools 启用时初始化）
	hooksMgr   *hooks.Manager      // Hooks 管理器
	bgTaskMgr  *bgtask.Manager     // 后台任务管理器
	mcpClients []*mcp.Client       // MCP 客户端列表

	// Pipeline DAG 编排（可选）
	pipeline *pipeline

	// HTTP/WebSocket 处理器（可选）
	askUserHandler     *AskUserHandler     // 异步提问处理器
	planConfirmHandler *PlanConfirmHandler // 异步计划确认处理器
	interruptHandler   *InterruptHandler   // 中断处理器

	// 可观测性子系统
	costTracker      *cost.Tracker            // 成本追踪器
	analyticsTracker *analytics.Tracker       // 分析追踪器
	obsRegistry      *observer.SyncObservable // Observer 注册表
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
	for _, opt := range opts {
		opt.apply(&app.config)
	}
	app.provider = app.config.provider
	app.fallback = app.config.fallback

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
		if app.config.bgTaskStore != nil {
			app.bgTaskMgr = app.config.bgTaskStore.(*bgtask.Manager)
		} else {
			app.bgTaskMgr = bgtask.NewManager("")
		}
		if bgTaskToolsProvider != nil {
			app.UseTools(bgTaskToolsProvider(app.bgTaskMgr)...)
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
			app.UseTools(taskToolsProvider(app.taskStore)...)
		}
	}

	// Plan 系统（WithPlanTools 启用时才初始化）。
	if app.config.enablePlanTools {
		if app.config.planStore != nil {
			app.planMgr = app.config.planStore
		} else {
			app.planMgr = plan.NewManager(".yume/plans")
		}
		if planToolsProvider != nil {
			app.UseTools(planToolsProvider(app.planMgr)...)
		}
	}

	// AskUser 工具（WithAskTools 启用时才注册）。
	if app.config.enableAskTools && askToolsProvider != nil {
		app.UseTools(askToolsProvider()...)
	}

	// 自动注册内置工具（WithBuiltinTools）。
	if app.config.builtinTools {
		app.autoRegisterBuiltinTools()
	}

	// 注册子 agent 工具（如果配置了）。
	if len(app.config.subAgentDefs) > 0 && app.provider != nil && subAgentToolsProvider != nil {
		app.UseTools(subAgentToolsProvider(app.provider, app.config.subAgentDefs)...)
	}

	// 注册 ToolKit 工具（WithToolKits）。
	for _, kit := range app.config.toolKits {
		app.UseTools(kit.Tools()...)
	}

	// 初始化 MCP 服务器并注册其工具。
	if len(app.config.mcpServers) > 0 {
		app.initMCP()
	}

	return app
}

// Tool registers a tool with the given name and definition.
// The ToolDef.Input struct is reflected into a JSON Schema automatically.
// Panics if a tool with the same name is already registered.
func (a *App) Tool(name string, def ToolDef) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.tools[name]; exists {
		panic(fmt.Sprintf("goagent: tool %q already registered", name))
	}

	// Generate JSON Schema from the Input struct.
	var inputSchema any
	if def.Input != nil {
		inputSchema = schema.Generate(def.Input)
	} else {
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}

	a.tools[name] = &registeredTool{
		def:         def,
		inputSchema: inputSchema,
	}
	a.toolOrder = append(a.toolOrder, name)
}

// UseTools registers multiple tools at once.
func (a *App) UseTools(tools ...NamedTool) {
	for _, t := range tools {
		a.Tool(t.Name, t.Def)
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
		a.UseTools(builtinToolsProvider()...)
	}
}

// initMCP 初始化 MCP 服务器并注册其工具。
func (a *App) initMCP() {
	for _, srv := range a.config.mcpServers {
		var transport mcp.Transport

		switch srv.Transport {
		case "http":
			if srv.URL != "" {
				transport = mcp.NewHTTPTransport(srv.URL)
			} else {
				continue
			}
		case "stdio", "":
			if srv.Command != "" {
				var err error
				transport, err = mcp.NewStdioTransport(srv.Command, srv.Args...)
				if err != nil {
					continue
				}
			} else {
				continue
			}
		default:
			continue
		}

		client := mcp.NewClient(transport)
		a.mcpClients = append(a.mcpClients, client)

		ctx := context.Background()
		if err := client.Connect(ctx); err != nil {
			continue
		}

		tools, err := mcp.DiscoverAndConvert(ctx, client)
		if err != nil || len(tools) == 0 {
			continue
		}

		// 将 MCP 工具转换为框架工具并注册
		for _, t := range tools {
			a.Tool(t.Name, ToolDef{
				Description: t.Description,
				Input:       t.InputSchema,
				Permission:  Normal,
				Execute:     t.Execute,
			})
		}
	}
}

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

// planToolsProvider 是由 builtin 包注入的 Plan 工具提供函数。
var planToolsProvider func(store plan.StoreInterface) []NamedTool

// subAgentToolsProvider 是由 builtin 包注入的子 agent 工具提供函数。
var subAgentToolsProvider func(prov provider.Provider, defs []agent.Definition) []NamedTool

// bgTaskToolsProvider 是由 builtin 包注入的后台任务工具提供函数。
var bgTaskToolsProvider func(store bgtask.StoreInterface) []NamedTool

// askToolsProvider 是由 builtin 包注入的 AskUser 工具提供函数。
var askToolsProvider func() []NamedTool

// RegisterTaskToolsProvider 由 builtin 包调用以注册 Task 工具提供函数。
func RegisterTaskToolsProvider(fn func(store task.StoreInterface) []NamedTool) {
	taskToolsProvider = fn
}

// RegisterPlanToolsProvider 由 builtin 包调用以注册 Plan 工具提供函数。
func RegisterPlanToolsProvider(fn func(store plan.StoreInterface) []NamedTool) {
	planToolsProvider = fn
}

// RegisterSubAgentToolsProvider 由 builtin 包调用以注册子 agent 工具提供函数。
func RegisterSubAgentToolsProvider(fn func(prov provider.Provider, defs []agent.Definition) []NamedTool) {
	subAgentToolsProvider = fn
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
		a.run(ctx, input, nil, events)
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
		// 构建一个虚拟 session 来传递历史消息。
		sess := &session.Session{
			ID:       "ephemeral",
			Messages: history,
		}
		a.run(ctx, input, sess, events)
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

		// 更新状态为运行中。
		_ = mgr.UpdateState(ctx, sessionID, sessionStateRunning)

		// 运行 agent，传入历史消息。
		a.run(ctx, input, sess, events)

		// 更新状态为空闲。
		_ = mgr.UpdateState(ctx, sessionID, sessionStateIdle)
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

// BgTasks 返回后台任务管理器。
// 需先启用 WithBgTaskTools()，否则返回 nil。
func (a *App) BgTasks() *bgtask.Manager {
	return a.bgTaskMgr
}

// BgTaskStore 返回后台任务存储接口。
// 需先启用 WithBgTaskTools()，否则返回 nil。
func (a *App) BgTaskStore() bgtask.StoreInterface {
	return a.bgTaskMgr
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
	if a.provider == nil {
		return ""
	}
	return a.provider.Capabilities().ModelID
}

// SetModel 动态切换当前使用的模型。
// 如果 provider 不支持运行时切换，返回 false。
func (a *App) SetModel(modelID string) bool {
	if switcher, ok := a.provider.(provider.ModelSwitcher); ok {
		switcher.SetModel(modelID)
		return true
	}
	return false
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

// RunCLI starts an interactive CLI REPL. Blocks until the user exits.
func (a *App) RunCLI() {
	runCLI(a)
}

// RunHTTP starts an HTTP server with SSE streaming. Blocks until the server stops.
func (a *App) RunHTTP(addr string) error {
	return runHTTP(a, addr)
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

// run is the internal entry point that wires up all subsystems and starts the agent loop.
// sess 为 nil 时创建无状态会话；非 nil 时使用已有历史消息。
func (a *App) run(ctx context.Context, input string, sess *session.Session, out chan<- Event) {
	a.mu.RLock()
	cfg := a.config
	tools := a.buildToolSet()
	mws := make([]MiddlewareWithFilter, len(a.middlewares))
	copy(mws, a.middlewares)
	a.mu.RUnlock()

	// 子系统初始化。

	// 1. Compaction Manager（对齐 Claude Code 流程）。
	compCfg := compaction.Config{
		AutoCompactThreshold: cfg.compaction.AutoCompactThreshold,
		MaxResultSize:        cfg.compaction.MaxResultSize,
	}
	if cfg.yoloPromptFile != "" {
		compCfg.PromptFile = cfg.yoloPromptFile
	} else if cfg.promptDir != "" {
		compCfg.PromptFile = filepath.Join(cfg.promptDir, prompts.Compact)
	}
	compMgr := compaction.NewManager(compCfg)
	// 设置 Provider 以支持新的 Compact() 方法（对齐 Claude Code 流程）。
	if a.provider != nil {
		compMgr.SetProvider(a.provider)
	}

	// 2. Permission Gate。
	permGate := permission.NewGate(wrapApprover(cfg.approver))

	// 应用权限模式（如有配置）。
	if cfg.permissionMode != nil {
		permGate.SetMode(*cfg.permissionMode)
	}

	// 应用权限规则（如有配置）。
	if cfg.permissionRules != nil {
		permGate.SetRules(cfg.permissionRules)
	}

	// 注入 YOLO LLM 分类器（对齐 Claude Code 的 YOLO classifier）。
	// 需要 provider 才能调用子模型进行权限分类。
	if a.provider != nil {
		yoloCfg := permission.YoloClassifierConfig{
			Mode: "both", // 两阶段：fast → thinking
		}
		if cfg.yoloPromptFile != "" {
			yoloCfg.PromptFile = cfg.yoloPromptFile
		} else if cfg.promptDir != "" {
			yoloCfg.PromptFile = filepath.Join(cfg.promptDir, prompts.YoloClassifier)
		}
		permGate.SetClassifier(permission.NewYoloClassifier(a.provider, yoloCfg))
	}

	// 3. Memory Manager。
	memMgr := memory.NewManager(memory.Config{
		MemoryDir:      cfg.memoryDir,
		ProjectContext: cfg.projectContextFiles,
	})

	// 4. Executor。
	exec := executor.New(executor.Config{
		MaxConcurrency: cfg.maxConcurrency,
	})

	// 5. Budget Tracker。
	budgetTracker := budget.NewTracker(budget.Config{
		TotalBudget: cfg.tokenBudget,
	})

	// 6. SessionMemory（会话内定期记忆提取）。
	var sessMem *sessionmem.SessionMemory
	if cfg.sessionMemoryCfg != nil && a.provider != nil {
		sessMem = sessionmem.New(a.provider, *cfg.sessionMemoryCfg)
	}

	// 7. 动态 System Prompt 组装。
	systemPrompt := a.buildSystemPrompt(cfg, memMgr)

	// 构建 loop 配置。
	loopCfg := loop.Config{
		Provider:     a.provider,
		Fallback:     a.fallback,
		Tools:        tools,
		Middlewares:  toLoopMiddlewares(mws),
		SystemPrompt: systemPrompt,
		MaxTurns:     cfg.maxTurns,
		Compaction:   compMgr,
		Permission:   permGate,
		Memory:       memMgr,
		Executor:     exec,
		Budget:       budgetTracker,
		Hooks:        &hooksRunnerAdapter{mgr: a.hooksMgr},
		Observer:     a.obsRegistry.Observer(),
		SessionID:    sess.ID,
	}

	// PlanChecker 仅在 Plan 系统启用时注入。
	if a.planMgr != nil {
		loopCfg.PlanChecker = &planCheckerAdapter{mgr: a.planMgr}
	}

	// 注册 PostSampling hook（SessionMemory）。
	if sessMem != nil {
		loopCfg.PostSampling = &sessionMemHookAdapter{sm: sessMem}
	}

	// 如果有 session，传入历史消息。
	if sess != nil && len(sess.Messages) > 0 {
		loopCfg.InitialMessages = sess.Messages
	}

	// 运行 agent 循环。
	agentLoop := loop.New(loopCfg)
	for ev := range agentLoop.Run(ctx, input) {
		pubEv := toPublicEvent(ev)
		// EventDone 时附带完整消息列表，供业务层持久化。
		if pubEv.Type == EventDone {
			pubEv.Messages = agentLoop.FinalMessages()
		}
		out <- pubEv
	}

	// 如果有 session 且启用了自动持久化，保存本轮新增的消息。
	// 默认开启自动持久化；WithAutoPersist(false) 可禁用。
	// 业务层也可自行从事件流收集数据并通过自己的逻辑持久化。
	autoPersist := cfg.autoPersist == nil || *cfg.autoPersist
	if sess != nil && cfg.sessionManager != nil && autoPersist {
		finalMsgs := agentLoop.FinalMessages()
		// 只持久化新增的消息（跳过 InitialMessages 部分）。
		startIdx := len(sess.Messages)
		for i := startIdx; i < len(finalMsgs); i++ {
			_ = cfg.sessionManager.AppendMessage(ctx, sess.ID, finalMsgs[i])
		}
	}
}

// buildSystemPrompt 使用 sysprompt.Builder 动态组装系统提示词。
func (a *App) buildSystemPrompt(cfg appConfig, memMgr *memory.Manager) string {
	builder := sysprompt.NewBuilder()

	if cfg.systemPrompt != "" {
		// 传统模式：用户提供的纯字符串
		builder.AddBasePrompt(cfg.systemPrompt)
	} else {
		// 默认：加载 prompt 体系（promptDir 优先，否则嵌入默认值）
		builder.AddSection("identity", resolvePrompt(cfg.promptDir, prompts.Identity), nil)
		builder.AddSection("doing_tasks", resolvePrompt(cfg.promptDir, prompts.DoingTasks), nil)
		builder.AddSection("actions", resolvePrompt(cfg.promptDir, prompts.Actions), nil)
		builder.AddSection("using_tools", resolvePrompt(cfg.promptDir, prompts.UsingTools), nil)
		builder.AddSection("tone_style", resolvePrompt(cfg.promptDir, prompts.ToneStyle), nil)
		builder.AddSection("output_efficiency", resolvePrompt(cfg.promptDir, prompts.OutputEfficiency), nil)
		builder.AddSection("reminder", resolvePrompt(cfg.promptDir, prompts.Reminder), nil)
	}

	// 环境信息和 Git 状态（WithGitContext 启用时才注入）。
	if cfg.enableGitStatus {
		builder.AddEnvironmentInfo()
		if root := sysprompt.DetectGitRoot(); root != "" {
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
func (a *App) GetSystemPrompt() string {
	return a.buildSystemPrompt(a.config, nil)
}

// buildPromptVars 构建提示词模板变量。
func (a *App) buildPromptVars() map[string]string {
	vars := map[string]string{
		"cwd":     ".",
		"OS":      sysprompt.DetectPlatform(),
		"date":    sysprompt.CurrentDate(),
		"boolean": "false",
	}
	if root := sysprompt.DetectGitRoot(); root != "" {
		vars["boolean"] = "true"
		vars["cwd"] = root
		vars["gitStatus"] = sysprompt.GetGitStatusText(root)
	}
	return vars
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
	c := newContextFromStd(ctx)
	a.inner.AfterTool(c, toolName, &Result{Content: result}, err)
}

// toPublicEvent converts an internal loop event to a public Event.
func toPublicEvent(ev loop.Event) Event {
	return Event{
		Type:       EventType(ev.Type),
		Text:       ev.Text,
		Thinking:   ev.Thinking,
		ToolName:   ev.ToolName,
		ToolInput:  ev.ToolInput,
		ToolUseID:  ev.ToolUseID,
		ToolResult: ev.ToolResult,
		Usage:      ev.Usage,
		Error:      ev.Error,
	}
}

// approverAdapter wraps a public goagent.Approver into a permission.Approver.
type approverAdapter struct {
	inner Approver
}

func (a *approverAdapter) Approve(toolName string, input string, level permission.Level) (bool, bool) {
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
	result, err := h.mgr.RunPreToolUse(ctx, toolName, input, "")
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
	_ = h.mgr.RunPostToolUse(ctx, toolName, input, result, toolErr, "")
}

func (h *hooksRunnerAdapter) RunStop(ctx context.Context) (block bool, message string, err error) {
	result, err := h.mgr.RunStop(ctx, "")
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
