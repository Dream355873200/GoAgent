package goagent

import (
	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/hooks"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/plan"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/sessionmem"
	"github.com/Dream355873200/GoAgent/task"
)

// Option configures an App.
type Option interface {
	apply(c *appConfig)
}

type optionFunc func(c *appConfig)

func (f optionFunc) apply(c *appConfig) { f(c) }

// appConfig holds all configuration for an App.
type appConfig struct {
	provider            provider.Provider
	providerConfig      *ProviderConfig // Provider 配置（与 provider 二选一）
	fallback            provider.Provider
	systemPrompt        string
	maxTurns            int
	maxConcurrency      int
	memoryDir           string
	projectContextFiles []string
	approver            Approver
	compaction          CompactionConfig
	tokenBudget         int // 总 token 预算，0 表示无限
	builtinTools        bool
	toolKits            []ToolKit
	sessionManager      *session.Manager
	autoPersist         *bool                      // nil 表示使用默认（true）
	permissionMode      *permission.PermissionMode // nil 表示使用默认
	permissionRules     *permission.RuleSet
	hooks               []hooks.Hook       // 用户注册的 hooks
	subAgentDefs        []agent.Definition // 子 agent 定义
	sessionMemoryCfg    *sessionmem.Config // 会话记忆配置（nil 表示禁用）
	useClaudePrompts    bool               // 是否使用 Claude Code 提示词体系
	promptConfig        PromptConfig       // Prompt 自定义配置

	// Observer 系统（可观测性）
	observers         []observer.Observer // 观察者列表
	costTracking      bool                // 是否启用成本追踪
	analyticsTracking bool                // 是否启用分析追踪

	// Store 接口（可注入自定义存储）
	taskStore   task.StoreInterface
	planStore   plan.StoreInterface
	bgTaskStore bgtask.StoreInterface

	// MCP 服务器配置
	mcpServers []agent.MCPServerConfig

	// 子系统开关（默认均不启用）
	enableTaskTools   bool
	enablePlanTools   bool
	enableAskTools    bool
	enableBgTaskTools bool
}

// ProviderConfig 是 LLM Provider 的配置，可直接传给 New()。
// 支持两种类型：OpenAI 兼容 API 和 Anthropic API。
//
// 示例：
//
//	app := goagent.New(goagent.ProviderConfig{
//	    Type:   "openai",
//	    Model:  "gpt-4o",
//	    APIKey: "sk-...",
//	    BaseURL: "https://api.openai.com/v1",
//	})
//
// 可与 With* Option 混用：
//
//	app := goagent.New(
//	    goagent.ProviderConfig{
//	        Type:   "openai",
//	        Model:  "deepseek-chat",
//	    },
//	    goagent.WithSystemPrompt("你是助手"),
//	    goagent.WithMaxTurns(50),
//	)
type ProviderConfig struct {
	// Type 指定 Provider 类型："openai" 或 "anthropic"。
	// 默认为 "openai"。
	Type string

	// Model 模型名称。
	// OpenAI 默认 "qwen2.5:7b"，Anthropic 默认 "claude-sonnet-4-6-v1"。
	Model string

	// APIKey API 密钥。留空表示无需鉴权（如本地 Ollama）。
	APIKey string

	// BaseURL API 基础 URL。
	// OpenAI 默认 "http://localhost:11434/v1"，Anthropic 不需要。
	BaseURL string
}

// ensure ProviderConfig implements Option.
var _ Option = ProviderConfig{}

func (cfg ProviderConfig) apply(c *appConfig) {
	c.providerConfig = &cfg
}

func defaultConfig() appConfig {
	return appConfig{
		maxTurns:       100,
		maxConcurrency: 10,
		approver:       nil, // nil means use StdinApprover in CLI, auto-deny in SDK
		compaction: CompactionConfig{
			AutoCompactThreshold: 0.8,
			MaxResultSize:        50_000,
		},
	}
}

// CompactionConfig controls context compression behavior.
type CompactionConfig struct {
	// AutoCompactThreshold is the fraction of the context window (0.0-1.0)
	// at which automatic compression kicks in. Default: 0.8
	AutoCompactThreshold float64

	// MaxResultSize is the maximum character count for a single tool result.
	// Results larger than this are persisted to disk and replaced with a reference.
	// Default: 50000
	MaxResultSize int
}

