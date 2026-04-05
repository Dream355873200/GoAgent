# GoAgent 架构详解

> 本文档详细描述 GoAgent 的内部架构，对齐 Claude Code 的核心设计。

---

## 1. Agent Loop 核心状态机

位置：`internal/loop/loop.go`

### 1.1 单次迭代 8 阶段

```
① 预处理（四层压缩） → ② Token 阻塞检查 → ③ 构建 API 工具定义
→ ④ 调用 API（流式） → ⑤ 消费流 + 流式工具启动
→ ⑥ 中止检查（合成 tool_result） → ⑦ 退出判断 / 恢复分支
→ ⑧ 工具执行 + 后处理
```

### 1.2 7 种 Continue 分支

| Transition | 触发条件 | 行为 |
|---|---|---|
| `NextTurn` | 正常工具调用完成 | 继续下一轮 |
| `CollapseDrainRetry` | 上下文折叠释放空间 | 重试 API 调用 |
| `ReactiveCompactRetry` | 413 后压缩成功 | 重试 API 调用 |
| `MaxOutputEscalate` | 首次 max_tokens 截断 | 提升 4k→64k 重试 |
| `MaxOutputRecovery` | 再次 max_tokens 截断 | 注入继续提示（最多 3 次） |
| `StopHookBlocking` | stop hook 阻止退出 | 注入修正消息 |
| `TokenBudgetContinuation` | token 预算未耗尽 | 继续执行 |

### 1.3 退出条件

- `completed` — LLM 返回 end_turn 且无工具调用
- `aborted` — context 被取消（用户中断）
- `context_exhausted` — token 超出硬限制且无压缩器
- `max_turns` — 达到最大轮次
- `model_error` — 不可恢复的 API 错误

### 1.4 合成 Tool Result

当 agent 被中断（ctx.Err()）时，loop 自动为所有未完成的 tool_use 生成合成 tool_result：

```
assistant: [tool_use id="abc" name="Bash"]
user:      [tool_result id="abc" content="工具执行被用户中断" is_error=true]
```

这维持了 API 的消息对齐要求（每个 tool_use 必须有对应 tool_result）。

---

## 2. 消息预处理流水线

位置：`internal/loop/preprocess.go`

6 层流水线，按顺序执行：

```
消息 → ① Compact Boundary 过滤 → ② Tool Result Budget 截断
→ ③ Snip Compact → ④ Micro Compact → ⑤ Context Collapse
→ ⑥ Proactive Auto Compact → 处理后的消息
```

每层独立判断是否需要执行，并返回释放的 token 数。

---

## 3. 压缩系统

位置：`compaction/`

### 3.1 层级设计

| 层 | 常量 | 名称 | 位置 | 机制 | 是否调模型 |
|---|------|------|------|------|----------|
| 0 | `LayerBudget` | Budget | `compaction.go` | 超大工具结果截断 | 否 |
| 1 | `LayerSnip` | Snip | `compaction.go` | 截断最旧消息 | 否 |
| 2 | `LayerMicro` | Micro | `compaction.go` | 删除已消费的工具结果 | 否 |
| 3 | `LayerCollapse` | Collapse | `compaction.go` | 折叠中间轮次 | 否 |
| 4 | `LayerAuto` | Auto | `compaction.go` | 调模型压缩为摘要 | 是 |

### 3.2 可组合层（Composable Layers）

`Config.Layers` 字段允许选择性启用压缩层：

```go
// 全部启用（向后兼容，Layers 为 nil 或空）
compaction.NewManager(compaction.Config{})

// 只启用 L0+L2+L4（Pipeline worker 场景）
compaction.NewManager(compaction.Config{
    Layers: []compaction.Layer{
        compaction.LayerBudget,
        compaction.LayerMicro,
        compaction.LayerAuto,
    },
})
```

`Apply()` 在每层执行前检查 `enabledLayers[LayerXxx]`，跳过未启用的层。

> **注意**: Layer 0 Budget 当前仅截断保留头尾，尚未实现真正的磁盘持久化 + 引用替换（标记为 TODO）。

### 3.3 Circuit Breaker

连续 autocompact 失败 3 次后停止尝试（`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`）。

### 3.4 Compact Boundary

位置：`compaction/boundary.go`

