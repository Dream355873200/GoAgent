# GoAgent — Go AI Agent 应用框架

> 基于 Claude Code 架构的 Go AI Agent 框架，约 10,000 行 Go 代码复刻 Claude Code 25,000+ 行 TypeScript 的核心逻辑。

[![Go Version](https://img.shields.io/badge/Go-1.24.2+-00ADD8?style=flat-square&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)](LICENSE)

---

## 目录

- [特性](#特性)
- [安装](#安装)
- [快速开始](#快速开始)
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
| **权限系统** | 三态权限（allow/deny/ask）、5 种权限模式、规则引擎 |
| **上下文管理** | 四层上下文压缩、Circuit Breaker、超大结果持久化到磁盘 |
| **会话** | JSONL 持久化、会话恢复、多会话并发控制 |
| **内存** | CLAUDE.md 三层加载、Auto Memory、SessionMemory |
| **工具生态** | 内置文件工具（Bash/Read/Write/Edit/Glob/Grep/AskUser）、MCP 客户端 |
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
    "os"

    "github.com/Dream355873200/GoAgent"
)

func main() {
    app := goagent.New(
        // 使用 OpenAI 兼容 API（支持 Ollama、OpenRouter、vLLM 等）
        goagent.WithProvider(goagent.OpenAI(openai.Config{
            BaseURL: "http://localhost:11434/v1",
            Model:   "qwen2.5:7b",
        })),
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
    "os"

    "github.com/Dream355873200/GoAgent"
    "github.com/Dream355873200/GoAgent/hooks"
)

type DeployInput struct {
    Service string `json:"service" desc:"服务名称"`
    Env     string `json:"env" desc:"目标环境：dev/staging/prod"`
}

func main() {
    app := goagent.New(
        goagent.WithProvider(goagent.OpenAI(openai.Config{
            APIKey:  os.Getenv("OPENAI_API_KEY"),
            BaseURL: "https://api.openai.com/v1",
            Model:   "gpt-4o",
        })),
        goagent.WithSystemPrompt("你是一个 DevOps 助手，擅长自动化部署和运维任务"),
        goagent.WithBuiltinTools(),
        // 交互模式：ReadOnly 和 Normal 自动通过
        goagent.WithPermissionMode(goagent.PermissionAcceptEdits),
        // 日志 Hook
        goagent.WithHooks(hooks.Log()),
    )

    // 注册自定义工具
    app.Tool("deploy", goagent.ToolDef{
        Description: "部署服务到指定环境",
        Input:       DeployInput{},
        Permission:  goagent.Dangerous,
        Execute: func(ctx goagent.Context, in DeployInput) (string, error) {
            ctx.Logger.Info("开始部署", "service", in.Service, "env", in.Env)
            return "部署成功", nil
        },
    })

    app.RunCLI()
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

## 核心概念

### App — 应用入口

`goagent.New()` 创建应用，链式配置：

```go
app := goagent.New(
    goagent.WithProvider(provider),
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

### 基本语法

```go
type SearchInput struct {
    Query string `json:"query" desc:"搜索关键词" required:"true"`
    Limit int    `json:"limit,omitempty" desc:"结果数量限制"`
}

app.Tool("search", goagent.ToolDef{
    Description: "搜索互联网获取最新信息",
    Input:       SearchInput{},
    Permission:  goagent.ReadOnly,
    Concurrent:  true,
    Execute: func(ctx goagent.Context, in SearchInput) (string, error) {
        results, err := searchAPI.Search(in.Query, in.Limit)
        if err != nil {
            return "", err
        }
        return formatResults(results), nil
    },
})
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

### 扩展字段

```go
app.Tool("bash", goagent.ToolDef{
    Description: "执行 shell 命令",
    Input:       BashInput{},
    Permission:  goagent.Normal,

    // 动态并发判定：git 命令可并发，其他串行
    IsConcurrencySafe: func(ctx context.Context, input json.RawMessage) bool {
        var in BashInput
        json.Unmarshal(input, &in)
        return strings.HasPrefix(in.Command, "git ")
    },

    // 中断时等待完成（不取消正在执行的命令）
    InterruptBehavior: func() string { return "block" },

    // 每个结果最多 50000 字符
    MaxResultSizeChars: 50000,

    // 预执行验证
    ValidateInput: func(ctx context.Context, input json.RawMessage) error {
        var in BashInput
        json.Unmarshal(input, &in)
        if strings.Contains(in.Command, "rm -rf /") {
            return fmt.Errorf("禁止执行危险命令")
        }
        return nil
    },

    // 自定义权限检查
    CheckPermissions: func(ctx context.Context, input json.RawMessage) (*goagent.PermissionCheck, error) {
        var in BashInput
        json.Unmarshal(input, &in)
        if isReadOnlyCommand(in.Command) {
            return &goagent.PermissionCheck{Behavior: "allow", Reason: "只读命令"}, nil
        }
        return nil, nil // 返回 nil 使用默认行为
    },

    Execute: func(ctx goagent.Context, in BashInput) (string, error) {
        return execCommand(in.Command)
    },
})
```

### 批量注册工具

```go
app.UseTools(
    goagent.QuickTool("deploy", "部署服务", goagent.Dangerous, deployFn, DeployInput{}),
    goagent.QuickReadOnlyTool("status", "查看状态", statusFn, StatusInput{}),
    goagent.QuickDangerousTool("rollback", "回滚", rollbackFn, RollbackInput{}),
)
```

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

### 自定义审批者

```go
type MyApprover struct{}

func (a *MyApprover) Approve(toolName, input string, perm goagent.Permission) (bool, bool) {
    if toolName == "delete_database" {
        fmt.Printf("危险操作！工具: %s，输入: %s\n", toolName, input)
        return false, false
    }
    return true, false
}

app := goagent.New(goagent.WithApprover(&MyApprover{}))
```

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
    goagent.WithProvider(provider),
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
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
)
```

| 工具 | 说明 | 权限 |
|------|------|------|
| `Read` | 读取文件，支持 offset/limit 和行号 | ReadOnly |
| `Write` | 创建或覆盖文件 | Normal |
| `Edit` | 精确字符串替换 | Normal |
| `Glob` | glob 模式文件搜索 | ReadOnly |
| `Grep` | 正则搜索，支持文件类型过滤 | ReadOnly |
| `Bash` | 执行 shell 命令，支持 timeout | Normal |
| `AskUser` | 向用户提问，获取输入 | ReadOnly |

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

### AskUser

```go
type AskUserInput struct {
    Question string `json:"question" desc:"要问用户的问题" required:"true"`
}
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
```

可用 ToolKit：

| ToolKit | 包含工具 |
|---------|---------|
| `FileKit()` | Read, Write, Edit |
| `SearchKit()` | Glob, Grep |
| `ShellKit()` | Bash |
| `InteractKit()` | AskUser |
| `CodeKit()` | FileKit + SearchKit + ShellKit |
| `AllKit()` | 全部内置工具 |

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
    goagent.WithProvider(provider),
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
    goagent.WithProvider(provider),
    goagent.WithCostTracking(),
)

// 启用使用分析
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithAnalytics(),
)

// 注册自定义 Observer
app := goagent.New(
    goagent.WithProvider(provider),
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
    goagent.WithProvider(provider),
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
    goagent.WithProvider(provider),
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
    goagent.WithProvider(provider),
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

### OpenAI 兼容 API

```go
// 连接本地 Ollama
goagent.WithProvider(goagent.OpenAI(openai.Config{
    BaseURL: "http://localhost:11434/v1",
    Model:   "qwen2.5:7b",
}))

// 连接 OpenRouter
goagent.WithProvider(goagent.OpenAI(openai.Config{
    APIKey:  "sk-or-...",
    BaseURL: "https://openrouter.ai/api/v1",
    Model:   "anthropic/claude-3.5-sonnet",
}))

// 连接 vLLM
goagent.WithProvider(goagent.OpenAI(openai.Config{
    BaseURL: "http://localhost:8000/v1",
    Model:   "meta-llama/Llama-3-8b",
}))
```

### Anthropic API

```go
goagent.WithProvider(goagent.Anthropic(
    os.Getenv("ANTHROPIC_API_KEY"),
    goagent.Model("claude-opus-4-6-20251114"),
))
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

### 方式一：使用内置提示词（推荐）

```go
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithClaudeCodePrompts(),  // 启用内置 Claude Code 提示词体系
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
| `system-workflow.prompt.md` | 兼容旧版 | ✅ 已集成 |
| `compact.prompt.md` | 上下文压缩提示词 | ✅ 已集成 |

### 方式二：完全自定义

```go
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithSystemPrompt("你是一个专注于代码审查的 AI 助手..."),
)
```

### 方式三：混合模式

内置提示词 + 自定义追加：

```go
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithClaudeCodePrompts(),
    goagent.WithSystemPrompt("\n\n额外指令：你只使用中文回答。"),
)
```

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
workflow := prompts.MustLoad(prompts.Workflow)

// 带变量替换
content := prompts.LoadWithVars(prompts.Compact, map[string]string{
    "cwd":     "/path/to/project",
    "date":    "2024-01-15",
})
```

### PromptConfig 自定义配置

`PromptConfig` 提供细粒度的提示词自定义，支持三种配置方式（优先级从高到低）：

```go
app := goagent.New(
    goagent.WithPromptConfig(goagent.PromptConfig{
        // 方式1：从文件加载（优先级高于内置提示词）
        Identity:     "my-identity.prompt.md",
        DoingTasks:   "my-tasks.prompt.md",

        // 方式2：直接使用字符串（优先级最高）
        IdentityText: "你是一个专属客服助手...",
        AppendToneStyle: "\n\n必须使用中文回答。",

        // 方式3：追加到内置提示词之后
        AppendReminder: "\n\n额外提醒：xxx",
    }),
)
```

**查看当前提示词**：

```go
// 获取当前使用的完整 system prompt
fmt.Println(app.GetSystemPrompt())
```

**PromptConfig 字段说明**：

| 字段（文件） | 字段（文本） | 追加字段 | 说明 |
|------------|------------|---------|------|
| `Identity` | `IdentityText` | `AppendIdentity` | Agent 身份定义 |
| `DoingTasks` | `DoingTasksText` | `AppendDoingTasks` | 执行任务指令 |
| `Actions` | `ActionsText` | `AppendActions` | 谨慎执行操作 |
| `UsingTools` | `UsingToolsText` | `AppendUsingTools` | 工具使用策略 |
| `ToneStyle` | `ToneStyleText` | `AppendToneStyle` | 语气和风格 |
| `OutputEff` | `OutputEffText` | `AppendOutputEff` | 输出效率 |
| `Reminder` | `ReminderText` | `AppendReminder` | 系统提醒 |
| `Workflow` | `WorkflowText` | `AppendWorkflow` | 兼容旧版 |
| `Compact` | `CompactText` | `AppendCompact` | 上下文压缩 |

**示例场景**：

```go
// 场景1：完全自定义身份
goagent.PromptConfig{
    IdentityText: "你是一个专注于代码审查的 AI 助手...",
}

// 场景2：追加额外要求
goagent.PromptConfig{
    AppendToneStyle: "\n\n所有回答必须使用中文。",
    AppendReminder: "\n\n重要：不要修改生产环境代码。",
}

// 场景3：使用自定义文件
goagent.PromptConfig{
    Identity:   "./prompts/my-identity.prompt.md",
    DoingTasks: "./prompts/my-tasks.prompt.md",
}
```

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
    WithProvider(provider),              // LLM Provider（必填）
    WithFallback(fallbackProvider),      // 备用 Provider

    // === 运行模式 ===
    WithBuiltinTools(),                  // 开启所有内置工具
    WithToolKits(FileKit(), ShellKit()), // 按包注册工具
    WithSystemPrompt("你是一个..."),     // 系统提示词
    WithClaudeCodePrompts(),             // 使用 Claude Code 原版提示词体系

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
