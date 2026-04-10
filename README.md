# GoAgent — Go AI Agent 应用框架

> 基于 Claude Code 架构的 Go AI Agent 应用框架，提供生产级的上下文压缩、权限管理、工具编排等核心能力。

[![Go Version](https://img.shields.io/badge/Go-1.24.2+-00ADD8?style=flat-square&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)](LICENSE)

---

## 目录

- [特性](#特性)
- [安装](#安装)
- [快速开始](#快速开始)
- [New() 参数说明](#new-参数说明)
- [核心概念](#核心概念)
- [工具定义](#工具定义)
- [权限系统](#权限系统)
- [运行模式](#运行模式)
- [会话管理](#会话管理)
- [内置工具](#内置工具)
- [ToolKit 工具包](#toolkit-工具包)
- [中间件](#中间件)
- [Hooks 框架](#hooks-框架)
- [Observer 可观测性](#observer-可观测性)
- [子 Agent 系统](#子-agent-系统)
- [Pipeline DAG 编排](#pipeline-dag-编排)
- [内存与记忆](#内存与记忆)
- [上下文压缩](#上下文压缩)
- [Provider 配置](#provider-配置)
- [CLI 命令](#cli-命令)
- [完整选项参考](#完整选项参考)
- [项目结构](#项目结构)
- [版本状态](#版本状态)
- [TODO: Agent Team 架构](#todo-agent-team-架构)
- [TODO: Pipeline Registry（社区工作流）](#todo-pipeline-registry社区工作流)

---

## 特性

| 类别 | 功能 |
|------|------|
| **核心循环** | Agent Loop 状态机（8 阶段）、流式工具执行、并发安全判定 |
| **权限系统** | 三态权限（allow/deny/ask）、5 种权限模式、YOLO LLM 分类器、规则引擎 |
| **上下文管理** | 四层上下文压缩、Circuit Breaker、超大结果持久化到磁盘 |
| **会话** | JSONL 持久化、会话恢复、多会话并发控制 |
| **内存** | CLAUDE.md 三层加载、Auto Memory、SessionMemory |
| **工具生态** | 内置工具（Read/Write/Edit/Glob/Grep/Bash/WebSearch/WebFetch）、MCP 客户端、按需子系统（Task/Plan/Ask/BgTask） |
| **任务系统** | Task/Todo V2（依赖管理）、Plan Mode、Skill 系统 |
| **调度** | Cron 调度（5-field cron + jitter + 3 天过期）、Background Agents |
| **编排** | Pipeline DAG 编排（拓扑调度、MapReduce 并行、Supervisor 审核） |
| **隔离** | Git Worktree 隔离执行 |
| **推理增强** | Extended Thinking（adaptive/enabled/disabled） |
| **可观测性** | Observer 接口（11 种事件）、Cost Tracking、Analytics、HTTP 端点 |
| **工程支持** | Token Budget 追踪、Diminishing Returns 检测、Cost Tracking、Analytics |
| **可靠性** | Rate Limit / Retry with Backoff、Hooks 框架（5 事件类型） |
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
        // 模型的推理过程
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
    case goagent.EventDone:
        // 会话结束，ev.Messages 包含完整消息
    case goagent.EventError:
        fmt.Fprintf(os.Stderr, "错误: %v\n", ev.Error)
    }
}
```

---

## 工具定义

### 推荐方式：InferTool（自动推断）

从函数签名自动生成 JSON Schema 和序列化逻辑：

```go
type SearchInput struct {
    Query string `json:"query" desc:"搜索关键词" required:"true"`
    Limit int    `json:"limit,omitempty" desc:"结果数量限制"`
}

type SearchResult struct {
    Items []string `json:"items"`
    Total int      `json:"total"`
}

// 默认 ReadOnly（自动通过，无需确认）
app.UseTools(goagent.InferTool("search", "搜索互联网获取最新信息", search))

// 指定权限级别
app.UseTools(goagent.InferTool("deploy", "部署服务", deploy, goagent.Normal))
app.UseTools(goagent.InferTool("restart", "重启服务", restart, goagent.Dangerous))
```

函数签名必须是 `func(context.Context, T) (D, error)`，其中：
- `T` — 输入结构体，struct tags 自动生成 JSON Schema
- `D` — 返回给 LLM 的结果。一般用 `string` 即可；如果返回结构体会自动 `json.Marshal` 为 string

### InferTool Options

InferTool 除了权限级别，还支持通过 Option 配置工具行为：

```go
app.UseTools(goagent.InferTool("bash", "执行命令", bashFn, goagent.Normal,
    goagent.WithInterruptMode("block"),  // 中断时等待完成（适合长时间运行的命令）
    goagent.WithMaxResultSize(50000),    // 结果最大字符数
))
app.UseTools(goagent.InferTool("search", "搜索", searchFn,
    goagent.WithConcurrent(),           // 允许与其他工具并行执行
))
```

### 批量注册

```go
app.UseTools(
    goagent.InferTool("deploy", "部署服务", deploy, goagent.Normal),
    goagent.InferTool("status", "查看状态", status),
    goagent.InferTool("restart", "重启服务", restart, goagent.Dangerous),
)
```

### 结构体标签参考

工具的 Input 结构体支持以下标签：

| 标签 | 作用 | 示例 |
|------|------|------|
| `json:"fieldName"` | JSON 字段名 | `json:"name"` |
| `desc:"描述"` | 字段描述，展示给 LLM | `desc:"用户名"` |
| `enum:"a,b,c"` | 枚举值 | `enum:"read,write,delete"` |
| `required:"true"` | 必填字段 | `required:"true"` |
| `json:"name,omitempty"` | 可选字段 | `json:"limit,omitempty"` |

---

## 权限系统

### 权限模式

```go
// 交互式开发（默认）：ReadOnly 和 Normal 自动通过
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionAcceptEdits))

// CI/CD：跳过所有权限检查
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionBypass))

// 规划模式：只允许只读操作
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionPlanOnly))

// 拒绝所有写操作
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionDenyAll))
```

### 自定义权限规则

```go
rules := goagent.NewPermissionRules().
    Allow("Read", "").       // 允许所有只读操作
    Allow("Bash", "git *").  // 允许 git 命令
    Deny("Bash", "rm *").    // 禁止 rm 命令
    Deny("Bash", "dd *").    // 禁止 dd 命令
    Ask("Write", "").        // 写操作需要确认

app := goagent.New(goagent.WithPermissionRules(rules))
```

规则优先级：`deny > ask > allow`。

### 异步审批（Web/API 场景）

HTTP 或 WebSocket 模式下，Agent 需要暂停等待前端用户的审批。框架内置 `PermissionHandler`，三步接入：

```go
// 1. 创建并注册
permHandler := goagent.NewPermissionHandler()
app := goagent.New(
    goagent.ProviderConfig{...},
    goagent.WithApprover(permHandler),
)

// 2. 后台 goroutine：监听权限请求，推给前端
go func() {
    for req := range permHandler.Requests() {
        // 通过 SSE / WebSocket 推送给前端
        sendToFrontend(map[string]any{
            "type":        "permission_request",
            "request_id":  req.RequestID,
            "tool_name":   req.ToolName,
            "tool_input":  req.ToolInput,
            "permission":  req.Permission,
        })
    }
}()

// 3. 前端用户点允许/拒绝后，你的 HTTP handler 收到回调
permHandler.Resolve(requestID, true, false, "")         // 允许
permHandler.Resolve(requestID, false, false, "用户拒绝") // 拒绝
```

完整示例见 [examples/web-api/](examples/web-api/)。

### 自定义审批逻辑

如果需要完全自定义审批流程（如接审批流、消息队列），直接实现 `Approver` 接口：

```go
type MyApprover struct{}

func (a *MyApprover) Approve(toolName, input string, perm goagent.Permission) (bool, bool) {
    // 返回 (allow, alwaysAllow)
    // alwaysAllow=true 表示后续同类工具不再询问
    return true, false
}

app := goagent.New(goagent.WithApprover(&MyApprover{}))
```

### YOLO 权限分类器

对齐 Claude Code 的 YOLO 权限分类器，使用 LLM 子模型判断工具调用是否安全。

**工作原理：**
- 当 provider 存在时自动注入，无需手动配置
- 两阶段分类：Stage 1（快速，256 tokens）→ Stage 2（深度思考，4096 tokens）
- 安全工具白名单（Read/Grep/Glob/Task 等）跳过分类，直接允许
- API 不可用时 fail-open，退回到正常权限逻辑
- 在规则引擎之后、默认级别检查之前执行

**权限检查流程：**
1. Deny 规则（最高优先级）
2. 权限模式（Bypass/AcceptEdits/Plan/DontAsk）
3. Allow 规则
4. **YOLO LLM 分类器** ← 自动注入
5. 默认级别检查（ReadOnly/Normal/RequireApproval/Dangerous）
6. 用户审批（Approver）

**Transcript 上下文：** 分类器会读取最近 20 条对话记录作为上下文，判断操作是否与用户意图一致。

**Prompt 自定义：** 修改 `prompts/yolo-classifier.prompt.md` 可自定义分类规则。

---

## 运行模式

### CLI REPL

```bash
go run ./cmd/goagent
```

交互式 TUI 界面，支持：
- 彩色输出和格式化
- 工具执行状态实时显示
- 推理过程（Thinking）展示
- 会话切换（`/sessions`）
- 任务列表（`/tasks`）

### HTTP SSE API

```go
app.RunHTTP(":8080")
```

**REST 端点：**

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/chat` | SSE 流式传输对话 |
| `POST` | `/execute` | 同步执行，返回结果 |
| `POST` | `/approve` | 权限审批响应 |
| `GET` | `/health` | 健康检查 |
| `GET` | `/tools` | 工具列表 |
| `GET` | `/tasks` | 任务列表 |
| `POST` | `/tasks` | 创建任务 |
| `GET` | `/tasks/{id}` | 获取任务详情 |
| `PUT` | `/tasks/{id}` | 更新任务 |
| `DELETE` | `/tasks/{id}` | 删除任务 |
| `GET` | `/plan` | 计划状态 |
| `POST` | `/plan` | 进入计划模式 |
| `DELETE` | `/plan` | 退出计划模式 |
| `GET` | `/bgtasks` | 后台任务列表 |
| `GET` | `/bgtasks/{id}` | 后台任务详情 |
| `POST` | `/bgtasks/{id}/stop` | 停止后台任务 |
| `GET` | `/usage` | Token 使用成本统计 |
| `GET` | `/audit` | 工具执行分析 |

**示例：**

```bash
# 创建任务
curl -X POST http://localhost:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{"subject": "实现登录功能", "description": "包含用户名密码和OAuth"}'

# 获取成本统计
curl http://localhost:8080/usage

# 获取分析统计
curl http://localhost:8080/audit
```

### 嵌入式 SDK

```go
// 单次执行（同步）
result, err := app.Execute(ctx, "写一个 hello world")

// 带历史的 SDK 调用
history := loadHistoryFromDB(userID)
for ev := range app.RunWithHistory(ctx, history, "继续上次的工作") {
    saveEvent(ev)
}
```

---

## 会话管理

### 基本用法

```go
store := session.NewFileStore("./sessions")
mgr := session.NewManager(store)

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionManager(mgr),
    goagent.WithBuiltinTools(),
)

// 第一轮
for ev := range app.RunSession(ctx, "user-123", "帮我创建一个 API") {
    handleEvent(ev)
}

// 第二轮（自动加载历史）
for ev := range app.RunSession(ctx, "user-123", "加上错误处理") {
    handleEvent(ev)
}
```

### 会话存储后端

```go
// 文件存储（默认，适合 CLI）
store := session.NewFileStore(".yume/sessions")

// 内存存储（测试用）
store := session.NewMemoryStore()

// 自定义存储：实现 Store 接口
type Store interface {
    Get(ctx context.Context, id string) (*Session, error)
    Save(ctx context.Context, s *Session) error
    AppendMessage(ctx context.Context, sessionID string, msg Message) error
    List(ctx context.Context) ([]SessionSummary, error)
    Delete(ctx context.Context, id string) error
}
```

---

## 内置工具

一行开启所有内置工具：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

> **注意**: `WithBuiltinTools()` 包含核心开发工具（Read/Write/Edit/Glob/Grep/Bash/WebSearch/WebFetch）。
> AskUser、Task、Plan、BgTask 等子系统工具默认不启用，需通过对应 Option 显式开启。

| 工具 | 说明 | 权限 |
|------|------|------|
| `Read` | 读取文件，支持 offset/limit 和行号 | ReadOnly |
| `Write` | 创建或覆盖文件 | Normal |
| `Edit` | 精确字符串替换 | Normal |
| `Glob` | glob 模式文件搜索 | ReadOnly |
| `Grep` | 正则搜索，支持文件类型过滤 | ReadOnly |
| `Bash` | 执行 shell 命令，支持 timeout | Normal |
| `WebSearch` | 搜索互联网 | Normal |
| `WebFetch` | 获取网页内容 | Normal |

### Read

```go
type ReadInput struct {
    FilePath    string `json:"file_path" desc:"要读取的文件路径" required:"true"`
    Offset      int    `json:"offset,omitempty" desc:"起始行号（1-based）"`
    Limit       int    `json:"limit,omitempty" desc:"最多读取行数"`
    ShowLineNum bool   `json:"show_line_numbers,omitempty" desc:"是否显示行号"`
}
```

### Write

```go
type WriteInput struct {
    FilePath string `json:"file_path" desc:"目标文件路径" required:"true"`
    Content  string `json:"content" desc:"文件内容" required:"true"`
}
```

### Edit

```go
type EditInput struct {
    FilePath   string `json:"file_path" desc:"要修改的文件" required:"true"`
    OldString  string `json:"old_string" desc:"要替换的确切文本" required:"true"`
    NewString  string `json:"new_string" desc:"替换后的文本" required:"true"`
}
```

### Bash

```go
type BashInput struct {
    Command    string `json:"command" desc:"要执行的命令" required:"true"`
    Timeout    int    `json:"timeout,omitempty" desc:"超时时间（秒）"`
    WorkDir    string `json:"workdir,omitempty" desc:"工作目录"`
}
```

### 子系统工具（默认不启用）

以下工具默认不注册，需通过 Option 显式启用：

| Option | 工具 | 说明 |
|--------|------|------|
| `WithAskTools()` | `AskUser` | 向用户提问，获取输入 |
| `WithTaskTools()` | `TaskCreate/Update/Get/List` | 任务管理（依赖、状态） |
| `WithPlanTools()` | `EnterPlanMode/ExitPlanMode` | 规划模式（只读探索 + 计划编写） |
| `WithBgTaskTools()` | `TaskStop/TaskOutput` | 后台任务管理 |

```go
// CLI 应用：启用全部子系统
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithTaskTools(),
    goagent.WithPlanTools(),
    goagent.WithAskTools(),
    goagent.WithBgTaskTools(),
)

// Web 应用：只启用核心工具
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
)
```

---

## ToolKit 工具包

按领域分组的工具集合：

```go
// 只注册文件操作
goagent.WithToolKits(goagent.FileKit())

// 文件 + 搜索
goagent.WithToolKits(goagent.FileKit(), goagent.SearchKit())

// Shell 执行
goagent.WithToolKits(goagent.ShellKit())

// 用户交互
goagent.WithToolKits(goagent.InteractKit())

// 代码开发全套
goagent.WithToolKits(goagent.CodeKit())

// 全部内置工具
goagent.WithToolKits(goagent.AllKit())

// Web 搜索
goagent.WithToolKits(goagent.WebKit())
```

可用 ToolKit：

| ToolKit | 包含工具 |
|---------|---------|
| `FileKit()` | Read, Write, Edit |
| `SearchKit()` | Glob, Grep |
| `ShellKit()` | Bash |
| `WebKit()` | WebSearch, WebFetch |
| `InteractKit()` | AskUser |
| `CodeKit()` | FileKit + SearchKit + ShellKit |
| `AllKit()` | FileKit + SearchKit + ShellKit + WebKit |

---

## 中间件

中间件在工具执行前后拦截，用于日志、审计、限流等：

```go
type LoggingMiddleware struct{}

func (m *LoggingMiddleware) BeforeTool(ctx goagent.Context, name string, input json.RawMessage) *goagent.Dision {
    ctx.Logger.Info("tool called", "name", name)
    return nil // 继续执行
}

func (m *LoggingMiddleware) AfterTool(ctx goagent.Context, name string, result *goagent.Result, err error) {
    if err != nil {
        ctx.Logger.Error("tool failed", "name", name, "error", err)
    } else {
        ctx.Logger.Info("tool completed", "name", name)
    }
}

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithMiddleware(&LoggingMiddleware{}),
)
```

### 内置中间件

```go
// 限流中间件：每个工具每分钟最多调用 10 次
app := goagent.New(
    goagent.WithMiddleware(ratelimit.New(10, time.Minute)),
)

// 审计中间件：记录所有工具调用
app := goagent.New(
    goagent.WithMiddleware(audit.New()),
)
```

---

## Hooks 框架

Hooks 在 agent 循环的关键事件点触发：

```go
// 5 种 Hook 类型
app := goagent.New(
    goagent.WithHooks(
        // 工具执行前
        hooks.PreToolUse(func(ctx context.Context, toolName string, input json.RawMessage) error {
            log.Printf("calling %s", toolName)
            return nil
        }),
        // 工具执行后
        hooks.PostToolUse(func(ctx context.Context, toolName string, input, result string, err error) {
            log.Printf("%s returned: %s", toolName, result)
        }),
        // LLM 生成前
        hooks.PreInference(func(ctx context.Context, req *provider.Request) error {
            return nil
        }),
        // LLM 生成后
        hooks.PostInference(func(ctx context.Context, req *provider.Request, resp *provider.Response) error {
            log.Printf("response: %s", resp.Message.Content[0].Text)
            return nil
        }),
        // 循环退出前
        hooks.OnStop(func(ctx context.Context) error {
            log.Println("agent stopping")
            return nil
        }),
    ),
)
```

### 预置 Hooks

```go
hooks.Log()          // 结构化日志
hooks.Audit()        // 审计日志
hooks.Metrics()      // Prometheus 指标
hooks.RateLimit(10)  // 限流
```

---

## Observer 可观测性

Observer 是后推送式的可观测性接口，与 Hooks 的预拦截互补：

- **Hooks**：预拦截，适合权限控制和流程干预
- **Observer**：后推送，适合监控、计费和审计

### 快速开始

```go
// 启用成本追踪
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithCostTracking(),
)

// 启用使用分析
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithAnalytics(),
)

// 注册自定义 Observer
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithObservers(myPrometheusObserver, myAuditLogger),
)
```

### 内置 Observer

| 选项 | 说明 |
|------|------|
| `WithCostTracking()` | 启用 Token 用量和 USD 成本追踪 |
| `WithAnalytics()` | 启用工具执行分析（调用次数、耗时、错误率） |

### 自定义 Observer

实现 `observer.Observer` 接口：

```go
type MyObserver struct{}

func (o *MyObserver) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
    prometheus.Record("token_usage", float64(usage.InputTokens+usage.OutputTokens))
}

func (o *MyObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
    log.Printf("tool started: %s", toolName)
}

func (o *MyObserver) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
    log.Printf("tool done: %s (%v)", toolName, duration)
}

func (o *MyObserver) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
    log.Printf("tool error: %s - %v", toolName, err)
}

func (o *MyObserver) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
    audit.Log("permission_granted", toolName, permission)
}

func (o *MyObserver) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
    audit.Log("permission_denied", toolName, permission, reason)
}

func (o *MyObserver) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
    log.Printf("compaction: freed %d tokens, reason: %s", tokensFreed, reason)
}

func (o *MyObserver) OnSessionStart(ctx context.Context, sessionID string) {}
func (o *MyObserver) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {}
func (o *MyObserver) OnError(ctx context.Context, err error) {}
```

### 获取追踪数据

```go
// 获取成本统计
summary := app.Usage()
fmt.Printf("Total cost: $%.4f\n", summary.TotalCostUSD)

// 获取分析统计
analytics := app.Analytics()
fmt.Printf("Tool calls: %d, Errors: %d\n", analytics.TotalCalls, analytics.TotalErrors)
```

### HTTP 端点

通过 HTTP API 访问追踪数据：

```bash
curl http://localhost:8080/usage
curl http://localhost:8080/audit
```

---

## 子 Agent 系统

注册子 agent 供主 agent 调用：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSubAgents(
        agent.Definition{
            Name:        "researcher",
            Description: "专门做代码搜索和分析",
            SystemPrompt: "你是一个代码研究助手...",
            MaxTurns:    5,
        },
        agent.Definition{
            Name:        "reviewer",
            Description: "代码审查专家",
            SystemPrompt: "你是一个代码审查专家...",
            MaxTurns:    3,
        },
    ),
)
```

LLM 可以通过 `Agent_researcher`、`Agent_reviewer` 等工具调用子 agent。子 agent 的结果通过 `EventSubAgentProgress` 事件实时推送给主循环。

---

## Pipeline DAG 编排

Pipeline 是基于 DAG（有向无环图）的多 Agent 编排引擎，支持拓扑调度、并行 Worker、Supervisor 审核。

### 核心概念

| 概念 | 说明 |
|------|------|
| **Node** | DAG 中的一个 Agent 实例 |
| **DependsOn** | 节点依赖，拓扑排序决定执行顺序 |
| **Concurrency** | Worker 数量，>1 时自动创建消息队列 |
| **Injects** | 队列注入权限，控制节点间数据流 |
| **Supervisor** | 可选的审核节点，审批/拒绝 Worker 结果 |
| **Review** | 标记结果需要 Supervisor 审核 |

### 节点参数

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `Name` | string | 必填 | 节点唯一标识 |
| `Agent` | *PipelineAgentDef | 必填 | Agent 定义（Name/Instruction/Tools） |
| `Concurrency` | int | 1 | Worker 数量，>1 自动创建消息队列 |
| `DependsOn` | []string | 无 | 依赖节点列表 |
| `Message` | string | 无 | 初始消息（Concurrency=1 时使用） |
| `Injects` | []string | 无 | 可注入队列的目标节点名称 |
| `MessageType` | any | string | 队列消息元素零值（如 `Task{}`） |
| `ResultType` | any | string | 队列结果元素零值（如 `Result{}`） |
| `Review` | bool | false | 是否需要 Supervisor 审核 |
| `ReviewBatch` | int | 1 | 累积多少条结果后触发审核 |
| `MaxRetries` | int | 3 | 最大拒绝次数（超限自动通过） |
| `OnResult` | func | 无 | 结果回调（审核通过后或无需审核时自动调用） |
| `QueueSize` | int | 64 | 队列缓冲区大小 |

### 线性 Pipeline

```go
// 定义任务和结果的类型
type Task struct {
    ID   string `json:"id"`
    Data string `json:"data"`
}

type Result struct {
    TaskID    string `json:"task_id"`
    Processed string `json:"processed"`
}

app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "将以下任务拆分为 3 个子任务...",
            Injects:     []string{"worker"},
            Agent: &goagent.PipelineAgentDef{
                Name:        "splitter",
                Instruction: "将大任务拆分为小任务，通过 send_task 工具发送到 worker",
                Tools:       splitTools,
            },
        },
        {
            Name:        "worker",
            Concurrency: 3,
            DependsOn:   []string{"splitter"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent: &goagent.PipelineAgentDef{
                Name:        "worker",
                Instruction: "处理单个子任务",
                Tools:       processTools,
            },
            OnResult: func(result any) {
                res := result.(Result)
                fmt.Printf("处理完成 %s: %s\n", res.TaskID, res.Processed)
            },
        },
    },
})

app.RunPipeline(context.Background())
```

`splitter` 的工具通过 `GetMessageQueue` 向 `worker` 队列推送任务（见[队列注入](#队列注入)）。
`worker` 处理完毕后，结果按 `ResultDefault` 类型传递给 `OnResult` 回调。

### 并行扇出 + 合并

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // 阶段 1：拆分
        {
            Name:        "split",
            Concurrency: 1,
            Message:     "分析以下需求，拆分到 3 个专业方向...",
            Injects:     []string{"a", "b", "c"},
            Agent:       splitAgent,
        },
        // 阶段 2：并行处理（自动并行，Concurrency>1 时创建队列）
        {
            Name:        "a",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentA,
        },
        {
            Name:        "b",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentB,
        },
        {
            Name:        "c",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentC,
        },
        // 阶段 3：合并（等 a/b/c 全部完成）
        {
            Name:        "merge",
            Concurrency: 1,
            DependsOn:   []string{"a", "b", "c"},
            Agent:       mergeAgent,
        },
    },
})
```

### Supervisor 审核

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "generator",
            Concurrency: 3,
            Message:     "生成 5 个创意方案，每个方案通过 submit_idea 工具提交",
            MessageType: Idea{},
            ResultType:  Idea{},
            Review:      true,        // 结果需要 Supervisor 审核
            ReviewBatch: 2,           // 每 2 条触发一次审核
            MaxRetries:  2,           // 最多拒绝 2 次后自动通过
            Agent:       ideationAgent,
            OnResult: func(result any) {
                idea := result.(Idea)
                log.Printf("审核通过: %s — %s", idea.Title, idea.Summary)
            },
        },
    },
    Supervisor: &goagent.PipelineAgentDef{
        Name:        "supervisor",
        Instruction: "审核方案：有创意且可行的调用 approve_result 批准，否则调用 reject_result 给出改进建议",
        // Supervisor 自动获得 wait_for_review / approve_result / reject_result 工具
    },
})
```

`Review=true` 标记节点产出的结果需要 Supervisor 审核。Supervisor 是独立于 DAG 的"上帝节点"，不参与拓扑调度，自动获得三个审核工具：
- `wait_for_review` — 等待一批结果
- `approve_result` — 批准，触发 `OnResult`
- `reject_result` — 拒绝并附带改进建议，结果重新入队（超过 `MaxRetries` 后自动通过）

### 并行独立节点 + SharedData 注入

多个无依赖的节点自动并行执行，适合同一数据源的不同维度处理：

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // splitter：拆剧本，下游注入到 enrichers 和 storyboard
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "请读取项目剧本，拆分为各集，并识别角色、场景和道具。",
            Review:      true,
            Injects:     []string{"character_enricher", "scene_enricher", "storyboard"},
            Agent:       splitterAgent,
        },
        // worldview：与 splitter 并行，直接注入剧本内容提取世界观
        // 无 Injects/CloseQueues（无下游），独立完成
        {
            Name:        "worldview",
            Concurrency: 1,
            Message:     buildWorldviewMessage(ctx, services, projectID, userID),
            Review:      true,
            Agent:       worldviewAgent,
        },
        // enrichers 依赖 splitter...
    },
    SharedData: map[string]any{
        "project_id": projectID,
        "user_id":    userID,
    },
    Supervisor: supervisorAgent,
})
```

工具通过 `GetPipelineDataInt64` 从 SharedData 获取注入的 project_id：

```go
func NewCreateWorldviewEntryTool(svc *service.WorldviewService, userID int64) ga.NamedTool {
    return ga.InferTool("create_worldview_entry", "创建世界观条目",
        func(ctx context.Context, input CreateWorldviewEntryInput) (*CreateWorldviewEntryOutput, error) {
            // project_id 从 SharedData 自动获取，无需 LLM 传入
            pid, ok := ga.GetPipelineDataInt64(ctx, "project_id")
            if !ok || pid == 0 {
                return nil, fmt.Errorf("无法获取 project_id")
            }
            return svc.CreateWorldview(ctx, pid, userID, input)
        })
}
```

### 队列注入

工具内通过 `GetMessageQueue` 向下游节点推送任务：

```go
func processorTool(ctx goagent.Context, in SomeInput) (string, error) {
    downstream := goagent.GetMessageQueue(ctx, "next_stage")
    if downstream != nil {
        // Push 的类型需匹配下游节点的 MessageDefault
        downstream.Push(Task{ID: "1", Data: "处理这个任务"})
        downstream.Push(Task{ID: "2", Data: "处理那个任务"})
    }
    return "已推送 2 个任务到下游", nil
}
```

只有在当前节点的 `Injects` 中声明了目标节点名称，才能获取到队列。

---

## 内存与记忆

### CLAUDE.md

在项目根目录创建 `CLAUDE.md`，自动注入到每次会话的 system prompt：

```markdown
# 项目说明

这是一个 Go 微服务项目，使用 Gin 框架。

## 常用命令

- 启动服务：`go run ./cmd/server`
- 运行测试：`go test ./...`
- 构建：`go build -o server ./cmd/server`

## 代码规范

- 使用 golangci-lint 进行代码检查
- 提交前必须通过所有测试
```

通过 `WithProjectContext()` 也可以指定多个上下文文件：

```go
app := goagent.New(
    goagent.WithProjectContext("PROJECT.md"),
    goagent.WithProjectContext("ARCHITECTURE.md"),
)
```

### Auto Memory

自动记忆：对话结束后从历史中提取重要信息，保存到 memory 文件：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithMemoryDir(".goagent/memory"),
)
```

记忆文件结构：
- `MEMORY.md` — 主记忆文件
- `topics/` — 按话题分类的子记忆文件

### SessionMemory

长对话中定期提取关键信息，防止在上下文压缩时丢失：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionMemory(sessionmem.Config{
        MinTokensToInit:        10000,   // 首次提取的 token 阈值
        MinTokensBetweenUpdate: 5000,    // 两次提取之间的最小 token 差
        MemoryDir:              ".yume/.session-memory",
    }),
)
```

---

## 上下文压缩

当上下文接近 token 上限时，自动触发压缩：

1. **工具结果压缩**：超大结果（> MaxResultSize）持久化到磁盘，替换为引用
2. **Compact Boundary**：找到语义完整的边界（如函数定义、markdown 标题）
3. **摘要压缩**：对保留的消息进行 LLM 摘要
4. **Circuit Breaker**：连续两次压缩效果不好时停止，防止过度压缩

```go
app := goagent.New(
    goagent.WithCompaction(goagent.CompactionConfig{
        AutoCompactThreshold: 0.8,  // 上下文达到 80% 时触发
        MaxResultSize:        50_000, // 单个结果最大字符数
    }),
)
```

---

## Provider 配置

### 方式一：直接使用 ProviderConfig（推荐）

```go
// 连接本地 Ollama（无需 API Key）
app := goagent.New(goagent.ProviderConfig{
    Model:   "qwen2.5:7b",
    BaseURL: "http://localhost:11434/v1",
})

// 连接 OpenAI
app := goagent.New(goagent.ProviderConfig{
    Model:  "gpt-4o",
    APIKey: "sk-...",
})

// 连接 OpenRouter
app := goagent.New(goagent.ProviderConfig{
    Model:   "anthropic/claude-3.5-sonnet",
    APIKey:  "sk-or-...",
    BaseURL: "https://openrouter.ai/api/v1",
})

// 连接 vLLM
app := goagent.New(goagent.ProviderConfig{
    Model:   "meta-llama/Llama-3-8b",
    BaseURL: "http://localhost:8000/v1",
})
```

### 方式二：使用 Anthropic

```go
app := goagent.New(goagent.ProviderConfig{
    Type:   "anthropic",
    Model:  "claude-opus-4-6",
    APIKey: "sk-ant-...",
})
```

### 方式三：环境变量（WithOpenAI / WithAnthropic）

全部参数从环境变量读取，适合 `.env` 配置：

```go
// OpenAI 兼容：读取 OPENAI_MODEL, OPENAI_BASE_URL, OPENAI_API_KEY
app := goagent.New(goagent.WithOpenAI())

// Anthropic：读取 ANTHROPIC_MODEL, ANTHROPIC_API_KEY
app := goagent.New(goagent.WithAnthropic())
```

### 混合使用

`ProviderConfig` 和 `With*` Option 可以混用：

```go
app := goagent.New(
    goagent.ProviderConfig{
        Model:   "deepseek-chat",
        APIKey:  "sk-...",
        BaseURL: "https://api.deepseek.com/v1",
    },
    goagent.WithSystemPrompt("你是一个 AI 助手"),
    goagent.WithMaxTurns(50),
    goagent.WithBuiltinTools(),
)
```

### 备用 Provider

当主 Provider 负载高时自动切换到备用：

```go
app := goagent.New(
    goagent.WithProvider(primaryProvider),
    goagent.WithFallback(fallbackProvider),
)
```

### 运行时模型切换

```go
app.SetModel("gpt-4-turbo")  // 返回 false 表示不支持
```

---

## Prompt 配置

GoAgent 内置了一套对齐 Claude Code 的中文提示词体系，同时支持用户自定义。

### 方式一：使用内置提示词（默认行为）

不传任何 prompt Option，自动加载嵌入的 7 个 prompt 文件（对齐 Claude Code）：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

内置提示词包含以下组件（位于 `prompts/` 目录），对齐 Claude Code：

| 文件 | 用途 | 状态 |
|------|------|------|
| `system-identity.prompt.md` | Agent 身份定义 | ✅ 已集成 |
| `system-doing-tasks.prompt.md` | 执行任务指令 | ✅ 已集成 |
| `system-actions.prompt.md` | 谨慎执行操作 | ✅ 已集成 |
| `system-using-tools.prompt.md` | 工具使用策略 | ✅ 已集成 |
| `system-tone-style.prompt.md` | 语气和风格 | ✅ 已集成 |
| `system-output-efficiency.prompt.md` | 输出效率 | ✅ 已集成 |
| `system-reminder.prompt.md` | 系统提醒 | ✅ 已集成 |
| `compact.prompt.md` | 上下文压缩提示词 | ✅ 已集成 |

### 方式二：完全自定义

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithSystemPrompt("你是一个专注于代码审查的 AI 助手..."),
)
```

### 方式三：外部目录覆盖

通过 `WithPromptDir` 从外部目录加载 prompt 文件，找不到的文件会自动 fallback 到嵌入默认值：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithPromptDir("./my-prompts"),
)
```

可使用 `prompts.ExportDefaults(dir)` 一键导出所有默认 prompt 到指定目录，然后修改文件后重启生效。

### 提示词变量

内置提示词支持以下变量自动替换：

| 变量 | 说明 |
|------|------|
| `$cwd` | 当前工作目录 |
| `$boolean` | 是否为 git 仓库 |
| `$OS` | 操作系统 |
| `$OS_version` | 系统版本 |
| `$date` | 当前日期 |
| `$knowledge_cutoff` | 模型知识截止日期 |
| `$gitStatus` | Git 状态摘要 |

### 自定义提示词文件

从自定义文件加载提示词：

```go
import "github.com/Dream355873200/GoAgent/prompts"

// 加载内置提示词
identity := prompts.MustLoad(prompts.Identity)

// 带变量替换
content := prompts.LoadWithVars(prompts.Compact, map[string]string{
    "cwd":     "/path/to/project",
    "date":    "2024-01-15",
})
```

### WithPromptDir 外部目录

通过 `WithPromptDir(dir)` 指定外部 prompt 目录，框架优先从此目录加载 prompt 文件，找不到的文件会自动 fallback 到嵌入的默认值。

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithPromptDir("./my-prompts"),
)
```

**一键导出并修改**：

```go
import "github.com/Dream355873200/GoAgent/prompts"

// 导出所有默认 prompt 到目录（已存在的不覆盖）
files, _ := prompts.ExportDefaults("./my-prompts")
fmt.Println("已导出:", files)
// 修改 ./my-prompts/system-identity.prompt.md 后重启生效
```

**文件名对应**：

| 文件名 | 说明 |
|--------|------|
| `system-identity.prompt.md` | Agent 身份定义 |
| `system-doing-tasks.prompt.md` | 执行任务指令 |
| `system-actions.prompt.md` | 谨慎执行操作 |
| `system-using-tools.prompt.md` | 工具使用策略 |
| `system-tone-style.prompt.md` | 语气和风格 |
| `system-output-efficiency.prompt.md` | 输出效率 |
| `system-reminder.prompt.md` | 系统提醒 |

### 项目上下文（CLAUDE.md）

推荐使用 `CLAUDE.md` 注入项目特定知识：

```go
app := goagent.New(
    goagent.WithProjectContext("CLAUDE.md"),  // 项目根目录
    goagent.WithProjectContext("docs/ARCHITECTURE.md"),  // 额外上下文
)
```

`CLAUDE.md` 示例：

```markdown
# 项目说明

这是一个 Go 微服务项目，使用 Gin 框架。

## 代码规范

- 使用 golangci-lint 进行代码检查
- 提交前必须通过所有测试

## 常用命令

- 启动服务：`go run ./cmd/server`
- 运行测试：`go test ./...`
```

---

## CLI 命令

在 TUI REPL 中可用以下命令：

| 命令 | 说明 |
|------|------|
| `/model <id>` | 切换模型 |
| `/sessions` | 列出所有会话 |
| `/session <id>` | 切换到指定会话 |
| `/clear` | 清空当前会话，创建新会话 |
| `/tasks` | 显示任务列表 |
| `/cost` | 显示当前会话费用统计 |
| `/permissions` | 显示当前权限模式 |
| `/help` | 显示帮助 |

---

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

详细架构说明见 [docs/architecture.md](docs/architecture.md) 和 [docs/mechanisms.md](docs/mechanisms.md)。

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

#### 4. 清理 swarm.go

现有 `SwarmAgent`、`Router`、`Handoff` 的职责已被 SubAgent（agent tool 由 LLM 决定调哪个）和 Pipeline 覆盖：

- `AgentDefinition` → 合并到统一定义
- `SwarmAgent.Handoff()` → 被 SubAgent 的 agent tool 取代
- `Router` → 不需要，LLM 自己决定交接目标
- `SwarmConfig` → 被PipelineConfig 和 SubAgent 覆盖

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

基础扎实后，模板化和社区生态水到渠成。

---

## 许可证

MIT License