压缩后标记边界消息 (`IsCompactBoundary = true`)，后续只取边界之后的消息，
保持 prompt cache 有效性。

### 3.5 可压缩工具白名单

位置：`compaction/whitelist.go`

只有白名单工具（Read, Bash, Grep, Glob, WebSearch, WebFetch, Agent, TaskOutput）
的结果才会被 micro compact 清理。

### 3.6 压缩后文件恢复

位置：`compaction/restore.go`

压缩后自动恢复最近读取的 5 个文件（`POST_COMPACT_MAX_FILES_TO_RESTORE = 5`），
生成合成的 tool_use + tool_result 消息对。

---

## 4. 工具执行引擎

### 4.1 StreamingExecutor

位置：`executor/executor.go`

API 还在流式返回时，已解析出的 tool_use 就开始执行：

```
API 流式返回:  ──text──tool_use_A──text──tool_use_B──done──
工具执行:              ────A执行中────          ──B执行中──
                                                        ──等待剩余──
```

分区策略：连续并发安全工具 → 并行批次，非并发工具 → 串行。

### 4.2 TrackedExecutor

位置：`executor/tracked.go`

5 种工具状态：

```
Queued → Executing → Completed
                   → Aborted (中断)
                   → Yielded (已返回给调用者)
```

功能：
- **AbortSiblings**: Bash 错误时取消兄弟工具
- **SyntheticResults**: 中断时生成合成 tool_result

### 4.3 动态并发判定

`ToolEntry.IsConcurrencySafe(ctx, input)` 优先于静态 `Concurrent` 字段。
例如 Bash 工具：`git` 命令可并发，文件写入不行。

### 4.4 输入验证 + 权限检查

执行前的双层验证：
1. `ValidateInput(ctx, input)` → 输入格式和安全性检查
2. `CheckPermissions(ctx, input)` → 自定义权限逻辑（allow/deny/ask）

### 4.5 结果截断

`MaxResultSizeChars` 限制每个工具结果的最大字符数，超出自动截断。

---

## 5. 权限系统

位置：`permission/`

### 5.1 三态权限

```
PermissionBehavior:
  Allow — 直接放行
  Deny  — 直接拒绝
  Ask   — 需要用户确认
```

### 5.2 五种权限模式

| 模式 | 行为 |
|------|------|
| Default | ReadOnly→allow, Normal→ask, Dangerous→ask |
| BypassPermissions | 所有工具→allow |
| AcceptEdits | ReadOnly+Normal→allow, Dangerous→ask |
| Plan | 只允许 ReadOnly 工具 |
| DontAsk | 不可确认→deny |

### 5.3 规则引擎

位置：`permission/rules.go`, `permission/matcher.go`

```
规则评估优先级: deny 规则 > ask 规则 > allow 规则
```

ToolPattern 语法：
- `"Bash"` — 匹配工具名
- `"Bash(git *)"` — 匹配工具名 + 输入前缀
- `"*"` — 匹配所有工具
- `"Read*"` — 前缀匹配

---

## 6. Token 估算

位置：`message/token.go`

### 类型感知密度

| 类型 | 密度 |
|------|------|
| 文本 | chars / 4 |
| JSON (tool_use, tool_result) | chars / 2 |
| 图片 | 2000 tokens (固定) |
| Thinking | chars / 3 |

### 参数

- **4/3 Padding Factor**: 所有估算结果乘以 4/3
- **Per-Message Overhead**: 每条消息 +10 tokens
- **Reserved Summary Tokens**: 20,000 (用于 autocompact)

### Warning State

| 状态 | 阈值 |
|------|------|
| OK | < 80% |
| Warning | 80%-90% |
| Error | 90%-95% |
| Blocking | > 95% |

---

## 7. 数据流

```
用户输入
    │
    ▼
┌─────────────────┐
│   goagent.App   │ ←── Tool 注册, Options 配置
│   Run(ctx, msg) │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  internal/loop  │ ←── 预处理, API 调用, 工具执行, 后处理
│   Loop.Run()    │
└────────┬────────┘
         │ 事件
         ▼
┌─────────────────┐
│   Event Channel │ → CLI 显示 / HTTP SSE / SDK 消费
└─────────────────┘
```
