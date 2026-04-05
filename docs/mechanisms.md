# GoAgent 内部机制详解

> 本文档详细描述 GoAgent v0.4 新增的各项机制。

---

## 1. Task/Todo V2 系统

位置：`task/task.go`

### 功能
- 任务 CRUD: Create / Update / Get / List / Delete
- 状态管理: pending → in_progress → completed (或 deleted)
- 依赖管理: BlockedBy / Blocks 双向链接
- 自增 ID (字符串)
- 内存存储（session 级别）

### 数据结构
```go
type Task struct {
    ID          string
    Subject     string         // 简短标题（祈使句）
    Description string         // 详细描述
    Status      Status         // pending/in_progress/completed/deleted
    Owner       string         // 负责 agent
    ActiveForm  string         // 进行中显示文本
    Metadata    map[string]any // 自定义元数据
    BlockedBy   []string       // 阻塞依赖
    Blocks      []string       // 被阻塞者
}
```

### 对应工具
- `TaskCreate` — 创建任务
- `TaskUpdate` — 更新状态/字段/依赖
- `TaskGet` — 获取任务详情
- `TaskList` — 列出所有任务摘要

---

## 2. Plan Mode 系统

位置：`plan/plan.go`

### 生命周期

```
Inactive → [EnterPlanMode] → Active → [编写计划] → [ExitPlanMode] → Approved → [执行] → Inactive
                                      [Cancel] → Inactive
```

### 核心行为
- Plan mode 下只允许 ReadOnly 权限工具（Read, Glob, Grep 等）
- Plan 文件存储在 `.claude/plans/<random-name>.md`
- 随机名称格式: `smooth-tumbling-unicorn`
- System prompt 注入 plan mode 提示
- 支持从文件加载已有 plan

---

## 3. Skill 系统

位置：`skill/skill.go`

### Skill 来源优先级

```
Builtin (内置) > Project (.claude/commands/) > User (~/.claude/commands/)
```

### Skill 发现
- 扫描 `.claude/commands/*.md` 文件
- 文件名 = skill 名称
- 首行非空非标题行 = 描述

### Skill 执行
- **Inline**: 将 skill 内容作为 prompt 注入当前对话
- **Fork**: 在独立 agent 中执行（待实现）
- `$ARGUMENTS` 占位符自动替换为用户参数

---

## 4. Cron 调度系统

位置：`cron/cron.go`

### 特点
- 标准 5-field cron: `minute hour dom month dow`
- 支持: `*`, `*/N`, `N-M`, `N,M,L`
- Session-only 生命周期（不持久化到磁盘）
- 循环任务 3 天自动过期
- 一次性任务 24 小时过期
- Jitter: 循环 ±15min, 一次性 ±90s

### API
```go
scheduler := cron.NewScheduler(onFire)
scheduler.Start(ctx)

job, _ := scheduler.Create("0 9 * * 1-5", "检查部署状态", true)
scheduler.Delete(job.ID)
scheduler.List()

scheduler.Stop()
```

---

## 5. Worktree 隔离

位置：`worktree/worktree.go`

### 工作流

```
主仓库 → [Enter] → .claude/worktrees/<name>/ (新分支 claude-worktree-<name>)
                           ↓
                    [工作...]
                           ↓
         [Exit keep] → 保留 worktree 和分支
         [Exit remove] → 删除 worktree 和分支
```

### 安全检查
- Exit remove 时检查未提交更改
- 需要 `discardChanges=true` 才能强制删除有更改的 worktree

---

## 6. Extended Thinking

位置：`thinking/thinking.go`

### 三种模式

| 模式 | 行为 |
|------|------|
| Adaptive | 根据工具数/上下文大小自动决定 |
| Enabled | 始终启用 |
| Disabled | 禁用 |

### Ultrathink
Budget > 10,000 tokens 时启用 ultrathink 模式。

