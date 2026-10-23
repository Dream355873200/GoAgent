// Package agui 定义 GoAgent 的 AG-UI 1.0 剖面使用的线上类型。
//
// 字段名与官方规范 spec/1.0/schema.json（https://ag-ui.com/spec/1.0/schema.json）
// 一一对应，只覆盖 GoAgent 发出与接受的子集；测试逐帧用官方 schema 校验
// （protocol/agui/testdata/schema-1.0.json）。AG-UI 的事件对象是封闭的
// （unevaluatedProperties:false），扩展只能放在 CUSTOM 事件与 metadata 中带
// 前缀的键里，本包的类型不提供任何额外的顶层字段。
//
// 本包只是方言：agent 内核（goagent.Event、internal/loop）不引用它，映射在
// HTTP 层完成。
package agui

import "encoding/json"

// ProtocolVersion 是本剖面实现的 AG-UI 协议版本（RUN_STARTED.protocolVersion）。
const ProtocolVersion = "1.0"

// 事件类型（EventType 枚举中 GoAgent 会发出的部分）。
const (
	TypeRunStarted              = "RUN_STARTED"
	TypeRunFinished             = "RUN_FINISHED"
	TypeRunError                = "RUN_ERROR"
	TypeTextMessageStart        = "TEXT_MESSAGE_START"
	TypeTextMessageContent      = "TEXT_MESSAGE_CONTENT"
	TypeTextMessageEnd          = "TEXT_MESSAGE_END"
	TypeToolCallStart           = "TOOL_CALL_START"
	TypeToolCallArgs            = "TOOL_CALL_ARGS"
	TypeToolCallEnd             = "TOOL_CALL_END"
	TypeToolCallResult          = "TOOL_CALL_RESULT"
	TypeReasoningStart          = "REASONING_START"
	TypeReasoningMessageStart   = "REASONING_MESSAGE_START"
	TypeReasoningMessageContent = "REASONING_MESSAGE_CONTENT"
	TypeReasoningMessageEnd     = "REASONING_MESSAGE_END"
	TypeReasoningEnd            = "REASONING_END"
	TypeMessagesSnapshot        = "MESSAGES_SNAPSHOT"
	TypeActivitySnapshot        = "ACTIVITY_SNAPSHOT"
	TypeCustom                  = "CUSTOM"
	TypeSubagentStarted         = "SUBAGENT_STARTED"
	TypeSubagentFinished        = "SUBAGENT_FINISHED"
	TypeSubagentError           = "SUBAGENT_ERROR"
)

// Metadata 是事件、消息、中断上的开放键值（键 ag-ui 保留给协议本身；GoAgent
// 的键以 "goagent." 为前缀）。
type Metadata map[string]any

// Event 是一个 AG-UI 事件。每种事件只填写规范为它定义的字段——字段集合由
// Type 决定，构造函数见 events.go，测试负责保证没有越界的字段。
type Event struct {
	Type      string   `json:"type"`
	Timestamp int64    `json:"timestamp,omitempty"`
	Metadata  Metadata `json:"metadata,omitempty"`

	// 运行级
	ThreadID        string         `json:"threadId,omitempty"`
	RunID           string         `json:"runId,omitempty"`
	ProtocolVersion string         `json:"protocolVersion,omitempty"`
	Outcome         *RunOutcome    `json:"outcome,omitempty"`
	Usage           []TokenUsage   `json:"usage,omitempty"`
	Message         string         `json:"message,omitempty"`
	Code            string         `json:"code,omitempty"`
	Messages        []Message      `json:"messages,omitempty"`
	Input           *RunAgentInput `json:"input,omitempty"`

	// 消息、推理、活动
	MessageID    string  `json:"messageId,omitempty"`
	Role         string  `json:"role,omitempty"`
	Delta        *string `json:"delta,omitempty"`
	ActivityType string  `json:"activityType,omitempty"`
	Replace      *bool   `json:"replace,omitempty"`
	Content      any     `json:"content,omitempty"`
	Name         string  `json:"name,omitempty"`
	Value        any     `json:"value,omitempty"`

	// 工具调用
	ToolCallID      string `json:"toolCallId,omitempty"`
	ToolCallName    string `json:"toolCallName,omitempty"`
	ParentMessageID string `json:"parentMessageId,omitempty"`

	// 子代理归属与生命周期
	SubagentRunID       string           `json:"subagentRunId,omitempty"`
	ParentSubagentRunID string           `json:"parentSubagentRunId,omitempty"`
	ParentToolCallID    string           `json:"parentToolCallId,omitempty"`
	Description         string           `json:"description,omitempty"`
	SubagentOutcome     *SubagentOutcome `json:"-"`
}

// MarshalJSON 处理两处同名不同义的字段：SUBAGENT_FINISHED 的 outcome 与
// RUN_FINISHED 的 outcome 形状不同，在 Go 里分开存放、线上合成同一个键。
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	if e.SubagentOutcome == nil {
		return json.Marshal(plain(e))
	}
	return json.Marshal(struct {
		plain
		Outcome *SubagentOutcome `json:"outcome"`
	}{plain(e), e.SubagentOutcome})
}

// RunOutcome 是 RUN_FINISHED 的结束原因：success（可带 pendingToolCallIds）、
// interrupt（带 interrupts）或 cancelled。
type RunOutcome struct {
	Type               string      `json:"type"`
	Interrupts         []Interrupt `json:"interrupts,omitempty"`
	PendingToolCallIDs []string    `json:"pendingToolCallIds,omitempty"`
}

