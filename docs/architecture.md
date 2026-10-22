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

### 1.2 继续下一次迭代的原因

| 触发条件 | 行为 |
|---|---|
| 正常工具调用完成 | 继续下一轮 |
| 主模型过载且配置了后备 | 切换到后备模型重试 |
| 上下文过长（413） | 折叠排空或响应式压缩后重试 |
| 输出被 max_tokens 截断 | 注入续写提示（最多 3 次；不提升 max_tokens，部分 OpenAI 兼容后端对显式的大值直接 400） |
| 推理模型只在思考通道输出 | 注入提示要求正文重述（最多 2 次） |
| 流中断 | 已到达的内容固化为锚点，注入续跑提示后重开流（连续 3 次失败才报错） |
| stop hook 阻止退出 | 注入修正消息 |

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

## 2. 对话记录与模型视图

位置：`internal/loop/loop.go`、`compaction/boundary.go`

循环同时维护两份消息：

- **对话记录**（transcript）：只追加。进入对话的每条消息都分配 UID，并经 `Config.OnMessage` 同步交给持久化。自动摘要（L4）不改写它，而是追加一条**摘要记录**：`IsCompactSummary`，`CompactBoundary` 指向摘要之后保留的第一条消息，同时是元消息（`IsMeta`），展示层回放时过滤。
- **模型视图**（view）：每次请求发给模型的消息。每次迭代开始时经压缩层（第 3 节）处理；L0–L3 是确定性的写时复制变换，只改视图。

运行开始时，视图由 `compaction.Project(InitialMessages)` 得到：最后一条摘要记录，加上它边界之后的全部消息。所以重新加载会话、同一事件流的排队续跑、宿主用 `RunWithHistory` 传回历史时，都直接还原视图，不会再请求模型摘要一遍。`Loop.FinalMessages()`、`EventDone.Messages`、`WithTranscriptSink` 给出的都是对话记录。

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

### 3.4 摘要记录与投影

见第 2 节。L4 摘要成功后，摘要消息带上 `CompactBoundary`；循环把它作为摘要记录追加进对话记录。`compaction.Project` 从对话记录还原视图。边界在记录中找不到时（记录被截断或改写过），放弃投影，交给压缩层按需重新摘要。

### 3.5 图片结果

含内联图片（`[IMAGE <mime> <base64>]`）的工具结果绝不做头尾截断，否则会产出损坏的 base64，请求必然 400。最近 40 条消息内的图片完整保留，更早的整体回收为占位符；占位符带上图片标记之后正文里出现的原图路径（Windows 或 POSIX 绝对路径）。

### 3.6 压缩后重注入

`WithPostCompactReminder` 注册的提醒源在每次实际压缩后被调用，返回的文本作为 user 消息追加在摘要之后，重新注入被摘要吞掉的关键上下文。这些提醒只属于本次运行的视图，不进入对话记录。

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

工具 panic 被执行器恢复为错误结果；ctx 取消后不再启动排队中的工具；等待在跑的工具有上限（`CancelGrace`）。

### 4.2 并发分级

`ToolDef.Effect`（`executor.Class`）决定编排策略：

| 分级 | 含义 | 并行规则 |
|---|---|---|
| `ClassReadOnly` | 无副作用 | 任何时候都可并行，包括独占工具运行期间 |
| `ClassParallel` | 有副作用但并发调用互不冲突 | 可与并行、只读工具并行，不与独占工具重叠 |
| `ClassExclusive` | 有全局副作用（写文件、git、Bash） | 独占执行 |

未设置 `Effect` 时按 `Concurrent` 推导：true 为 Parallel，false 为 Exclusive。

### 4.3 执行前检查链

每次工具调用执行前依次：
1. 入参按工具的 JSON Schema 校验（`WithToolInputValidation(false)` 关闭）；
2. plan 模式过滤；
3. 中间件 `BeforeTool`；
4. hooks `PreToolUse`；
5. 权限门（见第 5 节）。

任一步拒绝时，工具不执行，拒绝原因作为工具结果交给模型。

### 4.4 结果截断

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

