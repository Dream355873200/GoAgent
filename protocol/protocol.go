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
	TypeTextDelta        = "text_delta"         // 模型正文增量（Text）
	TypeThinking         = "thinking"           // 模型思考增量（Thinking）
	TypeToolStart        = "tool_start"         // 工具即将执行（ToolName/ToolInput/ToolUseID）
	TypeToolDone         = "tool_done"          // 工具执行完成（ToolName/ToolResult/ToolUseID）
	TypeNeedApproval     = "permission_request" // 请求权限审批（RequestID/ToolName/Permission）
	TypeUsage            = "usage"              // token 用量更新（Usage）
	TypeTurnComplete     = "turn_complete"      // 一轮 agent 循环完成
	TypeDone             = "done"               // 本轮 run 成功完成
	TypeError            = "error"              // 错误终止（Error）
	TypeProgress         = "progress"           // 工具中间进度（StatusKey 原地更新）
	TypeCompaction       = "compaction"         // 上下文压缩发生（信息性）
	TypeAskUser          = "ask_user"           // 向用户提问（RequestID/Question/Payload）
	TypePlanConfirm      = "plan_confirm"       // 请求确认计划（RequestID/PlanContent）
	TypeInterrupt        = "interrupt"          // 请求中断确认
	TypeInterrupted      = "interrupted"        // 用户主动终止（Text 携带原因）
	TypeRetrieval        = "retrieval"          // RAG 前置检索完成（信息性）
	TypeSteer            = "steer"              // 插话进入模型上下文（Text）
	TypeQueueRun         = "queue_run"          // 排队消息作为新一轮输入开跑（Text）
	TypeSubAgentProgress = "subagent_progress"  // 子 agent 运行进度
	TypeCustom           = "custom"             // 自定义事件（Name/Value）

	// --- 生命周期帧（传输层标注，不来自 agent loop） ---
	TypeRunStart = "run_start" // 一轮 run 开始（流内第一个帧，SessionID 绑定）
	TypeMetadata = "metadata"  // 一轮 run 结束（流内最后一个帧，ElapsedMs/Steered）
	// TypeResync 续传位置早于服务端缓冲：其间的帧已丢弃（Oldest 为缓冲中
	// 最旧帧的 session_seq）。客户端应从 /sessions/{id}/messages 重建，之后
	// 的帧照常到达。
	TypeResync = "resync"
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
	// SessionSeq 会话事件日志中的序号：会话内单调递增的不透明整数（不从 1
	// 开始）。断线后以最后收到的值调用 GET /runs/{session}/stream?after=
	// 续传。只属于本连接的帧（插话回执、排队回执、拒绝、流结束元数据）没有它。
	SessionSeq int64 `json:"session_seq,omitempty"`
	// RunID 一次会话运行的标识（run_start 与该运行的 metadata 帧携带）。
	RunID string `json:"run_id,omitempty"`
	// MessageID 帧所属消息在会话记录中的 UID（text_delta / thinking / tool_start
	// 属于本轮的助手消息，tool_done 属于该工具的结果消息），与
	// /sessions/{id}/messages 中消息的 uid 一致。
	MessageID string `json:"message_id,omitempty"`

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
	// ToolIsError 工具以错误结束（tool_done 携带），此时 ToolResult 是错误信息。
	ToolIsError bool `json:"tool_is_error,omitempty"`
	// Error 错误文本（error 帧）。
	Error string `json:"error,omitempty"`
	// Usage token 用量（usage 帧）。
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
	// Queued 会话忙但当前无可插话的轮次（任务尚在准备或收尾），消息改入
	// 排队车道，当前 run 结束后自动续跑（metadata 帧，Text 为排队项 ID）。
	Queued bool `json:"queued,omitempty"`
	// Reason 中断原因（interrupted 帧；POST /interrupt 的 reason 原样透传）。
	Reason string `json:"reason,omitempty"`
	// Oldest 服务端缓冲中最旧帧的 session_seq（resync 帧）。
	Oldest int64 `json:"oldest,omitempty"`

	// Name / Value 自定义事件的事件名（<命名空间>.<事件名>）与 JSON 值（custom 帧）。
	Name  string          `json:"name,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`

	// --- 子 agent 进度字段（subagent_progress 帧） ---
	AgentID       string `json:"agent_id,omitempty"`
	AgentDesc     string `json:"agent_desc,omitempty"`
	AgentStatus   string `json:"agent_status,omitempty"`
	AgentActivity string `json:"agent_activity,omitempty"`
	AgentToolUses int    `json:"agent_tool_uses,omitempty"`
	AgentTokens   int    `json:"agent_tokens,omitempty"`
	// AgentPath 帧属于某个子代理（归属路径，如 "toolu_01.toolu_07"）：
	// subagent_progress 描述的子代理；子代理事件全量转发时的正文、思考、工具帧；
	// 子代理里发起的 permission_request / ask_user。空 = 主 agent。
	AgentPath string `json:"agent_path,omitempty"`
}