// Approver is the interface for handling user permission prompts.
// The framework calls this when a tool requires approval.
type Approver interface {
	// Approve asks the user to approve a tool call.
	// Returns true to allow, false to deny.
	// toolName and input describe what the tool wants to do.
	// permission is the tool's declared Permission level.
	Approve(toolName string, input string, permission Permission) (allow bool, alwaysAllow bool)
}

// --- Options ---

// WithProvider sets the LLM provider.
func WithProvider(p provider.Provider) Option {
	return optionFunc(func(c *appConfig) {
		c.provider = p
	})
}

// WithFallback sets a fallback LLM provider for when the primary is overloaded.
func WithFallback(p provider.Provider) Option {
	return optionFunc(func(c *appConfig) {
		c.fallback = p
	})
}

// WithSystemPrompt sets the system prompt for the agent.
func WithSystemPrompt(prompt string) Option {
	return optionFunc(func(c *appConfig) {
		c.systemPrompt = prompt
	})
}

// PromptConfig 配置自定义 Prompt sections。
// 支持从文件加载或直接使用字符串。
type PromptConfig struct {
	// 从文件加载的 prompt（文件路径）
	Identity   string // system-identity.prompt.md
	DoingTasks string // system-doing-tasks.prompt.md
	Actions    string // system-actions.prompt.md
	UsingTools string // system-using-tools.prompt.md
	ToneStyle  string // system-tone-style.prompt.md
	OutputEff  string // system-output-efficiency.prompt.md
	Reminder   string // system-reminder.prompt.md
	Workflow   string // system-workflow.prompt.md (兼容旧版)
	Compact    string // compact.prompt.md

	// 直接使用字符串的 prompt（优先级高于文件）
	IdentityText   string
	DoingTasksText string
	ActionsText    string
	UsingToolsText string
	ToneStyleText  string
	OutputEffText  string
	ReminderText   string
	WorkflowText   string
	CompactText    string

	// 追加到现有 prompt 之后
	AppendIdentity   string
	AppendDoingTasks string
	AppendActions    string
	AppendUsingTools string
	AppendToneStyle  string
	AppendOutputEff  string
	AppendReminder   string
	AppendWorkflow   string
	AppendCompact    string
}

// WithPromptConfig 配置自定义 Prompt sections。
// 支持从文件加载或直接使用字符串。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithPromptConfig(goagent.PromptConfig{
//	        IdentityText: "你是一个客服助手...",
//	        AppendToneStyle: "\n\n必须使用中文回答。",
//	    }),
//	)
func WithPromptConfig(cfg PromptConfig) Option {
	return optionFunc(func(c *appConfig) {
		c.promptConfig = cfg
		c.useClaudePrompts = true // 启用 prompt 体系
	})
}

// WithClaudeCodePrompts 启用 Claude Code 原版提示词体系。
// 使用嵌入的中文提示词替代 WithSystemPrompt。
// 如果同时设置了 WithSystemPrompt，用户提示词会追加到 Claude Code 提示词之后。
func WithClaudeCodePrompts() Option {
	return optionFunc(func(c *appConfig) {
		c.useClaudePrompts = true
	})
}

// ScenarioPreset 是快捷场景预设类型。
type ScenarioPreset int

const (
	// ScenarioMinimal 最小化场景，仅包含核心提示词。
	ScenarioMinimal ScenarioPreset = iota
	// ScenarioWeb Web 开发场景，包含 Web 相关提示词。
	ScenarioWeb
	// ScenarioCodeReview 代码审查场景。
	ScenarioCodeReview
	// ScenarioCustomerSupport 客服助手场景。
	ScenarioCustomerSupport
)

// Scenario 返回对应场景的 PromptConfig。
// 这是一个快捷方式，无需手动配置每个 prompt 字段。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithScenario(goagent.ScenarioWeb),
//	)
func Scenario(preset ScenarioPreset) PromptConfig {
	switch preset {
	case ScenarioWeb:
		return PromptConfig{
			AppendIdentity:   "\n\n你是一个专业的 Web 开发助手，专注于帮助用户构建现代 Web 应用。",
			AppendDoingTasks: "\n\n- 优先使用主流前端框架（React/Vue/Angular）\n- 遵循最佳实践和性能优化",
		}
	case ScenarioCodeReview:
		return PromptConfig{
			AppendIdentity:   "\n\n你是一个专业的代码审查助手，专注于发现代码问题、提升代码质量。",
			AppendDoingTasks: "\n\n- 重点关注代码安全性、性能、可维护性\n- 提供具体的改进建议",
		}
	case ScenarioCustomerSupport:
		return PromptConfig{
			IdentityText:    "你是一个友好、专业的客服助手。\n- 用亲切的语气与用户交流\n- 耐心解答问题\n- 无法回答时如实说明",
			AppendToneStyle: "\n\n- 使用友好、耐心语气\n- 适当使用表情让对话更亲切",
		}
	default: // ScenarioMinimal
		return PromptConfig{
			// 仅使用内置提示词，不追加任何内容
		}
	}
}

