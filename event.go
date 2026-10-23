package goagent

import (
	"encoding/json"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// EventType categorizes events emitted by the agent loop.
type EventType int

const (
	// EventTextDelta — incremental text from the LLM.
	EventTextDelta EventType = iota

	// EventThinking — incremental thinking/reasoning from the LLM.
	// 模型的推理过程。前端可选择展示或折叠。
	EventThinking

	// EventToolStart — a tool is about to execute.
	EventToolStart

	// EventToolDone — a tool has finished executing.
	EventToolDone

	// EventNeedApproval — the framework is asking the user to approve a tool call.
	// In SDK mode, the caller can respond via the Approve/Deny callbacks on the event.
	EventNeedApproval

	// EventUsageUpdate — token usage information.
	EventUsageUpdate

	// EventTurnComplete — one turn of the agent loop completed.
	EventTurnComplete

	// EventDone — the agent session has completed successfully.
	EventDone

	// EventError — an error occurred.
	EventError

	// EventProgress — a tool is reporting intermediate progress.
	EventProgress

	// EventCompaction — context compression happened (informational).
	EventCompaction

	// EventSubAgentProgress — 子 agent 运行进度更新。
	// 用于在 TUI 中显示并发子 agent 的树形状态。
	// AgentID + ToolName(agent 描述) + ToolInput(活动描述) + Usage(token 计数)
	EventSubAgentProgress

	// EventAskUser — 向用户提问，等待回答。
	// 前端收到此事件后，应显示问题并通过 /askuser 端点响应。
	EventAskUser

	// EventPlanConfirm — 请求用户确认计划。
	// 前端收到此事件后，应显示计划内容并通过 /plan/confirm 端点响应。
	EventPlanConfirm

	// EventInterrupt — 请求中断当前执行。
	// 前端收到此事件后，可以选择继续或取消。
	EventInterrupt

	// EventInterrupted — 用户主动终止了当前执行（POST /interrupt 等）。
	// 与 EventError 分离：provider 超时/工具崩溃走 Error，用户主动停止走
	// 本事件——前端直接渲染「已停止」而非报错样式，无需 errors.Is 判断。
	// Text 携带可读原因。
	EventInterrupted

	// EventRetrieval — RAG 前置检索完成（WithRetrieval 形态 1）。
	// Text 携带摘要（query 前 60 字符 / 命中条数 / 注入字符数），
	// 供宿主观测检索开销与命中情况。信息性事件，无需前端反应。
	EventRetrieval

	// EventSteer — guide 车道消息已注入模型上下文（WithSteering）。
	// Text 携带注入的内容，前端渲染为提示行。
	// 注入时机：工具批结束边界追加进当前 run。
	// 车道语义由宿主决定：可以是用户插话，也可以是宿主/环境通知
	// （本引擎仅后者——用户输入走 queue 车道）。
	EventSteer

	// EventQueueRun — queue 车道消费：排队消息作为新一轮输入开跑
	// （WithSteering，run 结束后自动续跑，新 run 开始前发出）。
	// 与 EventSteer 分离：排队消息是用户真实输入（渲染为普通用户气泡、
	// 开新一轮），插话是运行中注入（渲染带插话标记、归属当前轮）。
	EventQueueRun

	// EventCustom — 自定义事件（Emit / Context.Emit / App.EmitEvent）。
	// CustomName 是事件名（<命名空间>.<事件名>），CustomValue 是 JSON 值。
	// 展示层信号：不进入会话记录，进入会话事件日志。
	EventCustom
)

// Event is emitted by the agent loop for the consumer to react to.
type Event struct {
	// Type identifies what kind of event this is.
	Type EventType

	// Text is set for EventTextDelta and EventProgress.
	Text string

	// Thinking is set for EventThinking — 模型的思考过程增量。
	Thinking string

	// ToolName is set for EventToolStart, EventToolDone, and EventNeedApproval.
	ToolName string

	// ToolInput is set for EventToolStart and EventNeedApproval (JSON).
	ToolInput json.RawMessage

	// ToolUseID is set for EventToolStart and EventToolDone.
	// 对齐 Claude Code 的 ToolUseBlock.id，用于前端关联 tool_start 和 tool_result。
	ToolUseID string

	// ToolResult is set for EventToolDone.
	ToolResult string

	// ToolIsError is set for EventToolDone: the tool finished with an error and
	// ToolResult carries the error message (lets UIs render failure states
	// without guessing from the text).
	ToolIsError bool

	// StatusKey 非空时本事件是「状态行更新」而非追加消息：客户端按 Key
	// 原地替换显示（如 429 重试的倒计时行「✻ 429 … Retrying in Xs ·
	// attempt N/10」）。Text 为空 = 清除该状态行。
	// 对齐 Claude Code 的 spinner/status 行为。
	StatusKey string

	// Usage is set for EventUsageUpdate.
	Usage *provider.Usage

	// Error is set for EventError.
	Error error

	// Messages 在 EventDone 时携带完整对话记录：传入的历史、本轮进入对话的
	// 全部消息（包含 thinking 内容），以及自动摘要时追加的摘要记录——压缩
	// 不改写它。业务层持久化这份记录、下一轮作为 history 传回即可：摘要记录
	// 让循环直接还原模型视图，不会再请求模型摘要一遍。摘要记录是元消息
	// （IsMeta），展示时与其他元消息一样过滤。仅在 EventDone 事件中设置，
	// 其他事件为 nil。
	Messages []message.Message

	// Approve/Deny are set for EventNeedApproval in SDK mode.
	// Call Approve() to allow the tool to run, Deny(reason) to reject.
	Approve func()
	Deny    func(reason string)

	// SubAgent 字段 — 用于 EventSubAgentProgress。
	// AgentID 是子 agent 的唯一标识（对应 ToolUseID）。
	AgentID string
	// AgentDesc 是子 agent 的描述（如 "Explore reference prompts"）。
	AgentDesc string
	// AgentStatus 是子 agent 的状态："running"/"done"。
	AgentStatus string
	// AgentActivity 是子 agent 当前正在做什么（如 "Searching for 1 pattern, reading 12 files…"）。
	AgentActivity string
	// AgentToolUses 是子 agent 使用的工具次数。
	AgentToolUses int
	// AgentTokens 是子 agent 消耗的 token 数。
	AgentTokens int
	// MessageID 是事件所属消息在对话记录中的 UID（message.Message.UID）：正文、
	// 思考、工具开始属于本轮的助手消息，工具结束属于该工具的结果消息。
	MessageID string
	// AgentPath 非空表示事件属于某个子代理（归属路径，如 "toolu_01.toolu_07"）：
	// subagent_progress 描述的子代理；WithSubagentStream(SubagentStreamFull) 时
	// 转发进父流的子代理正文、思考、工具事件；子代理里发起的审批与提问。
	AgentPath string

	// Question 是 EventAskUser 的问题内容。
	Question string
	// Answer 是 EventAskUser 的回答（由前端通过 /askuser 端点返回）。
	Answer string

	// RequestID 是交互请求（EventNeedApproval、EventAskUser）的标识，回答时
	// 原样带回（/approve、/askuser）。
	RequestID string
	// Permission 是 EventNeedApproval 请求的权限级别。
	Permission string
	// Payload 是 EventAskUser 的结构化载荷（AskStructured / AskSessionCtx 附加）。
	Payload map[string]any

	// PlanContent 是 EventPlanConfirm 的计划内容。
	PlanContent string
	// PlanConfirm 是 EventPlanConfirm 的用户确认结果。
	PlanConfirm bool

	// InterruptReason 是 EventInterrupt 的原因；EventInterrupted 时为中断方给出
	// 的原因（InterruptHandler.Interrupt 的 reason，没有时为空）。
	InterruptReason string

	// CustomName / CustomValue 是 EventCustom 的事件名与 JSON 值。
	CustomName  string
	CustomValue json.RawMessage
}
