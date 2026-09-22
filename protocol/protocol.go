// Package protocol 定义 GoAgent HTTP/SSE 传输层的线上协议（wire protocol）。
//
// 宿主（桌面壳 / CLI / 其他语言的客户端）与引擎之间的一切交互帧都收敛到
// 一个统一信封 Envelope：字符串 type、会话内单调 seq、协议版本 v。
// 字段集是历史线上格式的超集——旧客户端忽略新增字段照常工作（向后兼容），
// 新客户端按 type 分发并读取结构化载荷。
//
// 三类帧共享同一信封：
//  1. 流事件（text_delta/tool_start/tool_done/steer/...）——agent loop 产出
//  2. 交互原语（permission_request/ask_user/plan_confirm）——引擎向客户端
//     发起的请求，携带 request_id，客户端经 /approve /askuser /plan/confirm
//     回传决定
//  3. 生命周期帧（run_start/metadata）——传输层标注一轮 run 的起止
//
// 类型安全：Go 侧由 http.go 统一序列化；其他语言参考 sdk/typescript/。
package protocol

import "encoding/json"

// Version 当前协议版本。每个 SSE 帧的 v 字段携带；GET /protocol 返回详情。
// 语义：新增可选字段不升版本；改字段语义/删除字段才升。
const Version = 1

// 帧类型名（Envelope.Type 字段值）。
const (
	// --- 流事件（agent loop 产出） ---
	TypeTextDelta        = "text_delta"        // 模型正文增量（Text）
	TypeThinking         = "thinking"          // 模型思考增量（Thinking）
	TypeToolStart        = "tool_start"        // 工具即将执行（ToolName/ToolInput/ToolUseID）
	TypeToolDone         = "tool_done"         // 工具执行完成（ToolName/ToolResult/ToolUseID）
	TypeNeedApproval     = "permission_request" // 请求权限审批（RequestID/ToolName/Permission）
	TypeUsage            = "usage"             // token 用量更新（Usage）
	TypeTurnComplete     = "turn_complete"     // 一轮 agent 循环完成
	TypeDone             = "done"              // 本轮 run 成功完成
	TypeError            = "error"             // 错误终止（Error）
	TypeProgress         = "progress"          // 工具中间进度（StatusKey 原地更新）
	TypeCompaction       = "compaction"        // 上下文压缩发生（信息性）
	TypeAskUser          = "ask_user"          // 向用户提问（RequestID/Question/Payload）
	TypePlanConfirm      = "plan_confirm"      // 请求确认计划（RequestID/PlanContent）
	TypeInterrupt        = "interrupt"         // 请求中断确认
	TypeInterrupted      = "interrupted"       // 用户主动终止（Text 携带原因）
	TypeRetrieval        = "retrieval"         // RAG 前置检索完成（信息性）
	TypeSteer            = "steer"             // 插话进入模型上下文（Text）
	TypeQueueRun         = "queue_run"         // 排队消息作为新一轮输入开跑（Text）
	TypeSubAgentProgress = "subagent_progress" // 子 agent 运行进度

	// --- 生命周期帧（传输层标注，不来自 agent loop） ---
	TypeRunStart = "run_start" // 一轮 run 开始（流内第一个帧，SessionID 绑定）
	TypeMetadata = "metadata"  // 一轮 run 结束（流内最后一个帧，ElapsedMs/Steered）
)

