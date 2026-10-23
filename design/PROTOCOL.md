# 线协议演进设计（PROTOCOL.md，草案 v1）

> Phase 1 产出。输入：design/CONTRACT.md（v1 现状与两个宿主的依赖面）、design/ECOSYSTEM.md（生态调研与决策表）、AG-UI 1.0 规范源码审计。状态：**草案，待宿主评审（Phase 3）**。

## 0. 结论

1. **双剖面，同一内核。** 内核事件 `goagent.Event` 不变；出口有两个序列化剖面：**v1 剖面**（现有 Envelope，冻结、只加字段与修缺陷）与 **AG-UI 剖面**（实现 AG-UI 1.0，供新项目与第三方前端）。内核永远不 import 任何协议类型。
2. **v1 先修后冻**：本轮修 3 个缺陷（seq 写锁外分配、忙会话顶掉订阅、HTTP 下 AskUser 读 stdin），补 3 个字段（`tool_is_error`、中断原因、缓存 token）。全部是加字段，不升版本。
3. **AG-UI 剖面严格守规**：AG-UI 的事件对象是封闭的（`unevaluatedProperties:false`，官方 TS 客户端会剥离未知顶层字段并告警），所以**所有扩展只走两条合法通道**——`CUSTOM{name, value}` 事件与 `metadata` 中带前缀的键。不往顶层塞任何字段。
4. **自定义事件成为一等 API**：`ctx.Emit(name, value)` / `App.EmitEvent(sessionID, name, value)`；v1 剖面输出新帧 `custom`，AG-UI 剖面输出 `CUSTOM`。这是两个宿主都在绕的缺口。
5. **框架提供断线续传**：每个会话一个按 seq 编号的帧环形缓冲 + `GET /runs/{session}/stream?after=N` 重新挂接。AG-UI 规范没有续传机制，这是我们的差异化，在两个剖面上都提供。

## 1. 架构

```
                      ┌──────────────────────────────┐
  agent loop ──Event──▶  会话事件总线（新）            │
  ctx.Emit  ──Event──▶  · 每会话单调 seq（锁内分配）  │──▶ v1 剖面 mapper ──▶ /chat SSE、/runs/{id}/stream
  宿主 EmitEvent ─────▶  · 环形缓冲（续传）            │──▶ AG-UI mapper ───▶ /agui（POST RunAgentInput）
                      │  · 交互原语路由（审批/提问）   │
                      └──────────────────────────────┘
```

- **会话事件总线**取代现在"每个 HTTP 请求一个 seq 计数器 + 一组订阅"的做法。seq 按**会话**分配，在总线锁内分配并入缓冲，再分发给所有订阅者——同时修掉缺陷 #1（顺序颠倒）和 #2（订阅被顶掉），并让断线续传成为可能。
- **剖面 mapper** 是纯函数式的转换层：`func(Event, *mapperState) []Frame`。状态只用于 AG-UI 的开闭配对（消息/工具调用/推理的 START/END）。
- 铁律：`internal/loop` 与根包的 `Event` 不 import `protocol/` 与 `protocol/agui/` 的任何类型；方言只存在于 mapper 与它的测试里。

## 2. v1 剖面：先修后冻

### 2.1 修缺陷（行为修正，不改格式）

| 缺陷 | 修法 |
|---|---|
| seq 在写锁外分配，线上顺序可能颠倒 | 由会话事件总线在锁内分配，帧按 seq 顺序写出 |
| 会话忙且未开插话时，第二个请求顶掉运行中流的审批/提问订阅 | 审批/提问的路由归会话事件总线，不再按连接订阅；被拒绝的第二个请求不再触碰订阅 |
| HTTP 模式下内置 AskUser 未配置 handler 时读服务器 stdin | RunHTTP 时默认创建 AskUserHandler；stdin 回退只保留给 CLI |

### 2.2 加字段（不升版本）

> 已完成：`tool_is_error`、缓存 token（UPGRADE-v0.13.md §1.9）。`reason` 并入 B1。

