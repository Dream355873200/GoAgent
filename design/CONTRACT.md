# 线协议现状盘点（v1，2026-10-23）

> Phase 0 产出。来源：对框架源码（protocol/protocol.go、http.go、httpsecurity.go、event.go、streambus.go、各 *_handler.go、websocket/、sdk/typescript/）与两个宿主（amobileCreater desktop+engine、AnimeCreater 后端+前端）的逐文件审计。行号证据在调研原始记录中，本文只保留结论与关键位置。

## 一、结论先行

1. **v1 是一个真实在产的协议**：统一信封 `Envelope{seq, v, type, session_id, ...}`、`GET /protocol` 自描述、官方 TS 客户端 `goagent-client`。唯一可用的传输是 `POST /chat` 上的 SSE（只有 `data:` 行，15 秒一次 `: ping` 注释帧）。
2. **但协议文档和实际行为有 11 处不一致**（见第五节），其中两处是真缺陷：`seq` 在写锁外分配，并发写时线上顺序可能颠倒；会话忙且未开插话时，第二个请求会把正在运行的流的审批/提问通道顶掉。
3. **两个宿主的耦合形态完全不同**：amobileCreater 桌面端直接解析 v1 线格式（经 vendored 的 goagent-client）；AnimeCreater 根本不碰 GoAgent 的线格式——它在进程内读 `goagent.Event` 结构体，再重新编码成自己的 WebSocket 协议。
4. **两个宿主绕开的是同一组缺口**：没有类型化的自定义事件 API、`tool_is_error` 不上线、提问/中断与 `/chat` 连接绑死、断线后无法重新挂接正在运行的流。
5. **websocket/ 包是一个未接线的坏脚手架**（只回显确认，从不调用 App），可以删除。
6. **迁移代价**：在 v1 不变的前提下新增 AG-UI 剖面，两个宿主零成本；改动 v1 本身只会冲击 amobileCreater 桌面端。

## 二、帧类型（21 个 Type* 常量）

15 个来自循环/App 事件（经 `envelopeFromEvent` 转换，http.go:1088-1114）；`run_start`、`metadata`、`permission_request`、`ask_user`、`plan_confirm`、插话确认由传输层自行构造。

| 线上类型 | 来源 | 载荷字段 | 备注 |
|---|---|---|---|
| run_start | 传输层 | — | 只在正常路径；插话/排队/忙恢复路径没有 |
| text_delta | EvtTextDelta | text | |
| thinking | EvtThinking | thinking | |
| tool_start | EvtToolStart | tool_name, tool_input, tool_use_id | |
| tool_done | EvtToolDone | tool_name, tool_result, tool_use_id | **ToolIsError 丢失** |
| permission_request | 传输层（PermissionHandler 总线） | request_id, tool_name, tool_input, permission | 审批者必须是 `*PermissionHandler` 才发 |
| usage | EvtUsageUpdate | usage{input_tokens, output_tokens} | 缓存 token 丢失 |
| turn_complete | EvtTurnComplete | — | 文档说附带 usage，实际从不附带 |
| done | EvtDone | — | 一个流里可以出现多次（排队续跑） |
| error | EvtError | error | |
| progress | EvtProgress | text, status_key? | status_key 取值：suspend / budget / 429 重试键 / tool-progress[:id] |
| compaction | EvtCompaction | text | |
| ask_user | 传输层（AskUserHandler 总线） | request_id, question, payload | payload 是不透明对象，框架内无人设置 |
| plan_confirm | 传输层 | request_id, plan_content | **永远不会触发**（无人创建请求） |
| interrupt | EventInterrupt | — | **永远不会触发** |
| interrupted | EvtInterrupted | text（恒为"用户中断"） | 原因字段被丢弃 |
| retrieval | EventRetrieval | text | |
| steer | 传输层确认 / EvtSteer | text | 两种来源文本不同：确认帧是原文，循环帧包了 `<system-reminder>` |
| queue_run | EventQueueRun | text | |
| subagent_progress | EvtSubAgentProgress | agent_id/desc/status/activity/tool_uses/tokens | |
| metadata | 传输层 | elapsed_ms / steered / queued | 标记**流**结束而非**运行**结束 |

