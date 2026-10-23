# 子代理治理设计（SUBAGENTS.md，草案 v1）

> Phase 2 产出。输入：GoAgent 内核现状（agenttool.go、internal/loop/scope.go、steering.go、bgtask/）、ADK-Go / OpenAI Agents SDK / Eino / Genkit 四家的子代理源码审计、design/PROTOCOL.md。状态：**草案，待宿主评审（Phase 3）**。

## 0. 结论

1. **不是从零开始**：GoAgent 已经有异步子代理——`Agent_*` 工具的 `run_in_background` 经 bgtask 立即返回 task_id，完成后经插话的排队车道自动回注会话。缺的是**运行中干预**和**事件归属**。
2. **补三个原语**：给运行中的子代理发指示（`agent_send`）、只中断指定子代理（复用 TaskStop 并让同步子代理也可寻址）、子代理事件按归属并入父流（而不是只回传最终文本）。
3. **插话下沉到循环基础能力**：`loop.Scope` 增加插话来源；每个子代理运行持有自己的插话车道，以 `tool_use_id` 寻址。这是 ADK-Go（只有 Gemini Live 能运行中输入）、OpenAI（无）、Eino（无）都没有的能力。
4. **子代理内的审批冒泡到用户**：现在已经成立（子运行继承父运行的权限门），本设计只规定它在协议上的归属（带 subagentRunId）。
5. **不引入 handoff（控制权转移）**：与 GoAgent 的会话模型冲突，文档明确说明区别。

## 1. 现状

| 能力 | 现状 | 位置 |
|---|---|---|
| 同步子代理 | `Agent_*` 工具阻塞到子代理结束，返回最终文本 | agenttool.go:89 起 |
| 异步子代理 | `run_in_background:true` → bgtask 注册，立即返回 task_id；终态经 OnTaskDone → 排队车道回注 | agenttool.go:147 startBackgroundAgent |
| 终止子代理 | 仅后台：`TaskStop(task_id)` → bgtask.Kill 取消任务 ctx | bgtask/tools.go:25 |
| 查看进度 | 仅后台：`TaskOutput`；同步子代理只有 subagent_progress 事件（工具数、token、当前活动一行字） | bgtask/tools.go:98 |
| 子运行继承检查链 | 是（权限门、计划、hooks、中间件经 loop.Scope 结构性继承） | internal/loop/scope.go |
| 插话 | **只有顶层运行**：loop 在工具批边界调 `Steering.DrainGuides(SessionID)`；`Scope` 不带插话来源，子运行没有插话入口 | internal/loop/loop.go:948 |
| 子代理事件 | 只有聚合后的 subagent_progress；子代理的文本、工具调用不进父流 | agenttool.go:208 |

**用户可感知的问题**：用户在一个跑了十分钟的同步子代理期间插话，指示会在车道里等到子代理整个结束、父循环回到工具批边界才生效——用户以为在纠正当前正在做的事，实际上没有影响。

## 2. 竞品对照（源码审计结论）

| | ADK-Go | OpenAI Agents SDK | Eino | Genkit Go |
|---|---|---|---|---|
| 代理即工具 | agenttool：新会话、父阻塞、**子事件全部丢弃**、只回最终文本 | as_tool：嵌套 Runner、父阻塞；子事件只能经 `on_stream` 回调，**不进外层流** | NewAgentTool：`EmitInternalEvents` 时子事件带 RunPath 前缀并入父流 | delegate_to 工具；子代理中断被压平为普通工具文本 |
| 异步子代理 | v2 task 模式（隔离作用域，事件转发） | 无 | 无 | `Async` 时有 check/wait/abort 工具 |
| 运行中给子代理发指示 | **无**（RunLive.Send 仅 Gemini Live） | 无 | 无 | 无 |
| 子代理内审批 | 确认请求冒泡为父运行的长时工具 | 冒泡为外层运行的 interruption（进程级全局字典存嵌套状态） | 中断地址树冒泡，可按地址定向恢复 | 压平，无法冒泡 |
| 归属标识 | Branch 路径 `a.b`、IsolationScope | 无（只在 tracing 中嵌套） | RunPath | 无 |

