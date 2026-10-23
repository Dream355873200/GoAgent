/** GoAgent 线上协议类型——与 Go 库 protocol 包（protocol/protocol.go）一一对应。 */
/** 协议版本（Envelope.v 字段；GET /protocol 返回详情）。 */
export const PROTOCOL_VERSION = 1;
/** 帧类型名（Envelope.type 字段值）。 */
export const FrameType = {
    RunStart: "run_start",
    TextDelta: "text_delta",
    Thinking: "thinking",
    ToolStart: "tool_start",
    ToolDone: "tool_done",
    PermissionRequest: "permission_request",
    AskUser: "ask_user",
    /** 保留，当前服务端不产生。 */
    PlanConfirm: "plan_confirm",
    Progress: "progress",
    Usage: "usage",
    TurnComplete: "turn_complete",
    Compaction: "compaction",
    Retrieval: "retrieval",
    Steer: "steer",
    QueueRun: "queue_run",
    SubAgentProgress: "subagent_progress",
    /** 自定义事件：name 为 <命名空间>.<事件名>，value 为任意 JSON。 */
    Custom: "custom",
    Interrupted: "interrupted",
    Error: "error",
    Done: "done",
    Metadata: "metadata",
    /** 续传位置早于服务端缓冲：其间的帧已丢弃（oldest 为最旧帧的 session_seq），应从 messages() 重建。 */
    Resync: "resync",
};