### 自适应规则
- 工具数 > 5 → 启用
- 上下文 > 50k tokens → 启用
- 上下文使用 > 80% → budget 减半
- 上下文使用 > 90% → budget 降到 1000

---

## 7. 动态 System Prompt

位置：`sysprompt/sysprompt.go`

### 多段组装

```go
builder := sysprompt.NewBuilder()
builder.AddBasePrompt("You are a helpful assistant")
builder.AddEnvironmentInfo()   // platform, arch, shell
builder.AddCurrentDate()        // 2026-04-03
builder.AddGitStatus()          // branch, status
builder.AddMemory(claudemd)     // CLAUDE.md 内容
builder.AddMCPInstructions(mcp) // MCP server 说明

text, sections := builder.Build()
```

### Prompt Caching
每个 Section 可独立设置 `CacheControl{Type: "ephemeral"}`。
最后一个段自动添加 cache_control（如果没有的话）。

---

## 8. Rate Limit / Retry

位置：`retry/retry.go`

### 重试策略
- 指数退避: 1s → 2s → 4s → ... → 30s (上限)
- Jitter: ±10%
- 最大重试: 3 次
- 仅重试: 429 (RateLimitError), 529 (OverloadError), 5xx (ServerError)
- 不重试: 4xx (非 429)
- 支持 `retry-after` header

### 泛型 API

```go
result, err := retry.Retry(ctx, retry.DefaultConfig(), func(ctx context.Context) (T, error) {
    return apiCall(ctx)
})
```

---

## 9. Token Budget

位置：`budget/budget.go`

### 功能
- 总预算限制: `TotalBudget` (0 = 无限)
- 每轮记录: `RecordUsage(turn, input, output)`
- 状态检查: OK / Warning (<20% 剩余) / Exhausted / Diminishing

### Diminishing Returns 检测
连续 N 轮（默认 3）输出低于 MinOutputPerTurn（默认 50 tokens）→ 自动终止。

---

## 10. Auto Memory

位置：`memory/automemory.go`

### 文件结构

```
~/.claude/projects/<project>/memory/
├── MEMORY.md        # 主文件（自动截断 200 行）
├── debugging.md     # 话题子文件
├── patterns.md      # 话题子文件
└── preferences.md   # 话题子文件
```

### 功能
- `LoadMain()` — 加载 MEMORY.md（自动截断）
- `Search(keywords)` — 按关键词搜索相关文件
- `FormatForInjection()` — 格式化为 system prompt 注入文本
- `FilterDuplicates()` — 基于内容指纹去重

---

## 11. Withholding 机制

位置：`internal/loop/withholding.go`

### 暂扣原因

| 原因 | 触发条件 | 恢复方式 |
|------|---------|---------|
| PromptTooLong | 413 错误 | reactive compact |
| MaxOutputTokens | max_tokens 截断 | escalate / recovery |

### 工作流
1. 流消费中检测可恢复错误
2. 暂扣该事件（不立即 yield）
3. 尝试恢复（压缩/升级）
4. 成功 → 释放并重试
5. 失败 → 释放并报错

---

## 12. Stop Hooks

位置：`internal/loop/stophook.go`

### 执行时机
每轮结束（LLM 返回 end_turn 且无工具调用）后，在最终退出前执行。

### 内置 Hooks
- `MaxOutputTokensStopHook` — 检测输出是否不完整
- `TokenBudgetStopHook` — 检查 token 预算

### 阻塞机制
Hook 返回 `Block=true` 时：
1. 注入修正消息到对话历史
2. 设置 `TransStopHookBlocking` transition
3. 继续循环（不退出）

---

## 13. Hooks 框架

位置：`hooks/`

### 5 种事件

| 事件 | 触发时机 |
|------|---------|
| PreToolUse | 工具执行前 |
| PostToolUse | 工具执行后 |
| Stop | 每轮结束时 |
| PermissionRequest | 权限请求时 |
| SessionStart | 会话开始时 |

### Hook 类型
- `CommandHook` — 执行 shell 命令，通过环境变量传递上下文
- `FuncHook` — Go 函数回调