在进程内存在、但从不上线的：EventNeedApproval、EventAskUser、EventPlanConfirm、EventInterrupt 四种类型；ToolIsError、Messages、InterruptReason、缓存 token 等字段。

## 三、流生命周期（POST /chat）

- `seq`：**每个 HTTP 连接**一个计数器，从 1 开始，跨越这个流里的所有运行；不是按运行或按会话计。
- 准入（会话已在运行时）：

| 条件 | 帧序列 |
|---|---|
| 开了插话且插入成功 | steer（原文）、metadata{steered} |
| 开了插话但不可插入 | metadata{queued, text=排队项 ID} |
| 未开插话 | run_start、error（会话正在运行中）、metadata；**同时把正在运行的流的审批/提问订阅顶掉** |

- 正常流：`run_start` → 每轮 `[retrieval?] (usage, 增量, 工具帧, steer?, turn_complete)*` → `done|error|interrupted` → 有排队则 `queue_run` 加新一轮 → `metadata{elapsed_ms}`。
- 客户端断开：运行在后台继续（`context.WithoutCancel`），但交互订阅仍绑在死连接上，期间的审批/提问帧丢失。
- 追赶：**没有重新挂接端点**，`Last-Event-ID` 只出现在 CORS 允许列表里、从未读取。客户端只能轮询 `/sessions`、`/sessions/{id}/messages`（完整原始记录，无游标，只有已完成消息）、`/pending-ask`（只返回最新一条）、`/pending-approval`。
- 审批与提问**没有超时**，无人应答就一直阻塞到中断。

## 四、端点（35 个内置路由）

对话与交互：`POST /chat`（SSE）、`POST /execute`、`POST /approve`、`POST /askuser`、`GET /pending-ask`、`GET /pending-approval`、`POST /plan/confirm`（实际恒返回 404）、`POST /interrupt`、`POST|GET /queue`、`POST /queue/remove`。
运行时控制：`GET|POST /mode`、`GET|POST /thinking`、`GET|POST /model`。
资源：`/health`、`/tools`、`/tasks*`、`/plan`、`/bgtasks*`、`/sessions`、`/sessions/{id}/messages`、`/usage`、`/audit`、`/protocol`。

安全层（httpsecurity.go）：配置 `WithHTTPAuthToken` 时接受 `Authorization: Bearer` / `X-GoAgent-Token` / `?token=`，常数时间比较，覆盖全部路由；未配置时只绑回环、校验 Host、拒绝跨站 Origin；请求体上限 32 MiB。

`GET /protocol` 的自描述漏了 10 个路由和全部宿主路由，并且声称 `plan_confirm` 可用。

## 五、文档与行为的不一致（11 处）

| # | 不一致 | 严重度 |
|---|---|---|
| 1 | `seq` 在写锁外分配，四个 goroutine 并发写，线上顺序可能颠倒；"严格递增"的承诺不成立 | **缺陷** |
| 2 | 会话忙且未开插话时，第二个请求顶掉运行中流的审批/提问订阅，该运行之后的提问只能经 `/pending-*` 看到 | **缺陷** |
| 3 | `ToolIsError` 不上线，宿主用正则猜工具是否失败 | 缺口 |
| 4 | "首帧是 run_start" 在插话/排队/忙恢复路径不成立 | 文档 |
| 5 | `turn_complete` 附带 usage 从未实现 | 文档 |
| 6 | `interrupted` 的原因、`/approve` 与 `/interrupt` 与 `/plan/confirm` 的 reason 字段全部被丢弃 | 缺口 |
| 7 | `plan_confirm`、`interrupt` 帧永远不会出现 | 死代码 |
| 8 | 同一插话的两种帧文本不同（原文 vs reminder 包装） | 文档 |
| 9 | 无会话的 `/execute` 运行，审批会发到"恰好唯一"的已绑定连接 | 缺陷（低频） |
| 10 | 内置 AskUser 在未调用 `SetAskUserHandler` 时回退到**读服务器 stdin** | 缺陷（HTTP 模式） |
| 11 | `/sessions/{id}/messages` 注释说只含 user/assistant，实际含工具结果、图片、签名、插话提醒 | 文档 |