**可借鉴**：Eino 的"事件带路径并入父流但不进父历史"、ADK 的 Branch 点路径、OpenAI 的"子代理审批冒泡为外层中断"、Eino 的按地址定向恢复。
**我们独有**：运行中指示（四家都没有）。

## 3. 设计

### 3.1 统一的子代理句柄

每个子代理运行（同步或后台）在启动时注册到会话运行时（sessionruntime.go）的**子代理表**：

```go
type subagentRun struct {
    ID        string        // = 父运行中 Agent_* 调用的 tool_use_id
    Path      string        // 归属路径：父路径 + "." + ID，如 "toolu_01.toolu_07"
    Name      string        // Definition.Name
    Status    string        // running | done | failed | cancelled
    Steering  *steerLane    // 本子代理自己的插话车道
    Cancel    context.CancelFunc
    TaskID    string        // 后台运行时的 bgtask ID，同步运行为空
}
```

- **ID 就是 tool_use_id**：父模型调用 `Agent_*` 时天然产生，前端已经用它关联调用卡片，不需要新的 ID 体系。
- 同步子代理第一次变得**可寻址**——现在只有后台子代理有 task_id。
- 子代理运行结束后从表中移除，保留终态 5 分钟供查询。

### 3.2 插话下沉

- `loop.Scope` 新增 `Steering SteerSource`；`NewChild` 不继承父运行的插话来源，而是使用**子代理自己的车道**（来自 3.1 的句柄）。
- 每个循环（顶层或子代理）在自己的工具批边界消费自己的车道——与现在顶层运行的语义完全一致，只是车道按运行区分。
- `SteeringHub.Steer(sessionID, text)` 语义不变（投给顶层运行）；新增：

```go
hub.SteerAgent(sessionID, agentID, text) error   // 投给指定子代理
```

找不到运行中的子代理时返回错误（不静默投给顶层）。

### 3.3 父模型可用的工具

| 工具 | 作用 | 备注 |
|---|---|---|
| `agent_send{agent_id, message}` | 向运行中的子代理插话，在其下一个工具批边界注入 | 新增 |
| `agent_status{agent_id?}` | 查看一个或全部子代理的状态、当前活动、工具数、token | 新增；对后台子代理与 TaskOutput 等价 |
| `TaskStop{task_id}` | 终止后台子代理 | 现有；扩展为接受 agent_id |

**同步子代理期间父模型无法调用这些工具**（父循环正阻塞在 `Agent_*` 调用里）。所以：
- 父模型想边做边纠正 → 用 `run_in_background:true` 派发，再用 `agent_send` 纠正。工具描述里写清这一点。
- 用户想纠正同步子代理 → 走 3.4 的外部接口（这是主要场景）。

### 3.4 用户与宿主的外部接口

| 接口 | 说明 |
|---|---|
| `POST /queue` 与 `/chat` 插话增加可选 `agent_id` | 带 agent_id 时投给该子代理的车道 |
| `POST /interrupt` 增加可选 `agent_id` | 只取消该子代理的 ctx；父运行收到 `tool_result{is_error, "子代理已被用户中断"}` 并继续 |
| `GET /agents?session_id=` | 列出会话内的子代理（ID、路径、名称、状态、当前活动） |
| Go API：`app.SteerAgent`、`app.InterruptAgent`、`app.Subagents(sessionID)` | 宿主进程内使用（AnimeCreater 走这条） |

**插话目标的默认值**：不带 agent_id 时投给顶层运行（保持现有语义）。前端在用户点开某个子代理卡片后输入时带上 agent_id——这是 UI 决策，框架只提供能力。

### 3.5 子代理事件并入父流

