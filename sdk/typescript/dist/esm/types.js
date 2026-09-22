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
    PlanConfirm: "plan_confirm",
    Progress: "progress",
    StatusKey: "status_key",
    Usage: "usage",
    TurnComplete: "turn_complete",
    Compaction: "compaction",
    Retrieval: "retrieval",
    Steer: "steer",
    SubAgentProgress: "subagent_progress",
    Interrupted: "interrupted",
    Error: "error",
    Done: "done",
    Metadata: "metadata",
};
