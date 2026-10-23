# 生态调研纪要与决策表（Phase 0 产出，第二版）

> 2026-10-23 第二版。第一版是摘要级调研，部分结论有误；第二版基于 10 路并行的源码级审计重写（8 个外部仓库 + GoAgent 自身线协议 + 两个宿主）。文件路径与行号证据保留在审计原始记录中，本文只保留结论。
> 关联文档：CONTRACT.md（v1 现状）、PROTOCOL.md（协议设计）、SUBAGENTS.md（子代理设计）、ROADMAP-v0.14.md（实施计划）。

## 一、第一版的更正

| 第一版说法 | 更正 |
|---|---|
| GoAgent 的事件是"隐式契约，没有版本、schema、客户端" | **错**。已有正式 v1 协议（protocol/ 包、统一信封、seq、`GET /protocol`、官方 TS 客户端），但文档与行为有 11 处不一致（见 CONTRACT.md） |
| Go 生态没有前端事件协议 | **不准确**。ADK-Go 有 SSE 线格式（`/run_sse`，独立的 camelCase 线类型，非 session.Event 直出）和 A2A 服务端；Genkit 有 flow 的 SSE 格式（`{message}`/`{result}`/`{error}` 帧，可重连流）。准确说法是：**没有 Go 框架实现了一个可被陌生前端消费的标准前端协议**——ADK 与 Genkit 的格式都是私有的 |
| MCP Go SDK 是 v1.1.0 | 本地仓库 ROADMAP 写明 **v1.8.0**（ADK-Go 的 go.mod 也依赖 v1.8.0），支持规范 2024-11-05 至 2026-07-28 |
| AG-UI 社区 Go SDK 可作依赖 | **不建议**。手写类型，落后 1.0 规范（缺 `cancelled` outcome、`pendingToolCallIds`、`RUN_STARTED.protocolVersion`，ToolCallResult 只支持字符串，错误事件发成 CUSTOM）。应从 `spec/1.0/schema.json` 自行生成类型 |
| AG-UI 扩展字段可放 metadata 或顶层 | **只能放 metadata 或 CUSTOM**。规范中每个事件对象都是封闭的（`unevaluatedProperties:false`），官方 TS 客户端在执行阶段剥离未知顶层字段并告警 |

## 二、各家要点

### AG-UI 1.0（MIT；16.8k stars；`@ag-ui/core` 周下载约 169 万）

- **31 个事件类型**，信封只有 `type`（必填）、`timestamp`、`rawEvent`、`metadata`，可归属的事件另有 `subagentRunId`。
- **允许并发**：多条文本消息、多个工具调用、多个子代理可以同时打开、任意交错，工具调用可以在文本消息未结束时打开。前提是使用显式的 START/…/END 三元组——CHUNK 简写在同一车道里遇到新 id 会关闭前一个。
- **校验规则**：首帧必须是 RUN_STARTED；RUN_FINISHED 前必须关闭全部打开项；RUN_ERROR 之后不得再发事件；同一运行内子代理 ID 不可复用。
- **TOOL_CALL_START 必须带 parentMessageId**，否则客户端为每个调用新建一条助手消息。
- **历史**：客户端每次运行都发送完整 `messages`；服务端拥有历史的做法（LangGraph、ADK、Claude Agent SDK 集成）是按 id 去重并用 MESSAGES_SNAPSHOT 对齐。
- **人机协同是两次运行**：`RUN_FINISHED{outcome:interrupt, interrupts:[{id, reason, toolCallId, responseSchema, expiresAt}]}` → 下次运行带 `resume:[{interruptId, status, payload}]`，且必须覆盖全部待处理中断。审批推荐负载 `{approved, editedArgs?}`，拒绝写在负载里而不是 status；恢复后不重发 TOOL_CALL_START，直接对原 toolCallId 发 RESULT。
- **前端工具**：运行以 `success + pendingToolCallIds` 结束，下次运行以 `role:"tool"` 消息带回结果。
- **没有**：运行中客户端输入通道（插话）、取消端点（客户端断开请求即取消）、续传机制（不用 Last-Event-ID）、能力交换端点、面向服务端的一致性测试工具（68 个流式用例只测客户端）。
- 草案 1.1 只新增一个可选字段 `encryptedValueType`；规则是小版本只加不改。

