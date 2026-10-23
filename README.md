# GoAgent — Go AI Agent 应用框架

> 生产级 Go AI Agent 应用框架：上下文压缩、权限管理、工具编排、多会话多模式并发、运行中插话、子代理治理、Pipeline 编排、benchmark 评测，以及 MCP、AG-UI 协议支持等核心能力开箱即用。

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)](LICENSE)

---

## 目录

**入门**
- [特性](#特性) · [安装](#安装) · [快速开始](#快速开始)
- 📚 [三课渐进式教程](docs/tutorial.md)（最小 agent → 工具与多轮 → Pipeline）

**核心（本页）**
- [New() 参数说明](#new-参数说明) · [核心概念](#核心概念) · [进阶指南](#进阶指南)
- [完整选项参考](#完整选项参考) · [项目结构](#项目结构) · [版本状态](#版本状态)

**深度参考**（[docs/reference.md](docs/reference.md)）
- 权限系统 · 运行模式 · 会话管理 · 内置工具 · 中间件/Hooks/Observer
- 子 Agent · Pipeline 编排 · 内存与记忆 · 上下文压缩 · Provider · Prompt
- 架构深潜：[architecture.md](docs/architecture.md) · [mechanisms.md](docs/mechanisms.md)

**升级与路线图**
- 升级说明：[v0.14](UPGRADE-v0.14.md) · [v0.13](UPGRADE-v0.13.md) · [v0.12](UPGRADE-v0.12.md)
- [路线图](#路线图)：还没做的规划与设计讨论（[design/BACKLOG.md](design/BACKLOG.md)）

---

## 特性

| 类别 | 功能 |
|------|------|
| **核心循环** | Agent Loop 状态机（8 阶段）、流式工具执行、三级副作用调度（只读/并行/独占）、流中断锚点恢复 |
| **权限系统** | 三态权限（allow/deny/ask）、6 种权限模式（运行时可切换）、可插拔分类器（默认 YOLO LLM 分类器）、规则引擎、改参审批 |
| **上下文管理** | 四层上下文压缩、Circuit Breaker、超大结果持久化到磁盘、压缩后重注入（post-compact reminder） |
| **会话** | JSONL 持久化、会话恢复、多会话并发控制、会话级工作目录（单进程多项目并行）、孤立 tool_use 自愈 |
| **多模式** | 会话级注入点：按会话裁剪工具集 / 提示词目录 / 项目上下文 / 工具描述 / Skill 注册表——同一进程内不同会话跑不同「模式」 |
| **运行中交互** | Steering 双车道（guide 插话注入 + queue 排队续跑）、统一 `<system-reminder>` 注入通道、后台任务终态自动回注、子代理可寻址（单独发指示 / 中断 / 查询）、自定义事件（`ctx.Emit` / `App.EmitEvent`） |
| **内存** | CLAUDE.md 三层加载、Auto Memory、SessionMemory |
| **工具生态** | 内置工具（Read/Write/Edit/Glob/Grep/Bash/GitCommit/WebSearch/WebFetch/run_js）、MCP 客户端（官方 SDK：取消、进度、图片、工具列表变更、elicitation 转提问）与服务端（把工具和 agent 暴露为 MCP）、按需子系统（Task/Plan/Ask/BgTask/Issue）、AgentTool 转接头 |
| **任务系统** | Task/Todo V2（依赖管理）、Plan Mode、Skill 系统（YAML frontmatter） |
| **调度** | Cron 调度（5-field cron + jitter + 3 天过期）、Background Agents |
| **编排** | Pipeline DAG 编排（拓扑调度、MapReduce 并行、Supervisor 审核）、动态 Pipeline（LLM 运行时建图） |
| **隔离** | 工具执行沙箱（Tier 0/1）、Git Worktree 隔离执行、goja JS 沙箱执行器 |
| **推理增强** | Extended Thinking、思考强度（off/low/medium/high，运行时可调）、运行时切模型 |
| **检索** | RAG 四件套（Retriever/Embedder/Store/Loader + 多路融合） |
| **评测与测试** | benchmark 子包：断言原语、LLM Judge、pass^k、回归对比、JUnit XML；脚本化模型（`provider/scripted`）、HTTP 录制回放（`httprr`） |
| **可观测性** | Observer 接口（含 LLM 调用级钩子）、OpenTelemetry 导出（`gen_ai.*` 语义约定）、Cost Tracking、Analytics、HTTP 端点 |
| **工程支持** | Token Budget 追踪、Diminishing Returns 检测、挂起/恢复（SuspendGate） |
| **可靠性** | Rate Limit / Retry with Backoff（429 TPM 窗口对齐重试 + 状态行推送）、Hooks 框架（5 事件类型） |
| **协议与 SDK** | 统一 SSE 信封（seq 严格递增 + 版本盖章）、会话事件总线与断线续传、`GET /protocol` 自描述、AG-UI 1.0 剖面（审批停车、前端工具、改参审批）、零依赖 TypeScript 客户端 |
| **UI** | 交互式 TUI REPL（Bubble Tea）、HTTP/SSE API + REST 端点 |

---

## 安装

### 前置要求

- Go 1.25+
- LLM Provider（OpenAI 兼容 API、Anthropic API）

### 安装

GoAgent 是一个 Go 库，在你的模块里引入：

```bash
go get github.com/Dream355873200/GoAgent@latest
```

示例程序在 [examples/](examples/) 下（对话机器人、DevOps 助手、Web API 等），克隆源码、配置好模型的地址与密钥后即可运行。

---

## 快速开始

### 最简示例

```go
package main

import (
    "github.com/Dream355873200/GoAgent"
    "github.com/Dream355873200/GoAgent/cli"

    // 必须空白导入：builtin 包通过 init() 向框架注册内置工具实现，
    // 漏掉这行 WithBuiltinTools() 会静默无效（运行时日志会警告）
    _ "github.com/Dream355873200/GoAgent/builtin"
)

func main() {
    app := goagent.New(
        // 使用 OpenAI 兼容 API（支持 Ollama、OpenRouter、vLLM 等）
        goagent.ProviderConfig{
            Model:   "qwen2.5:7b",
            BaseURL: "http://localhost:11434/v1",
        },
        // 一行开启所有内置工具
        goagent.WithBuiltinTools(),
    )

    cli.Run(app) // 终端界面在 cli 包，只有导入它的程序才链接终端界面库
}
```

### 完整示例：DevOps 助手

```go
package main

import (
	"context"
	"log"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/cli"
	// 注册内置工具实现（空白导入，见最简示例说明）
	_ "github.com/Dream355873200/GoAgent/builtin"
	"github.com/Dream355873200/GoAgent/hooks"
)

type DeployInput struct {
	Service string `json:"service" desc:"服务名称"`
	Env     string `json:"env" desc:"目标环境：dev/staging/prod"`
}

type DeployResult struct {
	Status  string `json:"status"`
	Url     string `json:"url"`
	Version string `json:"version"`
}

func main() {
	app := goagent.New(
		goagent.ProviderConfig{
			Model:  "gpt-4o",
			APIKey: "sk-...",
		},
		goagent.WithSystemPrompt("你是一个 DevOps 助手，擅长自动化部署和运维任务"),
		goagent.WithBuiltinTools(),
		// 自动接受编辑：只读工具与文件编辑类工具（Write/Edit 等）自动通过，
		// Bash 等其他工具仍需确认
		goagent.WithPermissionMode(goagent.PermissionAcceptEdits),
		// 日志 Hook：每次调用工具前打印工具名
		goagent.WithHooks(hooks.NewFuncHook("log", []hooks.HookEvent{hooks.EventPreToolUse},
			func(ctx context.Context, hc *hooks.HookContext) (*hooks.HookResult, error) {
				log.Printf("调用工具 %s", hc.ToolName)
				return nil, nil
			})),
	)

	// 从函数签名自动推断 Input Schema，无需手写 ToolDef
	// 部署是写操作，指定 Normal 权限（首次询问，之后可"始终允许"）
	app.UseTools(goagent.InferTool("deploy", "部署服务到指定环境", deployService, goagent.Normal))

	cli.Run(app)
}

func deployService(ctx context.Context, in DeployInput) (DeployResult, error) {
	// 实际部署逻辑...
	return DeployResult{
		Status:  "deployed",
		Url:     "https://" + in.Service + "." + in.Env + ".example.com",
		Version: "v1.2.3",
	}, nil
}
```

### 三种运行模式

```go
// 模式 1：交互式 CLI REPL（TUI 界面，需导入 github.com/Dream355873200/GoAgent/cli）
cli.Run(app) // 导入 cli 包后 app.RunCLI() 同样可用

// 模式 2：HTTP SSE API（未配置 WithHTTPAuthToken 时只监听 127.0.0.1）
app.RunHTTP(":8080")

// 模式 3：嵌入式 SDK
events := app.Run(context.Background(), "帮我写一个 HTTP 服务器")
for ev := range events {
    switch ev.Type {
    case goagent.EventTextDelta:
        print(ev.Text)
    case goagent.EventToolStart:
        fmt.Printf("调用工具: %s\n", ev.ToolName)
    case goagent.EventDone:
        fmt.Println("完成")
    }
}
```

### 运行入口怎么选

| 入口 | 什么时候用 | 历史与落盘 |
|------|-----------|-----------|
| `Run(ctx, input)` | 单次问答、无状态嵌入调用 | 内存内一次性行走，不落盘 |
| `RunWithHistory(ctx, history, input)` | 宿主自己保管历史（自己存 `[]message.Message`） | 不落盘 |
| `RunSession(ctx, sessionID, input)` | 多轮会话（**推荐**，sessionID 须为安全文件名） | **唯一自动落盘**的入口：逐条持久化，重载续跑不重复压缩，"始终允许"授权/会话权限模式随会话保留 |
| `RunWithSessionHistory(ctx, sessionID, history, input)` | 需要会话身份（工作目录/工具过滤/权限随 sessionID 生效）但历史由宿主自管 | 不落盘 |
| `RunHTTP(addr)` | 把 App 变成 HTTP SSE 服务 | 见「HTTP API」节 |
| `RunPipeline(ctx)` / `RunDynPipeline(...)` | 固定/动态多 agent 流水线 | 见「Pipeline」节 |

---

## New() 参数说明

`goagent.New()` 接收可变参数 `...Option`，支持 `ProviderConfig` 和 `With*` 两种风格，可以混用：

```go
app := goagent.New(
    goagent.ProviderConfig{...},   // Provider 配置（直接传 struct）
    goagent.WithSystemPrompt(...), // 其他配置（Option 函数）
)
```

### ProviderConfig — LLM 提供者

| 字段 | 类型 | 说明 | 默认值 |
|------|------|------|--------|
| `Type` | string | Provider 类型：`"openai"` 或 `"anthropic"` | `"openai"` |
| `Model` | string | 模型名称 | `"qwen2.5:7b"` |
| `APIKey` | string | API 密钥，留空则无需鉴权 | `""` |
| `BaseURL` | string | API 基础 URL | `"http://localhost:11434/v1"` |

```go
// OpenAI 兼容（Ollama、OpenRouter、vLLM、DeepSeek 等）
goagent.ProviderConfig{
    Model:   "deepseek-chat",
    APIKey:  "sk-...",
    BaseURL: "https://api.deepseek.com/v1",
}

// Anthropic
goagent.ProviderConfig{
    Type:   "anthropic",
    Model:  "claude-sonnet-4-6",
    APIKey: "sk-ant-...",
}

// 环境变量快捷方式（读取 OPENAI_MODEL / OPENAI_BASE_URL / OPENAI_API_KEY）
goagent.WithOpenAI()

// 环境变量快捷方式（读取 ANTHROPIC_MODEL / ANTHROPIC_API_KEY）
goagent.WithAnthropic()
```

### With* Option 列表

共 66 个选项，按需取用——**日常使用只需 ★ 标记的 10 个**，其余面向高级场景，不读也不影响上手。

> ⚠️ 带 ⚠️ 的 builtin 系选项要求 `import _ "github.com/Dream355873200/GoAgent/builtin"`：
> 工具实现由该包的 `init()` 向框架注册（避免循环依赖），漏掉导入时选项静默无效，
> 运行时日志会给出警告。

| Option | 说明 | 默认值 |
|--------|------|--------|
| ★ `WithSystemPrompt(s)` | 系统提示词（纯字符串模式，不走 prompt 体系） | 无 |
| `WithPromptDir(dir)` | 外部 prompt 目录（优先从此加载，找不到 fallback 嵌入默认值） | 无 |
| ★ ⚠️ `WithBuiltinTools()` | 开启所有内置工具（Read/Write/Edit/Glob/Grep/Bash/run_js/GitCommit/WebSearch/WebFetch） | 不启用 |
| ⚠️ `WithTaskTools()` | 启用 Task 管理工具（TaskCreate/Update/Get/List），默认内存存储 | 不启用 |
| ⚠️ `WithPlanTools()` | 启用 Plan Mode 工具（EnterPlanMode/ExitPlanMode） | 不启用 |
| ⚠️ `WithAskTools()` | 启用 AskUser 工具（用户交互确认） | 不启用 |
| ⚠️ `WithBgTaskTools()` | 启用后台任务工具（TaskStop/TaskOutput） | 不启用 |
| ⚠️ `WithIssueTools()` | 启用问题记录工具（IssueReport/IssueResolve，测试驱动场景） | 不启用 |
| `WithLogger(l)` | 注入库内日志（pipeline 轨迹/节点错误现场），标准 `log/slog` | `slog.Default()` |
| `WithDebugMode()` | 测试模式：loop 细节日志（逐轮请求参数、工具调用明细）额外输出；关键事件（限流/切后备/错误）默认就有 | 不启用 |
| `WithToolKits(kits...)` | 按领域注册工具包（FileKit、ShellKit 等） | 无 |
| ★ `WithMaxTurns(n)` | 最大循环次数 | `100` |
| ★ `WithMaxConcurrency(n)` | 最大并发工具数 | `10` |
| `WithTokenBudget(n)` | Token 预算上限 | `0`（无限） |
| ★ `WithPermissionMode(mode)` | 权限模式（Default/AcceptEdits/Auto/Bypass/PlanOnly/DenyAll）；Auto 由 YOLO 分类器判定每个非只读调用，Bypass 不询问但高风险操作仍经分类器 | `Default`（ReadOnly 自动通过，其他询问） |
| ★ `WithPermissionRules(rules)` | 自定义权限规则（Allow/Deny/Ask） | 无 |
| ★ `WithApprover(a)` | 自定义审批者 | CLI 交互式询问，SDK 自动拒绝 |
| `WithMemoryDir(dir)` | 跨会话持久化内存目录 | 不启用 |
| `WithStateDir(name)` | 项目目录下存放运行状态的目录名（会话存储、计划、后台任务输出、项目 skill、worktree 等；进程级） | `.yume` |
| `WithProjectContext(path)` | 项目上下文文件（类似 CLAUDE.md） | 无 |
| ★ `WithSessionManager(mgr)` | 会话管理器，启用多轮持久化 | 不启用（单次会话） |
| ★ `WithAutoPersist(bool)` | 会话结束自动持久化 | `true` |
| `WithSessionWorkDir(fn)` | 按 sessionID 解析会话工作目录（注入 ctx，Bash cmd.Dir / 文件工具相对路径以此为基准，单进程多会话多项目互不串） | 不启用（进程 cwd） |
| `WithSessionToolFilter(fn)` | 按会话过滤本次 run 暴露给模型的工具（`fn(sessionID, toolName) bool`，不改注册表） | 不启用（全部工具） |
| `WithSessionPromptDir(fn)` | 按会话解析提示词目录（返回 `""` 沿用全局 `WithPromptDir`） | 不启用 |
| `WithSessionProjectContext(fn)` | 按会话追加项目上下文文件（接在全局 `WithProjectContext` 之后） | 不启用 |
| `WithSteering()` | 启用运行中插话双车道（guide 注入活跃 run / queue 排队续跑），`app.Steering()` 取回 hub | 不启用 |
| `WithPostCompactReminder(rs)` | 注册压缩后重注入源（可多次调用） | 无 |
| `WithSuspend()` | 启用挂起/恢复门闩（SuspendGate） | 不启用 |
| `WithThinkingEffort(e)` | 思考强度 `off/low/medium/high`（运行时 `SetThinkingEffort` 可改） | `""`（不干预） |
| `WithHTTPRoutes(routes)` | RunHTTP 内置路由之外注册宿主端点（复用同一 mux/App） | 无 |
| `WithSandbox(sb, policy)` | 启用工具执行沙箱（Tier 0-3，见「沙箱」节）：每次 Run/RunPipeline 创建隔离会话，路径白名单 + 工作副本 | 不启用（无沙箱，零开销） |
| `WithCompaction(cfg)` | 上下文压缩配置 | 阈值 `0.8`，结果上限 `50000` 字符 |
| `WithSessionMemory(cfg)` | 会话内定期记忆提取 | 不启用 |
| `WithSubAgents(defs...)` | 注册子 agent | 无 |
| `WithFallback(p)` | 备用 Provider（主 Provider 过载时切换） | 无 |
| ★ `WithHooks(h...)` | 注册 Hook 回调 | 无 |
| `WithObservers(obs...)` | 注册 Observer | 无 |
| `WithCostTracking()` | 启用成本追踪 | 不启用 |
| `WithAnalytics()` | 启用使用分析 | 不启用 |
| `WithMCP(servers...)` | 配置 MCP 服务器 | 无 |
| `WithTaskStore(s)` / `WithPlanStore(s)` / `WithBgTaskStore(s)` | 自定义存储后端（需配合对应的 Tools Option） | 内存/文件存储 |
| `WithGitContext()` | 在 system prompt 中注入环境信息和 Git 状态 | 不启用 |
| `WithYoloPromptFile(path)` | YOLO 分类 prompt 外部文件路径 | 嵌入默认值 |
| `WithYoloClassifier(bool)` | 开关 YOLO 分类器（auto / bypass 模式用它裁决调用） | 开启（需要 provider） |
| `WithPermissionClassifier(c)` | 用自定义分类器替换 YOLO（规则引擎、外部策略服务等） | 无 |
| `WithSessionStream(cfg)` | 会话事件日志（断线续传）的容量；HTTP 模式总是启用 | 2000 帧 / 8 MiB |
| `WithSubagentStream(mode)` / `WithMaxSubagents(n)` | 子代理事件并入父流的方式（摘要 / 全量）与每会话并发上限 | 摘要 / 8 |
| `WithAGUI(cfg)` | AG-UI 剖面参数（审批、提问、前端工具的停车超时） | 30 分钟 |
| `WithHTTPAuthToken(t)` | HTTP 鉴权令牌（未配置时只监听回环地址） | 无 |

---

## 核心概念

### App — 应用入口

`goagent.New()` 创建应用，链式配置：

```go
app := goagent.New(
    goagent.ProviderConfig{
        Model:  "gpt-4o",
        APIKey: "sk-...",
    },
    goagent.WithBuiltinTools(),
    goagent.WithPermissionMode(goagent.PermissionAcceptEdits),
)
```

### Tool — 工具定义

工具是 Agent 与外界交互的唯一方式。每个工具包含：

- **Description** — 描述（展示给 LLM）
- **Input** — 输入结构体（自动反射为 JSON Schema）
- **Permission** — 权限级别
- **Execute** — 执行函数

### Permission — 权限级别

| 级别 | 说明 | 行为 |
|------|------|------|
| `ReadOnly` | 只读操作 | 自动通过，无需询问 |
| `Normal` | 普通操作 | 首次询问，之后可"始终允许"（shell 命令按命令前缀记忆） |
| `RequireApproval` | 需审批操作 | 每次都询问 |
| `Dangerous` | 危险操作 | 醒目警告，每次询问，无"始终允许" |

### Event — 事件流

Agent 循环通过事件流与外界通信：

```go
for ev := range app.Run(ctx, "prompt") {
    switch ev.Type {
    case goagent.EventTextDelta:
        output += ev.Text
    case goagent.EventThinking:
        // 模型的推理过程（OpenAI 兼容端点的 delta.reasoning_content，
        // glm/deepseek-r1 等思考模型才有）
    case goagent.EventToolStart:
        // 工具开始执行
    case goagent.EventToolDone:
        // 工具执行完成
    case goagent.EventNeedApproval:
        // 需要用户审批（SDK 模式）
        ev.Approve() // 或 ev.Deny("reason")
    case goagent.EventUsageUpdate:
        fmt.Printf("Tokens: in=%d out=%d\n",
            ev.Usage.InputTokens, ev.Usage.OutputTokens)
    case goagent.EventCompaction:
        // 上下文压缩发生
    case goagent.EventProgress:
        // 进度/状态行。ev.StatusKey 非空时是「状态行原地更新」语义：
        // 客户端按 Key 替换显示（如 429 重试倒计时），ev.Text 为空 = 清除该行
    case goagent.EventDone:
        // 会话结束，ev.Messages 包含完整消息
    case goagent.EventError:
        fmt.Fprintf(os.Stderr, "错误: %v\n", ev.Error)
    }
}
```

### 可靠性：429 速率限制与重试

- **429 语义区分**：HTTP 429 返回 `provider.RateLimitError`（区别于过载 `OverloadError`）——速率限制切备用模型无济于事，必须等待
- **自动重试**：loop 对 `RateLimitError` 自动重试最多 10 次，等待对齐 TPM 窗口（15s 起步逐次递增至 60s）
- **状态行推送**：等待期间每秒推送带 `StatusKey` 的倒计时事件（形如「429 · Retrying in Xs · attempt N/10」），前端可原地更新显示
- **思考空回复恢复**：思考模型偶发「只输出 reasoning 无正文」的空回复，loop 有护栏自动恢复重试

### 沙箱（Sandbox）：工具执行层的隔离

Agent 对世界的全部作用力都经过工具调用——把路径解析与子进程派生这一个执行面拦住，单 agent、pipeline 节点、benchmark trial 三种模式自动全覆盖（都汇入 `ToolDef.call` 同一咽喉点，沙箱会话经 context 流到工具的 `Context.Sandbox` 字段）。

```go
// 目录沙箱：每次 Run 在 baseDir 下建独立临时根，产物即抛即弃
app := goagent.New(cfg, goagent.WithSandbox(
    goagent.NewDirSandbox(""),           // "" = os.TempDir()
    goagent.Policy{                       // 空 Policy 归一化为「沙箱根读写」
        Timeout: 30 * time.Second,        // Bash 单命令上限（min 语义）
    },
))

// worktree 沙箱：每次 Run 建独立 git worktree，天然可 diff/可回滚
// （benchmark 自迭代防 agent 改坏仓库的首选形态）
app := goagent.New(cfg, goagent.WithSandbox(
    goagent.NewWorktreeSandbox(repoRoot), goagent.Policy{},
))
```

强度阶梯：

| Tier | 实现 | 隔离强度 | 状态 |
|------|------|---------|------|
| 0 | 不配置 / `NoopSandbox` | 无（默认，零开销，行为与历史版本一致） | ✅ |
| 1 | `DirSandbox` / `WorktreeSandbox` | 进程内路径白名单（最长前缀匹配，无匹配拒绝）+ 工作副本 | ✅ |
| 2 | Docker / OS 级 | 进程隔离 | 接口预留（`ExecSession`） |
| 3 | WASM（wazero） | 能力模型 | 接口预留（L4 代码执行的地基） |

关键语义：

- **策略**：`Policy{FS []FSRule, Net, Env, Timeout}`——FS 白名单最长前缀优先，无匹配拒绝；Tier 1 只强制 FS 与 Timeout，Net/Env 携带不强制
- **优先级**：沙箱根非空时覆盖 `WithSessionWorkDir` 的工作目录（Bash/GitCommit/相对路径全部落沙箱内），仅在传入 `WithSandbox` 时激活
- **节点级覆盖**：`PipelineNode.Sandbox *Policy`——不受信的 fan-out 节点用更强策略，nil 继承 run 级
- **可读错误**：违规返回面向 LLM 的中文错误（「路径不在沙箱允许范围内」），模型可自纠重试
- **Tier 1 边界**：防意外不防恶意——进程内前缀检查挡不住工具代码里的 `os.Remove`，符号链接逃逸不防护。**进程级隔离是宿主部署者的责任**（不受信 workload 请用容器/VM 跑宿主进程）

#### run_js：goja 沙箱代码执行

内置的 `run_js` 工具让模型写一段 JavaScript，在嵌入式解释器（goja）里执行，用于计算、解析、数据变换。沙箱在场与否决定 JS 有没有文件能力，宿主不需要额外配置：

```go
// 形态 1：纯计算（无沙箱）——通往 os/fs/net 的调用路径物理不存在
app.UseTools(goagent.NamedTool{Name: "run_js", Def: builtin.RunJSTool()})

// 形态 2：沙箱注入（WithSandbox 生效时自动）——readFile/writeFile/listDir/stat
// 四个宿主函数都经 SandboxSession.ResolvePath 校验，能力与射程同时受限
app := goagent.New(cfg,
    goagent.WithSandbox(goagent.NewDirSandbox(""), goagent.Policy{Timeout: 30 * time.Second}),
    goagent.WithBuiltinTools(), // run_js 经 builtin.CoreTools() 一并注册
)
```

- **顶层 return**：代码包在一个函数里执行，直接写 `return {…}` 返回结果
- **资源限额**：超时默认 10s（`timeout_ms` 可调，上限 300s），到点强制中止；沙箱 `Policy.Timeout` 更小时以策略为准
- **失败也是产出**：编译错误带行号、运行错误带 JS 调用栈、路径违规带沙箱拒绝原因，全部结构化回传给模型修正
- **console.log**：输出附在结果后面

代码执行与 benchmark 的环境隔离都建立在这一层上；Tier 2/3 与后续阶段（确定性模式、WASM）见 [design/BACKLOG.md](design/BACKLOG.md)。

### 多会话多模式：会话级注入点

同一进程服务多个会话，每个会话可以是不同的「模式」（不同提示词风格 / 领域规范 / 可见工具 / 技能清单），切换模式只改会话映射，不重启进程。全部是可选注入点，未设置时行为与单模式完全一致：

```go
modeOf := func(sid string) string { return sessionModes[sid] } // 宿主自管的会话 → 模式映射

app := goagent.New(cfg,
    goagent.WithBuiltinTools(),
    goagent.WithSessionWorkDir(func(sid string) string { return projectDirs[sid] }),
    // 工具可见性：只影响本次 run 暴露给模型的工具表，不改注册表
    goagent.WithSessionToolFilter(func(sid, tool string) bool {
        return modes[modeOf(sid)].Allows(tool)
    }),
    // 提示词目录：返回 "" = 沿用全局 / 内置通用提示词
    goagent.WithSessionPromptDir(func(sid string) string { return modes[modeOf(sid)].PromptDir }),
    // 领域规范：追加在全局项目上下文之后
    goagent.WithSessionProjectContext(func(sid string) []string { return modes[modeOf(sid)].Rules }),
)

// Skill 工具按会话解析注册表：描述里的技能清单与执行时的查找都随会话走
app.Tool("Skill", builtin.SessionSkillTool(func(sid string) *skill.Registry {
    return modes[modeOf(sid)].Skills
}))
```

- `ToolDef.SessionDescription func(sessionID string) string`：任意工具都可按会话生成描述（非空覆盖静态 `Description`）。
- `builtin.ManagementDeps.SkillRegistryFn`：非空时 `ManagementTools` 注册会话感知版 Skill 工具。
- 已知未覆盖：pipeline 路径、compact / YOLO 分类器所用提示词仍取全局配置。

### Steering：运行中插话

长任务期间既不想中断、又不想等下一轮时，用双车道把信息送进正在跑的 run：

```go
app := goagent.New(cfg, goagent.WithSteering())
hub := app.Steering()

// guide 车道：在下一个工具批边界注入活跃 run（无活跃 run 返回 ErrNotSteerable）
_ = hub.Steer(sessionID, "用户刚在编辑器里改了 main.go，继续前请重新读取")

// queue 车道：排队，run 结束后自动取队首续跑；可 ListQueued / RemoveQueued 单项管理
item := hub.Enqueue(sessionID, "顺便把 README 也更新一下")
_ = item.ID
```

注入给模型的非对话文本（插话、压缩后重注入、工具结果内联提醒、后台任务完成通知）统一经 `reminder.Wrap` 打上 `<system-reminder source="…">` 标记，模型可辨识其工具性质，前端可按标记剥壳渲染。

### 动态 Pipeline：agent 现场建图

`create_pipeline` 工具让模型在运行时把大任务拆成 DAG 并执行：节点的工具只能从已注册的工具里按名选，节点间用 `push_task`（框架自动注入）向 `injects` 声明的下游推送任务，节点失败即终止。

```go
app.UseTools(app.CreatePipelineTool()) // 模型调用 create_pipeline 提交 spec

// 宿主也可以直接执行一份 spec
err := app.RunDynPipeline(ctx, &goagent.DynPipelineSpec{Nodes: []goagent.DynNodeSpec{
    {Name: "extract", Instruction: "提取文档要点", Tools: []string{"Read", "Glob"},
        Message: "读 docs/ 下的文档", Injects: []string{"report"}},
    {Name: "report", Instruction: "把要点整理成报告", Tools: []string{"Write"}},
}}, func(ev goagent.PipelineEvent) { log.Println(ev.Node, ev.Type, ev.Text) })
```

- 校验失败（有环、引用不存在的工具、超过 12 个节点、instruction 过长）返回模型可读的错误，模型修正后重试
- `depends_on` 省略时自动等待所有向它推送任务的上游；`output` 向下游声明本节点的产出格式
- 适合依赖关系由任务本身决定的多阶段任务（先提取→再润色→再审核）；需要现场判断的委派用子 agent

### HTTP 端点速览

`app.RunHTTP(addr)` 暴露 SSE 对话 + REST 管理面，所有 SSE 帧共用统一信封 `{seq, v, type, session_id, ...}`（`seq` 连接内严格递增且与线上顺序一致，跳号即丢帧）。`GET /protocol` 返回完整的帧类型与端点表。客户端可用 [`sdk/typescript`](sdk/typescript) 零依赖 TS 客户端。

| 类别 | 端点 |
|------|------|
| 对话 | `POST /chat`（SSE；会话忙时自动转 guide 插话）· `GET /runs/{session}/stream`（断线续传，`after=<session_seq>`）· `POST /interrupt`（需 `session_id`；带 `agent_id` 时只中断该子代理）· `POST /execute` |
| 交互 | `POST /approve` · `GET /pending-approval`（未决审批恢复）· `POST /askuser` · `GET /pending-ask`（未决提问恢复）· `POST /plan/confirm` |
| 排队 | `POST /queue` · `GET /queue` · `POST /queue/remove` |
| 运行时调参 | `GET/POST /mode`（权限模式；带 `session_id` 时只作用于该会话）· `GET/POST /thinking`（思考强度）· `GET/POST /model`（切模型） |
| 资源 | `/tasks` · `/plan` · `/bgtasks` · `/sessions` · `/sessions/{id}/messages` · `/agents`（子代理）· `/tools` · `/usage` · `/audit` |
| AG-UI | `POST /agui`（AG-UI 1.0 事件流）· `GET /agui/capabilities` |
| 元信息 | `GET /health` · `GET /protocol`（协议自描述：版本 + 帧类型表 + 端点表） |

宿主自有端点用 `WithHTTPRoutes` 挂到同一 mux。把工具与 agent 提供给其他 MCP 客户端用 `app.MCPHandler(opts)`（Streamable HTTP）或 `app.ServeMCPStdio(ctx, opts)`。

安全默认值：未配置 `WithHTTPAuthToken` 时只监听回环地址（对外服务需配置令牌，或在已受保护的网络中用 `WithHTTPInsecure()`）；跨站请求被拒绝，浏览器前端跨源访问用 `WithHTTPAllowedOrigins` 放行；不再自动批准工具调用，需显式配置 `WithApprover`。详见 [UPGRADE-v0.12.md](UPGRADE-v0.12.md)。

---
## 进阶指南

**刚入门** → [docs/tutorial.md](docs/tutorial.md)（三课渐进式教程：最小 agent → 工具与多轮 → Pipeline）

**按主题深入**（完整参考见 [docs/reference.md](docs/reference.md)）：

| 主题 | 一句话说明 | 适用场景 |
|------|----------|---------|
| [权限系统](docs/reference.md#权限系统) | 三态权限 + 6 种模式 + 可插拔分类器 | 生产环境安全控制 |
| [运行模式](docs/reference.md#运行模式) | CLI REPL / HTTP SSE / 嵌入式 SDK | 选择集成方式 |
| [会话管理](docs/reference.md#会话管理) | 多轮持久化 + 多会话并行（会话级工作目录） | Web 应用多用户 |
| [内置工具](docs/reference.md#内置工具) | Read/Write/Edit/GitCommit 等 + readstate 读写状态 | 文件类任务 |
| [中间件 / Hooks / Observer](docs/reference.md#中间件) | 拦截、事件、可观测性 | 审计与监控 |
| [子 Agent 系统](docs/reference.md#子-agent-系统) | LLM 驱动的任务委派 | 探索性大任务 |
| [Pipeline DAG 编排](docs/reference.md#pipeline-dag-编排) | 拓扑调度 + 并行 + Supervisor 审核 | 确定性多阶段任务 |
| [沙箱](#沙箱sandbox工具执行层的隔离) | 工具执行层隔离（Tier 0-3，路径白名单 + worktree 副本） | 不受信 agent / benchmark 自迭代 |
| [动态 Pipeline](#动态-pipelineagent-现场建图) | LLM 运行时自建 DAG | agent 自主拆解任务 |
| [内存与记忆](docs/reference.md#内存与记忆) | CLAUDE.md / Auto Memory / SessionMemory | 长期上下文 |
| [上下文压缩](docs/reference.md#上下文压缩) | 四层压缩 + Circuit Breaker | 长对话 |
| [Provider 配置](docs/reference.md#provider-配置) | 多端点 + 备用切换 + 运行时换模型 | 模型管理 |
| [Prompt 配置](docs/reference.md#prompt-配置) | 内置体系 / 自定义 / 外部覆盖 | 提示词工程 |

**架构深潜**：[architecture.md](docs/architecture.md) / [mechanisms.md](docs/mechanisms.md)


## 完整选项参考

```go
goagent.New(
    // === Provider ===
    ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},           // 直接配置（推荐）
    ProviderConfig{Type: "anthropic", Model: "claude-opus-4-6"},  // Anthropic
    WithOpenAI(),                                                  // 环境变量配置
    WithAnthropic(),                                               // 环境变量配置
    WithProvider(provider),                                        // 直接传入 provider.Provider 实例
    WithFallback(fallbackProvider),                                // 备用 Provider

    // === 运行模式 ===
    WithBuiltinTools(),                  // 开启所有内置工具
    WithToolKits(FileKit(), ShellKit()), // 按包注册工具
    WithSystemPrompt("你是一个..."),     // 系统提示词（纯字符串模式）
    WithPromptDir("./prompts"),           // 外部 prompt 目录覆盖

    // === 并发与限制 ===
    WithMaxTurns(100),                   // 最大循环次数（默认 100）
    WithMaxConcurrency(10),              // 最大并发工具数（默认 10）
    WithTokenBudget(100_000),            // Token 预算上限

    // === 权限 ===
    WithPermissionMode(PermissionAcceptEdits),  // 权限模式
    WithPermissionRules(rules),                  // 自定义规则
    WithApprover(approver),                      // 自定义审批者
    WithPermissionClassifier(classifier),        // 自定义分类器（替换 YOLO）

    // === 会话 ===
    WithSessionManager(mgr),           // 会话管理器
    WithAutoPersist(true),            // 自动持久化（默认开启）
    WithSessionWorkDir(fn),           // 会话 → 工作目录

    // === 会话级注入（多模式） ===
    WithSessionToolFilter(fn),         // 会话 → 可见工具
    WithSessionPromptDir(fn),          // 会话 → 提示词目录
    WithSessionProjectContext(fn),     // 会话 → 追加项目上下文

    // === 运行中交互 ===
    WithSteering(),                    // guide 插话 + queue 排队续跑
    WithSuspend(),                     // 挂起/恢复门闩
    WithPostCompactReminder(rs),       // 压缩后重注入源
    WithSessionStream(SessionStreamConfig{}), // 会话事件日志（断线续传）
    WithSubagentStream(SubagentStreamFull),   // 子代理事件全量并入父流
    WithMaxSubagents(8),                      // 每会话同时运行的子代理上限

    // === 推理 ===
    WithThinkingEffort("medium"),      // 思考强度 off/low/medium/high

    // === 内存 ===
    WithMemoryDir(".goagent/memory"),  // Auto Memory 目录
    WithProjectContext("CLAUDE.md"),   // 项目上下文文件

    // === 压缩 ===
    WithCompaction(CompactionConfig{
        AutoCompactThreshold: 0.8,
        MaxResultSize:        50_000,
    }),

    // === 增强 ===
    WithSessionMemory(cfg),  // 会话定期记忆提取
    WithSubAgents(defs...),  // 注册子 agents

    // === 中间件：构造后用 app.Use(mw, WithTools("Bash")) 注册 ===

    // === Hooks ===
    WithHooks(hook1, hook2),    // 注册 Hooks

    // === 可观测性 ===
    WithObservers(obs1, obs2),   // 注册 Observer
    WithCostTracking(),          // 启用成本追踪
    WithAnalytics(),             // 启用使用分析

    // === HTTP ===
    WithHTTPRoutes(routes),      // RunHTTP 额外路由
    WithHTTPAuthToken(token),    // 鉴权令牌（未配置时只监听回环地址）
    WithAGUI(AGUIConfig{ParkTimeout: 30 * time.Minute}), // AG-UI 停车超时

    // === Store 接口注入 ===
    WithTaskStore(myTaskStore),      // 自定义 Task 存储
    WithPlanStore(myPlanStore),       // 自定义 Plan 存储
    WithBgTaskStore(myBgTaskStore),   // 自定义 BgTask 存储
)
```

---

## 项目结构

```
goagent/
├── goagent.go              # 主包：App, New(), Tool(), Run*()
├── tool.go                 # ToolDef, Permission, Result
├── options.go              # 所有 Options（With*）
├── event.go                # Event, EventType
├── context.go              # Context（工具执行时的上下文）
├── middleware.go           # Middleware, Decision
├── toolkit.go              # ToolKit, QuickTool helpers
├── providers.go            # Anthropic/OpenAI 便捷创建函数
├── steering.go             # SteeringHub：guide 插话 / queue 排队双车道
├── streambus.go            # 按会话绑定的下发通道（ask/审批帧路由）
├── askuser_handler.go      # AskUser 提问路由 + 未决提问查询
├── sessionlog.go           # 会话事件日志（断线续传、交互按会话路由）
├── custom.go               # 自定义事件（Emit / EmitEvent / RegisterEvent）
├── subagents.go            # 子代理登记、指示、中断
├── agui.go                 # AG-UI 1.0 剖面（POST /agui）
├── mcpserver.go            # 把工具与 agent 暴露为 MCP 服务端
├── mcpelicit.go            # MCP elicitation → 会话提问
├── agenttool.go            # AgentTool 转接头（agent 包成 ToolDef）
├── pipeline.go             # Pipeline DAG 编排
├── dynpipeline.go          # 动态 Pipeline（LLM 运行时建图）
├── sandbox*.go             # 工具执行沙箱（Dir / Worktree）
├── retrieval.go            # RAG 门面
├── benchmark.go            # benchmark 门面（AgentTarget 等）
│
├── protocol/               # SSE 统一信封 + 协议自描述（agui/：AG-UI 1.0 线上类型）
├── httprr/                 # HTTP 录制回放（确定性测试）
├── reminder/               # <system-reminder> 注入通道
├── retrieval/              # RAG 四件套 + 参考实现
├── benchmark/              # 评测：Case/断言/Runner/Judge/报告
├── sdk/typescript/         # 零依赖 TypeScript 客户端（goagent-client）
│
├── internal/
│   └── loop/               # 核心 Agent 循环状态机
│       ├── loop.go        # 主循环（流式工具执行、压缩、重试、中断）
│       ├── scope.go       # 运行作用域：子运行继承检查链与观测
│       ├── steering.go    # 插话车道
│       └── suspend.go     # 挂起/恢复
│
├── provider/               # LLM Provider 接口
│   ├── provider.go        # Provider interface + Request/Response
│   ├── anthropic/          # Anthropic 实现
│   ├── openai/            # OpenAI 兼容实现
│   └── scripted/          # 脚本化模型（测试）
│
├── message/                # 消息类型 + Token 估算
│   ├── message.go         # Message, ContentBlock, ToolCall
│   └── token.go           # 类型感知 token 估算
│
├── executor/               # 工具执行引擎
│   ├── executor.go        # Executor + StreamingExecutor
│   └── tracked.go         # TrackedExecutor + Sibling Abort
│
├── permission/             # 权限系统
│   ├── permission.go      # Gate + 6 种模式 + 3 态 + Classifier 接口
│   ├── rules.go           # RuleSet + 优先级评估（工具模式与入参值匹配）
│   ├── yolo.go            # YOLO 分类器（默认的 Classifier）
│   └── edit.go            # 审批改参
│
├── compaction/             # 对话记录投影 + 多层上下文压缩
│   ├── compaction.go      # Manager + Circuit Breaker
│   ├── boundary.go        # Project：对话记录 → 模型视图
│   └── restore.go         # 压缩后文件恢复（已废弃，未接线）
│
├── memory/                 # 记忆系统
│   ├── memory.go          # Manager + BuildSystemPromptSuffix
│   ├── claudemd.go       # CLAUDE.md 三层加载
│   └── automemory.go     # Auto Memory + 去重 + 搜索
│
├── hooks/                  # Hooks 框架
│   └── hooks.go           # HookEvent + Manager + Hook interface
│
├── observer/               # Observer 可观测性接口
│   ├── observer.go        # Observer interface + MultiObserver + NopObserver
│   └── oteltrace/         # OpenTelemetry 导出（gen_ai.* 语义约定）
│
├── agent/                  # Sub-Agent 系统
│   ├── agent.go           # Definition + Runner（基于主循环）
│   ├── stream.go          # 工具调用活动摘要（进度显示）
│   └── mcp.go             # MCPServerConfig
│
├── mcp/                    # MCP 客户端（官方 Go SDK 承载）
│   ├── dial.go            # ServerConfig + Dial/Connect
│   ├── client.go          # Client 门面（ListTools/CallTool/OnToolsChanged）
│   ├── sdk.go             # stdio / Streamable HTTP、进度、取消、图片
│   ├── elicit.go          # elicitation（服务器向用户索取输入）
│   ├── tools.go           # MCP→框架工具转换
│   └── transport.go       # 早期传输实现（Deprecated）
│
├── session/                # 会话管理
│   ├── session.go         # Session + State + Metadata
│   ├── storage.go         # JSONL 持久化 + FileStore/MemoryStore
│   └── restore.go         # 会话恢复
│
├── builtin/                # 内置工具
│   ├── tools.go          # Read/Write/Edit/Glob/Grep
│   ├── bash.go           # Bash
│   ├── askuser.go        # AskUser
│   ├── js.go             # run_js（goja 沙箱执行器）
│   ├── management.go     # Task/Plan/BgTask/Skill 工具（含会话感知 Skill）
│   └── kits.go           # ToolKit 注册
│
├── task/                   # Task/Todo V2
│   ├── task.go           # Store + Task + 依赖管理
│   └── store.go          # StoreInterface 接口
│
├── plan/                   # Plan Mode
│   ├── plan.go           # Manager + Enter/Exit
│   └── store.go          # StoreInterface 接口
│
├── bgtask/                 # 后台任务
│   ├── bgtask.go         # Manager + TaskState
│   └── store.go          # StoreInterface 接口
│
├── skill/                  # Skill 系统
│   └── skill.go          # Registry + Execute
│
├── cron/                   # Cron 调度
│   └── cron.go           # Scheduler + jitter
│
├── worktree/               # Worktree 隔离
│   └── worktree.go        # Manager + Enter/Exit
│
├── thinking/               # Extended Thinking
│   └── thinking.go        # Config + adaptive/enabled/disabled
│
├── sysprompt/              # 动态 System Prompt
│   └── sysprompt.go      # Builder + Section + CacheControl
│
├── retry/                  # Rate Limit / Retry
│   └── retry.go           # Retry[T] + Backoff
│
├── budget/                 # Token Budget
│   └── budget.go          # Tracker + Diminishing Returns
│
├── cost/                   # Cost Tracking
│   ├── cost.go           # Tracker + 内置定价
│   └── observer.go        # Observer 包装（接入 loop）
│
├── analytics/              # 使用分析
│   ├── analytics.go      # Tracker + per-tool 统计
│   └── observer.go       # Observer 包装（接入 loop）
│
├── sessionmem/             # 会话记忆
│   └── sessionmem.go     # SessionMemory + 定期提取
│
├── extractmem/             # 记忆提取
│   └── extractmem.go     # Extractor
│
├── driver/                 # Driver 接口
│   └── driver.go         # Driver interface + Event types
│
├── cli.go                  # RunCLI 入口（终端界面在 cli/ 包，导入后可用）
├── cli/                    # 终端界面（Bubble Tea）
├── http.go                 # HTTP/SSE 服务器 + REST API
│
├── design/                 # 设计文档与待办（BACKLOG.md）
├── docs/                   # 教程、参考、架构说明
│
└── examples/
    ├── chatbot/           # 对话机器人示例
    ├── devops-bot/        # DevOps 助手示例
    ├── compaction-only/   # 压缩系统独立示例
    ├── executor-only/     # 执行器独立示例
    ├── dynpipe-verify/    # 动态 Pipeline 端到端验证
    ├── prompts/           # 自定义提示词示例
    ├── web-api/           # Web API 示例
    └── web-react/         # React 前端示例（Vite）
```

---

## 版本状态

| 版本 | 内容 | 状态 |
|------|------|------|
| v0.1 | 骨架：App + Tool + Loop + Compaction + Permission + Provider | ✅ 完成 |
| v0.2 | 增强：Token estimation + Preprocessing + Withholding + StopHooks + TrackedExecutor | ✅ 完成 |
| v0.3 | 生态：Session + Hooks + Memory/CLAUDE.md + Agent + MCP + Cost | ✅ 完成 |
| v0.4 | 新机制：Task + Plan + Skill + Cron + Worktree + Thinking + SysPrompt + Retry + Budget | ✅ 完成 |
| v0.5 | 完整对齐：Built-in Tools + WebSearch/WebFetch + MCP 工具注入 + CLI 命令 | ✅ 完成 |
| v0.6 | 可观测性重构：Observer 系统 + Store 接口 + Driver 接口 + 完整 REST API | ✅ 完成 |
| v0.7 | 多会话架构 + 工具质量对标：会话级工作目录、HTTP 多会话并行、Task/Issue 按会话分区、readstate 读写状态跟踪、429 TPM 重试 + 状态行、GitCommit 工具、Skill 清单内嵌 | ✅ 完成 |
| v0.8 | 嵌入者体验：WithLogger 日志注入（slog）、pipeline 错误严格传播、PipelineConfig.OnEvent 事件透出、WithIssueTools 独立 Option、nil provider 守卫、swarm 清理 | ✅ 完成 |
| v0.9 | 能力扩展：沙箱 Tier 0/1、动态 Pipeline L1、EventInterrupted、LLM observer 钩子、挂起/恢复、RAG 四件套、benchmark B0-B2、run_js、AgentTool、WithHTTPRoutes | ✅ 完成 |
| v0.10 | 运行时交互：Steering 双车道 + 条目化队列、统一信封协议 + TS SDK、system-reminder 通道、压缩后重注入、三级副作用调度、流中断恢复、后台任务终态回注、Skill frontmatter、权限模式/思考强度/模型运行时切换、提问帧路由修复 + 未决提问查询 | ✅ 完成 |
| v0.11 | 多模式：会话级工具过滤 / 提示词目录 / 项目上下文、会话级工具描述、会话感知 Skill 工具 | ✅ 完成 |
| v0.12 | 稳定性与安全加固：新增 auto 权限模式、bypass 模式的高风险操作经 YOLO 分类器把关、子运行继承权限门、工具入参校验、Pipeline 调度与审核语义修正、HTTP 安全默认值、逐条同步落盘、压缩写时复制、用量口径统一、WebFetch SSRF 防护，以及循环 / 执行器 / Provider / 记忆等模块的修复。升级说明见 [UPGRADE-v0.12.md](UPGRADE-v0.12.md) | ✅ 完成 |
| v0.13 | 架构收敛：子运行（pipeline / 团队 / 子 agent）统一使用主循环实现并结构性继承检查链与观测；对话记录与模型视图分开（自动摘要以摘要记录追加，重新加载不再重复摘要）；会话运行时（"始终允许"会话内跨运行有效、按会话切换权限模式、CloseSession）；终端界面移到 cli 包；清理死代码，`.yume` 改为 WithStateDir 配置。升级说明见 [UPGRADE-v0.13.md](UPGRADE-v0.13.md) | ✅ 完成 |
| v0.14 | 协议与生态：会话事件总线与断线续传、自定义事件、子代理治理（可寻址、单独插话/中断、事件并入父流）、MCP 换用官方 SDK（取消/进度/图片/列表变更/elicitation）并可作为 MCP 服务端、AG-UI 1.0 剖面（审批停车、前端工具、改参审批）、OpenTelemetry 导出、脚本化模型与 HTTP 录制回放、可插拔分类器。升级说明与宿主迁移清单见 [UPGRADE-v0.14.md](UPGRADE-v0.14.md) | ✅ 完成 |

详细架构说明见 [docs/architecture.md](docs/architecture.md) 和 [docs/mechanisms.md](docs/mechanisms.md)。

---

## 路线图

还没做的规划（Plan-Execute、Supervisor 与路由节点、PipelineState、Pipeline Registry、动态 Pipeline L2–L5、沙箱 Tier 2/3、
自迭代闭环等）与仍有参考价值的设计决策记录（与 eino / LangGraph 的定位差异、明确不做的方向）见
[design/BACKLOG.md](design/BACKLOG.md)。v0.14 的协议与生态设计见 [design/](design/)（PROTOCOL.md、SUBAGENTS.md、ECOSYSTEM.md）。

---

## 许可证

[MIT License](LICENSE) © 2026 残梦