### 5.2 六种权限模式

| 模式 | 行为 |
|------|------|
| Default | ReadOnly→allow, Normal→ask, Dangerous→ask |
| AcceptEdits | ReadOnly + 编辑类工具（`FileEdit: true`）→allow（高风险的编辑除外），其余→ask |
| Auto | ReadOnly / 白名单→allow，其余→YOLO 分类器判定（不可用时 ask） |
| BypassPermissions | 普通操作→allow，高风险操作（`permission.HighRisk`、命中 ask 规则）→YOLO 分类器判定（不可用时 ask） |
| Plan | 只允许 ReadOnly 工具 |
| DontAsk | 不可确认→deny |

### 5.3 规则引擎

位置：`permission/rules.go`, `permission/subject.go`

```
规则评估优先级: deny 规则 > ask 规则 > allow 规则
```

模式语法：
- `"Bash"` — 匹配工具名
- `"Bash(git *)"` — 匹配工具名 + 从入参中提取的值（命令、路径、URL 等）
- `"*"` — 匹配所有工具
- `"Read*"` — 工具名前缀匹配

shell 命令按控制操作符拆成子命令逐个匹配：deny / ask 任一子命令命中即生效，allow 要求每个子命令都被覆盖。

### 5.4 "始终允许"授权

用户勾选"始终允许"后，授权记在权限门的 `permission.Grants` 上：普通工具按工具名记，shell 命令按"工具名 + 命令的前一到两个词"记。同一会话的各次运行共享同一份授权（见第 9 节），不跨会话。

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

---

## 8. 子运行与作用域

位置：`internal/loop/scope.go`

pipeline 节点与 supervisor、团队队长与成员、子 agent（`agent.Runner`）都使用同一个循环实现。

- 每个循环把自己的**作用域**（`loop.Scope`）随 ctx 交给每一次工具执行。作用域包括：
  - 检查链：权限门、plan 模式、hooks、中间件；
  - 入参校验开关；
  - 观测出口：observer、日志、debug、会话 ID；
  - 主 provider 与后备。
- 在工具里派生的子循环经 `loop.NewChild(ctx, cfg)` 创建：
  - 检查链一律取自作用域，子运行只能和父运行一样严格；
  - 观测出口和会话 ID 在未设置时沿用；
  - 未指定 provider 时沿用父运行的 provider，与父运行相同时一并沿用后备；
  - 子循环标记为 `Nested`，不发会话开始/结束事件。
- 例外是两类 pipeline：宿主在任何运行之外直接启动的，以及设置了 `PipelineConfig.TrustedTools` 的，都不继承检查链（宿主信任）。由模型编排的 `create_pipeline` / `run_team` 不在任何带权限门的运行之内时，按 App 配置新建一套作用域。
- 子运行嵌套深度上限 `MaxSubRunDepth = 3`。

```
App.run ── loop.New(主循环) ──┬── 工具 ctx 携带 Scope
                              │
            create_pipeline ──┼── pipeline 节点：loop.New + Scope.ApplyTo
            run_team ─────────┼── 队长 / 成员：loop.NewChild
            子 agent 工具 ─────┴── agent.Runner：loop.NewChild
```

---

## 9. 会话运行时

位置：`sessionruntime.go`

每个会话有一个跨运行常驻的运行时：
- 工具的键值存储（`Context.Store` / `Load`）；
- 压缩管理器（自动摘要的熔断状态）；
- "始终允许"授权；
- 会话级的运行时权限模式。

真实会话的运行时登记在 App 上，直到 `App.CloseSession`。没有会话身份的运行（`Run`、`RunWithHistory`）每次使用一个新的运行时，彼此隔离。

App 登记所有进行中运行的权限门：
- `SetPermissionMode` 作用于所有在跑运行（模式来自会话级设置的除外）；
- `SetSessionPermissionMode` 只作用于该会话；
- Guard 的增删作用于所有在跑运行。

权限模式优先级：会话级运行时设置 > `WithSessionPermissionMode` 的解析结果 > 全局运行时覆写 > `WithPermissionMode`。