---

## 14. Session 管理

位置：`session/`

### 状态流转

```
Idle → Running → Idle (正常完成)
              → RequiresAction (等待用户操作)
              → Suspended (暂停)
              → Completed (最终完成)
```

### JSONL 持久化
每条记录一行 JSON:
```jsonl
{"type":"message","timestamp":"...","data":{...}}
{"type":"metadata","timestamp":"...","data":{...}}
{"type":"boundary","timestamp":"...","data":{...}}
{"type":"state","timestamp":"...","data":{"state":"running"}}
```

### 恢复
- 从 JSONL 重建消息历史
- `EnsureToolResultPairing()` 修复孤立的 tool_use（没有对应 tool_result）

---

## 15. Cost Tracking

位置：`cost/cost.go`

### 内置定价 (2025)

| 模型 | Input/M | Output/M | Cache Read/M | Cache Write/M |
|------|---------|----------|-------------|--------------|
| claude-opus-4-6 | $15.00 | $75.00 | $1.50 | $18.75 |
| claude-sonnet-4-6 | $3.00 | $15.00 | $0.30 | $3.75 |
| claude-haiku-4-5 | $0.80 | $4.00 | $0.08 | $1.00 |

### API
```go
tracker := cost.NewTracker()
tracker.Record("claude-sonnet-4-6", 1000, 500, 200, 100)
fmt.Println(tracker.FormatSummary())
// 总成本: $0.0113 (66.67% 输入, 33.33% 输出)
// Token 使用: 1000 输入, 500 输出, 300 缓存
```

---

## 16. Prompt Caching

位置：`provider/provider.go`

### 请求结构

```go
type Request struct {
    SystemBlocks []SystemBlock  // 多段 system prompt
    Tools        []ToolDefinition
    // ...
}

type SystemBlock struct {
    Text         string
    CacheControl *CacheControl  // {"type": "ephemeral"}
}

type ToolDefinition struct {
    Name         string
    InputSchema  any
    CacheControl *CacheControl  // 工具级缓存
}
```

### 缓存策略
- 最后 1-2 个 system block 自动加 `cache_control: ephemeral`
- 工具列表末尾的工具加 `cache_control`
- Usage 返回 `CacheReadTokens` / `CacheCreateTokens`

---

## 17. Pipeline DAG 编排引擎

位置：`pipeline.go`

### 概述

Pipeline 提供 DAG 流水线编排，支持多节点依赖调度、并行 worker、supervisor 审核。适用于多 agent 协作场景（如分镜生成 → 画面制作 → 质量审核）。

### 17.1 核心配置

#### PipelineConfig

```go
type PipelineConfig struct {
    Nodes      []PipelineNode     // DAG 节点列表
    Supervisor *PipelineAgentDef  // 上帝节点（可选）
}
```

#### PipelineNode — 用户需配置的字段

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `Name` | string | 必填 | 节点唯一标识 |
| `Agent` | *PipelineAgentDef | 必填 | 节点的 agent 定义 |
| `Concurrency` | int | 1 | worker 并发数。>1 时自动创建 message 队列 |
| `DependsOn` | []string | nil | 依赖的节点名列表，全部完成后才启动 |
| `Message` | string | "" | 初始消息（Concurrency=1 时使用） |
| `Injects` | []string | nil | 可推送 message 的下游节点名列表 |
| `MessageType` | reflect.Type | string | message 队列元素类型 |
| `ResultType` | reflect.Type | string | result 队列元素类型 |
| `Review` | bool | false | 是否需要 supervisor 审核 |
| `ReviewBatch` | int | 1 | 攒多少个 result 触发一次审核 |
| `MaxRetries` | int | 3 | 审核拒绝后最大重试次数 |
| `OnResult` | func(any) | nil | result 通过后的回调 |
| `QueueSize` | int | 64 | 队列缓冲大小 |

