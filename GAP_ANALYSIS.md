# GoAgent vs Claude Code: 第三轮差距分析

> 基于实际代码核查（非文档），标注每个模块的真实状态。
> P0 = 核心循环必须有 | P1 = 生产可用必须有 | P2 = 完整复刻需要
>
> **当前状态**: 60+ Go 文件, 10,500+ 行, Anthropic/OpenAI Provider 已实现（CLI 可运行）
>
> **更新**: MCP 工具注入 ✅ | WebSearch/WebFetch ✅ | CLI 命令完整 ✅

---

## 已确认实现（骨架或完整）

| # | 模块 | 文件 | 实际状态 |
|---|------|------|---------|
| 1 | Token Estimation | `message/token.go` | ✅ 完整 |
| 2 | 四层压缩 + Circuit Breaker | `compaction/` | ✅ 完整 |
| 3 | 预处理流水线 | `internal/loop/preprocess.go` | ✅ 完整 |
| 4 | Withholding | `internal/loop/withholding.go` | ✅ 完整 |
| 5 | Stop Hooks | `internal/loop/stophook.go` | ✅ 完整 |
| 6 | ToolDef 扩展字段 | `tool.go` | ✅ 完整 |
| 7 | 三态权限 + 规则引擎 | `permission/` | ✅ 完整 |
| 8 | TrackedExecutor | `executor/tracked.go` | ✅ 完整 |
| 9 | Session JSONL 持久化 | `session/` | ✅ 完整 |
| 10 | CLAUDE.md 三层加载 | `memory/claudemd.go` | ✅ 完整 |
| 11 | Hooks 框架 | `hooks/` | ✅ 完整 |
| 12 | Sub-Agent + ForkedAgent | `agent/` | ✅ 完整 |
| 13 | MCP 客户端 + 工具注入 | `mcp/` | ✅ 完整（刚完成注入） |
| 14 | Cost Tracking | `cost/cost.go` | ✅ 完整 |
| 15 | CLI REPL + 完整命令 | `cli.go`, `tui/` | ✅ 完整（/model, /sessions 等） |
| 16 | HTTP/SSE | `http.go` | ✅ 完整 |
| 17 | Anthropic Provider | `provider/anthropic/` | ✅ 完整 |
| 18 | OpenAI Provider | `provider/openai/` | ✅ 完整 |
| 19 | Analytics | `analytics/analytics.go` | ✅ 完整 |
| 20 | Retry/Backoff | `retry/retry.go` | ✅ 完整 |
| 21 | Budget Tracker | `budget/budget.go` | ✅ 完整 |
| 22 | Task/Todo V2 + ToolDef | `task/task.go` | ✅ 完整 |
| 23 | Plan Mode + ToolDef | `plan/plan.go` | ✅ 完整 |
| 24 | Skill 系统 + ToolDef | `skill/skill.go` | ✅ 完整 |
| 25 | Cron 调度 + ToolDef | `cron/cron.go` | ✅ 完整 |
| 26 | Worktree + ToolDef | `worktree/worktree.go` | ✅ 完整 |
| 27 | Extended Thinking | `thinking/thinking.go` | ✅ 完整 |
| 28 | 动态 System Prompt | `sysprompt/sysprompt.go` | ✅ 完整 |
| 29 | Auto Memory | `memory/automemory.go` | ✅ 完整 |
| 30 | Post-Compact 文件恢复 | `compaction/restore.go` | ✅ 完整 |
| 31 | Synthetic Results on Abort | `loop.go` | ✅ 已实现 |
| 32 | 动态并发判定 | `loop.go` | ✅ 已实现 |
| 33 | ValidateInput/CheckPermissions | `loop.go` | ✅ 已实现 |
| 34 | Prompt Cache 数据结构 | `provider.go`, `sysprompt.go` | ✅ 结构就绪 |
| 35 | Built-in Tools (Read/Write/Edit/Glob/Grep/Bash/AskUser) | `builtin/` | ✅ 完整 |
| 36 | WebSearch/WebFetch | `builtin/web.go` | ✅ 刚完成 |
| 37 | MCP 工具注入 | `goagent.go` | ✅ 刚完成 |

---

## 第三轮差距：已大幅缩减

> **核心发现**: 大多数模块逻辑已实现，问题是"写了没串起来"。
> 优先排序调整：P1-1~P1-2（内置工具）提升为最高优先级。

---

### P0: 框架无法干活（已全部完成 ✅）

#### P0-1: 内置文件工具 (Read/Write/Edit/Glob/Grep)
- **现状**: ✅ 已完整实现
- **文件**: `builtin/tools.go`
- **内容**: Read, Write, Edit, Glob, Grep 五个工具

#### P0-2: 内置 Bash 工具
- **现状**: ✅ 已完整实现
- **文件**: `builtin/bash.go`
- **内容**: Shell 执行、动态并发、timeout

#### P0-3: 已有包接入主循环（集成层）
- **现状**: ✅ 已完整接入
- **文件**: `goagent.go run()`
- **内容**: sysprompt.Builder、retry、budget、session 均已接入