## 六、TS 客户端（goagent-client 0.1.0）

- 不处理 seq 跳号、不做版本校验、不带鉴权头。
- `steer()` 有缺陷：POST 到 `/chat` 后对 SSE 响应调用 `res.json()`，必然抛错；会话空闲时还会意外开一整轮。
- `sessions()` 的类型与服务端实际返回不符；`Envelope` 缺 `queued`；`FrameType`/`PROTOCOL_VERSION` 未导出；`dist/` 产物早于源码最后一次修改。
- package.json 把 typescript 列为运行时依赖，与"零依赖"的描述矛盾。

## 七、宿主依赖面

### amobileCreater（engine 用 RunHTTP + 桌面端直连 v1）

- 线格式只用于 `POST /chat`；约 20 个 GoAgent REST 端点和约 20 个宿主扩展端点经 Electron IPC 转发。
- 桌面端读取的帧：text_delta、thinking、tool_start/done、subagent_progress、permission_request、ask_user（含 `payload.kind="confirm"` 等确认卡语义）、steer、queue_run、progress（无 status_key 的帧被丢弃）、compaction、error、interrupted、usage、run_start。
- 忽略：metadata、seq、v、`/protocol`。断线后每 3 秒轮询，用 `/sessions/{id}/messages` 全量重建。
- **领域语义被塞进 v1 帧**：确认卡走 `ask_user.payload`；流水线节点、团队成员、后台代理都复用 `subagent_progress`；后台任务完成靠正则解析 `<system-reminder>` 文本；工具失败靠正则 `/退出码非零|失败|错误/`。
- 一处实际 bug：桌面端认为节点失败状态是 `'error'`，框架发的是 `'failed'`，失败节点永远不会显示为错误。

### AnimeCreater（进程内读 Event，自有 WebSocket 协议）

- 后端 `ServerEvent` 镜像了 v1 的字段命名，另加 `data`、`agent`、`ts`、`options`、`tool_error`；前端 `agentProtocol.ts` 声明"对齐 goagent Envelope"。
- **自建了 30+ 种领域帧**（canvas_command、storyboard_cards、agent_start/progress/complete、quality_*、parsed_*、pipeline_complete……），经自建的 ctx 事件总线直接写入运行流，绕过框架——原因是框架唯一的公开出口只有 `ctx.Progress(string)` 和 `EmitSubAgentProgress`。
- 自建了按运行计的 seq + 500 帧重放缓冲 + `reconnect{last_event_index}` 断线重连、自建提问路由（因为 `SetAskUserCallbackCtx` 是进程级单槽）、自建取消。
- 审批事件 `EventNeedApproval` 直接自动批准。

### 迁移暴露面

| 改动 | amobileCreater | AnimeCreater |
|---|---|---|
| 新增 AG-UI 剖面，v1 不变 | 无影响 | 无影响 |
| v1 新增可选字段（如 tool_is_error） | 无影响，且可删掉正则 | 无影响 |
| v1 改字段语义/删字段 | **高**：桌面端解析层 + 手工重新 vendor | 无（不用线格式） |
| `goagent.Event` 结构体改形 | 编译期破坏（engine 用 replace 直接跟随） | 编译期破坏 |

## 八、对设计的直接输入

- v1 冻结并长期支持，只做**加字段**（不升版本）与**修缺陷**。
- 第一批加字段：`tool_is_error`、`interrupted` 的原因、`usage` 缓存 token。
- 第一批修缺陷：seq 在写锁内分配；忙会话不顶掉运行中流的订阅；HTTP 模式下内置 AskUser 不回退 stdin。
- 自定义事件 API 是两个宿主都在绕的缺口，优先级最高。
- 断线重新挂接（按 seq 续传）是两个宿主都自建过的能力，应由框架提供。
- `websocket/` 包删除；`plan_confirm` 与 `interrupt` 帧在文档中标为保留未用。
