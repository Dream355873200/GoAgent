# GoAgent — 设计文档

> 基于 Claude Code 架构的 Go Agent 应用框架
> 57 Go 文件 | 9,902 行 | 23 个包 | `go vet ./...` 通过

---

## 1. 定位

GoAgent 是一个 **1:1 复刻 Claude Code 架构** 的 Go AI Agent 框架。将 Claude Code 25,000+ 行 TypeScript 的核心逻辑浓缩为 ~10,000 行 Go 代码，覆盖：

- Agent Loop 核心状态机（8 阶段 + 7 种 continue 分支）
- 四层上下文压缩 + Circuit Breaker
- 流式工具执行（StreamingExecutor + TrackedExecutor）
- 三态权限系统（allow/deny/ask + 5 种模式 + 规则引擎）
- Task/Todo V2 系统（依赖管理）
- Plan Mode（只读浏览 + 计划文件）
- Skill 系统（命令发现 + prompt 注入）
- Cron 调度（5-field cron + jitter + 3 天过期）
- Worktree 隔离（git worktree 创建/退出）
- Extended Thinking（adaptive/enabled/disabled）
- 动态 System Prompt 组装（多段 + cache_control）
- Token Budget 追踪 + Diminishing Returns 检测
- Rate Limit / Retry with Backoff
- Session JSONL 持久化 + 恢复
- Hooks 框架（5 事件类型）
- Sub-Agent 系统
- MCP 客户端（stdio + http）
- Cost Tracking（内置 Anthropic 定价）
- Auto Memory（MEMORY.md + 话题子文件）

**用户使用方式不变**：定义 Tool → 选权限 → 选运行模式。

## 2. 核心设计原则

- **用户只写业务逻辑**：框架内部的复杂性不暴露
- **约定优于配置**：零配置能跑，配了更好
- **Go 惯用法**：channel 做流式、interface 做抽象、goroutine 做并发
- **可插拔不可渗透**：Provider / Approver / Store 可替换，内部机制用户不需要碰
- **中文注释**：所有代码注释使用中文

## 3. 包结构总览

```
goagent/                        # 主包：App, ToolDef, Permission, Event, Options
├── internal/loop/              # 核心 agent 循环状态机
│   ├── loop.go                 # 8 阶段主循环
│   ├── preprocess.go           # 6 层消息预处理
│   ├── withholding.go          # 可恢复错误暂扣
│   └── stophook.go             # Stop Hook 系统
│
├── provider/                   # LLM Provider 接口
│   ├── provider.go             # Provider interface + Request/Response + CacheControl
│   ├── anthropic/              # Anthropic 实现（骨架）
│   └── openai/                 # OpenAI 兼容实现（骨架）
│
├── message/                    # 消息类型 + Token 估算
│   ├── message.go              # Message, ContentBlock, ToolCall
│   └── token.go                # 类型感知估算, 4/3 padding, warning state
│
├── compaction/                 # 四层上下文压缩
│   ├── compaction.go           # Manager + Apply() + Circuit Breaker
│   ├── boundary.go             # Compact Boundary 管理
│   ├── whitelist.go            # 可压缩工具白名单
│   └── restore.go              # 压缩后文件恢复
│
├── executor/                   # 工具执行引擎
│   ├── executor.go             # Executor + StreamingExecutor
│   └── tracked.go              # TrackedExecutor + AbortSiblings + SyntheticResults
│
├── permission/                 # 权限系统
│   ├── permission.go           # Gate + 5 模式 + 3 态
│   ├── rules.go                # RuleSet + 优先级评估
│   └── matcher.go              # ToolPattern 匹配
│
├── session/                    # 会话管理
│   ├── session.go              # Session + State + Metadata
│   ├── storage.go              # JSONL 持久化
│   └── restore.go              # 会话恢复 + 孤立修复
│
├── memory/                     # 记忆系统
│   ├── memory.go               # Manager + BuildSystemPromptSuffix
│   ├── claudemd.go             # CLAUDE.md 三层加载
│   └── automemory.go           # Auto Memory + 去重 + 搜索
│
├── hooks/                      # Hooks 框架
│   ├── hooks.go                # HookEvent + Manager + Hook interface
│   └── command.go              # CommandHook + FuncHook
│
├── agent/                      # Sub-Agent 系统
│   ├── agent.go                # Definition + Runner
│   ├── context.go              # AgentContext
│   └── tool.go                 # AgentToolDef
│
├── mcp/                        # MCP 客户端
│   ├── client.go               # Client + Connect/ListTools/CallTool
│   ├── transport.go            # StdioTransport + HTTPTransport
│   ├── types.go                # JSON-RPC 协议类型
│   └── tools.go                # MCP→框架工具转换
│
├── task/                       # Task/Todo V2
│   └── task.go                 # Store + Task + 依赖管理
│
├── plan/                       # Plan Mode
│   └── plan.go                 # Manager + Enter/Exit/IsToolAllowed
│
├── skill/                      # Skill 系统
│   └── skill.go                # Registry + Discover + Execute
│
├── cron/                       # Cron 调度
│   └── cron.go                 # Scheduler + 5-field parser + jitter
│
├── worktree/                   # Worktree 隔离
│   └── worktree.go             # Manager + Enter/Exit + git worktree
│
├── thinking/                   # Extended Thinking
│   └── thinking.go             # Config + adaptive/enabled/disabled
│
├── sysprompt/                  # 动态 System Prompt
│   └── sysprompt.go            # Builder + Section + CacheControl
│
├── retry/                      # Rate Limit / Retry
│   └── retry.go                # Retry[T] + Backoff + RateLimitError
│
├── budget/                     # Token Budget
│   └── budget.go               # Tracker + Diminishing Returns
│
├── cost/                       # Cost Tracking
│   └── cost.go                 # Tracker + 内置定价 + Summary
│
├── analytics/                  # 使用分析
│   └── analytics.go            # Tracker + per-tool 统计
│
├── schema/                     # struct → JSON Schema
│   └── reflect.go
│
├── cli.go                      # CLI REPL
├── http.go                     # HTTP/SSE 服务器
├── context.go                  # goagent.Context
├── middleware.go               # Middleware 接口
├── providers.go                # 便捷 provider 创建
│
├── docs/                       # 文档
│   ├── architecture.md         # 架构详解
│   └── mechanisms.md           # 内部机制详解
│
└── examples/
    ├── devops-bot/             # DevOps 助手示例
    ├── compaction-only/        # 压缩系统示例
    └── executor-only/          # 执行器示例
```