| 字段 | 所在帧 | 说明 |
|---|---|---|
| `tool_is_error` | tool_done | 来自 `Event.ToolIsError`；宿主可以删掉文本正则 |
| `reason` | interrupted | 中断原因（`/interrupt` 的 reason 原样透传） |
| `usage.cache_read_input_tokens`、`usage.cache_creation_input_tokens` | usage | 已在 provider.Usage 中，补上转发（键名同 Anthropic API 与 provider.Usage 的 JSON 标签） |
| `name`、`value` | custom（新帧类型） | 自定义事件，见第 4 节 |

`agent_status` 的取值在文档中固定为 `running | done | failed`（桌面端目前按 `error` 判断失败，属宿主侧 bug，评审时同步告知）。

### 2.3 冻结承诺

> 已完成：`GET /protocol` 补全并加防漂移测试；websocket/ 已删除；2.1 的三个缺陷已修复（UPGRADE-v0.13.md §1.9）。`profiles` 字段待 B5（AG-UI 剖面）一起加。

- 保留全部现有帧类型与字段名；`plan_confirm` 与 `interrupt` 帧在文档中标为"保留，当前不产生"。
- `GET /protocol` 补全遗漏的 10 个路由，并新增 `profiles: ["v1", "agui-1.0"]`。
- websocket/ 包删除（未接线、只回显，无人使用）。

## 3. AG-UI 剖面

### 3.1 端点与输入

- `POST /agui`，请求体为 AG-UI `RunAgentInput{threadId, runId, messages, tools, context, state, forwardedProps, resume}`，响应 `text/event-stream`，帧格式 `data: <json>\n\n`。
- `threadId` ↔ GoAgent 会话 ID（须通过 safename 校验）。
- **历史归属：服务端拥有。** 与 LangGraph、ADK、Claude Agent SDK 的集成做法一致：GoAgent 的会话记录是权威历史；`messages` 中按 id 去重，只把会话里没有的消息当作新输入；结束时发 `MESSAGES_SNAPSHOT` 对齐客户端。对外流式输出使用会话记录中消息的 UID 作为 `messageId`，保证下一轮客户端回传时能按 id 匹配。
- `RUN_STARTED.protocolVersion = "1.0"`（规范要求生产者声明）。

### 3.2 事件映射

| GoAgent 内核事件 | AG-UI 事件 | 说明 |
|---|---|---|
| 运行开始 | RUN_STARTED{threadId, runId, protocolVersion} | |
| EvtTextDelta | TEXT_MESSAGE_START（首个增量时）/ TEXT_MESSAGE_CONTENT | 文本消息在出现工具调用或轮次结束时 END |
| EvtThinking | REASONING_START / REASONING_MESSAGE_START{role:"reasoning"} / REASONING_MESSAGE_CONTENT | thinking 签名映射为 REASONING_ENCRYPTED_VALUE，客户端须原样回传 |
| EvtToolStart | TOOL_CALL_START{toolCallId, toolCallName, **parentMessageId**} + TOOL_CALL_ARGS（整段输入一次发出）+ TOOL_CALL_END | **必须带 parentMessageId**，否则客户端为每个调用新建一条助手消息；并行工具调用使用显式 START/END 三元组，不用 CHUNK 简写 |
| EvtToolDone | TOOL_CALL_RESULT{messageId, toolCallId, content} | 错误结果在 metadata 中标 `goagent.toolIsError` |
| 审批请求 | RUN_FINISHED{outcome:{type:"interrupt", interrupts:[{id, reason:"tool_call", toolCallId, responseSchema}]}} | 见 3.3 |
| 提问（AskUser） | RUN_FINISHED{outcome: interrupt, reason:"ask_user", responseSchema} | 同一机制 |
| EvtSubAgentProgress / 子代理事件 | SUBAGENT_STARTED/FINISHED/ERROR + 信封 subagentRunId | 见 SUBAGENTS.md |
| EvtUsageUpdate | CUSTOM `goagent.usage` | RUN_FINISHED.usage 携带汇总 |
| EvtCompaction | CUSTOM `goagent.compaction` | |
| EvtProgress（带 status_key） | ACTIVITY_SNAPSHOT{activityType:"goagent.status", messageId: status_key} | 状态行原地替换语义正好对应 activity |
| EvtProgress（无 status_key） | CUSTOM `goagent.progress` | |
| EvtSteer / EvtQueueRun | CUSTOM `goagent.steer` / `goagent.queueRun` | AG-UI 无运行中输入通道 |
| EvtRetrieval | CUSTOM `goagent.retrieval` | |
| 自定义事件 | CUSTOM{name, value} | 原样 |
| EvtDone | RUN_FINISHED{outcome:{type:"success"}} | |
| EvtInterrupted | RUN_FINISHED{outcome:{type:"cancelled"}} | 先关闭所有打开的消息/调用 |
| EvtError | RUN_ERROR{message, code} | 之后不得再发任何事件 |