// WithScenario 应用快捷场景预设。
// 等价于 WithPromptConfig(Scenario(preset))。
func WithScenario(preset ScenarioPreset) Option {
	return WithPromptConfig(Scenario(preset))
}

// WithMaxTurns sets the maximum number of agent loop iterations. Default: 100.
func WithMaxTurns(n int) Option {
	return optionFunc(func(c *appConfig) {
		c.maxTurns = n
	})
}

// WithMaxConcurrency sets the maximum number of concurrent tool executions. Default: 10.
func WithMaxConcurrency(n int) Option {
	return optionFunc(func(c *appConfig) {
		c.maxConcurrency = n
	})
}

// WithMemoryDir sets the directory for persistent cross-session memory.
// The framework automatically manages files in this directory.
// If not set, cross-session memory is disabled.
func WithMemoryDir(dir string) Option {
	return optionFunc(func(c *appConfig) {
		c.memoryDir = dir
	})
}

// WithProjectContext adds a project context file that is loaded into every session.
// Equivalent to CLAUDE.md in Claude Code.
func WithProjectContext(path string) Option {
	return optionFunc(func(c *appConfig) {
		c.projectContextFiles = append(c.projectContextFiles, path)
	})
}

// WithApprover sets the approval handler for tool permission prompts.
func WithApprover(a Approver) Option {
	return optionFunc(func(c *appConfig) {
		c.approver = a
	})
}

// WithCompaction sets the context compression configuration.
func WithCompaction(cfg CompactionConfig) Option {
	return optionFunc(func(c *appConfig) {
		c.compaction = cfg
	})
}

// WithTokenBudget 设置总 token 预算。0 表示无限（默认）。
// 当消耗超过预算时，agent 循环会自动终止。
func WithTokenBudget(tokens int) Option {
	return optionFunc(func(c *appConfig) {
		c.tokenBudget = tokens
	})
}

// WithSessionManager 设置会话管理器，启用多轮对话持久化。
// 配置后可使用 App.RunSession() 接续对话。
//
// 示例：
//
//	store := session.NewMemoryStore()              // 内存存储（测试）
//	store := session.NewFileStore("./sessions")    // 文件存储
//	mgr := session.NewManager(store)
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithSessionManager(mgr),
//	)
//
//	// 创建新会话
//	sessionID, _ := mgr.Create(ctx, nil)
//
//	// 第一轮对话
//	for ev := range app.RunSession(ctx, sessionID, "你好") { ... }
//
//	// 第二轮对话（自动加载历史）
//	for ev := range app.RunSession(ctx, sessionID, "继续上面的话题") { ... }
func WithSessionManager(mgr *session.Manager) Option {
	return optionFunc(func(c *appConfig) {
		c.sessionManager = mgr
	})
}

// WithAutoPersist 控制 RunSession() 是否在结束后自动持久化消息。
// 默认为 true（自动持久化到 SessionStore）。
// 设为 false 时，框架只负责加载历史、管理并发，不自动写入。
// 开发者需自行从事件流或 FinalMessages() 收集数据并持久化。
//
// 适用场景：
//   - false: 业务层需要自定义持久化逻辑（如存 DB、加业务字段）
//   - true:  快速原型、CLI 工具等不需要自定义存储的场景
func WithAutoPersist(enabled bool) Option {
	return optionFunc(func(c *appConfig) {
		c.autoPersist = &enabled
	})
}

// --- Built-in Approvers ---

// AutoApprover returns an Approver that automatically approves everything.
// Use in CI/CD or trusted environments. Not recommended for production.
func AutoApprover() Approver {
	return &autoApprover{}
}

type autoApprover struct{}

func (a *autoApprover) Approve(string, string, Permission) (bool, bool) {
	return true, false
}

// --- WithBuiltinTools ---

// WithBuiltinTools 一行开启所有内置工具（Read/Write/Edit/Glob/Grep/Bash/WebSearch/WebFetch）。
// 等价于手动调用 app.UseTools(builtin.CoreTools()...)。
// 注意：AskUser 不在此列，需通过 WithAskTools() 单独启用。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithBuiltinTools(),
//	    goagent.WithAskTools(),
//	)
func WithBuiltinTools() Option {
	return optionFunc(func(c *appConfig) {
		c.builtinTools = true
	})
}