## 4. 用户 API

### 4.1 最小示例

```go
app := goagent.New(
    goagent.WithProvider(goagent.Anthropic(os.Getenv("API_KEY"))),
    goagent.WithSystemPrompt("你是一个 DevOps 助手"),
)

app.Tool("deploy", goagent.ToolDef{
    Description: "部署服务到指定环境",
    Input:       DeployInput{},
    Permission:  goagent.Dangerous,
    Execute: func(ctx goagent.Context, in DeployInput) (string, error) {
        return kubectl.Deploy(in.Service, in.Env)
    },
})

app.RunCLI()
```

### 4.2 扩展工具定义

```go
app.Tool("bash", goagent.ToolDef{
    Description: "执行 shell 命令",
    Input:       BashInput{},
    Permission:  goagent.Normal,

    // 动态并发判定：git 命令可并发，其他不行。
    IsConcurrencySafe: func(ctx context.Context, input json.RawMessage) bool {
        var in BashInput
        json.Unmarshal(input, &in)
        return strings.HasPrefix(in.Command, "git ")
    },

    // 中断时等待完成（不取消正在执行的命令）。
    InterruptBehavior: func() string { return "block" },

    // 每个结果最多 50000 字符。
    MaxResultSizeChars: 50000,

    // 预执行验证。
    ValidateInput: func(ctx context.Context, input json.RawMessage) error {
        var in BashInput
        json.Unmarshal(input, &in)
        if strings.Contains(in.Command, "rm -rf /") {
            return fmt.Errorf("禁止执行危险命令")
        }
        return nil
    },

    Execute: func(ctx goagent.Context, in BashInput) (string, error) {
        return execCommand(in.Command)
    },
})
```

### 4.3 运行模式

```go
app.RunCLI()                        // 终端 REPL
app.RunHTTP(":8080")                // HTTP SSE API
events := app.Run(ctx, "prompt")    // 嵌入式 SDK
result, _ := app.Execute(ctx, "p")  // 单次执行
```

## 5. 版本状态

| 版本 | 内容 | 状态 |
|------|------|------|
| v0.1 | 骨架：App + Tool + Loop + Compaction + Permission + Provider | ✅ 完成 |
| v0.2 | 增强：Token estimation + Preprocessing + Withholding + StopHooks + TrackedExecutor | ✅ 完成 |
| v0.3 | 生态：Session + Hooks + Memory/CLAUDE.md + Agent + MCP + Cost | ✅ 完成 |
| v0.4 | 新机制：Task + Plan + Skill + Cron + Worktree + Thinking + SysPrompt + Retry + Budget | ✅ 完成 |
| v0.5 | P2 完整对齐：Built-in Tools + Git + WebSearch + Background Agents + ... | 🔜 待定 |

详细架构和机制说明见 `docs/architecture.md` 和 `docs/mechanisms.md`。
完整差距分析见 `GAP_ANALYSIS.md`。
