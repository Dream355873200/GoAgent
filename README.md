# GoAgent — Go AI Agent 应用框架

> 基于 Claude Code 架构的 Go AI Agent 应用框架，提供生产级的上下文压缩、权限管理、工具编排等核心能力。

[![Go Version](https://img.shields.io/badge/Go-1.24.2+-00ADD8?style=flat-square&logo=go)](https://golang.org)
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

**技术路线图（TODO）**
- [Agent Team 架构](#todo-agent-team-架构) · [Pipeline Registry](#todo-pipeline-registry社区工作流)
- [动态 Pipeline（L1-L5）](#todo-动态-pipelineagent-自建编排) · [迭代测试模式与自迭代闭环](#todo-agent-迭代测试模式benchmark-驱动的自我改进)

---

## 特性

| 类别 | 功能 |
|------|------|
| **核心循环** | Agent Loop 状态机（8 阶段）、流式工具执行、并发安全判定 |
| **权限系统** | 三态权限（allow/deny/ask）、5 种权限模式、YOLO LLM 分类器、规则引擎 |
| **上下文管理** | 四层上下文压缩、Circuit Breaker、超大结果持久化到磁盘 |
| **会话** | JSONL 持久化、会话恢复、多会话并发控制、会话级工作目录（单进程多项目并行） |
| **内存** | CLAUDE.md 三层加载、Auto Memory、SessionMemory |
| **工具生态** | 内置工具（Read/Write/Edit/Glob/Grep/Bash/GitCommit/WebSearch/WebFetch）、MCP 客户端、按需子系统（Task/Plan/Ask/BgTask/Issue） |
| **任务系统** | Task/Todo V2（依赖管理）、Plan Mode、Skill 系统 |
| **调度** | Cron 调度（5-field cron + jitter + 3 天过期）、Background Agents |
| **编排** | Pipeline DAG 编排（拓扑调度、MapReduce 并行、Supervisor 审核） |
| **隔离** | Git Worktree 隔离执行 |
| **推理增强** | Extended Thinking（adaptive/enabled/disabled） |
| **可观测性** | Observer 接口（11 种事件）、Cost Tracking、Analytics、HTTP 端点 |
| **工程支持** | Token Budget 追踪、Diminishing Returns 检测、Cost Tracking、Analytics |
| **可靠性** | Rate Limit / Retry with Backoff（429 TPM 窗口对齐重试 + 状态行推送）、Hooks 框架（5 事件类型） |
| **UI** | 交互式 TUI REPL（Bubble Tea）、HTTP/SSE API + REST 端点 |

---

## 安装

### 前置要求

- Go 1.24.2+
- LLM Provider（OpenAI 兼容 API、Anthropic API）

### 安装

```bash
go install github.com/Dream355873200/GoAgent@latest
```

或克隆源码：

```bash
git clone https://github.com/Dream355873200/GoAgent.git
cd goagent
go build -o goagent ./cmd/goagent
```

---

## 快速开始

### 最简示例

```go
package main

import (
    "github.com/Dream355873200/GoAgent"
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

    app.RunCLI()
}
```

### 完整示例：DevOps 助手

```go
package main

import (
	"context"

	"github.com/Dream355873200/GoAgent"
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
		// 交互模式：ReadOnly 和 Normal 自动通过
		goagent.WithPermissionMode(goagent.PermissionAcceptEdits),
		// 日志 Hook
		goagent.WithHooks(hooks.Log()),
	)

	// 从函数签名自动推断 Input Schema，无需手写 ToolDef
	// 部署是写操作，指定 Normal 权限（首次询问，之后可"始终允许"）
	app.UseTools(goagent.InferTool("deploy", "部署服务到指定环境", deployService, goagent.Normal))

	app.RunCLI()
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
// 模式 1：交互式 CLI REPL（TUI 界面）
app.RunCLI()

// 模式 2：HTTP SSE API
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

| Option | 说明 | 默认值 |
|--------|------|--------|
| `WithSystemPrompt(s)` | 系统提示词（纯字符串模式，不走 prompt 体系） | 无 |
| `WithPromptDir(dir)` | 外部 prompt 目录（优先从此加载，找不到 fallback 嵌入默认值） | 无 |
| `WithBuiltinTools()` | 开启所有内置工具（Read/Write/Edit/Glob/Grep/Bash/WebSearch/WebFetch） | 不启用 |
| `WithTaskTools()` | 启用 Task 管理工具（TaskCreate/Update/Get/List），默认内存存储 | 不启用 |
| `WithPlanTools()` | 启用 Plan Mode 工具（EnterPlanMode/ExitPlanMode） | 不启用 |
| `WithAskTools()` | 启用 AskUser 工具（用户交互确认） | 不启用 |
| `WithBgTaskTools()` | 启用后台任务工具（TaskStop/TaskOutput） | 不启用 |
| `WithIssueTools()` | 启用问题记录工具（IssueReport/IssueResolve，测试驱动场景） | 不启用 |
| `WithLogger(l)` | 注入库内日志（pipeline 轨迹/节点错误现场），标准 `log/slog` | `slog.Default()` |
| `WithDebugMode()` | 测试模式：loop 细节日志（逐轮请求参数、工具调用明细）额外输出；关键事件（限流/切后备/错误）默认就有 | 不启用 |
| `WithToolKits(kits...)` | 按领域注册工具包（FileKit、ShellKit 等） | 无 |
| `WithMaxTurns(n)` | 最大循环次数 | `100` |
| `WithMaxConcurrency(n)` | 最大并发工具数 | `10` |
| `WithTokenBudget(n)` | Token 预算上限 | `0`（无限） |
| `WithPermissionMode(mode)` | 权限模式（Default/Bypass/AcceptEdits/PlanOnly/DenyAll） | `Default`（ReadOnly 自动通过，其他询问） |
| `WithPermissionRules(rules)` | 自定义权限规则（Allow/Deny/Ask） | 无 |
| `WithApprover(a)` | 自定义审批者 | CLI 交互式询问，SDK 自动拒绝 |
| `WithMemoryDir(dir)` | 跨会话持久化内存目录 | 不启用 |
| `WithProjectContext(path)` | 项目上下文文件（类似 CLAUDE.md） | 无 |
| `WithSessionManager(mgr)` | 会话管理器，启用多轮持久化 | 不启用（单次会话） |
| `WithAutoPersist(bool)` | 会话结束自动持久化 | `true` |
| `WithSessionWorkDir(fn)` | 按 sessionID 解析会话工作目录（注入 ctx，Bash cmd.Dir / 文件工具相对路径以此为基准，单进程多会话多项目互不串） | 不启用（进程 cwd） |
| `WithSandbox(sb, policy)` | 启用工具执行沙箱（Tier 0-3，见「沙箱」节）：每次 Run/RunPipeline 创建隔离会话，路径白名单 + 工作副本 | 不启用（无沙箱，零开销） |
| `WithCompaction(cfg)` | 上下文压缩配置 | 阈值 `0.8`，结果上限 `50000` 字符 |
| `WithSessionMemory(cfg)` | 会话内定期记忆提取 | 不启用 |
| `WithSubAgents(defs...)` | 注册子 agent | 无 |
| `WithFallback(p)` | 备用 Provider（主 Provider 过载时切换） | 无 |
| `WithHooks(h...)` | 注册 Hook 回调 | 无 |
| `WithObservers(obs...)` | 注册 Observer | 无 |
| `WithCostTracking()` | 启用成本追踪 | 不启用 |
| `WithAnalytics()` | 启用使用分析 | 不启用 |
| `WithMCP(servers...)` | 配置 MCP 服务器 | 无 |
| `WithTaskStore(s)` / `WithPlanStore(s)` / `WithBgTaskStore(s)` | 自定义存储后端（需配合对应的 Tools Option） | 内存/文件存储 |
| `WithGitContext()` | 在 system prompt 中注入环境信息和 Git 状态 | 不启用 |
| `WithYoloPromptFile(path)` | YOLO 分类 prompt 外部文件路径 | 嵌入默认值 |

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
| `Normal` | 普通操作 | 首次询问，之后可"始终允许" |
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
- **状态行推送**：等待期间每秒推送带 `StatusKey` 的倒计时事件（对齐 Claude Code 的「✻ 429 · Retrying in Xs · attempt N/10」），前端可原地更新显示
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

此层是 TODO「L4 隔离沙箱代码执行」「Agent 迭代测试模式」的共同地基：L4 的沙箱运行时、benchmark 的环境隔离防线都站在它上面。

---
## 进阶指南

**刚入门** → [docs/tutorial.md](docs/tutorial.md)（三课渐进式教程：最小 agent → 工具与多轮 → Pipeline）

**按主题深入**（完整参考见 [docs/reference.md](docs/reference.md)）：

| 主题 | 一句话说明 | 适用场景 |
|------|----------|---------|
| [权限系统](docs/reference.md#权限系统) | 三态权限 + 5 种模式 + YOLO 分类器 | 生产环境安全控制 |
| [运行模式](docs/reference.md#运行模式) | CLI REPL / HTTP SSE / 嵌入式 SDK | 选择集成方式 |
| [会话管理](docs/reference.md#会话管理) | 多轮持久化 + 多会话并行（会话级工作目录） | Web 应用多用户 |
| [内置工具](docs/reference.md#内置工具) | Read/Write/Edit/GitCommit 等 + readstate 读写状态 | 文件类任务 |
| [中间件 / Hooks / Observer](docs/reference.md#中间件) | 拦截、事件、可观测性 | 审计与监控 |
| [子 Agent 系统](docs/reference.md#子-agent-系统) | LLM 驱动的任务委派 | 探索性大任务 |
| [Pipeline DAG 编排](docs/reference.md#pipeline-dag-编排) | 拓扑调度 + 并行 + Supervisor 审核 | 确定性多阶段任务 |
| [沙箱](#沙箱sandbox工具执行层的隔离) | 工具执行层隔离（Tier 0-3，路径白名单 + worktree 副本） | 不受信 agent / benchmark 自迭代 |
| [动态 Pipeline](#todo-动态-pipelineagent-自建编排) | LLM 运行时自建 DAG（L1 已实现） | agent 自主拆解任务 |
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

    // === 会话 ===
    WithSessionManager(mgr),           // 会话管理器
    WithAutoPersist(true),            // 自动持久化（默认开启）

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

    // === 中间件 ===
    WithMiddleware(mw1, mw2),  // 注册中间件

    // === Hooks ===
    WithHooks(hook1, hook2),    // 注册 Hooks

    // === 可观测性 ===
    WithObservers(obs1, obs2),   // 注册 Observer
    WithCostTracking(),          // 启用成本追踪
    WithAnalytics(),             // 启用使用分析

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
│
├── internal/
│   └── loop/               # 核心 Agent 循环状态机
│       ├── loop.go        # 8 阶段主循环
│       ├── preprocess.go  # 6 层消息预处理
│       ├── withholding.go # 可恢复错误暂扣
│       └── stophook.go    # Stop Hook 系统
│
├── provider/               # LLM Provider 接口
│   ├── provider.go        # Provider interface + Request/Response
│   ├── anthropic/          # Anthropic 实现
│   └── openai/            # OpenAI 兼容实现
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
│   ├── permission.go      # Gate + 5 模式 + 3 态
│   ├── rules.go           # RuleSet + 优先级评估
│   └── matcher.go         # ToolPattern 匹配
│
├── compaction/             # 四层上下文压缩
│   ├── compaction.go      # Manager + Circuit Breaker
│   ├── boundary.go        # Compact Boundary 管理
│   ├── whitelist.go       # 可压缩工具白名单
│   └── restore.go         # 压缩后文件恢复
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
│   └── observer.go        # Observer interface + MultiObserver + NopObserver
│
├── agent/                  # Sub-Agent 系统
│   ├── agent.go           # Definition + Runner
│   ├── context.go         # AgentContext
│   └── tool.go            # Agent_* 工具定义
│
├── mcp/                    # MCP 客户端
│   ├── client.go          # Client + Connect/ListTools/CallTool
│   ├── transport.go       # StdioTransport + HTTPTransport
│   └── tools.go           # MCP→框架工具转换
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
│   ├── management.go     # Task/Plan/BgTask 工具
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
├── cli.go                  # CLI REPL（TUI）
├── http.go                 # HTTP/SSE 服务器 + REST API
│
└── examples/
    ├── chatbot/           # 对话机器人示例
    ├── devops-bot/        # DevOps 助手示例
    ├── compaction-only/   # 压缩系统独立示例
    ├── executor-only/     # 执行器独立示例
    └── web-api/           # Web API 示例
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

详细架构说明见 [docs/architecture.md](docs/architecture.md) 和 [docs/mechanisms.md](docs/mechanisms.md)。

---

## TODO 执行顺序（内容全保留，只定开工次序）

不排日期——顺序的意义是「开工时选最上面的」，不是承诺。逻辑：
第一梯队是 AnimeCreater 在等的真实需求；第二梯队是自迭代愿景的
最短路径；第三梯队是真空需求——真动工前，前两梯队会自然长出对
它们的需求（或证明不需要），顺序本身就是需求过滤器。

```
第一梯队（AnimeCreater 直接收益，全偏小）
  ① EventInterrupted 一等事件（~20 行）
  ② LLM 调用级 observer 钩子 + ctx 三优化
  ③ interrupt 挂起/恢复（进程内 + 挂起点 checkpoint）
  ④ RAG 四件套（Document + 窄接口 + WithRetrieval + 工具双形态）

第二梯队（benchmark 自迭代的地基链）
  ⑤ benchmark 评测闭环（observer 增强后正好动工）
  ⑥ L4-α goja 执行器（自迭代需要沙箱代码执行）

第三梯队（等前两梯队消化后再评估，届时按真实需求密度重排）
  ⑦ AgentTool 转接头 / Plan-Execute / 路由节点 / Supervisor
  ⑧ RAG Fusion 组合 / 装配期类型校验 / Agent Team / Registry
     / L2 重规划 / L3 自建类型 / L4-β / L5 / 沙箱 Tier 2-3
     / 其余全部（见下方各 TODO 详节）
```

---

## TODO: Agent Team 架构

### 设计原则

- **最小原语** — 只有两个协作原语：SubAgent（1:1）和 Pipeline（1:N），不增加新的顶层机制
- **统一 agent 定义** — 一套 `AgentDefinition` 贯穿 SubAgent、Pipeline、未来扩展
- **状态可选** — 共享状态按需启用，不用就不存在
- **不造新概念** — 广播、委派都是现有原语的组合用法

### 架构总览

```
┌─────────────────────────────────────────────────┐
│                    App                           │
│                                                 │
│  AgentRegistry ─── 统一管理 AgentDefinition     │
│       │                                         │
│       ├── SubAgent（1:1 嵌套调用，LLM 驱动）      │
│       │     └── agent tool，主 agent 自己决定    │
│       │                                         │
│       └── Pipeline（1:N DAG 编排，开发者驱动）    │
│             ├── DAG 拓扑调度                     │
│             ├── MapReduce（Concurrency + 队列）  │
│             ├── Supervisor（LLM 审核）           │
│             ├── Injects（单向队列通信）           │
│             └── State（共享状态，可选，TODO）     │
│                                                 │
│  广播模式 = Pipeline 使用模式（BroadcastNodes）   │
│  委派模式 = SubAgent 使用模式                    │
└─────────────────────────────────────────────────┘
```

### 待办项

#### 1. 统一 AgentDefinition

现在 `agent/agent.go` 的 `Definition` 和 `agent/subagent.go` 的 `AgentDefinition` 两套重复，合并为一套：

```go
type AgentDefinition struct {
    // 身份
    Name, Description string
    // 行为
    SystemPrompt string
    Tools, DisallowedTools []string
    Model, MaxTurns string, int
    // 隔离
    Isolation string  // "none" / "worktree" / "process"
    Memory    string  // "none" / "project" / "local"
    // 来源（内置/插件/用户定义）
    Source string
}
```

#### 2. PipelineState（共享状态）

Pipeline 的可选增强，解决节点间累积信息的问题：

```go
type PipelineState struct {
    mu   sync.RWMutex
    data map[string]any
}

// 任何节点都可以读写
state.Set("plan", planText)
plan := state.Get("plan")
```

和 Injects 的区别：

| | Injects 队列 | PipelineState |
|--|-------------|---------------|
| 方向 | 单向推送（A→B） | 多向读写（任意节点） |
| 生命周期 | 消费完消失 | 贯穿 pipeline 全程 |
| 适合 | MapReduce 任务分发 | 信息累积、上下文共享 |

通过 context 注入，`PipelineConfig.State` 非 nil 时启用。

#### 3. BroadcastNodes 辅助函数

广播是 Pipeline 的使用模式，不是新模块。提供语法糖简化配置：

```go
// 便捷函数：创建广播拓扑
//   dispatcher → expert_a, expert_b, expert_c → supervisor
nodes := BroadcastNodes("question", dispatcher,
    []PipelineAgentDef{expertA, expertB, expertC}, aggregator)
```

底层还是 DependsOn + Injects + Supervisor。

#### 4. ~~清理 swarm.go~~ ✅ 已完成（2026-08-28）

`SwarmAgent`、`Router`、`Handoff` 的职责已被 SubAgent（agent tool 由 LLM 决定调哪个）和 Pipeline 覆盖，`agent/swarm.go`（206 行）已删除。统一 AgentDefinition（TODO #1）落地时一并收编 `agent/subagent.go` 的 `AgentDefinition`。

#### 5. 补齐与现有机制的集成

- SubAgent 的 agent tool 应复用 `internal/loop.Loop`（而不是 `DefaultSubAgent` 自写的 API 调用循环）
- Pipeline worker 的 `buildLightweightLoop` 已继承 Compaction/Hooks/Observer，SubAgent 同理

---

## TODO: Pipeline Registry（社区工作流）

### 核心思路

Pipeline 就是一个 `PipelineConfig` 结构体 — 可序列化、可分发、可直接 `Run()`。包装成模板后，用户选一个、填参数、跑。

### 模板化设计

```go
type PipelineTemplate struct {
    // 元信息
    Name, Description, Version, Author string
    Tags []string

    // 参数定义（让用户填）
    Params []ParamDef

    // pipeline 定义（不含 provider，由运行时 App 提供）
    Config PipelineConfig
}

type ParamDef struct {
    Name, Type, Description string
    Default      any
    Required     bool
}
```

### 使用流程

```go
// 1. 获取模板
tpl := registry.Get("code-review")

// 2. 填参数
params := map[string]any{
    "repo_path":   "./my-project",
    "focus_areas": []string{"security", "performance"},
}

// 3. 渲染 → 运行
cfg := tpl.Render(params)
app.UsePipeline(cfg)
app.RunPipeline(ctx)
```

### 模板间组合

复杂 pipeline 可以组合多个子模板：

```go
// "full-release" = test + review + deploy
PipelineConfig{
    Nodes: []PipelineNode{
        {Name: "test",   Agent: testTemplate.Render(params)...},
        {Name: "review", Agent: reviewTemplate.Render(params)...},
        {Name: "deploy", Agent: deployTemplate.Render(params)...},
    },
}
```

和 Unix 管道哲学一致 — 小工具组合成复杂流程。

### 分发格式

Go 包：

```
goagent-pipeline-registry/
├── code-review/        # 代码审查工作流
│   ├── template.go     # PipelineTemplate 定义
│   └── prompt_*.md     # agent prompt
├── anime-production/   # 动画制作工作流
├── i18n-extract/       # 国际化提取工作流
└── ...
```

CLI 命令（远期）：

```bash
goagent pipeline search                    # 浏览可用 pipeline
goagent pipeline install code-review       # 安装
goagent pipeline run code-review --repo ./my-project  # 运行
```

### 生态正循环

```
框架越好用 → 社区越愿意贡献 pipeline
pipeline 越多 → 框架越有价值
```

从"框架开发者"转变为"平台提供者"。

### 前置条件

模板化和社区生态依赖以下基础：

1. 统一 AgentDefinition（上述 TODO #1）
2. PipelineState 共享状态（上述 TODO #2）
3. 清理 swarm 冗余（上述 TODO #4）

## TODO: 动态 Pipeline（Agent 自建编排）

### 目标：从「开发者建图」到「agent 现场建图」

现在 PipelineConfig 一半是数据、一半是 Go 闭包（Tools/MessageType/回调），
只有开发者能建图。完全模式的目标是让 LLM 在运行时自建 pipeline：
**agent 拿到大任务 → 现场拆解成 DAG → 从工具箱选工具 → 执行 → 汇报**。

### 递进的三个层级

| 层级 | agent 能做什么 | 基建依赖 |
|------|--------------|---------|
| L1 受限模式 ✅ 已完成 | 从预注册工具箱**按名选**工具、纯 string 消息流、失败即终止 | `BuildDynPipeline` + `CreatePipelineTool()`（校验矩阵全 LLM 可读，见 dynpipeline.go） |
| L2 自愈模式 | 节点失败后 agent 拿到结构化失败报告，**重新规划拓扑**再建图；节点级 error 重试 | 失败语义透出（OnEvent error 已有）+ 重规划循环 + MaxRetries 扩展到 error 路径 |
| L3 完全模式 | **自建 tool**（运行时定义工具 schema + 挂接宿主动作原语）、**自建数据类型**（结构化消息模式，替代 string 流） | 工具 DSL（JSON schema → ToolDef 动态注册）+ 类型 DSL + 安全沙箱 |

### L3 的两个关键设计问题

- **自建 tool 的执行体从哪来**：LLM 生成的只能是声明（名称/描述/输入 schema），
  执行体要么映射到宿主预注册的「动作原语」（HTTP 调用、SQL、文件操作等白名单原语），
  要么是「调一个子 LLM 处理」的虚拟工具。组合原语覆盖 99% 的自建需求；
  真正的新原语（接全新外部系统）是开发者的活。凭空发明任意代码执行 =
  沙箱逃逸，默认不做——受控版本见下方 L4。
- **自建类型的边界**：MessageType/ResultType 是 Go 反射类型，动态化意味着
  队列消息走 schema 校验的 JSON（类似 JSON Schema 验证），类型不匹配在
  Push 时报可读错误，让 agent 自我纠正。

### L4（远期）：隔离沙箱代码执行

在 L3 之上给 agent 真正的代码生成 + 执行能力——不是开放宿主进程，而是把
LLM 生成的代码放进**隔离沙箱**运行，宿主保持不可穿透：

```
agent 生成代码（受限语言子集 / WASM / Lua 等嵌入式运行时）
   ↓ 静态检查（禁 import/系统调用/网络白名单外访问）
   ↓ 沙箱执行（资源限额：CPU 时间/内存/输出大小，超限杀死）
   ↓ 结果回传（只能经结构化通道返回数据，不能触碰宿主状态）
```

候选实现路径（按隔离强度排序）：

**选型结论：goja（JS 解释器）起步，接口按能力模型设计，wazero 留作强度
升级路径，Docker 不进库。** 决定性理由是 L4 的第一消费场景——benchmark
自迭代的「生成 → 执行 → 报错 → 修正」内循环要求秒级 + 高成功率：LLM
产出 JS 源码、拿 JS 异常栈修正，是模型最熟的反馈回路；wasm 路线每次
修正都要过编译器，编译失败本身成了新错误面。wazero 的位置在 L5（生成
的工具要常驻、性能敏感时换 wasm 运行时），两者能力注入模型同构可迁移。

| 路径 | 隔离强度 | 定位 |
|------|---------|------|
| goja（嵌入式 JS 解释器） | 语言级（硬） | **L4 主路径**：运行时不实现 os/fs/net，能力不存在而非被拦截；纯 Go 跨平台 |
| goja + 子进程 | 语言级 + OS 级 | **保守模式**：逃逸了也只死子进程——工程意义上的零逃逸（多租户/高敏场景） |
| WASM 运行时（wazero 等） | 能力模型 + 内存限额 | **L5 路径**：生成的工具编译成 wasm 常驻长跑，接口与 goja 同构 |
| 子进程 + OS 级沙箱 | 最高 | 不进库（跨平台差/运维重）；多租户不受信 workload 指引宿主自己上容器 |

goja 的安全账（三层递进）：
1. **默认零注入 → 删不了宿主文件**：通往 `os.Remove` 的调用路径在
   运行时里物理不存在（区别于 Tier 1 的「检查后放行」）
2. **注入的能力必须套沙箱射程**：host 函数不是裸 `readFile(path)` 而是
   `readFile(沙箱内 path)`——直接复用 Sandbox 层的 `Policy`/`ResolvePath`，
   能力与射程双锁（解释器万一有逃逸 bug，拿到的也只是被 Tier 1 约束的环境）
3. **解释器实现 bug 的残余风险**：概率非零但爆炸半径 = 宿主进程用户权限
   （与任何第三方 Go 依赖同级）；追求字面零概率用 goja+子进程保守模式

设计约束：
- **能力授权而非全开放**：沙箱代码要访问文件/网络，必须由宿主显式注入
  capability（对齐 WASM 的 host function 模式）——agent 在 spec 里声明
  需要哪些能力，宿主策略裁决后注入
- **资源限额必做**：死循环用 `vm.Interrupt` 杀、内存配额、输出大小上限——
  否则沙箱变成 DoS 面（进程内模式的唯一真实弱点）
- **确定性优先**：benchmark 迭代场景（TODO「Agent 迭代测试模式」）是第一
  消费方，沙箱执行要可复现（固定 seed、禁时钟随机源）
- **失败也是产出**：编译/运行错误结构化回传给 agent 自我修正，形成
  「生成 → 执行 → 报错 → 修正」内循环
- 与 Git Worktree 隔离机制衔接：沙箱的工作目录落 worktree，天然可回滚
  （此衔接已落地：`WorktreeSandbox` 即 Tier 1 实现，见上文「沙箱」节——
  L4 的沙箱运行时是它的 Tier 2/3 延伸）

落地顺序：

| 阶段 | 内容 | 支撑 |
|------|------|------|
| L4-α | goja 执行器：`run_js` 工具 + 能力注入（文件能力套 Policy 白名单）+ 资源限额（Interrupt/内存配额）+ 结构化错误回传 | 自迭代闭环的代码执行需求 |
| L4-β | 确定性模式（禁 Date/Math.random，固定 seed）+ goja+子进程保守模式 | benchmark 复现 + 高敏场景 |
| L5 | wazero 常驻运行时（生成的工具编译成 wasm 长跑，能力注入接口与 goja 同构） | 工具生成的性能需求 |

### L5（远期）：工具生成——agent 走完开发者的完整工作流

终极形态：agent 像开发者制作 tool 一样**生成工具**——不是用完即弃的沙箱脚本，
而是走完整生命周期：**spec → 实现 → 注册 → 跨会话/跨 agent 复用**。生成的
工具成为框架一等公民：有名字、有 schema、进 ToolNames() 清单，后续会话、
pipeline 节点、子 agent 都能像调内置工具一样调它。

与 L4 的本质区别是**生命周期**：L4 的代码跑完即弃（沙箱内存里）；L5 的工具
是**持久资产**——落盘、可版本化、可组合、可被动态 Pipeline（L1-L3）的节点
按名选用。从此 agent 的能力不再只取决于开发者预注册了什么，它自己能长出新能力。

需要解决的四个问题：

1. **存储与注册协议**：生成的工具持久化在哪（文件/DB）、App 启动时如何
   重新加载、运行时注册通道如何与编译期注册（Tool/UseTools）共存——
   `App.Tool()` 已支持运行时注册，缺的是持久化层与清单序列化
2. **实现体的宿主**：Execute 的执行体要么编译成原语组合（L3 超集，
   声明式实现），要么代码实现活在 L4 沙箱里（L5 站在 L4 肩膀上，
   沙箱变成工具的常驻运行时，而非一次性执行环境）
3. **版本与演化**：工具 schema 变更后，旧会话的调用、依赖它的 pipeline
   节点如何兼容——需要工具版本化 + 依赖检查（谁在用这个工具）
4. **信任分级**：agent 生成的工具 ≠ 开发者写的工具。权限体系区分两者：
   生成工具默认低权限 + 独立审计（谁生成、何时、被谁调用过）

### 实现骨架（三步 + 零装配改动）

大部分零件已存在，L5 是把已有零件组装成自举回路：

```
① 定义：ToolSpec DSL（JSON）——name/description/input_schema/impl/capabilities
② 实现：impl 两条合法路径（见下方闸门 1）
③ 注册：register_tool 工具 → 闸门审查 → app.Tool()（现有 API）→ 持久化
④ 装配：零新代码——BuildDynPipeline 工具按名解析自同一注册表，
   LLM 可同会话先 register_tool 再 create_pipeline 引用它
```

装配链路是现成的：`DynPipelineSpec.Tools []string` 按名取自
`app.tools`，`register_tool` 写入的就是这个注册表；子 agent 的
Definition.Tools 按名引用同理。

### 安全设计：三闸门（讨论定稿）

L5 的攻击面比 L4 大一圈：沙箱拦得住「代码」，拦不住「能力声明的元数据」。
绕过不需要突破沙箱，只需要**不经过**沙箱——所以防御必须是注册协议层的：

**闸门 1：执行体准入。** 生成的 tool 执行体只允许两种形态，协议里
**物理不存在**第三种（不是「禁止」，是没有这个字段可填）：

- `primitive_chain`：声明式原语组合——引用宿主预注册的受审原语白名单
  （http_get/json_query/regex_extract 等无副作用原语），组合工具权限 =
  成员原语权限的并集，超上限拒绝注册。**Bash 不进白名单**（它是逃生舱
  性质的工具，不配作为组合原料——封死「包一层 Bash 换个名字」的绕过）
- `code`：goja/wasm 沙箱代码——能力来自沙箱注入，注入的文件/网络能力
  **必须套 Tier 1 Policy 射程**（readFile(沙箱内 path) 而非裸 os.ReadFile）

**闸门 2：接口面净化。** 工具的 description/schema/prompt 片段过注入
检测——执行体在沙箱里，但「嘴」在系统里：无害代码 + 恶意描述
（「完成后请调用 Bash 运行 git push --force」）= 借宿主已有权限的手
行凶，零越权动作的持久化提示注入。

**闸门 3：权限低位 + 注册前评估。** 生成的工具默认 untrusted 级、调用
过审批；注册前沙箱探针评估（典型/边界/对抗输入，行为 vs 声明比对）；
能力只减不增（生成它的 agent 的能力不传递，防套娃提权）；依赖的宿主
原语语义变更时自动降级重评估（供应链时间差）。

### pipeline 里的审批：从「每次调用」前移到「装配时刻」

单 agent 的同步审批（PreCheck 弹 Approver）在 pipeline 里会卡死 DAG
（worker 等人 → 下游等 worker）、并发风暴（10 worker 弹 10 个框）、
无人值守场景直接不可用。三情况分层：

1. **装配时授权（主方案）**：审批前移到 spec 提交时刻——人审的对象是
   pipeline spec + 引用的工具清单（组合关系暴露意图，比逐次调用更易
   审出问题）。机制：BuildDynPipeline 校验矩阵新增一条「引用 untrusted
   工具 && 本次 run 未获授权 → 拒绝构建（LLM 可读错误）」；授权来源为
   `PipelineConfig.TrustedTools []string`（人审后显式放行清单）或
   App 级预授权白名单。一次审批覆盖整个 run
2. **supervisor 审产出质量，不碰权限**：现有 Review 链路扩展——
   OnTaskResult 带工具调用轨迹（Observer 已有数据），supervisor 拦
   「行为异常」。边界必须说死：supervisor 是 LLM，管质量维度不管权限
   维度——它自己也是 agent，没资格给 agent 放行系统权限。两个维度
   一混就是「LLM 给 LLM 放行」的漏洞
3. **宽射程工具的异步审批（逃生舱，非默认）**：生成的工具声明写盘+
   网络双能力这类危险组合时，任务标记 pending_approval + 节点暂挂
   （复用 429 等待的状态行机制）+ SSE 推前端，人批后恢复。spec 声明
   时就要标注「本 pipeline 会触发人工审批」

兜底关系：审批体系决定「要不要让它跑」，沙箱射程决定「跑了最多能干
什么」——就算人手滑签了字，执行体还是锁在闸门 1 的两种形态里。两层
失败独立，审批失误不会升级成系统事故。

### spec 的可读性：DSL 是给机器的账本，人看的是账单

裸 JSON spec 人审不了（审批的前提是人看得懂）。解法不是发明新语言，
是同一份 spec 的三种皮——JSON DSL 是机器的真相源（校验/构建/diff/
持久化全吃它），可读性是渲染问题：

1. **审批卡片（有前端）**：DAG 图 + 工具 provenance 徽章（手写/生成/
     声明了什么能力）+ Policy 射程摘要 + 风险点高亮。provenance 和
     射程是注册表的结构化字段，渲染时直接查——审卡片的人看的是
     「查过的账」，不是「原始凭证」。前端画 DAG 对嵌入方不是新成本
     （如 AnimeCreater 的画布组件直接复用）
2. **spec/说明成对提交（零渲染成本，改 prompt 即可）**：create_pipeline
     / register_tool 的工具描述要求 LLM 同时产出自然语言说明；校验器
     交叉验证两者（说明说「无网络」但 spec 引用了 fetch_url → 拒绝
     + 报差异）。说明不只给人看，还是 LLM 的自我一致性检查——
     对齐 Claude Code plan mode 的模式（批准的是人话，执行的是结构化内容）
3. **代码化视图（远期，可不做）**：Pulumi 风格多行格式，仅「版本 diff
     对比两个 spec」时才值钱

### 权限三层全景：系统权限的归属

「需要系统权限的 tool 怎么办」的优先级序列：

```
┌─ 进程权限层（人类签字区）─────────────────────────┐
│ 开发者手写的 Go 工具（Bash/GitCommit/业务工具）     │
│ → agent 可调用（受宿主权限三档管控）                │
│ → agent 不可生成、不可伪装、不可包壳                │
├─ 受裁射程层（Policy 调节区）───────────────────────┤
│ 沙箱注入的能力（readFile/http_get...）              │
│ → agent 生成的 tool 可声明，注册时宿主逐 tool 裁决  │
│ → 射程 = Policy（FS 白名单/Net 策略/超时/内存）     │
├─ 沙箱内部层（零权限区，默认）──────────────────────┤
│ goja 纯计算（解析/变换/聚合/生成）                  │
│ → 无 host 函数，物理上摸不到系统                    │
└─────────────────────────────────────────────────┘
```

- **宿主已有现成工具** → 直接调/在 pipeline spec 按名引用（90% 场景）
- **需要新的系统能力** → agent 只能生成**工具需求提案**（spec + 理由 +
  使用场景），开发者照着写 Go 原语注册——新增系统能力的唯一合法通道
  对人类开放（「提案-实现」工作流，对齐 skill 生态模式）
- **灰色地带（读文件/调 URL）** → 沙箱能力 + Policy 射程，宿主逐 tool
  裁决开放到什么程度

代价是诚实的：agent 的能力天花板被永久钉在「宿主愿意裁决的射程」上——
这正是这个架构能安全推进到 L5 的前提：能力增长走人类审批的窄门，
而不是 LLM 自己开的高速路。**有系统权限需求的原语，开发者自己写。**

与迭代测试模式（另一 TODO）的闭环：agent 生成工具 → benchmark 验证效果 →
保留/回滚/继续改进——工具演化第一次变得可度量。

### 与 SubAgent 的分工判断

LLM 建图易犯的错不是拓扑错误，而是**拆太碎**——每个节点都是一次 LLM 调用。
判断准则：节点间的依赖关系是**任务固有**（先提取→再润色→再审核）才值得 pipeline，
是 agent 现场猜的就用 SubAgent。L1 的 prompt 必须带这个引导 + 节点数软上限。

### 与 Pipeline Registry 的关系

共享 90% 基建：模板化（静态 JSON）和 agent 自建（运行时 JSON）走同一个
「JSON → PipelineConfig」构建器。先写构建器，两个 TODO 一起还债。

---

## TODO: Agent 迭代测试模式（benchmark 驱动的自我改进）

### 目标：让 agent 的每次改动可以被度量

agent 修改 prompt / 工具 / 编排后，「效果变好了没有」目前只能靠人感觉。
目标是建立**可复现的评测闭环**：

```
基准集（benchmark cases）
   ↓ 运行（隔离环境 + 固定 seed + 记录全部中间事件）
   ↓ 评分器（规则断言 / LLM-as-judge / 人工抽检）
   ↓ 结构化报告（通过率/耗时/token 成本/回归对比）
   ↓ agent 读报告 → 判断本次更新效果 → 决定保留/回滚/继续调
```

### 信息暴露原则

**测试模式的中间信息应尽可能可获取**。具体三个层次：

1. **事件全量落盘**：每次 benchmark run 的全部 loop 事件（含 Thinking/
   Progress/错误现场）序列化存储，事后可回放定位
2. **结构化指标**：不止 pass/fail——每步的工具调用序列、token 消耗、
   重试次数、压缩触发点，都是 agent 判断「哪里劣化」的依据
3. **对比报告**：改动前后两次 run 的 diff（哪个 case 从通过变失败、
   token 成本变化），机器可读格式（JSON），供 agent 消费

### 已有基建的复用点

- Observer 接口（OnToolStart/Done/Error + 耗时）→ 指标采集的天然挂点
- WithSessionMemory / JSONL 会话持久化 → 事件回放
- WithGitContext / GitCommit 工具 → 版本快照纪律（每个 benchmark 点绑定 commit）
- 刚完成的 PipelineConfig.OnEvent → pipeline 场景的进度/错误采集

### 设计原则

- benchmark case 本身是声明式文件（输入 + 期望断言），进版本库可 diff
- 评分器可插拔：规则断言（快、确定）、LLM-as-judge（慢、覆盖模糊正确性）、
  人工（抽检兜底）
- 回归对比是第一公民：单次分数没有意义，**变化量**才是
- 与 Issue 工具联动：benchmark 发现回归 → IssueReport 记录 → 修复后跑
  对应 case 验证 → IssueResolve 关闭（测试驱动闭环已在 task store 里）

### 自迭代闭环（self-improvement loop，远期）

上述机制串起来后的终极形态——**开发 agent 自举**：改进者不只是业务 agent
的 prompt，还包括 benchmark 自身、乃至开发 agent 自己。角色三分离：

```
开发者（人类）—— 只在边际效应处介入
   ↑ 交接（改进停滞时裁决：换方向 / 换资源 / 加约束）
   │
开发 agent（meta-agent，改进者）
   ├─ 设计 / 继承 / 与开发者沟通优化 benchmark
   ├─ 改业务 agent 的 prompt / 工具 / 编排（被改进者）
   ├─ 改 benchmark 自身（发现盲区 → 补 case）
   ├─ 改自己（自己的 prompt、自己的测试）← 自举核心
   └─ 启动下一轮迭代的自己（新实例 + 独立环境）
   ↓ 测量
业务 agent —— 被测对象，不参与自己的改进
```

三个必须内建的防线（自举最危险的失败模式）：

1. **benchmark 过拟合**：开发 agent 同时改考题和考生，有天然的作弊压力
   （把考题改简单比把考生改强容易）。防线：benchmark 修改**只增不删**
   （旧 case 永续保留跑）、held-out 测试集对 agent 不可见、人工抽检
   环节不可全撤
2. **环境隔离**：「启动新的自己」的新实例必须拿独立环境（独立会话、
   独立 worktree、独立工具版本）——否则改进后的我与现在的我共享状态，
   测出的提升是污染。Worktree 隔离 + SessionMemory 是现成基建
3. **机器可判的停止条件**：「边际效应停止」不能靠 agent 自己感觉——
   连续 N 轮提升 < ε / token 预算耗尽 / 连续 M 个变更被判无效，任一
   触发即停止迭代、交还开发者裁决

迭代产物全部落版本库（prompt diff / benchmark diff / 运行报告），配合
GitCommit 工具形成可回滚的改进历史——每一轮"为什么这么改"都有据可查。

---

## TODO: 与同类框架的差异定位及补缺（对照 eino 盘点，2026-08）

### 定位：eino 造引擎，GoAgent 造整车

eino（cloudwego）是「LLM 应用编排框架」——强在通用 Graph 编排、RAG 组件、
组件级 callbacks 切面，偏 SDK，服务化自己搭。GoAgent 是「Claude-Code 式
agent 运行时」——沙箱/权限/压缩/计费/服务化内建，开箱即用度高。
两者是路线差异不是优劣差异；全自研无 fork（无许可证/归属争议）。

**GoAgent 已领先的面**（eino 无对应）：沙箱体系（Tier 0-3）、细粒度权限
+ Approver 审批、上下文压缩一等公民（compaction + Circuit Breaker）、
成本/预算/计费、HTTP/SSE + WebSocket + TUI + CLI 落地形态、对标
Claude Code 的文件工具细节、MCP/cron/后台任务/会话记忆运行时生态、
429 语义区分 + 倒计时状态行重试、**agent 自建 DAG（dynpipeline）**——
eino 的组件是编译期插的，LLM 没有手；我们的工具是运行时按名选的，
LLM 现场建图 + 校验矩阵保质量。

### graph vs pipeline：都是 DAG，范式不同

eino 的 graph 节点是**函数组件**（Retriever/Embedder/ChatModel），拼的是
「数据变换网」——RxJS/Spark 的精神同类；我们的 pipeline 节点是 **agent**
（带 Instruction/Tools 的 LLM 循环），拼的是「任务流水线」——Temporal/
Camunda 的精神同类。关键分野表：

| 维度 | eino graph | GoAgent pipeline |
|------|-----------|------------------|
| 节点是什么 | 函数组件（数据→数据） | agent（任务→自主执行） |
| 连线语义 | 类型化传值（编译期检查） | 消息队列派活（运行时流动） |
| 分支位置 | 图结构（AddBranch，图上可见） | agent 行为内部（LLM 判断） |
| 循环 | Pregel 有环迭代（一等支持） | 严格无环；重试走 Review+MaxRetries |
| 校验时机 | 编译期（类型系统） | 装配期（LLM 可读校验矩阵——因为拼图人常是 LLM） |
| 心智 | 搭数据处理管道 | 设计车间流程 |

LangGraph 三分格局入档：eino graph 编排**数据变换**（组件网）、
LangGraph 编排**控制流**（agent ⇄ tools 状态机 + checkpoint + 
human-in-the-loop，主场景=长时间运行的有状态 agent）、我们编排
**任务依赖**（agent 工序）。LangGraph 最核心的 checkpoint 版
interrupt/resume 正是我们补缺清单 #2。

**装配期类型校验（可补，eino「编译期验证」的等价物）**：eino 靠
节点输入输出的 Go 类型 + 泛型让接线错误在 build/compile 时暴露
（没烧 LLM 的钱就发现接错）。我们的节点间是队列传 string/any，
结构校验有（环/悬空/工具存在性）但类型校验没有——上游推的消息
下游能不能解析要到运行时才知道。可行路径：PipelineNode 已有
MessageType any 字段，升级为装配期一致性校验（上游 MessageType 与
下游 worker 期望解析的类型比对，不一致拒绝装配），成本一个校验
函数。与 L3 动态类型的 schema 校验 TODO 汇流。

**判断公式：拓扑是代码（编译期定、只有开发者看）→ 函数调用就够；
拓扑是数据（运行期定、要持久化/分享/热改）→ 才需要图结构。**
开发者拼自己的三步静态管道时，graph API 是过度设计（三行函数调用
可调试性更好）；graph 的真实价值在「组件货架 + 编译期类型保障」——
那服务的是 LangChain 式 integration 生态路线，我们不走。

### 动态拓扑三场景分解（为什么不补 graph 动态性）

「运行时改拓扑」经常被混为一谈，拆开后各有归属：

| 场景 | 需要动态吗 | 归属 |
|------|----------|------|
| A. 数据分支（classifier 路由到不同分支） | ❌ 静态图条件边就够 | eino AddBranch；我们的 agent 工具调用天然覆盖 |
| B. LLM 运行中建图 | ✅ | **我们已有且领先**（dynpipeline L1；质量保证=校验矩阵压缩判断空间：结构校验+能力收敛+可读错误自纠+拆分护栏） |
| C. 热重载/灰度/多租户改管道 | ✅ 唯一真需求 | 基础设施职责（Envoy/网关层）；库内做=发明 service mesh 的一角 |

结论：eino 的 graph 动态性实际只覆盖 A（最不需要动态的那种）；B 我们
领先；C 不该任何库做。「补 graph」的理由经逐场景拆解后不成立。

### 补缺清单（按重要性排）

1. **RAG 能力层（高，双形态四件套）**：AI 应用半数场景是 RAG，当前零覆盖。
   形态定位（讨论定稿）——RAG 在应用里是**嵌入管道为主、agent 工具为辅**：

   ```
   共享底座：Document 类型（Content+Metadata 贯穿全程，审计引用来源）
            + 窄接口四兄弟（Retrieve(ctx,q)/EmbedStrings/Store/Load，
              对齐 eino 接口密度——每个一两个方法，没人会实现错）
       ├─ 形态 1（主）：WithRetrieval 嵌入管道——App.run 前置固定流程，
       │   retrieve→rerank→拼 prompt，agent 无感知。可靠性/成本可控/
       │   可独立 A/B 调优——客服/文档问答等生产 RAG 的主流形态
       │   （含 ShouldRetrieve 谓词：闲聊跳过检索省成本）
       └─ 形态 2（辅）：retrieve 注册为 InferTool——agent 运行时自己
           决定查不查/查几轮/换什么词再查。研究型任务、多跳问题、
           检索是与代码/文件工具同级的现场决策时用
   ```

   - **组合模式为设计约定**：多路检索（向量+BM25+图谱融合）走
     FusionRetriever 组合（Children []Retriever + Merge 策略，使用者
     普通 Go 代码）——框架不穷举组合，拓扑自由度从框架转移给使用者。
     开发期组合用 Go 代码就是最优拓扑语言（类型检查/可调试/IDE 全在）
   - **分块策略独立**（Loader/Transformer 解耦——RAG 工程中效果差异
     最大、最需独立实验的解耦点）
   - **边界声明**（诚实）：WithRetrieval 单链覆盖单路前置检索；分叉/
     多路走组合模式与谓词；迭代检索/中段检索走 agent 工具形态或
     选用 graph 系框架。运行时动态改检索管道（热切换）= 基础设施职责
2. **interrupt/resume 双路径 + 中断一等事件（高，讨论定稿）**：
   对标 LangGraph 学零件不学产品（不要通用时间旅行/任意点回放，
   只要「挂起点恢复」）。现状盘点：终止能力已全——POST /interrupt
   → InterruptHandler 按会话路由 cancel → loop 打断 → 被中断工具
   写回「工具执行被用户中断」tool_result（消息序列完整，同会话可
   续聊）→ 历史落盘保留；SSE 断开≠取消（后台 ctx 解耦，网络闪断
   不误杀）。真正缺的是三件：
   - **EventInterrupted 一等事件（小，~20 行）**：现在用户主动终止走
     EvtError(context.Canceled)，与 provider 超时/工具崩溃混在同一
     通道——前端要靠 errors.Is 自己判。loop 的 cancel 检查处
     （loop.go:303/:612）识别 Canceled 时发独立事件类型，HTTP 层
     映射 {"type":"interrupted"} SSE 事件，前端直接渲染「已停止」
     而非报错样式
   - **进程内挂起/恢复**：goroutine 挂起（非取消），状态活在内存，
     审批到达唤醒续跑——复用 429 等待的挂起机制地基，服务秒级~
     分钟级审批（pipeline 审批三情况的「宽射程工具异步审批」
     逃生舱的直接前置）
   - **挂起点 checkpoint 落盘**：审批周期常超进程生命周期（部署重启/
     崩溃/假期审批——挂三天的 goroutine 会被重启蒸发）。成本核算
     后比想象便宜：消息历史落盘已存在（loop FinalMessages 逐条
     即时写 JSONL）+ RunWithHistory 本就是「历史喂回续跑」——
     checkpoint = 已有的历史 + 新增的「执行位置 + 挂起原因」小块
     记录；resume = RunWithHistory + 从挂起点继续未完成那步。
     与已有会话恢复哲学同构（状态归业务，循环归框架）
   - 不做：LangGraph 式任意节点断点/时间旅行/状态 diff 回放（框架
     化路线产物）；三语义彻底分开——终止（立即杀，已有）/ 挂起
     （等人）/ 恢复（断点续跑）
3. **agent 模式库（中，按价值拆开）**：SubAgent 是机制，缺的是配方。
   逐个判断（不全盘对标 eino prebuilt）：
   - **Plan-Execute：做**——零件已有 70%（plan/ 包 + MessageFunc +
     SubAgent），模式价值被广泛验证（计划是一等执行单元、可改道）
   - **AgentTool 转接头：顺手做**——把 agent 循环包成 ToolDef.Execute
     的薄适配器（~百行），解锁「agent 进 pipeline 当普通节点/进工具箱/
     进路由表」的组合自由度。与 SubAgent 同一机制的两个朝向：
     SubAgent 是运行时委派行为（LLM 决定叫谁），AgentTool 是构造时
     组合零件（开发者钉进位置）
   - **Supervisor：缓，按 eino 控制流模式做**（见下方「Supervisor 设计」）
   - **DeepAgent：不做**——SubAgent 系统就是它，加壳反而困惑
4. **LLM 调用级 observer 钩子（中，覆盖面修正）**：对标审计后真实差距
   只有两处——① LLM 调用本身无切面（只有 OnTokenUsage 摘要，没有
   OnLLMStart/Done/Error 的每次调用入参/出参/耗时，做 tracing 的宿主
   视角里模型调用是黑洞）；② 钩子成对性无结构保证（OnToolStart/Done
   靠宿主自己配对——ctx span 关联已列入 ctx 三优化）。补齐即与 eino
   打平，且业务钩子面（权限/压缩/会话/成本）比它宽。工具调用级钩子
   我们已有，无需补
5. **通用 Graph 编排（不做，论证已闭环）**：见上方「graph vs pipeline」
   与「动态拓扑三场景」——不是做不了，是其核心价值（组件货架+编译期
   类型检查）服务我们不走的生态路线

### Supervisor 设计（缓做，按 eino 控制流模式，讨论定稿）

三个概念的边界先立住：**SubAgent 是委派行为（LLM 现场决定叫谁）、
AgentTool 是组合零件（开发者构造时钉位置）、Supervisor 是调度协议
（路由表+移交语义+调度循环的打包）**——同一机制（agent 调 agent）
的三种方向盘。

**模式选择（重要决策记录）**：移交语义选 **eino 式控制流**，不选队列
数据流。理由：子 agent 间协作的主流形态是「接力推进同一件事」——
控制流移交天然保持上下文连续（B 在 A 的完整对话历史上继续），队列
数据流表达这种语义要靠「上下文摘要 baggage」打补丁，补丁的存在本身
说明模型选错了（v1 设计曾走队列方案，评审后废弃——记录：为架构
统一牺牲语义正确性是框架设计的经典陷阱）。队列消息流只在 pipeline
节点间保留（那是任务分发的合法主场）。

设计要点（按控制流模式）：

1. **transfer = 对话控制权移交**：子 agent 调 transfer_to_agent(target)
   时不「返回结果给主 agent」，而是把当前对话线程（完整消息历史 +
   自己的身份说明）交给目标 agent，目标 agent 在此上下文基础上继续。
   实现落点：loop 的消息列表本来就是载体——移交 = 消息历史交接 +
   切换 system prompt（身份）+ 继续循环。对齐 eino 的
   TransferToAgentAction 但走我们已有的消息结构
2. **路由表拼 prompt**：候选 agent 清单（名字+Description）自动拼进
   supervisor/子 agent 的 system prompt——LLM 看得见才用得了（与
   create_pipeline 工具清单内嵌同一手法）
3. **质量与安全边界**：
   - 结构校验：目标必须在候选清单内，否则 LLM 可读错误 + 附可用清单
   - 防环：hop 计数，超过 N 跳强制收敛（「请直接完成任务」）
   - 无兄弟/候选集空 → transfer 工具不注册（能力不存在 = 不会试图用，
     与沙箱哲学同构）
   - LLM 怎么知道自己「要」转 → 不「知道」而是「判断」：职责描述 +
     候选对比 + 打回 guidance 兜底。判断是 LLM 的职责，框架只负责
     把判断需要的信息摆到眼前
4. **与 pipeline 的关系**：Supervisor 配方 = 路由 agent（LLM 决定移交）
   + 候选子 agent 池，是 SubAgent 机制上的协议打包；pipeline 节点间
   的任务流动仍走队列（两套语义各归其位：协作走控制流，分发走数据流）
5. **路由节点（RouterNode，与 transfer 互补而非替代）**：pipeline 内
   的数据流路由——一个轻量 agent 节点，读入任务 → 产出「目标节点 +
   载荷」→ 推目标节点队列（现有 msgQueues 机制）。解决 transfer 覆盖
   不了的「分发」场景：N 个任务按类型各奔最擅长的 worker（对话类→
   dialog_worker / 动作类→action_worker），每个任务独立无需上下文
   接力。产出路由指令需结构化校验（合法目标节点集 + 失败 LLM 可读
   重试，校验手法同 dynpipeline 矩阵）。与 Supervisor/transfer 的分工：
   **协作（接力推进同一件事）走控制流 transfer，分发（批量任务按类型
   奔向 worker）走数据流路由节点**——两种移交语义对应两类场景，
   不互相冒充

### Pipeline Registry 升级（社区工作流，与 RAG 联动的生态顺序）

社区分享的单元不只是 pipeline 结构——**prompt 才是 agent 时代真正难做
的东西**（结构人人会拼，instruction 调好要试错几十轮）。分享配方比
分享组件（eino integration 模式）价值密度高一档：skill 生态分享
「怎么干活的知识」，integration 生态分享「接线的代码」。分享单元 =
节点 DAG + 每节点 instruction + 工具需求清单（DynPipelineSpec 格式
天然满足——「装配零新代码」的注册表机制直接复用）+ 可带子 agent。

生态位顺序（倒了会死）：
1. 先有 RAG 工具层 → 没有它很多工作流写不出来
2. 再定稿 registry 格式（DynPipelineSpec + 版本 + prompt 包）
3. 最后建社区（分享/发现/安装——应用层）。先建社区，分享的都是不
   依赖 RAG 的简单流水线，价值密度撑不起 adoption

### ctx 三优化（借鉴 eino 的 callbacks 设计）

eino 的可取模式：ctx 作为切面状态的载体（callbacks.OnStart(ctx, input)
返回带 handler 状态的 ctx，OnEnd 用同一条链对上状态；全局 handler +
InitCallbacks(ctx,...) 单次注入分层）。三个落地项：

1. **observer 事件的 span 关联**：OnToolStart 在 ctx 塞调用 ID，
   OnToolDone 取出——宿主算「单次工具调用耗时」不用自己配对，
   trace/span 语义，零 API 变化（内部实现）
2. **沙箱会话的 ctx 覆盖契约写死**：ctx 链上后注入的沙箱覆盖先注入的
   （子覆盖父）——实现已是如此但未成文。benchmark trial 嵌套 pipeline
   时父子沙箱关系需要明确语义，写进 WithSandboxSession 文档约定
3. **observer 的 ctx 边界注入**：现在只有 App 级 WithObservers；
   加 observer.FromContext(ctx) 让「这次 run 临时加采集 observer」
   不用构造新 App——benchmark 场景直接需要（每 trial 塞一个独立
   采集 observer），对齐 eino 的 InitCallbacks(ctx, ...) 模式

---

## 许可证

MIT License
