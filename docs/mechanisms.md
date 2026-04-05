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