规范约束（来自 AG-UI 客户端校验器）：首帧必须是 RUN_STARTED；RUN_FINISHED 之前必须关闭所有打开的消息、调用、推理与子代理；RUN_ERROR 之后不得再发事件。mapper 维护开闭状态，在任何边界前强制关闭——这也是所有现有集成共同采用的模式。

### 3.3 审批的两次运行模型

AG-UI 的人机协同是"两次运行"：本次运行以 `outcome:interrupt` 结束，客户端下次请求在 `resume[]` 中携带决定。GoAgent 内核的审批是"阻塞等待"——两者通过**审批停车**桥接：

1. 审批请求到达时，mapper 发出 TOOL_CALL_END（如未关闭），以 `RUN_FINISHED{outcome:interrupt}` 结束本次 HTTP 流；**内核运行不结束**，在审批点继续阻塞（就是现在的行为）。
2. 客户端下次 `POST /agui` 带 `resume:[{interruptId, status, payload:{approved, editedArgs?}}]`；服务端把决定交给阻塞中的审批点，并把这次新流挂接到同一个内核运行上，从审批点继续输出。
3. 内核运行在审批点停车超过超时（默认 30 分钟，可配置）则按拒绝处理。
4. `interruptId` 编码 `会话ID::请求ID`（参照 Mastra 的做法：客户端只回传 interruptId，不回传 metadata）。

按规范，恢复后的运行**不重发** TOOL_CALL_START/ARGS/END，直接对原 toolCallId 发 TOOL_CALL_RESULT。审批负载格式采用规范推荐的 `{approved: boolean, editedArgs?: object}`；`editedArgs` 是完整替换，作为后续改参审批的输入（v1 剖面当前不支持改参）。

### 3.4 前端工具

`RunAgentInput.tools` 中的工具作为"前端执行工具"注入本次运行（AskUser 的泛化）：模型调用时 mapper 发出 START/ARGS/END 后以 `outcome:success + pendingToolCallIds` 结束本次流；内核在该工具处停车，客户端下次请求以 `role:"tool"` 消息带回结果。复用 3.3 的停车机制。**第二批实现**。

### 3.5 一致性验证

AG-UI 没有面向服务端的一致性测试工具（官方 68 个流式用例只测客户端）。我们的验证方式：
1. 用支持 `unevaluatedProperties` 的 JSON Schema 2020-12 校验器，对 mapper 输出逐帧校验 `spec/1.0/schema.json`。
2. 在 CI 里用官方 TS `HttpAgent` 连接测试服务器，出现任何 `[ag-ui]` 告警或校验错误即失败。
3. AG-UI 的 Go 类型**自持**（从 schema.json 生成），不依赖社区 Go SDK——后者是手写的，落后 1.0 规范（缺 `cancelled` outcome、`pendingToolCallIds`、`RUN_STARTED.protocolVersion` 等）。

## 4. 自定义事件 API