// WithToolKits 注册一组 ToolKit 预设。
// ToolKit 是按领域分组的工具集合，如 FileKit、ShellKit 等。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithToolKits(goagent.FileKit(), goagent.ShellKit()),
//	)
func WithToolKits(kits ...ToolKit) Option {
	return optionFunc(func(c *appConfig) {
		c.toolKits = append(c.toolKits, kits...)
	})
}

// --- 权限配置 Options ---

// WithPermissionMode 设置权限模式。
// 控制框架如何处理工具权限：
//   - PermissionDefault — 默认（ReadOnly 自动通过，其他询问用户）
//   - PermissionBypass — 绕过所有权限（CI/CD 环境，等同 AutoApprover）
//   - PermissionAcceptEdits — 自动接受 ReadOnly 和 Normal 工具
//   - PermissionPlanOnly — 只允许只读工具（规划模式）
//   - PermissionDenyAll — 不询问，直接拒绝所有非只读操作
//
// 示例：
//
//	// CI/CD 环境，跳过所有权限
//	app := goagent.New(goagent.WithPermissionMode(goagent.PermissionBypass))
//
//	// 规划模式，只允许读取
//	app := goagent.New(goagent.WithPermissionMode(goagent.PermissionPlanOnly))
func WithPermissionMode(mode PermissionModeOption) Option {
	return optionFunc(func(c *appConfig) {
		m := permission.PermissionMode(mode)
		c.permissionMode = &m
	})
}

// PermissionModeOption 是公共权限模式类型。
type PermissionModeOption int

const (
	// PermissionDefault 默认权限模式。
	PermissionDefault PermissionModeOption = PermissionModeOption(permission.ModeDefault)
	// PermissionBypass 绕过所有权限检查。适用于 CI/CD 或完全受信环境。
	PermissionBypass PermissionModeOption = PermissionModeOption(permission.ModeBypassPermissions)
	// PermissionAcceptEdits 自动接受 ReadOnly 和 Normal 级别的工具调用。
	PermissionAcceptEdits PermissionModeOption = PermissionModeOption(permission.ModeAcceptEdits)
	// PermissionPlanOnly 只允许只读操作，拒绝所有写操作。
	PermissionPlanOnly PermissionModeOption = PermissionModeOption(permission.ModePlan)
	// PermissionDenyAll 不询问用户，直接拒绝所有非只读操作。
	PermissionDenyAll PermissionModeOption = PermissionModeOption(permission.ModeDontAsk)
)

// WithPermissionRules 设置自定义权限规则。
// 规则按优先级评估：deny > ask > allow。
//
// 示例：
//
//	rules := goagent.NewPermissionRules().
//	    Allow("Read", "").     // 允许所有 Read
//	    Allow("Bash", "git *"). // 允许 git 命令
//	    Deny("Bash", "rm *").  // 禁止删除命令
//	    Ask("Write", "")       // 写文件需要确认
//
//	app := goagent.New(goagent.WithPermissionRules(rules))
func WithPermissionRules(rules *PermissionRules) Option {
	return optionFunc(func(c *appConfig) {
		c.permissionRules = rules.toInternal()
	})
}

// PermissionRules 是面向开发者的权限规则构建器。
// 支持链式调用。
type PermissionRules struct {
	rules []permRuleEntry
}

type permRuleEntry struct {
	toolPattern  string
	inputPattern string
	behavior     permission.PermissionBehavior
	reason       string
}

// NewPermissionRules 创建权限规则构建器。
func NewPermissionRules() *PermissionRules {
	return &PermissionRules{}
}

// Allow 添加一条允许规则。
// toolPattern 匹配工具名（支持 * 通配符）。
// inputPattern 匹配输入内容（空字符串匹配所有）。
func (r *PermissionRules) Allow(toolPattern, inputPattern string) *PermissionRules {
	r.rules = append(r.rules, permRuleEntry{
		toolPattern:  toolPattern,
		inputPattern: inputPattern,
		behavior:     permission.BehaviorAllow,
		reason:       "规则允许",
	})
	return r
}

// Deny 添加一条拒绝规则。
func (r *PermissionRules) Deny(toolPattern, inputPattern string) *PermissionRules {
	r.rules = append(r.rules, permRuleEntry{
		toolPattern:  toolPattern,
		inputPattern: inputPattern,
		behavior:     permission.BehaviorDeny,
		reason:       "规则拒绝",
	})
	return r
}