// Envelope 统一事件信封。所有 SSE data 帧共用；按 Type 分发后取对应字段。
// 字段是可选的（omitempty）——旧客户端遇到的未知字段照常忽略。
type Envelope struct {
	// Seq 连接内单调递增序号（从 1 起）。客户端用于断线检测（跳号 =
	// 丢帧，应走 /sessions/{id}/messages 回放补齐）与乱序防护。
	Seq int64 `json:"seq"`
	// Version 协议版本（protocol.Version）。
	Version int `json:"v"`
	// Type 帧类型名（上述 Type* 常量之一）。
	Type string `json:"type"`
	// SessionID 本帧归属的会话——多会话并行时客户端按它路由。
	SessionID string `json:"session_id,omitempty"`

	// RequestID 交互原语的请求标识（ask_user/permission_request/plan_confirm）。
	// 客户端经 /askuser /approve /plan/confirm 回传时原样携带。
	RequestID string `json:"request_id,omitempty"`

	// StatusKey 非空时本帧是「状态行更新」而非追加消息：客户端按 Key
	// 原地替换显示（429 重试倒计时等）；Text 空 = 清除该状态行。
	StatusKey string `json:"status_key,omitempty"`
	// Text 正文增量 / 进度 / 插话内容 / 终止原因。
	Text string `json:"text,omitempty"`
	// Thinking 模型思考过程增量。
	Thinking string `json:"thinking,omitempty"`
	// ToolName 工具名（tool_start/tool_done/permission_request）。
	ToolName string `json:"tool_name,omitempty"`
	// ToolUseID 工具调用标识（前端关联 start 与 done）。
	ToolUseID string `json:"tool_use_id,omitempty"`
	// ToolInput 工具调用参数（tool_start 携带，JSON 对象）。
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	// ToolResult 工具执行结果文本（tool_done 携带）。
	ToolResult string `json:"tool_result,omitempty"`
	// Error 错误文本（error 帧）。
	Error string `json:"error,omitempty"`
	// Usage token 用量（usage 帧 / turn_complete 附带）。
	Usage *Usage `json:"usage,omitempty"`

	// Question ask_user 的问题文本。
	Question string `json:"question,omitempty"`
	// PlanContent plan_confirm 的计划全文。
	PlanContent string `json:"plan_content,omitempty"`
	// Permission permission_request 请求的权限级别。
	Permission string `json:"permission,omitempty"`

	// Payload 结构化交互载荷。通用库不感知领域语义（如确认卡的选项
	// 列表），宿主工具经 AskStructured 附加任意 JSON 对象，客户端按
	// Payload 里的 kind 字段分发渲染（替代旧的文本前缀内嵌协议）。
	Payload map[string]any `json:"payload,omitempty"`

	// --- 生命周期帧专属字段 ---
	// ElapsedMs 本轮 run 耗时（metadata 帧）。
	ElapsedMs int64 `json:"elapsed_ms,omitempty"`
	// Steered 消息经插话通道注入当前 run（metadata 帧，steered=true 时
	// 客户端知道本轮流在插话确认后立即结束）。
	Steered bool `json:"steered,omitempty"`

	// --- 子 agent 进度字段（subagent_progress 帧） ---
	AgentID       string `json:"agent_id,omitempty"`
	AgentDesc     string `json:"agent_desc,omitempty"`
	AgentStatus   string `json:"agent_status,omitempty"`
	AgentActivity string `json:"agent_activity,omitempty"`
	AgentToolUses int    `json:"agent_tool_uses,omitempty"`
	AgentTokens   int    `json:"agent_tokens,omitempty"`
}

// Usage token 用量统计。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ChatRequest POST /chat 与 POST /queue 的请求体。
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