### AG-UI 集成的共同模式与痛点

- **模式**：每次运行一个有状态的翻译器，跟踪打开项并在边界前强制关闭；恰好一个终止事件；未流式输出的工具调用在结果到达时补发 START/ARGS/END；服务端拥有历史时按 id 去重。
- **痛点**：老客户端兼容迫使新特性默认关闭（LangGraph 的子代理事件默认关闭，因为旧客户端遇到 SUBAGENT_STARTED 会断流）；流式事件、快照、客户端回传之间的 id 一致性；聚合的"最终"事件导致重复；**几乎所有集成一次只支持一个待处理中断**（Claude、Mastra、CrewAI 都把审批串行化）。
- **规范不够用的地方**（各集成自行发明）：`PredictState`、`on_interrupt` 自定义事件、`exit`/`Exit`、合成的 `confirm_changes` 工具、按框架命名的 metadata 命名空间。只有 LangGraph 发 SUBAGENT_* 事件，且需手动开启。
- **A2A 中间件没有把任务状态映射到 AG-UI 事件**——状态消息变成文本，任务变成 RAW。

### ADK-Go v2（Apache-2.0；Go 1.26；最接近的竞品）

- 一切基于 genai 类型；运行流是 `iter.Seq2[*session.Event, error]`；v2 用图引擎执行代理。
- **人机协同是函数调用协议**：合成 `adk_request_confirmation` / `adk_request_input` 调用 + LongRunningToolIDs，客户端用同 ID 的 FunctionResponse 恢复；运行状态不持久化，每轮从会话事件重建。
- **三种子代理路径**：transfer（同会话交接）、agenttool（新会话、父阻塞、**子事件全部丢弃**、子状态不回写）、v2 task 模式（隔离作用域，事件转发）。
- **非 Live 运行没有任何插话通道**；Live 只支持 Gemini。
- OTel `gen_ai.*` 覆盖完整（span：invoke_agent / generate_content / execute_tool / compact_events）；压缩有滑动窗口与尾部保留两种策略。
- 缺口：无评测（REST 评测路由返回 501）、planner/代码执行/鉴权处理器是空桩、无上下文缓存管理、`agent.Agent` 是封闭接口（外部包无法实现）、无原生 Anthropic 提供方。

### OpenAI Agents SDK v0.24（Python，MIT）

- 人机协同完整：`needs_approval` → `RunResult.interruptions` → `RunState.to_json()`（schema 版本 "1.21"，已演进 22 版）→ approve/reject（可设"始终"）→ `Runner.run(agent, state)` 恢复。
- `as_tool` 嵌套运行的审批会冒泡为外层运行的中断——代价是嵌套状态存在进程级全局字典里（按 `id(tool_call)` 为键）。
- 子事件只能经 `on_stream` 回调，不进外层流；`ToolApprovalItem` 从不作为流事件发出。
- 默认 max_turns=10；工具失败默认给模型一句固定的通用错误文本。
- 测试工具值得借鉴：`ScriptedModel`（FIFO 脚本步骤，125 个测试文件在用）。
- 没有服务端层、没有 A2A、没有内置 OTel。

### Eino（Apache-2.0）

- **中断与恢复是最强项**：代理、图节点、工具三个层级都能中断，中断信号形成地址树（`agent:x;tool:y:callID`），可按中断 ID 定向恢复并附带数据；检查点用 gob 编码（任何 `any` 背后的具体类型都要注册）。
- 代理即工具在 `EmitInternalEvents` 下把子事件带 RunPath 前缀并入父流，但不进父历史、不能驱动父的控制流——这是子事件转发的好范式。
- DeepAgent 预置：`task` 工具（等同 Claude Code 的 Task）、todo、文件系统中间件。
- 缺口：工具出错默认终止整个运行；事件通道无界、无背压；mock 在 internal/ 下外部不可用；无服务端层；无 OTel；README 示例无法编译。