// Ask 添加一条需询问规则。
func (r *PermissionRules) Ask(toolPattern, inputPattern string) *PermissionRules {
	r.rules = append(r.rules, permRuleEntry{
		toolPattern:  toolPattern,
		inputPattern: inputPattern,
		behavior:     permission.BehaviorAsk,
		reason:       "规则要求确认",
	})
	return r
}

// AllowWithReason 添加带说明的允许规则。
func (r *PermissionRules) AllowWithReason(toolPattern, inputPattern, reason string) *PermissionRules {
	r.rules = append(r.rules, permRuleEntry{
		toolPattern:  toolPattern,
		inputPattern: inputPattern,
		behavior:     permission.BehaviorAllow,
		reason:       reason,
	})
	return r
}

// DenyWithReason 添加带说明的拒绝规则。
func (r *PermissionRules) DenyWithReason(toolPattern, inputPattern, reason string) *PermissionRules {
	r.rules = append(r.rules, permRuleEntry{
		toolPattern:  toolPattern,
		inputPattern: inputPattern,
		behavior:     permission.BehaviorDeny,
		reason:       reason,
	})
	return r
}

// toInternal 转换为内部 RuleSet。
func (r *PermissionRules) toInternal() *permission.RuleSet {
	rs := permission.NewRuleSet()
	for _, entry := range r.rules {
		rs.AddRule(permission.Rule{
			ToolPattern:  entry.toolPattern,
			InputPattern: entry.inputPattern,
			Behavior:     entry.behavior,
			Reason:       entry.reason,
		})
	}
	return rs
}

// --- 权限预设 ---

// PermissionPresetCI 返回 CI/CD 环境的权限预设。
// 所有工具自动通过，无需人工确认。
func PermissionPresetCI() Option {
	return WithPermissionMode(PermissionBypass)
}

// PermissionPresetPlan 返回规划模式的权限预设。
// 只允许只读工具，拒绝所有写操作。
func PermissionPresetPlan() Option {
	return WithPermissionMode(PermissionPlanOnly)
}

// PermissionPresetInteractive 返回交互式开发的权限预设。
// ReadOnly 和 Normal 自动通过，Dangerous 仍需确认。
func PermissionPresetInteractive() Option {
	return WithPermissionMode(PermissionAcceptEdits)
}

// --- Hooks ---

// WithHooks 注册 hook 回调。
// Hooks 在 agent 循环的关键事件点触发（工具执行前后、循环退出前等）。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithHooks(myLoggingHook, myAuditHook),
//	)
func WithHooks(h ...hooks.Hook) Option {
	return optionFunc(func(c *appConfig) {
		c.hooks = append(c.hooks, h...)
	})
}

// --- Sub-Agents ---

// WithSessionMemory 启用会话内定期记忆提取。
// 在长对话中定期提取关键信息到 session_memory.md，防止重要上下文在 autocompact 时丢失。
// 对齐 Claude Code 的 SessionMemory 机制。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithSessionMemory(sessionmem.Config{
//	        MinTokensToInit:        10000,
//	        MinTokensBetweenUpdate: 5000,
//	        MemoryDir:              ".yume/.session-memory",
//	    }),
//	)
func WithSessionMemory(cfg sessionmem.Config) Option {
	return optionFunc(func(c *appConfig) {
		c.sessionMemoryCfg = &cfg
	})
}

// WithSubAgents 注册子 agent 定义。
// 每个 Definition 会自动生成一个 Agent_<name> 工具供 LLM 调用。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithSubAgents(
//	        agent.Definition{
//	            Name:         "researcher",
//	            Description:  "专门做代码搜索和分析的 agent",
//	            SystemPrompt: "你是一个代码研究助手...",
//	            MaxTurns:     5,
//	        },
//	    ),
//	)
func WithSubAgents(defs ...agent.Definition) Option {
	return optionFunc(func(c *appConfig) {
		c.subAgentDefs = append(c.subAgentDefs, defs...)
	})
}

// --- Observer 系统 Options ---

// WithObservers 注册一个或多个 Observer。
// Observer 接收 agent 循环的事件（工具执行、Token 用量、权限决策等），
// 用于接入 Prometheus、审计日志、外部监控系统等。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithObservers(myPrometheusObserver, myAuditLogger),
//	)
func WithObservers(obs ...observer.Observer) Option {
	return optionFunc(func(c *appConfig) {
		c.observers = append(c.observers, obs...)
	})
}