// ApproveRequest POST /approve 请求体——权限审批决定。
type ApproveRequest struct {
	RequestID   string `json:"request_id"`
	SessionID   string `json:"session_id,omitempty"`
	Allow       bool   `json:"allow"`
	AlwaysAllow bool   `json:"always_allow,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// AskUserAnswer POST /askuser 请求体——用户对提问的回答。
type AskUserAnswer struct {
	RequestID string `json:"request_id"`
	Answer    string `json:"answer"`
}

// PlanConfirmAnswer POST /plan/confirm 请求体——计划确认决定。
type PlanConfirmAnswer struct {
	RequestID string `json:"request_id"`
	Confirm   bool   `json:"confirm"`
	Reason    string `json:"reason,omitempty"`
}

// InterruptRequest POST /interrupt 请求体——终止指定会话正在执行的 run。
type InterruptRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Description GET /protocol 的响应——协议自描述（版本、帧类型、端点表）。
// 客户端启动时可拉取一次做兼容性校验（Version 不匹配即警告）。
type Description struct {
	Version   int            `json:"version"`
	Events    []FrameInfo    `json:"events"`
	Endpoints []EndpointInfo `json:"endpoints"`
}

// FrameInfo 帧类型说明。
type FrameInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// EndpointInfo 端点说明。
type EndpointInfo struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// Describe 返回当前协议的自描述文档（http.go 的 GET /protocol 使用）。
func Describe() Description {
	return Description{
		Version: Version,
		Events: []FrameInfo{
			{TypeRunStart, "一轮 run 开始（流内第一帧），绑定 session_id"},
			{TypeTextDelta, "模型正文增量"},
			{TypeThinking, "模型思考增量"},
			{TypeToolStart, "工具即将执行"},
			{TypeToolDone, "工具执行完成"},
			{TypeNeedApproval, "请求权限审批（回传 POST /approve）"},
			{TypeAskUser, "向用户提问（回传 POST /askuser；payload 为结构化载荷）"},
			{TypePlanConfirm, "请求确认计划（回传 POST /plan/confirm）"},
			{TypeProgress, "工具中间进度（status_key 原地更新）"},
			{TypeUsage, "token 用量更新"},
			{TypeTurnComplete, "一轮 agent 循环完成"},
			{TypeCompaction, "上下文压缩发生（信息性）"},
			{TypeRetrieval, "RAG 前置检索完成（信息性）"},
			{TypeSteer, "插话消息进入模型上下文"},
			{TypeQueueRun, "排队消息作为新一轮输入开跑（队列消费）"},
			{TypeSubAgentProgress, "子 agent 运行进度"},
			{TypeInterrupted, "用户主动终止"},
			{TypeError, "错误终止"},
			{TypeDone, "run 成功完成"},
			{TypeMetadata, "run 结束元数据（流内最后一帧）"},
		},
		Endpoints: []EndpointInfo{
			{"POST", "/chat", "SSE 流式对话；会话忙时自动转插话通道（steered=true）"},
			{"POST", "/queue", "排队消息（run 结束后自动续跑；返回条目 ID）"},
			{"GET", "/queue?session_id=", "查看排队消息（含条目 ID）"},
			{"POST", "/queue/remove", "按 ID 取走排队消息（撤回编辑 / 立即发送，返回原文）"},
			{"POST", "/approve", "权限审批响应"},
			{"GET", "/mode", "当前权限模式（default/accept_edits/plan/bypass/deny_all）"},
			{"POST", "/mode", "运行时切换权限模式（立即生效，含进行中的 run）"},
			{"GET", "/thinking", "当前思考强度档位（off/low/medium/high；空 = 不干预）"},
			{"POST", "/thinking", "运行时切换思考强度（自下一次 run 起生效）"},
			{"GET", "/model", "当前模型 ID"},
			{"POST", "/model", "运行时切换模型（无需重启；provider 需支持 ModelSwitcher）"},
			{"POST", "/askuser", "提问回答响应"},
			{"POST", "/plan/confirm", "计划确认响应"},
			{"POST", "/interrupt", "终止当前 run"},
			{"POST", "/execute", "同步执行（非流式）"},
			{"GET", "/sessions", "会话列表"},
			{"GET", "/sessions/{id}/messages", "会话历史回放"},
			{"GET", "/tools", "已注册工具"},
			{"GET", "/tasks", "任务列表（?session_id= 路由）"},
			{"GET", "/plan", "计划状态"},
			{"GET", "/bgtasks", "后台任务列表"},
			{"GET", "/usage", "token 用量统计"},
			{"GET", "/audit", "工具执行分析"},
			{"GET", "/protocol", "本协议自描述"},
			{"GET", "/health", "健康检查"},
		},
	}
}