#### P0-4: 12 个 ToolDef 注册
- **现状**: ✅ 已完整注册
- **文件**: `builtin/management.go`
- **内容**: Task(4) + Plan(2) + Skill(1) + Cron(3) + Worktree(2) + BgTask(2) = 14 个工具
  - EnterPlanMode/ExitPlanMode (2)
  - Skill (1)
  - CronCreate/CronDelete/CronList (3)
  - EnterWorktree/ExitWorktree (2)
- **估算**: ~400 行

---

### P1: 生产可用（12 个）

#### P1-1: Anthropic API Provider
### P1: 生产可用（已完成大部分 ✅）

#### P1-1: Anthropic API Provider
- **现状**: ✅ 已完整实现
- **文件**: `provider/anthropic/anthropic.go`
- **内容**: Stream + Complete + Extended Thinking + Prompt Cache

#### P1-2: MCP 工具注入到主循环
- **现状**: ✅ 已完成
- **文件**: `goagent.go` - `initMCP()`
- **内容**: stdio/http 传输、工具发现与注册

#### P1-3: AskUser 工具
- **现状**: ✅ 已完整实现
- **文件**: `builtin/askuser.go`
- **内容**: stdin 读取 + TUI 回调

#### P1-4: Sibling Abort 集成
- **现状**: ⚠️ 部分实现
- **文件**: `executor/tracked.go`
- **内容**: AbortSiblings 存在但 loop 集成不完整

#### P1-5: InterruptBehavior 区分处理
- **现状**: ⚠️ 部分实现
- **文件**: `tool.go`
- **内容**: 有字段定义但执行层未区分

#### P1-6: Remember 机制
- **现状**: ⚠️ 部分实现
- **文件**: `memory/automemory.go`
- **内容**: Save() 存在，触发入口需要 hook

#### P1-7: 多模型切换 (/model)
- **现状**: ✅ 已完整实现
- **文件**: `tui/commands.go`
- **内容**: `/model` 命令 + `SetModel()` 接口

#### P1-8: 完整 CLI 命令集
- **现状**: ✅ 已完整实现
- **文件**: `tui/commands.go`
- **内容**: /help, /clear, /compact, /status, /model, /tools, /cost, /tasks, /bg, /sessions, /resume, /config, /exit

#### P1-9: WebSearch / WebFetch 工具
- **现状**: ✅ 已完成
- **文件**: `builtin/web.go`
- **内容**: DuckDuckGo HTML 搜索、HTML 解析

#### P1-10: Git 工具包
- **现状**: ⚠️ 仅有 bash 实现
- **内容**: `builtin/bash.go` 通过 shell 执行 git，无独立包

#### P1-11: Background Agents
- **现状**: ⚠️ 部分实现
- **文件**: `agent/subagent.go`
- **内容**: Background 字段存在，接入 loop 需要完善

#### P1-12: Prompt Cache 完整传递
- **现状**: ⚠️ 结构就绪
- **文件**: `sysprompt/sysprompt.go`
- **内容**: SystemBlocks 有但 run() 使用有限

---

### P2: 完整对齐（待完成）

| # | 缺失 | 估算 |
|---|------|------|
| 1 | Image/PDF/Notebook 阅读 | ~200 行 |
| 2 | NotebookEdit 工具 | ~150 行 |
| 3 | Tool Use Summary | ~80 行 |
| 4 | Dynamic Tool Refresh | ~60 行 |
| 5 | Permission AI Classifier | ~120 行 |
| 6 | 完整 Session Restore | ~150 行 |
| 7 | Attachment System | ~200 行 |
| 8 | Streaming Fallback (Mid-Stream) | ~80 行 |
| 9 | Fast Mode | ~40 行 |
| 10 | Query Tracking / Chain ID | ~40 行 |
| 11 | Swarm / Multi-Agent | ~300 行 |
| 12 | DI 模式 (可测试主循环) | ~100 行 |
| 13 | 自动工具降级 (不支持 tools 时) | ~60 行 |

---

## 代码量总估算

| 优先级 | 数量 | 已完成 | 估算行数 |
|--------|------|--------|---------|
| P0 | 4 | ✅ 4 | ~1,220 行 |
| P1 | 12 | ✅ 8, ⚠️ 4 | ~1,525 行 |
| P2 | 13 | ❌ 0 | ~1,580 行 |
| **合计** | **29** | **12✅, 4⚠️, 13❌** | **~4,325 行** |

### 完整对比

| 维度 | GoAgent 当前 | 完整实现后 | Claude Code |
|------|-------------|-----------|-------------|
| 文件数 | 57 | ~75 | ~120+ |
| 代码行数 | 9,902 | ~14,200 | ~25,000+ |
| 包数 | 23 | ~26 | N/A |

---

## 本轮实施顺序

### Step 1: 内置工具 (P0-1 + P0-2)
新建 `builtin/` 包，实现 Read/Write/Edit/Glob/Grep/Bash 六个工具。

### Step 2: 集成层 (P0-3)
把 retry/budget/sysprompt/session/thinking 接入 goagent.go 和 loop.go。

### Step 3: ToolDef 注册 (P0-4)
为 task/plan/skill/cron/worktree 创建 ToolDef 并注册。

### Step 4: P1 功能
AskUser, Sibling Abort, InterruptBehavior, Remember, CLI 增强, MCP 注入。

### Step 5: P1 工具
WebSearch/WebFetch, Git 包, Background Agents。
