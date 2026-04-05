package goagent

import (
	"encoding/json"

	"github.com/anthropic-community/goagent/message"
	"github.com/anthropic-community/goagent/provider"
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

	// ToolResult is set for EventToolDone.
	ToolResult string

	// Usage is set for EventUsageUpdate.
	Usage *provider.Usage

	// Error is set for EventError.
	Error error

	// Messages 在 EventDone 时携带本轮完整消息列表。
	// 业务层可用此数据自行持久化对话记录（包含 thinking 内容）。
	// 仅在 EventDone 事件中设置，其他事件为 nil。
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

	// Question 是 EventAskUser 的问题内容。
	Question string
	// Answer 是 EventAskUser 的回答（由前端通过 /askuser 端点返回）。
	Answer string

	// PlanContent 是 EventPlanConfirm 的计划内容。
	PlanContent string
	// PlanConfirm 是 EventPlanConfirm 的用户确认结果。
	PlanConfirm bool

	// InterruptReason 是 EventInterrupt 的原因。
	InterruptReason string
}