### Genkit Go

- 工具中断：`tool.Interrupt` → 生成以 `FinishReason "interrupted"` 返回 → `call.Restart/Respond` + `WithResume` 恢复；同轮其他工具照常完成，输出暂存以免恢复时重跑。
- 中间件三钩子：`WrapGenerate` / `WrapModel` / `WrapTool`；内置 ToolApproval（白名单 + 可选 LLM 裁判）、Budget、ContextCompression、Retry、Fallback。
- Dev UI 的反射 API 有文档（v1 HTTP、v2 WebSocket JSON-RPC），第三方运行时理论上可以接入。
- 预览版代理运行时：会话存储、快照、分离运行与中止；**子代理中断被压平为普通工具文本**。
- A2UI 是声明式 UI 载荷格式，与 AG-UI（事件传输）无关。

### MCP Go SDK v1.8.0（MIT 与 Apache-2.0 混合，新代码为 Apache-2.0）

- 客户端与服务端齐全：命令行/stdio、Streamable HTTP（含 Last-Event-ID 重连）、旧版 SSE；进度、取消、list_changed、elicitation、sampling、roots、logging、keepalive、OAuth、MRTR。
- **GoAgent 自研 mcp/ 包的缺口**：协议版本 2025-03-26 且不校验协商结果；发送空能力；丢弃全部通知；**取消只是停止等待，从不通知服务端**（服务端继续执行）；图片变成文字占位符（框架其实支持 `[IMAGE]` 标记）；丢弃结构化输出与注解；HTTP 关闭可能挂住；无重连、无 OAuth。
- engine 宿主自建了连接中心与重连，并有一处 bug：前缀拼接未做名称清洗，服务器名含特殊字符时重连会把自己的工具当成冲突跳过。
- 迁移只换内部、保留门面（`ServerConfig`、`Connect`、`Client.Disconnect/ServerInfo`、`MCPToolDef`——engine 只用这些）。风险：SDK 先尝试 `server/discover`，老服务器可能卡到超时（可按服务器固定协议版本规避）；旧协议下 elicitation 拿不到工具调用的 ctx。

### Anthropic SDK（服务端上下文工程）

- **beta 中**：`context_management.edits`（`clear_tool_uses_20250919`、`clear_thinking_20251015`、`compact_20260112`）、显式 `compaction` 请求、工具搜索与 `defer_loading`、对话中途增删工具（不破坏提示缓存）、缓存未命中诊断、`task_budget`。正式版参数里都没有。
- 对 GoAgent 的含义：提供方层要能**原样回传不透明块**（压缩块的 signature/encrypted_content、带签名的 thinking），解析 `applied_edits` 与 `usage.iterations`，并在轮次之间提供改参钩子。

### LangChainGo（MIT）

- 代理是老式的（两个靠正则解析模型文本，函数代理用已废弃的 WithFunctions 且所有参数挤进一个字符串）、工具只收发字符串、工具出错即终止；无多代理、无人机协同、无 MCP、无服务端、无追踪。
- **值得借鉴的只有 httprr**：HTTP 录制回放传输，按清洗后的请求精确匹配，适合确定性地测试多轮代理循环（Go 移植时应改为每个键保留响应队列，以支持"同一请求不同响应"的重试场景）。

### A2A（经 ADK-Go 依赖的 a2a-go v2.6.0 侧证）

- 任务状态：SUBMITTED / WORKING / INPUT_REQUIRED / AUTH_REQUIRED / COMPLETED / CANCELED / FAILED / REJECTED（终态为后四者）；事件：TaskStatusUpdateEvent、TaskArtifactUpdateEvent（append/lastChunk）。
- ADK 的映射：长时工具调用 → INPUT_REQUIRED；事件错误 → FAILED；运行结束 → COMPLETED。A2A 1.0 用 JSON-RPC，另有 0.3 兼容端点。