// Usage token 用量统计。InputTokens 不含缓存部分，缓存读写单独计数
// （字段名同 Anthropic API）。
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheReadTokens   int `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// ChatRequest POST /chat 与 POST /queue 的请求体。
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
	// ResumeQueue 空闲唤醒：message 为空时取该会话 queue 车道队头作为本轮
	// 输入（后台任务终态通知等）；队列空则直接结束。会话忙时忽略（在跑
	// 的 run 结束后会自行消费队列）。
	ResumeQueue bool `json:"resume_queue,omitempty"`
	// AgentID 非空时消息作为指示投给该运行中的子代理（在它的工具批边界注入），
	// 不开新 run；子代理不在运行时返回错误。
	AgentID string `json:"agent_id,omitempty"`
	// Tools 声明本次运行的前端工具（客户端自己执行）：模型调用时照常发
	// tool_start 帧（name 命中本清单即归客户端执行），结果用
	// POST /chat/tool-result 回传；调用等待结果有 30 分钟上限，超时按失败
	// 结束。断线后用 GET /pending-tools 查未完成的调用。
	Tools []FrontendToolDecl `json:"tools,omitempty"`
}

// FrontendToolDecl 是前端工具声明（ChatRequest.Tools 元素）。
type FrontendToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// FrontendToolResult POST /chat/tool-result 的请求体——前端执行完工具后
// 把结果回传给等待中的运行。error 非空表示执行失败（作为工具错误交给模型）。
type FrontendToolResult struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content,omitempty"`
	Error      string `json:"error,omitempty"`
}

// FrontendToolCall 是正在等待前端执行结果的工具调用。
type FrontendToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
}