// WithCostTracking 启用成本追踪。
// 内部自动创建 cost.Observer + cost.Tracker，并注册为 Observer。
// 可通过 App.Usage() 获取追踪结果。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithCostTracking(),
//	)
//
//	// 获取 token 使用统计
//	summary := app.Usage()
func WithCostTracking() Option {
	return optionFunc(func(c *appConfig) {
		c.costTracking = true
	})
}

// WithAnalytics 启用使用分析追踪。
// 内部自动创建 analytics.Observer + analytics.Tracker，并注册为 Observer。
// 可通过 App.Analytics() 获取追踪结果。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithAnalytics(),
//	)
//
//	// 获取分析统计
//	summary := app.Analytics()
func WithAnalytics() Option {
	return optionFunc(func(c *appConfig) {
		c.analyticsTracking = true
	})
}

// --- Store 接口注入 Options ---

// WithTaskStore 注入自定义 Task 存储后端。
// 如果不注入，默认使用内存存储（task.NewStore()）。
//
// 示例：
//
//	// 使用 Redis 存储
//	redisStore := myRedisTaskStore{} // 实现 task.StoreInterface
//	app := goagent.New(
//	    goagent.WithTaskStore(redisStore),
//	)
func WithTaskStore(store task.StoreInterface) Option {
	return optionFunc(func(c *appConfig) {
		c.taskStore = store
	})
}

// WithPlanStore 注入自定义 Plan 存储后端。
// 如果不注入，默认使用文件存储（plan.NewManager(".yume/plans")）。
//
// 示例：
//
//	dbStore := myDBPlanStore{} // 实现 plan.StoreInterface
//	app := goagent.New(
//	    goagent.WithPlanStore(dbStore),
//	)
func WithPlanStore(store plan.StoreInterface) Option {
	return optionFunc(func(c *appConfig) {
		c.planStore = store
	})
}

// WithBgTaskStore 注入自定义后台任务存储后端。
// 如果不注入，默认使用内存存储（bgtask.NewManager("")）。
//
// 示例：
//
//	// 使用数据库存储
//	dbStore := myDBBgTaskStore{} // 实现 bgtask.StoreInterface
//	app := goagent.New(
//	    goagent.WithBgTaskStore(dbStore),
//	)
func WithBgTaskStore(store bgtask.StoreInterface) Option {
	return optionFunc(func(c *appConfig) {
		c.bgTaskStore = store
	})
}

// --- MCP Servers Options ---

// WithMCP 配置 MCP 服务器。
// MCP 服务器的工具会自动发现并注册到 agent 工具集。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithMCP(agent.MCPServerConfig{
//	        Name:      "filesystem",
//	        Transport: "stdio",
//	        Command:   "npx",
//	        Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "/path/to/dir"},
//	    }),
//	)
func WithMCP(servers ...agent.MCPServerConfig) Option {
	return optionFunc(func(c *appConfig) {
		c.mcpServers = append(c.mcpServers, servers...)
	})
}

// --- 子系统开关 Options ---

// WithTaskTools 启用 Task 管理工具（TaskCreate/TaskUpdate/TaskGet/TaskList）。
// 默认不启用。CLI 应用如需任务管理能力需显式添加此 Option。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithTaskTools(),
//	)
func WithTaskTools() Option {
	return optionFunc(func(c *appConfig) {
		c.enableTaskTools = true
	})
}

// WithPlanTools 启用 Plan Mode 工具（EnterPlanMode/ExitPlanMode）。
// 默认不启用。CLI 应用如需规划模式需显式添加此 Option。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithPlanTools(),
//	)
func WithPlanTools() Option {
	return optionFunc(func(c *appConfig) {
		c.enablePlanTools = true
	})
}

// WithAskTools 启用 AskUser 工具。
// 默认不启用。CLI/TUI 应用如需与用户交互确认需显式添加此 Option。
// 也可通过 InteractKit() 手动注册。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithAskTools(),
//	)
func WithAskTools() Option {
	return optionFunc(func(c *appConfig) {
		c.enableAskTools = true
	})
}

// WithBgTaskTools 启用后台任务工具（TaskStop/TaskOutput）。
// 默认不启用。CLI 应用如需后台任务管理需显式添加此 Option。
//
// 示例：
//
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithBgTaskTools(),
//	)
func WithBgTaskTools() Option {
	return optionFunc(func(c *appConfig) {
		c.enableBgTaskTools = true
	})
}