// Interrupt 是运行在继续之前需要外部给出的东西（审批、回答）。下一次请求的
// RunAgentInput.Resume 以 ID 作答。
type Interrupt struct {
	ID             string         `json:"id"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message,omitempty"`
	ToolCallID     string         `json:"toolCallId,omitempty"`
	ResponseSchema map[string]any `json:"responseSchema,omitempty"`
	ExpiresAt      string         `json:"expiresAt,omitempty"`
	SubagentRunID  string         `json:"subagentRunId,omitempty"`
	Metadata       Metadata       `json:"metadata,omitempty"`
}

// SubagentOutcome 是 SUBAGENT_FINISHED 的结束原因：success 或 suspended。
type SubagentOutcome struct {
	Type         string   `json:"type"`
	InterruptIDs []string `json:"interruptIds,omitempty"`
}

// TokenUsage 是一个模型的用量（RUN_FINISHED.usage 的元素）。
type TokenUsage struct {
	Provider              string `json:"provider,omitempty"`
	Model                 string `json:"model,omitempty"`
	InputTokens           int    `json:"inputTokens,omitempty"`
	OutputTokens          int    `json:"outputTokens,omitempty"`
	TotalTokens           int    `json:"totalTokens,omitempty"`
	CachedInputTokens     int    `json:"cachedInputTokens,omitempty"`
	CacheWriteInputTokens int    `json:"cacheWriteInputTokens,omitempty"`
}

// RunAgentInput 是 POST /agui 的请求体。
type RunAgentInput struct {
	ThreadID        string          `json:"threadId"`
	RunID           string          `json:"runId"`
	ProtocolVersion string          `json:"protocolVersion,omitempty"`
	ParentRunID     string          `json:"parentRunId,omitempty"`
	State           json.RawMessage `json:"state,omitempty"`
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	Context         []Context       `json:"context,omitempty"`
	ForwardedProps  json.RawMessage `json:"forwardedProps,omitempty"`
	Resume          []ResumeEntry   `json:"resume,omitempty"`
}

// ResumeEntry 回答一个中断。Status 为 resolved 或 cancelled；Payload 是答案
// （审批：{approved, editedArgs?}；提问：{answer} 或字符串）。
type ResumeEntry struct {
	InterruptID string          `json:"interruptId"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Metadata    Metadata        `json:"metadata,omitempty"`
}

// Message 是对话中的一条消息（规范的 Message 联合类型，按 role 区分）。
// Content 的形状随 role 变化：user 与 tool 为字符串或内容块数组，其余为字符串。
type Message struct {
	ID             string     `json:"id"`
	Role           string     `json:"role"`
	Content        any        `json:"content,omitempty"`
	Name           string     `json:"name,omitempty"`
	ToolCalls      []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID     string     `json:"toolCallId,omitempty"`
	Error          string     `json:"error,omitempty"`
	EncryptedValue string     `json:"encryptedValue,omitempty"`
	ActivityType   string     `json:"activityType,omitempty"`
	SubagentRunID  string     `json:"subagentRunId,omitempty"`
	Metadata       Metadata   `json:"metadata,omitempty"`
}

// ToolCall 是助手消息中的一个工具调用。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具调用的函数名与参数（JSON 字符串）。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 是客户端声明的前端工具。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Metadata    Metadata        `json:"metadata,omitempty"`
}

// Context 是客户端附带的一段上下文。
type Context struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// AgentCapabilities 是 GET /agui/capabilities 的响应（规范的 AgentCapabilities）。
type AgentCapabilities struct {
	Identity       *IdentityCapabilities       `json:"identity,omitempty"`
	Transport      *TransportCapabilities      `json:"transport,omitempty"`
	Tools          *ToolsCapabilities          `json:"tools,omitempty"`
	MultiAgent     *MultiAgentCapabilities     `json:"multiAgent,omitempty"`
	Reasoning      *ReasoningCapabilities      `json:"reasoning,omitempty"`
	HumanInTheLoop *HumanInTheLoopCapabilities `json:"humanInTheLoop,omitempty"`
	Custom         map[string]any              `json:"custom,omitempty"`
}

// IdentityCapabilities 描述 agent 自身。
type IdentityCapabilities struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// TransportCapabilities 描述传输能力。
type TransportCapabilities struct {
	Streaming bool `json:"streaming"`
	Resumable bool `json:"resumable"`
}

// ToolsCapabilities 描述工具能力。
type ToolsCapabilities struct {
	Supported      bool `json:"supported"`
	ParallelCalls  bool `json:"parallelCalls"`
	ClientProvided bool `json:"clientProvided"`
}

// MultiAgentCapabilities 描述多 agent 能力。
type MultiAgentCapabilities struct {
	Supported  bool `json:"supported"`
	Delegation bool `json:"delegation"`
	Handoffs   bool `json:"handoffs"`
}

// ReasoningCapabilities 描述推理能力。
type ReasoningCapabilities struct {
	Supported bool `json:"supported"`
	Streaming bool `json:"streaming"`
	Encrypted bool `json:"encrypted"`
}

// HumanInTheLoopCapabilities 描述人机协同能力。
type HumanInTheLoopCapabilities struct {
	Supported        bool `json:"supported"`
	Approvals        bool `json:"approvals"`
	Interrupts       bool `json:"interrupts"`
	ApproveWithEdits bool `json:"approveWithEdits"`
}