#### PipelineAgentDef — 子 agent 定义

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `Name` | string | 必填 | agent 名称 |
| `Instruction` | string | 必填 | system prompt |
| `Tools` | []NamedTool | nil | 可用工具列表 |
| `Provider` | provider.Provider | nil（继承父 App） | LLM 提供者 |
| `MaxTurns` | int | 100 | 最大轮次 |

### 17.2 调度机制

```
拓扑排序 → 启动无依赖节点 → 节点完成 → 检查下游依赖 → 启动就绪节点 → ... → 全部完成
```

1. **拓扑排序**: 基于 `DependsOn` 计算执行顺序，检测循环依赖
2. **并行启动**: 所有入度为 0 的节点同时启动
3. **依赖传播**: 节点完成后检查下游节点的所有依赖是否满足
4. **队列注入**: 通过 `Injects` 声明跨节点队列通信

### 17.3 Worker 模式

| 模式 | 条件 | 行为 |
|------|------|------|
| 单 worker | Concurrency=1 | 直接执行 `Message`，返回结果 |
| 多 worker | Concurrency>1 | N 个 goroutine 并行消费 message 队列 |

#### Lightweight Loop

Pipeline worker 使用精简的 `loop.Loop`，包含：
- **Provider** + **Tools** + **SystemPrompt** + **MaxTurns** + **Executor**
- **Compaction**: 可组合压缩，只启用 L0(Budget) + L2(Micro) + L4(Auto)
- **Hooks**: 继承父 App 的 `hooksMgr`（如果有）
- **Observer**: 继承父 App 的 `obsRegistry`（如果有）

跳过的重型子系统：Permission、Memory、Budget、SessionMemory、PlanChecker。

### 17.4 Supervisor 审核

当存在 `Review=true` 节点且配置了 `Supervisor` 时，框架自动注入三个审核工具：

| 工具 | 说明 |
|------|------|
| `wait_for_review` | 阻塞等待审核事件，返回待审 result 批次 |
| `approve_result` | 批准 result，触发 OnResult 回调 |
| `reject_result` | 拒绝 result + 修改指导，重新入队重试 |

#### 审核流程

```
Worker 完成 → handleResult → 进入待审队列
     → 攒够 ReviewBatch 或 message 队列为空 → 触发 reviewSignal
     → Supervisor wait_for_review 收到事件
     → approve/reject 每个 result
     → reject: 原始消息 + guidance 重新入队（不超过 MaxRetries）
     → 全部 Review 节点审核完成 → Done 信号 → Pipeline 退出
```

### 17.5 队列通信（Injects）

```go
// 节点 A 声明可向 B 推送
PipelineNode{Name: "planner", Injects: []string{"drawer"}}

// 节点 A 的 tool 中获取队列并推送
func(ctx goagent.Context, in MyInput) (string, error) {
    q := goagent.GetMessageQueue(ctx, "drawer")
    q.Push(DrawTask{...})
    return "已派发", nil
}
```

框架保证：
- 只有 `Injects` 中声明的下游队列才能获取（权限控制）
- 上游节点完成后自动关闭 `Injects` 声明的下游队列（通知 worker 退出）

### 17.6 使用示例

```go
app := goagent.New(goagent.WithProvider(prov))

app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:    "planner",
            Agent:   &goagent.PipelineAgentDef{Name: "planner", Instruction: "..."},
            Message: "请为这个故事生成 5 个分镜",
            Injects: []string{"drawer"},
        },
        {
            Name:        "drawer",
            Agent:       &goagent.PipelineAgentDef{Name: "drawer", Instruction: "..."},
            Concurrency: 3,
            DependsOn:   []string{"planner"},
            Review:      true,
            ReviewBatch: 2,
            OnResult:    func(r any) { fmt.Println("画面完成:", r) },
        },
    },
    Supervisor: &goagent.PipelineAgentDef{
        Name:        "supervisor",
        Instruction: "你是质量审核员。审核画面质量...",
    },
})

err := app.RunPipeline(ctx)
```