```go
// 工具、中间件、hook 中：
ctx.Emit("animecreater.canvas_command", CanvasCommand{Action: "add_node", Node: n})

// 宿主从外部向会话推送（与 Steering 同一通道模型）：
app.EmitEvent(sessionID, "amc.device_changed", DeviceState{...})
```

- **名称**：`<命名空间>.<事件名>`，点号分隔。`goagent.` 前缀保留给框架。宿主使用自己的前缀（`animecreater.`、`amc.`）。（采用点号而非斜杠，与 AG-UI metadata 键的前缀惯例一致。）
- **值**：任意可 JSON 序列化的值；序列化在发射点完成，失败返回错误而不是静默丢弃。
- **可选的 schema 注册**：`app.RegisterEvent(name, schemaOf[T]())`，注册后发射时校验（复用 `schema.Validate`），`GET /protocol` 列出已注册事件。
- **内核表示**：新增 `EventCustom`，字段 `CustomName string`、`CustomValue json.RawMessage`。观察者（Observer）照常收到。
- **线上**：v1 剖面输出 `{type:"custom", name, value}`；AG-UI 剖面输出 `CUSTOM{name, value}`。
- **持久化**：默认不进会话记录（自定义事件是展示层信号，不是对话内容）；需要重放的进入第 5 节的环形缓冲。

预期效果：AnimeCreater 的 30+ 种领域帧和自建事件总线、amobileCreater 塞进 `ask_user.payload` 与 `subagent_progress` 的领域语义，都可以迁到这一个 API 上。

## 5. 断线续传

- 会话事件总线为每个会话保留最近 N 帧（默认 2000 帧或 8 MiB，先到为准），帧带会话内单调 seq。
- `GET /runs/{session}/stream?after=<seq>`：先补发缓冲中 seq > after 的帧，再切换为实时流。若 after 已滑出缓冲，首帧返回 `{type:"resync", oldest:<seq>}`，客户端改用 `/sessions/{id}/messages` 全量重建。
- v1 剖面的 `/chat` 首帧 `run_start` 携带本流起始 seq；AG-UI 剖面不在顶层放 seq，而是放在 `metadata["goagent.seq"]`（metadata 对所有事件合法，且客户端不剥离）。
- 断线期间的审批/提问：因为路由归会话总线而非连接，重新挂接后待处理的交互帧随补发重新到达，`/pending-*` 端点保留作兼容。

## 6. 版本与能力

- v1 剖面：`v: 1` 不变，规则维持"加可选字段不升版本"。
- AG-UI 剖面：`RUN_STARTED.protocolVersion = "1.0"`。AG-UI 规范没有能力交换端点，我们提供 `GET /agui/capabilities`，返回 AG-UI `AgentCapabilities` 结构（声明 `tools.parallelCalls`、`humanInTheLoop.interrupts`、`reasoning.encrypted`、`transport.resumable` 等）。

## 7. 实施拆分（建议进入 v0.14）

| 批次 | 内容 | 风险 |
|---|---|---|
| P1 | v1 修缺陷 + 加字段；会话事件总线（seq、缓冲、交互路由）；删除 websocket/ | 低：全部向后兼容 |
| P2 | 自定义事件 API（Emit/EmitEvent/RegisterEvent，两个剖面输出） | 低：纯增量 |
| P3 | 断线续传端点；TS 客户端修缺陷并支持续传、custom 帧 | 低 |
| P4 | AG-UI 剖面：映射 + 审批停车 + 一致性测试 | 中：新协议面 |
| P5 | AG-UI 前端工具；改参审批 | 中 |

## 8. 待评审决策点

1. **自定义事件命名分隔符**：点号（`animecreater.canvas_command`，建议）还是斜杠。
2. **自定义事件默认是否进会话记录**：建议默认不进，提供 `Persist` 选项。
3. **审批停车超时**：建议默认 30 分钟，超时按拒绝处理。
4. **断线续传缓冲大小**：建议 2000 帧 / 8 MiB。
5. **AnimeCreater 是否迁到框架的续传与自定义事件**：可选，不迁也不受影响。