- 新增 `WithSubagentStream(mode)`：`summary`（默认，现状：只有 subagent_progress）/ `full`（子代理的文本增量、工具调用、工具结果全部转发）。
- 转发的事件带归属字段：内核 `Event` 新增 `AgentPath string`（空 = 顶层）。
- 转发事件**不进父运行的会话记录**（参照 Eino：事件路径与父路径不完全相等时不进父历史），只进事件总线；子代理自己的记录仍由子运行持有，完成后可经 `GET /subagents/{session}/{agent_id}/messages` 查看（amobileCreater 已经自建了同类的 trace 端点）。
- 线上：
  - v1 剖面：现有帧加 `agent_path` 字段（加字段，不升版本）。
  - AG-UI 剖面：子代理启动发 `SUBAGENT_STARTED{subagentRunId: ID, name, parentToolCallId: ID, parentSubagentRunId: 父 ID}`，其事件信封带 `subagentRunId`，结束发 `SUBAGENT_FINISHED{outcome}` 或 `SUBAGENT_ERROR`。AG-UI 规范允许子代理并行、事件任意交错，且要求并发时每个事件都带归属——与本设计一致。

### 3.6 子代理内的审批与提问

- 子运行继承父运行的权限门（现状），审批请求经会话事件总线到达用户。本设计补上**归属**：审批帧带 `agent_path`（v1）/ 信封 `subagentRunId`（AG-UI，Interrupt 也携带 subagentRunId）。
- 用户的决定按 request_id 路由回阻塞中的审批点，与归属无关——不需要 OpenAI 那种进程级嵌套状态字典，因为 GoAgent 的审批是阻塞等待而不是序列化后恢复。
- 后台子代理在父运行结束后仍需审批时：审批帧照常进入会话事件总线；没有在线订阅者时走 `/pending-approval`，并在用户下次连接时经断线续传补发（PROTOCOL.md 第 5 节）。

### 3.7 资源治理

| 项 | 规则 |
|---|---|
| 嵌套深度 | 沿用 `MaxSubRunDepth = 3` |
| 每会话并发子代理 | 新增上限，默认 8；超出时 `Agent_*` 返回错误让模型串行化 |
| 孤儿 | 父运行结束时，同步子代理随父 ctx 取消（现状）；后台子代理继续运行（现状），由 bgtask 管理 |
| 会话关闭 | `CloseSession` 取消该会话全部子代理 |

## 4. 不做的事

- **handoff（控制权永久转移给另一个代理）**：OpenAI SDK 的 handoff 让新代理接管对话、父代理不再恢复。GoAgent 的会话有唯一的主代理与权限上下文，handoff 会让"谁对这个会话负责"变得不确定。需要专家直接答复用户时，用子代理返回结果、主代理转述即可。
- **子代理状态的序列化恢复**（Eino 的检查点、OpenAI 的 RunState）：GoAgent 的会话记录 + 断线续传已覆盖"重连后看到进度"，进程重启后恢复运行中的子代理不在本期范围。

## 5. 实施拆分（建议进入 v0.14，排在 PROTOCOL.md P1–P3 之后）

| 批次 | 内容 | 依赖 |
|---|---|---|
| S1 | 子代理表 + 同步子代理可寻址；`InterruptAgent`、`POST /interrupt` 的 agent_id | 会话运行时 |
| S2 | 插话下沉（Scope.Steering、每运行车道）；`SteerAgent`、`agent_send` 工具、插话接口的 agent_id | S1 |
| S3 | `agent_status` 工具、`GET /agents` | S1 |
| S4 | 子代理事件并入父流（AgentPath、WithSubagentStream） | PROTOCOL P1 的会话事件总线 |
| S5 | AG-UI 剖面的 SUBAGENT_* 映射 | PROTOCOL P4 |

## 6. 待评审决策点

1. **插话默认目标**：不带 agent_id 时投给顶层运行（建议，保持兼容），还是投给"最近活跃的子代理"。
2. **子代理事件默认模式**：`summary`（建议，零行为变化）还是 `full`。
3. **每会话并发子代理上限**：建议 8。
4. **被中断的子代理给父模型的结果文本**：建议明确说明"被用户中断"，让父模型不要自动重试同一任务。
