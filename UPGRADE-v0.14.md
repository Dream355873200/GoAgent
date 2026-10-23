# GoAgent v0.14 升级指南：会话事件总线、自定义事件、子代理治理、MCP 换芯、AG-UI、可观测与测试工具、扩展

本文面向使用 GoAgent 的项目，说明从 v0.13 升级到 v0.14 时需要知道的一切：线协议与 HTTP 行为的变化、
Go API 的变化、每个宿主具体要改什么。设计依据见 `design/`（PROTOCOL.md、SUBAGENTS.md、ROADMAP-v0.14.md）。

还没有升级到 v0.13 的项目，先读 [UPGRADE-v0.13.md](UPGRADE-v0.13.md)。

**兼容性原则**：v1 线协议只加字段、加帧类型，不改已有字段的语义，协议版本 `v` 仍为 1；Go API 只加不删。
少数行为修正在第 0 节速查表里逐条标出。

---

## 目录

- [0. 三分钟速查](#0-三分钟速查)
- [1. 会话事件总线与断线续传（B1）](#1-会话事件总线与断线续传b1)
- [2. 自定义事件（B2）](#2-自定义事件b2)
- [3. 子代理治理（B3）](#3-子代理治理b3)
- [4. MCP 换用官方 SDK（B4）](#4-mcp-换用官方-sdkb4)
- [5. AG-UI 1.0 剖面（B5）](#5-ag-ui-10-剖面b5)
- [6. 可观测与测试工具（B6）](#6-可观测与测试工具b6)
- [7. 扩展：前端工具、改参审批、MCP elicitation、MCP 服务端、可插拔分类器（B7）](#7-扩展前端工具改参审批mcp-elicitationmcp-服务端可插拔分类器b7)
- [8. 宿主迁移清单](#8-宿主迁移清单)
- [9. 验证情况](#9-验证情况)

---

## 0. 三分钟速查

| 变化 | 影响谁 | 要不要改代码 |
|---|---|---|
| `/chat` 帧新增 `session_seq`、`run_id`；`interrupted` 帧新增 `reason`；新帧 `resync` | 直接解析 v1 线格式的客户端 | 不用改；想要断线续传时读 `session_seq` |
| 新端点 `GET /runs/{session}/stream`（断线续传） | HTTP 客户端 | 可选：替换断线后的轮询 |
| 审批与提问改经会话事件日志下发：挂在同一会话上的所有连接都能收到；客户端断开后请求留在日志里，续传时重新到达 | HTTP 客户端 | 不用改 |
| `/chat` 的连接断开后，HTTP handler 不再等运行结束才返回；运行照常跑完，`POST /interrupt` 在断开后仍然有效 | HTTP 宿主 | 不用改 |
| 宿主进程内调用 `RunSession` / `RunWithSessionHistory` 的运行同样写入会话事件日志（HTTP 模式下可被 `/runs/{session}/stream` 观察） | engine 的团队成员会话等 | 不用改 |
| `session.Manager.Acquire` 的忙错误可用 `errors.Is(err, session.ErrBusy)` 判断（文本不变） | 匹配错误文本的宿主 | 可选 |
| `EventType.String()` 对 `EventNeedApproval` 返回 `permission_request`（原为 `need_approval`，与线上帧名统一） | 打印事件类型名的宿主 | 基本不用 |
| 新事件类型 `EventCustom` 与新帧 `custom{name, value}`（自定义事件） | 按事件类型 `switch` 的宿主、v1 客户端 | 不用改（未知类型照常忽略）；想用时见第 2 节 |
| 子代理可寻址：每个 `Agent_*` 调用（同步或后台）以其 `tool_use_id` 登记，可查询、可单独发指示、可单独中断 | 所有用子代理的宿主 | 不用改；新能力见第 3 节 |
| `subagent_progress` 新增状态 `cancelled`（子代理被单独中断）与字段 `agent_path` | 解析 `agent_status` 的客户端 | 建议把 `cancelled` 渲染为"已停止" |
| 每个会话同时运行的子代理默认上限 8 个，超出时 `Agent_*` 返回错误让模型串行化 | 一轮里并行派发很多子代理的场景 | 需要更多时 `WithMaxSubagents(n)` |
| `WithSubAgents` 额外注册 `agent_send`、`agent_status` 两个工具 | 用 `WithSubAgents` 的宿主 | 不想暴露给模型时用 `WithSessionToolFilter` 过滤 |
| 后台子代理启动回执多了 `agent_id`；`TaskOutput` / `TaskStop` 也接受 agent_id | 依赖回执文本的宿主（如正则解析） | 检查正则 |
| **新依赖** `github.com/modelcontextprotocol/go-sdk v1.8.0`（`mcp` 包内部换用官方 SDK）；`gorilla/websocket` 不再需要 | 所有用 `replace` 或升级版本的宿主 | **升级后在宿主模块执行 `go mod tidy`**（engine 必做，否则 `go build` 报 go.mod 需更新） |
| MCP 工具结果里的图片以内联图片交给模型（此前是"[image …]"占位文字）；只有结构化结果时以其 JSON 作为文本 | 用 MCP 工具的宿主 | 不用改 |
| MCP 握手默认请求协议版本 2025-11-25（此前 2025-03-26），服务端可协商为旧版本 | 对接老 MCP 服务器的宿主 | 个别服务器异常时用 `ServerConfig.ProtocolVersion` 固定版本 |
| 新端点 `POST /agui`、`GET /agui/capabilities`（AG-UI 1.0 剖面）；`GET /protocol` 新增 `profiles: ["v1","agui-1.0"]` | 想接 AG-UI 生态前端（CopilotKit 等）的项目 | 可选，见第 5 节 |
| v1 帧新增 `message_id`（与 `/sessions/{id}/messages` 中消息的 `uid` 一致）；会话记录中助手消息与工具结果的 UID 在流式阶段就已确定 | v1 客户端 | 可选：用它把流式内容与回放记录对上 |
| 审批拒绝原因现在送达模型：`/approve` 的 `reason`（以及 AG-UI 审批的 reason、停车超时）写进工具结果 `权限被拒绝: <原因>`，此前一律是"用户拒绝" | 用 `/approve` 带 reason 的客户端 | 不用改 |
| `observer.MultiObserver` 现在转发可选的 `LLMObserver` 钩子（此前 App 有两个以上观察者时这些钩子静默失效） | 实现了 `LLMObserver` 且同时开了成本追踪/分析等的宿主 | 不用改；此前收不到的 `OnLLMStart/Done/Error` 现在会收到 |
| 每次 `OnLLMStart` 恰好对应一次 `OnLLMDone` 或 `OnLLMError`：换后备、压缩后重试、流中断恢复、429 等待被中断此前不报结束，现在各报一次 `OnLLMError` | 实现了 `LLMObserver` 的宿主 | 按 `OnLLMError` 计失败数的宿主会看到这些此前漏掉的失败 |
| 新包 `observer/oteltrace`（OpenTelemetry 导出，`gen_ai.*` 语义约定）、`provider/scripted`（脚本化模型）、`httprr`（HTTP 录制回放）；新可选接口 `observer.ContextObserver` | 想要链路追踪、确定性测试的宿主 | 可选，见第 6 节 |
| `anthropic.WithHTTPClient`、`openai.Config.HTTPClient`：自定义 provider 的 HTTP 客户端 | 需要代理、追踪、录制回放的宿主 | 可选 |
| **新依赖** `go.opentelemetry.io/otel` v1.46.0（及仅测试用的 `otel/sdk`），`golang.org/x/sys` 升到 v0.47.0 | 所有宿主 | 与 MCP 新依赖一起 `go mod tidy` 一次即可 |
| AG-UI 前端工具（`RunAgentInput.tools`）：以 `success + pendingToolCallIds` 停车，tool 消息带回结果；改参审批（`editedArgs`）；`/agui/capabilities` 的 `clientProvided`、`approveWithEdits` 为 true | AG-UI 客户端 | 可选，见第 7 节 |
| **v1 也支持前端工具**：`/chat` 新增 `tools` 字段（客户端声明、客户端执行）、`POST /chat/tool-result` 回传结果、`GET /pending-tools` 查未完成调用；TS 客户端 0.5.0（`chat({tools})`、`toolResult()`、`pendingTools()`） | v1 客户端 | 可选；与 AG-UI 版共享同一内核机制，v1 流不断、无需停车 |
| v1 `POST /approve` 新字段 `input`（批准时替换工具参数）；TS 客户端 0.4.0 `approve(id, true, {input})` | v1 客户端 | 可选 |
| `WithMCP` 连接的 MCP 服务器发起的 elicitation 转成所在会话的提问（`payload.kind = "mcp_elicitation"`），此前客户端不声明该能力 | 用 `WithMCP` 且服务器会索取输入的宿主 | 客户端按 `kind` 渲染表单；不处理的提问照常可以文本作答 |
| 新增 `App.MCPServer` / `MCPHandler` / `ServeMCPStdio`：把工具与 agent 暴露为 MCP 服务端 | 想被其他 MCP 客户端调用的宿主 | 可选 |
| `WithPermissionClassifier`：可插拔分类器；`permission.Gate.SetClassifier` 参数改为 `Classifier` 接口 | 直接调用 `Gate.SetClassifier` 的代码 | 传 `*YoloClassifier` 的调用不用改 |

---

## 1. 会话事件总线与断线续传（B1）

### 1.1 是什么

每个会话有一条只追加的**会话事件日志**：运行开始、运行中的每个事件、运行结束，以及该会话的审批请求与提问。
条目带会话内单调递增的序号 `session_seq`，日志保留最近一段（默认 2000 条或约 8 MiB，先到为准），
会话空闲且没有读取方 30 分钟后释放。

HTTP 的 `POST /chat` 不再直接转发运行的事件通道，而是从这条日志写出帧；新增的
`GET /runs/{session}/stream` 从日志的任意位置续读。于是：

- **断线续传**：客户端断开后运行继续（与以前一样），重连时带上最后收到的 `session_seq`，补齐期间的帧再转实时。
- **审批与提问不再绑在某一条连接上**：它们进入会话日志，挂在该会话上的所有连接都收到；
  原连接断开时请求留在日志里，续传连接重新收到后照常经 `/approve`、`/askuser` 作答。
  `/pending-approval`、`/pending-ask` 保留作兼容。
- **发布永不阻塞**：慢客户端只是游标落后；落后超出缓冲时收到 `resync` 帧（应从 `/sessions/{id}/messages` 重建）。

### 1.2 线协议变化（v 仍为 1）

| 字段 / 帧 | 出现在 | 说明 |
|---|---|---|
| `session_seq` | 所有来自会话日志的帧 | 会话内单调递增的**不透明整数**（不从 1 开始，日志释放后重建也继续增大）。插话回执、排队回执、拒绝、流结束元数据等只属于本连接的帧没有它 |
| `run_id` | `run_start`、该运行的 `metadata` | 一次会话运行的标识（排队续跑属于同一次运行） |
| `reason` | `interrupted` | `POST /interrupt` 给出的原因 |
| `resync`（新帧）+ `oldest` | 续传位置早于缓冲时的首帧 | `oldest` 是缓冲中最旧帧的 `session_seq`，其后的帧照常到达 |

`seq` 的语义不变：连接内从 1 起连续递增，`/chat` 首帧仍是 `run_start`、末帧仍是 `metadata`。

### 1.3 `GET /runs/{session}/stream`

| 参数 | 含义 |
|---|---|
| `after=N` | 补发 `session_seq > N` 的帧，再转实时。也可以用 `Last-Event-ID` 头传入（浏览器 `EventSource` 重连时自动带上） |
| 省略 `after` | 从会话**进行中运行的开头**读起；没有进行中的运行时只返回一个 `metadata` 帧 |
| `after=0` | 从缓冲中最旧的帧读起 |
| `follow=1` | 跨运行持续推送，直到客户端断开；否则会话空闲且已读到最新时以 `metadata` 结束 |

本端点的日志帧额外带 SSE `id:` 行（值为 `session_seq`）。鉴权与其他端点相同。

**客户端的典型用法**（替代"断线后每 3 秒轮询 `/sessions/{id}/messages`"）：

```ts
// goagent-client 0.3.0：chat 自动续传
for await (const f of agent.chat({ message, sessionId, autoResume: true })) { render(f); }

// 或者手动：记住最后的 session_seq，断线后
for await (const f of agent.resume(sessionId, { after: lastSessionSeq })) { render(f); }
```

### 1.4 Go API

| API | 说明 |
|---|---|
| `WithSessionStream(SessionStreamConfig{MaxFrames, MaxBytes, IdleTTL})` | 启用会话事件日志并设置上限（零值取默认）。HTTP 模式总是启用 |
| `app.WatchSession(sessionID, after) *SessionWatcher` | 打开读取游标；`after` 可为 `WatchFromActiveRun`、`WatchFromNow`、`0` 或上次读到的 `Seq` |
| `SessionWatcher.Next(ctx) (SessionEvent, error)` / `Idle()` / `Cursor()` / `Close()` | 逐条读取；日志被 `CloseSession` 关闭时读完缓冲后返回 `ErrSessionStreamClosed` |
| `SessionEvent{Seq, Kind, RunID, Event, Elapsed, Oldest}` | `Kind`：`SessionRunStarted` / `SessionRunEvent` / `SessionRunFinished` / `SessionResync` |
| `InterruptHandler.RegisterCause(sessionID, cancelCause)` | 以 `*InterruptError{Reason}` 为原因取消；运行的 `EventInterrupted.InterruptReason` 带上原因 |
| `Event.RequestID` / `Event.Permission` / `Event.Payload` | 审批请求与提问进入会话日志时的载荷 |
| `session.ErrBusy` | 会话忙错误的哨兵值 |

进程内宿主（自建 WebSocket 等传输）可以用 `WithSessionStream` + `WatchSession` 取代自建的"按运行计的序号 +
重放缓冲 + 断线重连"。只有会话运行（`RunSession`、`RunWithSessionHistory`）写入日志；`Run`、`RunWithHistory`
没有会话身份，不写。

### 1.5 行为细节

- **运行结束帧在会话释放之后写出**：客户端收到 `metadata` 后立即发下一条消息，不会撞上"会话正在运行中"。
- **没有开始的运行不进日志**：会话被并发占用、存储读取失败、唤醒时队列已空等情况，结果只写给本连接（没有 `session_seq`）。
- **`/chat` 连接断开**：handler 立即返回；运行的事件通道由后台排空，中断登记随运行（而不是随连接）注销。
- 审批与提问的路由顺序：会话的显式订阅（`PermissionHandler.Subscribe` / `AskUserHandler.Subscribe`，
  engine 的团队成员转发用的就是它）优先，其次是会话事件日志（HTTP 模式），最后是共享 channel（SDK 直连消费）。
  没有会话身份的运行（如 `/execute`）不进日志。
- `PlanConfirmHandler` 不再在 `/chat` 里转发：它没有任何产生请求的入口（`plan_confirm` 帧一直标注为"保留，当前不产生"）。

---

## 2. 自定义事件（B2）

### 2.1 是什么

宿主的领域信号（画布指令、设备状态、节点进度……）此前只能塞进 `progress` 文本、`ask_user.payload` 或
`subagent_progress`，或者像 AnimeCreater 那样绕开框架自建事件总线。v0.14 把它做成一等 API：

```go
// 工具、中间件、hook 中——随所在运行的事件流下发：
ctx.Emit("animecreater.canvas_command", CanvasCommand{Action: "add_node", Node: n})
goagent.Emit(stdCtx, "animecreater.canvas_command", cmd) // 只有 context.Context 时（hook 里）

// 宿主从运行之外向会话推送：
app.EmitEvent(sessionID, "amc.device_changed", DeviceState{...})

// 可选：登记 schema，发射时校验，GET /protocol 列出
app.RegisterEvent("animecreater.canvas_command", CanvasCommand{}) // Go 类型或 JSON Schema（map / json.RawMessage）
```

### 2.2 规则

| 项 | 规则 |
|---|---|
| 名称 | `<命名空间>.<事件名>`，只含字母、数字、`_`、`-`，点号分隔；`goagent.` 前缀保留给框架 |
| 值 | 任意可 JSON 序列化的值，序列化在发射点完成；失败、或不符合登记的 schema 时返回错误（不静默丢弃） |
| 运行之中（`Emit`） | 事件进入所在运行的事件流；子 agent、pipeline 节点、团队成员的工具里发射的事件同样到达最外层的流。ctx 不属于任何运行时返回 `ErrNotInRun` |
| 运行之外（`EmitEvent`） | 会话有进行中的运行时进入该运行的事件流（SDK 消费方与 HTTP 客户端都收到）；没有时写入会话事件日志（HTTP 模式或 `WithSessionStream`）；都没有时返回 `ErrSessionNotActive` |
| 持久化 | 不进入会话记录（展示层信号，不是对话内容）；进入会话事件日志，可经续传重放 |
| 观察者 | 实现 `observer.CustomEventObserver`（`OnCustomEvent(ctx, name, value)`）的观察者收到 |

### 2.3 线上与 Go 表示

| 位置 | 表示 |
|---|---|
| Go 事件 | `Event{Type: EventCustom, CustomName, CustomValue json.RawMessage}` |
| v1 帧 | `{type:"custom", name, value, session_seq}` |
| `GET /protocol` | 新字段 `custom_events: [{name, schema}]`（登记过 schema 的事件） |
| TS 客户端 0.3.0 | `FrameType.Custom`；`Envelope.name` / `Envelope.value`；`Description.custom_events` |

### 2.4 迁移建议

- **AnimeCreater**：30 多种领域帧（`canvas_command`、`storyboard_cards`、`quality_*`……）可以改用 `ctx.Emit`，
  事件名加 `animecreater.` 前缀；自建的 ctx 事件总线与"按运行计的 seq + 重放缓冲"可由 `Emit` + 第 1 节的
  `WatchSession` 取代。不迁也不受影响。
- **amobileCreater**：塞进 `ask_user.payload` 的确认卡仍然走提问（它需要回答）；纯展示的领域状态
  （流水线节点、团队成员、后台代理的进度）可改用 `amc.` 前缀的自定义事件，不再复用 `subagent_progress`。

---

## 3. 子代理治理（B3）

### 3.1 是什么

此前只有后台子代理（`run_in_background`）有 task_id；同步子代理不可寻址，用户在它运行期间插话，指示要等它
整个结束、父循环回到工具批边界才生效。v0.14：

- **统一句柄**：每个子代理运行（同步或后台）启动时登记到会话的子代理表，ID 就是父运行中 `Agent_*` 调用的
  `tool_use_id`（前端已经用它关联调用卡片）。嵌套子代理的归属路径是 `父路径.ID`。结束后保留 5 分钟供查询。
- **插话下沉**：每个子代理运行有自己的插话车道，在它自己的工具批边界消费。子代理在指示送达前结束时，
  指示转交给父运行（注明"未送达"），不会丢。
- **按子代理中断**：只取消该子代理；父运行收到明确的错误结果（"已被用户中断。不要自动重试同一任务……"）后继续。
- **子代理事件并入父流**（可选）：`WithSubagentStream(SubagentStreamFull)` 时子代理的正文与思考增量、工具调用
  与结果、插话带 `AgentPath` 转发进父运行的事件流，不进入父运行的对话记录。默认 `summary`（只有 `subagent_progress`）。
- **资源治理**：每会话同时运行的子代理上限（默认 8）；`CloseSession` 中断该会话全部运行中的子代理。

### 3.2 Go API

| API | 说明 |
|---|---|
| `app.Subagents(sessionID) []SubagentInfo` | 运行中与最近 5 分钟内结束的子代理（ID、Path、Name、Status、Activity、ToolUses、Tokens、Background、TaskID、时间、Result） |
| `app.Subagent(sessionID, agentID)` | 单个子代理的快照与结束后保留的对话记录 |
| `app.SteerAgent(sessionID, agentID, text)` | 向运行中的子代理发指示；找不到时返回 `ErrAgentNotFound`（不会静默投给顶层运行） |
| `app.InterruptAgent(sessionID, agentID, reason)` | 只中断该子代理；后台子代理同时标记 bgtask 为 killed |
| `app.AgentControlTools()` | `agent_send`、`agent_status` 两个工具（`WithSubAgents` 自动注册；手工装配 `App.AgentTool` 的宿主可自行注册） |
| `WithSubagentStream(mode)` | `SubagentStreamSummary`（默认）/ `SubagentStreamFull` |
| `WithMaxSubagents(n)` | 每会话同时运行的子代理上限（默认 `DefaultMaxSubagents` = 8） |
| `AgentPathFromContext(ctx)` | 工具所在子代理的归属路径（主 agent 中为空） |
| `Event.AgentPath`、`PermissionRequest.AgentPath`、`AskUserRequest.AgentPath` | 事件、审批、提问的归属 |
| `agent.Runner.ForwardTo(path)` | 直接使用 `agent.Runner` 的宿主：把子 agent 的过程事件带路径转发进所在运行的事件流 |

**注意**：父模型在同步子代理运行期间阻塞在 `Agent_*` 调用里，无法调用 `agent_send`；想边做边纠正就用
`run_in_background:true` 派发。用户与宿主纠正同步子代理走外部接口（下表）——这是主要场景。

### 3.3 HTTP 与线协议

| 接口 | 说明 |
|---|---|
| `POST /chat` 带 `agent_id` | 作为指示投给运行中的子代理，不开新 run；回执 `steer{agent_path}` + `metadata{steered}`，子代理不在运行时 `error` + `metadata` |
| `POST /queue` 带 `agent_id` | 同上，JSON 回执 `{status, agent_id, steered}`；不在运行时 404。子代理没有排队车道 |
| `POST /interrupt` 带 `agent_id` | 只中断该子代理（父运行继续）；不在运行时 404 |
| `GET /agents?session_id=` | 子代理列表 |
| `GET /agents/{id}?session_id=` | `{agent, messages}`：快照与结束后保留的对话记录（不含元消息） |
| 帧字段 `agent_path` | `subagent_progress` 描述的子代理；full 模式下转发的正文/思考/工具帧；子代理里发起的 `permission_request` / `ask_user` |

TS 客户端 0.3.0：`agents()`、`agent()`、`steerAgent()`、`interruptAgent()`；`Envelope.agent_path`；`SubagentInfo` 类型。

**full 模式下客户端必须按 `agent_path` 区分**：不认识 `agent_path` 的旧客户端会把子代理的正文当成主 agent 的正文渲染，
所以 full 是显式开启的。

### 3.4 迁移建议

- **amobileCreater 桌面端**：子代理卡片已经按 `tool_use_id` 关联，可直接在卡片上加"发指示""停止"按钮
  （`steerAgent` / `interruptAgent`，或带 `agent_id` 的 `/queue`、`/interrupt`）；`subagent_progress` 的 `agent_status`
  多了 `cancelled`。engine 的团队成员会话不是框架子代理（它们是独立会话），不受影响。
- **AnimeCreater**：自建的取消可改用 `InterruptAgent`；子代理的细粒度时间线可改用 full 模式的事件流
  （或保留 `WithSubAgentTraceDir` 的轨迹文件）。

---

## 4. MCP 换用官方 SDK（B4）

### 4.1 是什么

`mcp` 包此前是手写的 JSON-RPC 客户端（协议 2025-03-26，无取消、无进度、无工具列表变更）。v0.14 起
`mcp.Dial` / `mcp.Connect`（以及 `WithMCP`）由官方 Go SDK `github.com/modelcontextprotocol/go-sdk v1.8.0` 承载，
**门面不变**：`ServerConfig`、`Connect`、`Dial`、`Client.Disconnect/ServerInfo/ServerName/ListTools/CallTool`、
`FrameworkTool`、`ToolName`/`ServerPrefix`、`goagent.MCPToolDef` 的签名与行为保持一致。接通的能力：

| 能力 | 效果 |
|---|---|
| 取消 | 运行被中断（ctx 取消）时调用立即返回，并向服务端发 `notifications/cancelled`，服务端的处理随之取消 |
| 进度 | 服务端的 `notifications/progress` 显示为该工具调用的状态行（`progress` 帧，`status_key` 为 `tool-progress:<id>`），不进入模型上下文 |
| 图片 | 结果中的第一张图片以内联图片交给模型（provider 转成多模态输入）；多余的图片与音频保留占位说明 |
| 工具列表变更 | 服务端发 `notifications/tools/list_changed` 后客户端重新列工具；`WithMCP` 自动同步注册表（新增注册、变化更新、消失移除，只动 `mcp__<server>__` 前缀的工具，对之后开始的运行生效） |
| 结构化结果 | 只有 `structuredContent`、没有内容项时以其 JSON 作为文本 |
| 健壮性 | stdio 服务器误打到 stdout 的非 JSON 日志行被丢弃（不会断开会话）；关闭时先关 stdin，等待退出，超时再终止进程 |

### 4.2 新增 API

| API | 说明 |
|---|---|
| `ServerConfig.ProtocolVersion` | 握手请求的协议版本，空 = `mcp.DefaultProtocolVersion`（2025-11-25）。更新的 2026-07-28 会先探测 `server/discover`，不认识它的老服务器可能一直不回应，所以默认不用 |
| `Client.CallToolWithProgress(ctx, name, args, onProgress)` / `mcp.Progress` | 接收进度通知 |
| `Client.OnToolsChanged(fn)` | 订阅工具列表变更（重新列出后以新列表回调） |
| `Client.NegotiatedVersion()` | 协商出的协议版本 |
| `mcp.WithProgressReporter(ctx, report)` | 让 `FrameworkTool.Execute` 把进度交给 `report`（`MCPToolDef` 已接到 `Context.Progress`） |
| `ToolCallResult.ModelText()` | 交给模型的文本（第一张图片为内联图片标记） |
| `App.RemoveTool(name)` | 注销工具（对之后开始的运行生效） |

`mcp.NewClient`、`Transport`、`NewStdioTransport*`、`NewHTTPTransport*` 标记为 Deprecated：直接给传输构造的客户端
仍走旧实现（不支持上表能力）。两个宿主都没有使用它们。

### 4.3 迁移建议

- **amobileCreater engine**：升级后执行 `go mod tidy`（新依赖）。插件 MCP 用的 `mcp.Connect` 不用改。想让插件服务器的
  工具列表变更生效，在 `connectPluginMCP` 里对 `client.OnToolsChanged` 做与初次注册相同的处理（按 `mcp__<server>__`
  前缀增删 `ReplaceTool` / `RemoveTool`）；不订阅则保持现状（只在重连时刷新）。
- **AnimeCreater**：不直接使用 `mcp` 包，无影响。

---

## 5. AG-UI 1.0 剖面（B5）

### 5.1 是什么

同一个内核、同一条会话事件日志，多一个出口方言：`POST /agui` 实现 [AG-UI](https://ag-ui.com) 1.0 协议，
请求体是 `RunAgentInput`，响应是 AG-UI 事件流（`data: <json>`）。v1 剖面不受影响，两者可以同时使用。
所有扩展只走 AG-UI 的两条合法通道——`CUSTOM` 事件（名称 `goagent.*` 或宿主的自定义事件名）与 `metadata`
中带 `goagent.` 前缀的键——不在顶层加任何字段（AG-UI 的事件对象是封闭的）。

### 5.2 约定

| 项 | 约定 |
|---|---|
| `threadId` | 就是 GoAgent 的会话 ID（同样经 safename 校验） |
| 历史 | 服务端拥有：请求的 `messages` 按 id 去重，只有会话里还没有的 `user` 消息作为本次输入（多条时按序合并、沿用最后一条的 id）；运行结束时发 `MESSAGES_SNAPSHOT`，客户端据此对齐。流式消息 id 与会话记录 UID 一致 |
| `context` | 作为 `<context>` 块附在输入后面 |
| 审批与提问 | 两次运行：本次以 `RUN_FINISHED{outcome:{type:"interrupt", interrupts}}` 结束（审批 `reason:"tool_call"` + `toolCallId`；提问 `reason:"ask_user"`，结构化载荷在 `metadata["goagent.payload"]`）；内核运行在审批点继续阻塞。下一次请求带 `resume:[{interruptId, status, payload}]`：审批 `payload` 为 `{approved, reason?}` 或布尔值，提问为 `{answer}` 或字符串；`status:"cancelled"` 视为拒绝。新流从停车处接着输出，不重发 `TOOL_CALL_START` |
| 停车超时 | 默认 30 分钟，超时按拒绝处理（提问得到空回答），`WithAGUI(AGUIConfig{ParkTimeout})` 可改 |
| 有待作答中断时的新请求 | `RUN_ERROR{code:"pending_interrupts"}`；会话忙为 `thread_busy`；没有新 user 消息为 `no_input` |
| 映射 | 正文 → `TEXT_MESSAGE_*`；思考 → `REASONING_*`；工具 → `TOOL_CALL_START/ARGS/END`（带 `parentMessageId`）+ `TOOL_CALL_RESULT`（错误在 `metadata["goagent.toolIsError"]`）；子代理 → `SUBAGENT_STARTED/FINISHED/ERROR` 与 `subagentRunId` 归属（运行结束仍在跑的后台子代理以 `suspended` 结束）；状态行 → `ACTIVITY_SNAPSHOT{activityType:"goagent.status"}`；用量、压缩、插话、排队、检索、子代理进度 → `CUSTOM goagent.*`；自定义事件 → `CUSTOM{name, value}`；用户中断 → `RUN_FINISHED{outcome:cancelled}`；错误 → `RUN_ERROR` |
| 续传位置 | 每个映射自会话日志的事件在 `metadata["goagent.seq"]` 带 `session_seq`（可用 v1 的 `/runs/{session}/stream` 续传）；AG-UI 本身没有重连机制，`/agui` 不提供续传端点 |
| 能力 | `GET /agui/capabilities` 返回 `AgentCapabilities`（流式、并行工具调用、推理、多代理委派、审批与中断） |

暂不支持（B7）：`RunAgentInput.tools`（前端工具）与改参审批（`approveWithEdits` 为 false）。`state` 与 `forwardedProps` 被忽略。

### 5.3 一致性验证

- 每个发出的事件都用官方 `spec/1.0/schema.json`（JSON Schema 2020-12，含 `unevaluatedProperties:false`）逐帧校验；
  schema 文件随测试放在 `protocol/agui/testdata/`（MIT，注明来源）。
- 流级规则按官方客户端的校验器检查：首帧 `RUN_STARTED`、终止帧之后无事件、结束前文本/推理/工具调用/子代理全部关闭、
  归属的子代理先开后用。
- 官方 TS 客户端 `@ag-ui/client` 1.0.3 的 `HttpAgent` 对真实服务端跑通：两次审批中断 + 两次 resume 后完成，客户端零告警、零错误。

### 5.4 Go API

| API | 说明 |
|---|---|
| `WithAGUI(AGUIConfig{ParkTimeout})` | AG-UI 剖面参数 |
| `protocol/agui` 包 | AG-UI 1.0 线上类型（与 schema 字段名一一对应），宿主自建传输时可直接用 |
| `ReasonApprover` / `permission.ReasonApprover` | 审批者给出拒绝原因（`PermissionHandler` 已实现） |
| `Event.MessageID` | 事件所属消息在会话记录中的 UID |

### 5.5 迁移建议

新项目或第三方前端（CopilotKit、AG-UI 生态的任意客户端）直接接 `/agui`；amobileCreater 桌面端与 AnimeCreater
继续用 v1 或进程内 API，不受影响。

---

## 6. 可观测与测试工具（B6）

三个新包，全部可选：`observer/oteltrace`（OpenTelemetry 导出）、`provider/scripted`（脚本化模型）、`httprr`（HTTP 录制回放）。
不用它们的宿主只受两处影响：`LLMObserver` 钩子配对修正（6.3）与 go.mod 新增的依赖（6.6）。

### 6.1 OpenTelemetry 导出（`observer/oteltrace`）

```go
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(otlpExporter)) // 宿主自己的 TracerProvider
app := goagent.New(..., goagent.WithObservers(oteltrace.New(tp, oteltrace.Options{
    AgentName:    "engine",    // gen_ai.agent.name，默认 "goagent"
    ProviderName: "anthropic", // gen_ai.provider.name，空则不设置
})))
```

span 名称与属性遵循 OpenTelemetry 生成式 AI 语义约定（`gen_ai.*`）：

| span | 何时 | 主要属性 |
|---|---|---|
| `invoke_agent <agent>` | 一次运行（会话的一条输入） | `gen_ai.operation.name`、`gen_ai.agent.name`、`gen_ai.conversation.id`（会话 ID）、`goagent.turns`、`goagent.cost_usd` |
| `chat <model>`（Client） | 一次模型请求 | `gen_ai.request.model`、`gen_ai.request.max_tokens`、`gen_ai.response.model`、`gen_ai.response.finish_reasons`、`gen_ai.usage.input_tokens`（含缓存部分）/`output_tokens`/`cache_read.input_tokens`/`cache_creation.input_tokens`；失败时 Error 状态与 `error.type`（`rate_limit`、`overloaded`、`prompt_too_long`、`cancelled`、`timeout`、`error`） |
| `execute_tool <tool>` | 一次工具调用 | `gen_ai.tool.name`、`gen_ai.tool.call.id`、`gen_ai.tool.type`、`goagent.duration_ms`；失败时 Error 状态与 `error.type=tool_error` |

- **层级**：`chat` 与 `execute_tool` 挂在 `invoke_agent` 下；子代理的 `chat`/`execute_tool` 挂在派发它的 `execute_tool Agent_*` 下
  （并带 `goagent.agent_path`）；后台子代理同样挂在派发处。运行 ctx 里已有 span（例如 HTTP 中间件建立的请求 span）时，
  `invoke_agent` 挂在它下面。
- **span 经 ctx 传递**：工具执行的 ctx 里带着它的 `execute_tool` span，工具内部经 ctx 发起的调用（otelhttp 包装的 HTTP 客户端、
  带追踪的数据库驱动、MCP 调用）自动成为它的子 span。
- **事件**：权限请求/授予/拒绝（`goagent.permission.*`）、压缩（`goagent.compaction`）、自定义事件（`goagent.custom_event`，
  值按 `MaxEventValueBytes` 截断，默认 1024）记在所在 span 上；用户中断记为 `goagent.cancelled` 事件，不算失败。
- 默认**不记录提示词与回复内容**（语义约定把内容采集定为可选项）。

### 6.2 新的可选观察者接口 `observer.ContextObserver`

```go
type ContextObserver interface {
    RunContext(ctx context.Context) context.Context  // OnSessionStart 之后调用，返回本次运行此后使用的 ctx
    ToolContext(ctx context.Context) context.Context // 工具执行前（权限检查之前）调用，返回这次工具执行使用的 ctx
}
```

观察者借它把自己的值（典型是追踪 span）放进循环随后使用的 ctx，模型请求、工具、子代理、工具内部的调用由此继承父子关系。
`MultiObserver`、`observer.IntoContext` 注入的观察者链都会转发。`oteltrace` 就是用它实现 6.1 的层级；自己对接其他追踪系统的宿主
可以照做。配套的 `executor.ToolCall.Context` 字段让直接使用 `executor` 包的代码为单个调用变换 ctx。

### 6.3 `LLMObserver` 钩子配对修正（行为变化）

此前有四条路径调用了 `OnLLMStart` 却不报告结束：主模型过载切换后备、请求被拒后响应式压缩重试、流中断后锚点恢复重开、
429 等待期间被中断。现在**每次 `OnLLMStart` 恰好对应一次 `OnLLMDone` 或 `OnLLMError`**，这几种情况各报告一次 `OnLLMError`
（它们本来就是失败的请求）。按 `OnLLMError` 统计失败率的宿主会看到这些此前漏掉的失败；按 `OnLLMStart` 开 span 的实现
不再有永不结束的 span。

### 6.4 脚本化模型（`provider/scripted`）

确定性地测试 agent 循环：每次模型请求按顺序消费一个 `Step`，并记录请求供断言。

```go
m := scripted.New(
    scripted.Tool("Read", map[string]any{"path": "a.go"}),
    scripted.Text("读完了，没问题。").WithUsage(120, 30),
)
m.Route(scripted.SystemContains("你是代码探索助手"), // 子代理与主 agent 共用一个模型时按请求分流
    scripted.Tool("Grep", map[string]any{"pattern": "TODO"}),
    scripted.Text("找到 3 处"))
app := goagent.New(goagent.WithProvider(m))
// ... 运行 ...
m.AssertDone(t) // 脚本恰好用完
```

| API | 说明 |
|---|---|
| `Step{Text, Thinking, ToolCalls, Err, StreamErr, Usage, Stop, ChunkSize, Check}` | 一次请求的应答：正文、思考、工具调用（ID 为空时自动生成 `call-<请求序号>-<i>`）、请求直接失败、流出正文后以流错误结束、用量、停止原因（默认按有无工具调用推断）、分块大小、请求断言 |
| `Text` / `Tool` / `Tools` / `Fail` | 构造常用的步骤 |
| `.WithUsage(in, out)` / `.WithThinking(s)` / `.WithChunkSize(n)` / `.Expect(fn)` | 链式补充 |
| `Route(matcher, steps...)` + `SystemContains` / `LastUserContains` | 按请求分流到不同脚本（先于默认脚本，按添加顺序匹配） |
| `Requests()` / `Remaining()` / `AssertDone(t)` / `WithModelID(id)` | 断言与配置 |

与 `provider.MockProvider` 的区别：脚本用完时返回错误（而不是重复最后一条），支持思考、分块、错误、按请求断言与分流。

### 6.5 HTTP 录制回放（`httprr`）

第一次对真实服务录制（模型提供方、MCP 服务器），之后在没有网络、没有密钥的环境里原样回放：

```go
rr, err := httprr.Open("testdata/weather.httprr", http.DefaultTransport)
if errors.Is(err, fs.ErrNotExist) {
    t.Skip("没有录制文件：设置 GOAGENT_HTTPRR=record 并提供密钥后录制")
}
defer rr.Close()
prov := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"), anthropic.WithHTTPClient(rr.Client()))
```

| 项 | 规则 |
|---|---|
| 模式 | 环境变量 `GOAGENT_HTTPRR`：`record`（或 `1`、`true`）录制全部文件；其他非空值作为正则匹配文件路径、只录制匹配的；未设置时回放。`OpenMode(path, httprr.Record/Replay, rt)` 显式指定 |
| 匹配 | 方法 + URL + 请求体精确匹配（**请求头不参与**：SDK 的 User-Agent、`X-Stainless-*` 随版本和机器变化） |
| 重复请求 | 同一请求录到多次时按录制顺序依次回放（先 429 后成功的重试）；用完或找不到时返回错误，错误里提示方法与 URL 相同的录制条数 |
| 清洗 | 录制前删除 `Authorization`、`X-Api-Key`、`Cookie`、`User-Agent`、`X-Stainless-*` 等请求头、URL 中的 `key`/`api_key` 参数与响应的 `Set-Cookie`；`ScrubReq` / `ScrubResp` 追加自定义清洗（同时作用于回放的匹配键，例如把请求体里的时间戳换成常量） |
| 文件 | 首行 `httprr trace v1`，每条记录一行 `<请求字节数> <响应字节数>` 后跟 HTTP/1.1 线格式的请求与响应（与 golang.org/x/oscar 的 httprr 同构）；录制时不发 `Accept-Encoding`，文件保持可读 |

配套新增：`anthropic.WithHTTPClient(*http.Client)`、`openai.Config.HTTPClient`（代理、追踪、录制回放都用它；不要设置
`http.Client.Timeout`，它覆盖整个流式响应）。

### 6.6 依赖

go.mod 新增 `go.opentelemetry.io/otel`、`otel/trace` v1.46.0（`oteltrace` 使用）与仅测试使用的 `otel/sdk` v1.46.0，
`golang.org/x/sys` 升到 v0.47.0。otel 固定在 v1.46.0：v1.47.0 要求 Go 1.26，而本模块保持 `go 1.25.0`。宿主升级后同样执行
`go mod tidy`（与 B4 的要求相同，做一次即可）。

### 6.7 迁移建议

- **amobileCreater engine**：需要链路追踪时加一个 `oteltrace.New(tp, ...)` 观察者即可得到运行 → 模型请求 / 工具 → 子代理的完整树；
  engine 的工具内部若经 ctx 发起 HTTP 调用，用 otelhttp 包装客户端就能挂到对应工具下。测试里手写的假 provider 可以换成
  `provider/scripted`。
- **AnimeCreater**：同上；若实现了 `LLMObserver` 并据 `OnLLMError` 计数，注意 6.3 的修正。
- 两个宿主都可以用 `httprr` 为关键对话流程录制回放测试（录制一次真实请求，CI 里离线回放）。

---

## 7. 扩展：前端工具、改参审批、MCP elicitation、MCP 服务端、可插拔分类器（B7）

### 7.1 AG-UI 前端工具（`RunAgentInput.tools`）

AG-UI 请求里的 `tools` 作为**前端执行的工具**注入本次运行（与已注册工具重名的忽略；名称须匹配 `^[A-Za-z0-9_-]{1,64}$`，
否则 `RUN_ERROR{code:"invalid_tools"}`）。流程：

1. 模型调用前端工具：流里照常有 `TOOL_CALL_START/ARGS/END`，内核在该工具处等待；本次 AG-UI 运行以
   `RUN_FINISHED{outcome:{type:"success", pendingToolCallIds:[...]}}` 结束（规范的写法：完成的运行留下待前端回答的调用）。
2. 客户端执行后，下一次请求的 `messages` 带上 `{role:"tool", toolCallId, content, error?}`：结果交给等待中的工具
   （`error` 非空时作为工具错误），新的流从停车处接着输出——不重发 `TOOL_CALL_START`，也不回显客户端自己给出的
   `TOOL_CALL_RESULT`，结束时的 `MESSAGES_SNAPSHOT` 含该 tool 消息。
3. 同一轮多个前端工具：可以分批回答，没回答的立即再以 `success + pendingToolCallIds` 结束。

| 情况 | 结果 |
|---|---|
| 有待回答的前端工具时发来既没有 `resume` 也没有对应 tool 消息的请求 | `RUN_ERROR{code:"pending_tool_calls"}` |
| 停车中的请求带了会话里没有的新 user 消息 | `RUN_ERROR{code:"thread_busy"}`（先作答再发新消息） |
| 停车超时（`AGUIConfig.ParkTimeout`，默认 30 分钟） | 前端工具得到"等待结果超时"的错误结果 |
| 审批与前端工具同时待处理 | 先以 `interrupt` 结束（规范里 interrupt 不能带 pendingToolCallIds），作答后再以 `success + pendingToolCallIds` 结束 |
| resume 只回答了部分中断 | 立即再次以 `interrupt` 结束，列出没回答的（此前会停在原处不再输出） |

`/agui/capabilities` 的 `tools.clientProvided` 现为 `true`。

#### v1 剖面的同款能力

v1 也可以声明前端工具（`POST /chat` 的 `tools` 字段，类型与 AG-UI 的声明一致），但**协议形态更直接**——v1 的流不断，模型调用时 `tool_start` 帧实时推给客户端，客户端执行完 `POST /chat/tool-result` 回传（`error` 非空 = 工具失败），等待中的运行即刻继续；不存在停车与续跑。配套 `GET /pending-tools?session_id=` 查未完成的调用（断线重连后补齐），等待结果上限 30 分钟，到点按失败结束。TS 客户端 0.5.0：`chat({tools})`、`toolResult(sessionId, toolCallId, content, error?)`、`pendingTools(sessionId)`。

### 7.2 改参审批

批准工具调用时可以**替换它的参数**（完整替换）：

| 入口 | 写法 |
|---|---|
| AG-UI | resume 的审批载荷 `{approved:true, editedArgs:{...}}`；审批中断的 `responseSchema` 声明了 `editedArgs`；`humanInTheLoop.approveWithEdits` 在审批者是 `PermissionHandler` 时为 `true` |
| v1 | `POST /approve` 新字段 `input`：`{request_id, session_id, allow:true, input:{...}}`；TS 客户端 0.4.0 `approve(id, true, {input})` |
| Go | `PermissionRequest.ApproveWithInput(input)`、`PermissionHandler.ResolveWithInput(sessionID, requestID, input)` |
| 自定义审批者 | 批准时调用 `permission.SetEditedInput(ctx, input)`（ctx 不支持改参时返回 false，应改为拒绝） |

修改后的参数按工具的入参 schema 校验，不合格时**不执行**并把原因作为工具结果交给模型；执行成功时结果末尾注明
"（用户在审批时修改了参数，实际执行的参数：…）"，让模型知道发生了什么。改参批准不记为"始终允许"。流中的
`tool_start` / `TOOL_CALL_ARGS` 仍是模型给出的原参数。

### 7.3 MCP elicitation ↔ ask_user

MCP 服务器在工具调用中向用户索取输入（`elicitation/create`）时，转成**触发它的会话里的一次提问**：

- `WithMCP` 连接的服务器默认开启（客户端声明 form 与 url 两种 elicitation 能力）。自行 `mcp.Connect` 的宿主设置
  `ServerConfig.Elicit = goagent.ElicitViaAskUser` 即可（工具须经 `MCPToolDef` 注册，才能定位会话）。
- 提问的 `payload`：`{kind:"mcp_elicitation", server, mode:"form"|"url", message, schema（form）, url、elicitation_id（url）, error（上次回答无效的原因）}`。
  客户端按 `kind` 渲染表单或链接。
- 回答写法：表单内容的 JSON 对象（接受）；`{"action":"decline"}` / `{"action":"cancel"}`；只有一个字段的表单可以直接答文本
  （按字段类型转换，布尔接受"是/否、yes/no"等）；url 模式答任意非空文本表示同意；空回答表示取消。不符合表单时附上原因
  重新提问（共 3 次）。
- 没有提问处理器、不在会话里、或同一客户端上几个调用同时进行且不属于同一会话（无法判断是谁触发的）时拒绝。

`mcp` 包新增：`ServerConfig.Elicit`、`ElicitHandler`、`Elicitation`、`ElicitResponse`、`ElicitAccept/Decline/Cancel`、
`WithElicitScope(ctx, scope)`（并发调用按范围定位；`MCPToolDef` 以会话 ID 为范围）。

### 7.4 把 GoAgent 暴露为 MCP 服务端

```go
// Streamable HTTP（挂在宿主自己的路由与鉴权之后）
h, err := app.MCPHandler(goagent.MCPServerOptions{
    Tools: []string{"search_docs", "create_ticket"},       // 只暴露列出的工具
    Agent: &goagent.MCPAgentTool{KeepSessions: true},      // 把 agent 本身暴露为 run_agent 工具
})
mux.Handle("/mcp", authMiddleware(h))

// stdio（MCP 客户端以子进程启动宿主）：stdout 是协议流，日志写 stderr
err := app.ServeMCPStdio(ctx, opts)

// 或拿到 SDK 的 *mcp.Server 自行选择传输、再添加提示词/资源
server, err := app.MCPServer(opts)
```

| 项 | 行为 |
|---|---|
| 暴露的工具 | 客户端直接调用，不经模型、不经审批者（MCP 客户端负责向它的用户确认，工具带 `readOnlyHint` / `destructiveHint` 注解）；入参按 schema 校验；**deny 规则、Guard 与权限模式照常生效**（规划模式、不询问模式、auto 模式的分类器）；`Context.Progress` 转成 MCP 进度通知；结果开头的内联图片标记转成 MCP 图片；每个 MCP 会话对应一个 GoAgent 会话（键值存储等），MCP 会话结束时释放；`WithSandbox` 照常生效 |
| agent 工具（默认名 `run_agent`） | 入参 `{task, session_id?}`；像 `RunSession` 一样跑完返回最终回复（`structuredContent: {text, session_id?}`）；客户端取消调用即中断运行；带进度令牌时工具调用与子代理进展以进度通知下发 |
| 会话 | `KeepSessions: true`（需要 `WithSessionManager`）：省略 `session_id` 时新建并在结果里返回，带上时延续；同一会话同时只能有一个调用。否则每次调用是一次独立运行 |
| 运行中的审批 | App 的审批者是 `PermissionHandler` 时经 **MCP elicitation** 交给客户端的用户确认（`{approve, reason?}`）；客户端不支持 elicitation 时拒绝并把原因交给模型；其他审批者（`AutoApprover`、自定义）照常由它们决定；未配置审批者时需要审批的调用被拒绝（启动时告警） |
| 运行中的提问 | 经 elicitation 交给客户端（`{answer}`）；不支持时回答"用户无法回答"。暴露 agent 时若 App 没有提问处理器会自动装一个（内置 AskUser 否则会去读 stdin——stdio 模式下那是协议流） |

### 7.5 可插拔分类器

```go
type Classifier interface {
    ClassifyCall(ctx context.Context, req ClassifyRequest) (Classification, error)
}
// ClassifyRequest{ToolName, Input, Level, Why, Transcript}；Classification{Behavior, Reason}
```

`WithPermissionClassifier(c)` 用自定义分类器替换默认的 YOLO 分类器（规则引擎、外部策略服务，或在 YOLO 之前加一道快速
判断的组合；`permission.ClassifierFunc` 让普通函数实现它）。调用时机不变：auto 模式判定每个非只读、非白名单的调用，
bypass 模式判定高风险调用与命中 ask 规则的调用。返回 `BehaviorAllow` 放行、`BehaviorDeny` 拒绝（原因交给模型）、
`BehaviorAsk` 或错误时改为询问审批者（没有审批者则拒绝）；每次调用 30 秒超时。设置后 `WithYoloClassifier*` 系列不再生效。

`permission.Gate.SetClassifier` 的参数类型从 `*YoloClassifier` 改为 `Classifier` 接口（`*YoloClassifier` 实现了它，
原有调用不用改；传入 nil 的 `*YoloClassifier` 等于不设分类器）。YOLO 的裁决来源仍标为 `yolo_classifier`，自定义分类器
为 `classifier`。

### 7.6 迁移建议

- **amobileCreater engine**：插件 MCP 用 `mcp.Connect` 自建连接，想让插件服务器的 elicitation 走桌面端的提问卡片，
  给 `ServerConfig` 加 `Elicit: goagent.ElicitViaAskUser`（工具已经经 `MCPToolDef` 注册）；桌面端按 `payload.kind ==
  "mcp_elicitation"` 渲染表单。审批卡片可以加"修改参数后批准"（`/approve` 的 `input`）。想把引擎能力提供给别的
  MCP 客户端时用 `MCPHandler` / `ServeMCPStdio`。
- **AnimeCreater**：不用 AG-UI / v1 线协议，主要可用的是 `WithPermissionClassifier`（接入自己的策略）与
  `PermissionRequest.ApproveWithInput`（自建审批 UI 的改参）。
- 不需要这些能力的宿主不用改任何代码。

---

## 8. 宿主迁移清单

### amobileCreater engine（go.mod `replace` 直连本仓库）

必做：

1. 在 engine 模块执行 `go mod tidy`（新依赖：MCP 官方 SDK、OpenTelemetry API，`golang.org/x/sys` 升版）。不执行时
   `go build` 报 go.mod 需要更新。已在 engine 的临时副本上验证：tidy 之后 `go build ./...`、`go vet ./...` 通过，未改动宿主文件。
2. 检查是否有代码按文本解析后台子代理的启动回执（多了 `agent_id`）。
3. 用 `WithSubAgents` 时会多出 `agent_send`、`agent_status` 两个工具；不想暴露给模型时用 `WithSessionToolFilter` 过滤。
4. 一轮里并行派发子代理超过 8 个的场景：`WithMaxSubagents(n)`。

可选（按收益排序）：

- 桌面端断线后的 3 秒轮询换成 `/runs/{session}/stream?after=<session_seq>`（第 1 节）。
- 塞进 `ask_user.payload` / `subagent_progress` 的领域语义改用自定义事件（第 2 节）。
- 子代理卡片加"发指示 / 停止"（`agent_id`，第 3 节）；审批卡片加"修改参数后批准"（`/approve` 的 `input`，第 7.2 节）。
- 插件 MCP：`client.OnToolsChanged` 同步工具列表（第 4 节）；`ServerConfig.Elicit = goagent.ElicitViaAskUser`（第 7.3 节）。
- 链路追踪：`oteltrace.New(tp, ...)`（第 6 节）。
- 桌面端 vendored 的 goagent-client 升到 0.4.0：新增 `resume()`、`autoResume`、`agents()`/`steerAgent()`/`interruptAgent()`、
  `approve(..., {input})`、`FrameType.Custom/Resync`；`agent_status` 新增 `cancelled`。

### AnimeCreater（固定在 ba8a515 伪版本）

1. 从 ba8a515 升级会依次经过 v0.12、v0.13 的行为变化：先读 [UPGRADE-v0.12.md](UPGRADE-v0.12.md)、[UPGRADE-v0.13.md](UPGRADE-v0.13.md)
   （符号层面安全：ba8a515 → v0.13 的导出 API 零删除）。
2. 升级后执行 `go mod tidy`。
3. 实现了 `LLMObserver` 的观察者：多个观察者时此前收不到的钩子现在会收到（第 0 节）；换后备、压缩重试、流中断恢复各报一次
   `OnLLMError`（第 6.3 节）。
4. 可选：自建事件总线的 30+ 种领域帧迁到自定义事件（`ctx.Emit` / `App.EmitEvent`，第 2 节）；自建取消改用 `InterruptAgent`
   （第 3 节）；自己的权限策略接 `WithPermissionClassifier`（第 7.5 节）；测试用 `provider/scripted` 与 `httprr`（第 6 节）。

### 直接使用 v1 线协议或 AG-UI 的其他客户端

- v1：未知帧类型与字段照常忽略即可；想要断线续传读 `session_seq`、`resync`（第 1 节）。
- AG-UI：`POST /agui` 是新端点，按 AG-UI 1.0 接入；前端工具与改参审批见第 7.1、7.2 节。

---

## 9. 验证情况

- `go build ./...`、`go vet ./...`、`go test -race ./...` 全部通过。
- B1 新增测试：`sessionlog_test.go`（日志顺序、条数与字节上限、resync、游标起点、关闭与重建续号、闲置释放）、
  `http_b1_test.go`（`/chat` 帧的 `session_seq`/`run_id`、断线续传、审批经日志到达续传连接、中断原因上线、
  空闲会话的续传端点与 `Last-Event-ID`、未开始的运行不进日志、进程内会话运行可被游标读取）。
  三处关键行为做过反向验证：运行结束帧先于会话释放、中断不带原因、审批不进日志时，对应测试均失败。
- B2 新增测试：`custom_b2_test.go`（工具内 `Emit` 到达 SDK 事件流与 `custom` 帧且不进会话记录、名称与值校验、
  schema 登记与 `/protocol` 列出、`EmitEvent` 的三种路由、子 agent 内发射的事件到达主运行、多观察者时可选钩子送达）。
  子 agent 转发与 `MultiObserver` 转发两处做过反向验证。
- B3 新增测试：`subagents_b3_test.go`（同步子代理登记与运行中指示、单独中断而父运行继续、并发上限、两种事件模式、
  未送达指示转交父运行、子代理内审批带归属与全部 HTTP 端点、后台子代理按 agent_id 查找与会话关闭时中断）。
  插话下沉、中断结果、全量转发、未送达转交、并发上限五处做过反向验证。
- B4：原有 5 个 MCP 测试（stdio 服务器穿插非 JSON 日志行与服务端 ping、HTTP 会话与 SSE 响应、翻页、名称净化、
  进程提前退出、超时）在 SDK 实现上全部通过；新增 `mcp/sdk_test.go`（进度、取消通知到达服务端、图片与结构化结果、
  工具列表变更）与 `mcptools_b4_test.go`（`WithMCP` 的进度状态行与注册表同步），四处做过反向验证。另用 go-sdk 自带的
  示例服务器（stdio）实连：握手 2025-11-25、schema 原样、调用成功、断开 2ms。engine 在临时副本上 `go mod tidy` 后编译通过
  （未改动宿主文件）。
- B5：`agui_b5_test.go` 全部帧经官方 schema 校验并做流级规则检查，覆盖运行与快照 id 对齐、历史去重、审批停车/批准/拒绝、
  提问停车、停车超时按拒绝（原因送达模型）、用户中断、模型错误、自定义事件/思考/子代理映射、能力描述；官方 `@ag-ui/client`
  1.0.3 实连 16 项检查全部通过。
- B6：`observer/oteltrace/oteltrace_test.go` 用官方 SDK 的内存记录器断言整棵 span 树——宿主 span → `invoke_agent` →
  `chat`/`execute_tool`，子代理的 `chat`/`execute_tool` 挂在 `execute_tool Agent_sub` 下（主运行与子代理用相同的调用 ID
  也不混淆），工具内部经 ctx 开的 span 挂在工具下，自定义事件记在工具 span 上，属性、用量、失败状态与 `error.type` 正确。
  `internal/loop/ctxobs_test.go` 覆盖 `ContextObserver` 的 ctx 可见范围与 LLM 钩子配对（反向验证：去掉修正后序列为
  `[start start start done]`，测试失败）。`httprr` 测试经真实的 OpenAI 与 Anthropic provider 录制两轮 agent 运行再离线换密钥回放，
  覆盖密钥/Cookie/UA/`X-Stainless-*`/URL key 清洗、同一请求的响应队列、多发与不匹配的报错、自定义清洗参与匹配、按环境变量选模式。
  `provider/scripted` 自带测试覆盖顺序、分块、自动 ID、分流、断言、失败与用完报错。
- B7：`permission/classifier_test.go`（自定义分类器的放行/拒绝/询问、出错与无效裁决改为询问、typed nil、bypass 只判定高风险、
  请求字段）与 `classifier_b7_test.go`（拒绝原因经工具结果到达模型）；`mcp/elicit_test.go`（form 与 url 两种模式、处理函数的 ctx
  带着触发调用的值、没有处理函数时不声明能力、并发调用按范围定位）与 `mcpelicit_b7_test.go`（`WithMCP` → 官方 SDK 服务器的
  elicitation → 会话提问，不合格回答附原因重问、单字段文本回答、回答解析表）；`mcpserver_b7_test.go`（暴露工具的注解、schema
  校验、deny 规则、图片、会话键值、进度通知；agent 工具的审批与提问经 elicitation、进度、延续会话、非法 session_id；客户端不支持
  elicitation 时的退路）；`agui_b7_test.go`（前端工具停车与接续、`pending_tool_calls`、并行前端工具分批作答、改参合法/不合法、
  能力描述、v1 `/approve` 的 `input` 且不记为始终允许）。三处做过反向验证（不回显客户端结果、改参传递、未回答的前端工具
  立即再停车），均被测试发现。官方 `@ag-ui/client` 1.0.3 实连前端工具往返 + editedArgs 审批 8 项全部通过、零告警；
  GoAgent 的 MCP 客户端以 stdio 子进程方式连接 `ServeMCPStdio` 实连 4 项通过（握手 2025-11-25、直接调用工具、交给 agent、
  断开 3ms）。engine 临时副本 `go mod tidy` 后 `go build ./...`、`go vet ./...` 通过。
- TS 客户端：0.3.0 对真实服务端做了 18 项冒烟：鉴权、`/protocol` 新条目、`chat({autoResume})`
  在第 3 帧后模拟断网仍得到完整且 `session_seq` 连续的流、`custom` 帧、`agent_path`、运行中 `agents()` 与
  `steerAgent()`（指示到达子代理的下一次请求）、`resume()` 的 `after=0` 与 `after=N`、`agent()`、对已结束子代理的 404。
  0.4.0 只给 `approve()` 加了 `input` 选项（`dist/` 已重新构建），对真实服务端 3 项冒烟通过（工具按替换后的参数执行并注明）。