## 三、竞争格局

| 维度 | GoAgent | ADK-Go | Eino | Genkit Go | OpenAI SDK |
|---|---|---|---|---|---|
| 前端协议 | **v1 私有 + 规划 AG-UI** | 私有 SSE | 无 | 私有 SSE | 无 |
| 运行中插话 | **有（顶层）**，规划下沉到子代理 | 仅 Gemini Live | 无 | 无 | 无 |
| 子代理事件并入父流 | 规划中 | 仅 v2 task 模式 | 有（opt-in） | 无 | 仅回调 |
| 人机协同 | 阻塞式审批 + 停车（规划） | 函数调用协议 | 地址树定向恢复 | 工具中断 | RunState 序列化 |
| 多提供方 | OpenAI 兼容 + Anthropic | Gemini + OpenAI（实验） | 生态仓库 | 多插件 | OpenAI + LiteLLM |
| MCP | 自研客户端（缺口多） | 官方 SDK 客户端 | 无 | mcp-go 客户端+服务端 | 三种传输 |
| OTel | 无 | 完整 gen_ai.* | 无 | genkit:* + 可选 gen_ai.* | 无（私有追踪） |
| 评测 | benchmark 包 | 无（501） | 生态仓库 | 弱（正则/相等） | 无 |
| 断线续传 | 无（规划） | 无 | 无 | 有（流 ID） | 无 |

**差异化位置**：运行中插话（四家都没有，GoAgent 已有顶层能力）+ 标准前端协议（AG-UI，Go 生态无人实现）+ 断线续传（只有 Genkit 有私有实现）。
**明显短板**：OTel（ADK 完整）、MCP 成熟度（官方 SDK）、测试工具（ScriptedModel / httprr 式录制回放）、许可证（**仓库没有 LICENSE 文件**，README 声称 MIT 但链接失效）。

## 四、决策表

| # | 决策 | 结论 | 文档 |
|---|---|---|---|
| D1 | 前端协议 | v1 冻结并修缺陷 + 新增 AG-UI 1.0 剖面；内核不 import 协议类型 | PROTOCOL.md |
| D2 | 扩展位置 | AG-UI 剖面只用 CUSTOM 与 metadata（带 `goagent.` 前缀），不加顶层字段 | PROTOCOL.md §3 |
| D3 | AG-UI 类型来源 | 从 spec/1.0/schema.json 生成，自持；不依赖社区 Go SDK | PROTOCOL.md §3.5 |
| D4 | 人机协同 | 审批停车桥接 AG-UI 的两次运行模型；负载 `{approved, editedArgs?}` | PROTOCOL.md §3.3 |
| D5 | 自定义事件 | `ctx.Emit` / `App.EmitEvent` / 可选 schema 注册 | PROTOCOL.md §4 |
| D6 | 续传 | 会话事件总线 + 环形缓冲 + `/runs/{session}/stream?after=` | PROTOCOL.md §5 |
| D7 | 子代理 | 以 tool_use_id 寻址的统一句柄；插话下沉；子事件带路径并入父流（借鉴 Eino） | SUBAGENTS.md |
| D8 | handoff | 不引入 | SUBAGENTS.md §4 |
| D9 | MCP | mcp/ 内部换成官方 SDK v1.8.0，保留门面；随后接进度、取消、图片、elicitation | ROADMAP-v0.14.md |
| D10 | 观测 | 新增 OTel 导出，span 与属性对齐 gen_ai.* 语义约定（参照 ADK-Go 的实现） | ROADMAP-v0.14.md |
| D11 | 测试工具 | 公开脚本化模型（参照 ScriptedModel）+ httprr 式录制回放 | ROADMAP-v0.14.md |
| D12 | 许可证 | MIT，LICENSE 已补（作者确认） | — |