// ApproveRequest POST /approve 请求体——权限审批决定。
type ApproveRequest struct {
	RequestID   string `json:"request_id"`
	SessionID   string `json:"session_id,omitempty"`
	Allow       bool   `json:"allow"`
	AlwaysAllow bool   `json:"always_allow,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Input 批准时替换工具参数（完整替换，按工具的入参 schema 校验；不记为始终允许）。
	Input json.RawMessage `json:"input,omitempty"`
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
	// AgentID 非空时只中断该子代理：父运行收到"被用户中断"的工具结果后继续。
	AgentID string `json:"agent_id,omitempty"`
}

// Description GET /protocol 的响应——协议自描述（版本、帧类型、端点表）。
// 客户端启动时可拉取一次做兼容性校验（Version 不匹配即警告）。
type Description struct {
	Version int `json:"version"`
	// Profiles 服务端提供的线协议剖面：v1（本协议）与 agui-1.0（POST /agui）。
	Profiles  []string       `json:"profiles"`
	Events    []FrameInfo    `json:"events"`
	Endpoints []EndpointInfo `json:"endpoints"`
	// CustomEvents 宿主登记了 schema 的自定义事件（App.RegisterEvent）。
	CustomEvents []CustomEventInfo `json:"custom_events,omitempty"`
}

// CustomEventInfo 已登记的自定义事件。
type CustomEventInfo struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema,omitempty"`
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
		Version:  Version,
		Profiles: []string{"v1", "agui-1.0"},
		Events: []FrameInfo{
			{TypeRunStart, "一轮 run 开始，绑定 session_id，带 run_id 与 session_seq（会话忙时的插话、排队、拒绝不开新 run，流里没有本帧）"},
			{TypeTextDelta, "模型正文增量"},
			{TypeThinking, "模型思考增量"},
			{TypeToolStart, "工具即将执行"},
			{TypeToolDone, "工具执行完成（tool_is_error=true 表示以错误结束）"},
			{TypeNeedApproval, "请求权限审批（回传 POST /approve）；经会话事件日志到达挂在该会话上的所有连接"},
			{TypeAskUser, "向用户提问（回传 POST /askuser；payload 为结构化载荷）；经会话事件日志到达挂在该会话上的所有连接"},
			{TypePlanConfirm, "保留：请求确认计划（当前不产生）"},
			{TypeProgress, "工具中间进度（status_key 非空时原地更新）"},
			{TypeUsage, "token 用量更新（含缓存读写 token）"},
			{TypeTurnComplete, "一轮 agent 循环完成"},
			{TypeCompaction, "上下文压缩发生（信息性）"},
			{TypeRetrieval, "RAG 前置检索完成（信息性）"},
			{TypeSteer, "插话消息进入模型上下文"},
			{TypeQueueRun, "排队消息作为新一轮输入开跑（队列消费）"},
			{TypeSubAgentProgress, "子 agent 运行进度（agent_status：running/done/failed/cancelled；agent_path 为归属路径）"},
			{TypeCustom, "自定义事件：name 为 <命名空间>.<事件名>（goagent. 保留给框架），value 为任意 JSON；登记了 schema 的见 custom_events"},
			{TypeInterrupt, "保留：请求中断确认（当前不产生）"},
			{TypeInterrupted, "用户主动终止（reason 为 POST /interrupt 给出的原因）"},
			{TypeError, "错误终止"},
			{TypeDone, "run 成功完成（排队续跑时一个流里可有多个）"},
			{TypeMetadata, "流结束元数据（流内最后一帧：elapsed_ms / steered / queued；会话运行结束时带 run_id 与 session_seq）"},
			{TypeResync, "续传位置早于服务端缓冲（oldest 为缓冲中最旧帧的 session_seq）：其间的帧已丢弃，应从 /sessions/{id}/messages 重建"},
		},
		Endpoints: []EndpointInfo{
			{"POST", "/chat", "SSE 流式对话；会话忙时：启用插话则转插话或排队（metadata.steered / queued），未启用则 error 拒绝；带 agent_id 时作为指示投给运行中的子代理；带 tools 时声明前端工具（模型调用后由客户端执行并经 /chat/tool-result 回传）"},
			{"POST", "/chat/tool-result", "前端工具执行结果回传（等待中的运行即刻继续）"},
			{"GET", "/pending-tools?session_id=", "会话中等待前端执行结果的工具调用（断线重连后补齐）"},
			{"POST", "/agui", "AG-UI 1.0 剖面：请求体 RunAgentInput（threadId = 会话 ID），响应 AG-UI 事件流；审批与提问以 RUN_FINISHED{outcome:interrupt} 停车，下一次请求带 resume[] 作答"},
			{"GET", "/agui/capabilities", "AG-UI AgentCapabilities"},
			{"GET", "/runs/{session}/stream?after=&follow=", "会话事件流续传：先补发 session_seq > after 的帧（after 省略 = 从进行中运行的开头；0 = 缓冲中最旧的帧；也接受 Last-Event-ID 头），再转实时；follow=1 时跨运行持续推送，否则会话空闲后以 metadata 结束"},
			{"POST", "/execute", "同步执行（非流式、无会话）"},
			{"POST", "/approve", "权限审批响应"},
			{"GET", "/pending-approval?session_id=", "会话待处理的审批（断线重连后补齐）"},
			{"POST", "/askuser", "提问回答响应"},
			{"GET", "/pending-ask?session_id=", "会话待回答的提问（最新一条；断线重连后补齐）"},
			{"POST", "/plan/confirm", "保留：计划确认响应（当前没有请求会产生）"},
			{"POST", "/interrupt", "终止指定会话当前的 run（session_id 必填）；带 agent_id 时只中断该子代理，父运行继续"},
			{"POST", "/queue", "排队消息（run 结束后自动续跑；返回条目 ID）；带 agent_id 时作为指示投给运行中的子代理"},
			{"GET", "/queue?session_id=", "查看排队消息（含条目 ID）"},
			{"POST", "/queue/remove", "按 ID 取走排队消息（撤回编辑 / 立即发送，返回原文）"},
			{"GET", "/mode", "当前权限模式（?session_id= 查询会话级设置）"},
			{"POST", "/mode", "切换权限模式（带 session_id 只作用于该会话；mode 为空清除会话设置）"},
			{"GET", "/thinking", "当前思考强度档位（off/low/medium/high；空 = 不干预）"},
			{"POST", "/thinking", "运行时切换思考强度（自下一次 run 起生效）"},
			{"GET", "/model", "当前模型 ID"},
			{"POST", "/model", "运行时切换模型（provider 需支持 ModelSwitcher）"},
			{"GET", "/sessions", "会话列表"},
			{"GET", "/sessions/{id}/messages", "会话完整记录回放（不含元消息）"},
			{"GET", "/tools", "已注册工具"},
			{"GET", "/tasks", "任务列表（?session_id= 路由）"},
			{"POST", "/tasks", "创建任务"},
			{"GET", "/tasks/{id}", "获取任务"},
			{"PUT", "/tasks/{id}", "更新任务"},
			{"DELETE", "/tasks/{id}", "删除任务"},
			{"GET", "/plan", "计划状态"},
			{"POST", "/plan", "进入计划模式"},
			{"DELETE", "/plan", "退出计划模式"},
			{"GET", "/bgtasks", "后台任务列表"},
			{"GET", "/bgtasks/{id}", "后台任务详情"},
			{"POST", "/bgtasks/{id}/stop", "停止后台任务"},
			{"GET", "/usage", "token 用量统计"},
			{"GET", "/audit", "工具执行分析"},
			{"GET", "/agents?session_id=", "会话中的子代理（运行中与最近 5 分钟内结束的）：ID、归属路径、状态、活动、工具数、token"},
			{"GET", "/agents/{id}?session_id=", "单个子代理的快照与结束后保留的对话记录"},
			{"GET", "/protocol", "本协议自描述"},
			{"GET", "/health", "健康检查"},
		},
	}
}
