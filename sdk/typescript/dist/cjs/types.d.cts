/** GoAgent 线上协议类型——与 Go 库 protocol 包（protocol/protocol.go）一一对应。 */
/** 协议版本（Envelope.v 字段；GET /protocol 返回详情）。 */
export declare const PROTOCOL_VERSION = 1;
/** 帧类型名（Envelope.type 字段值）。 */
export declare const FrameType: {
    readonly RunStart: "run_start";
    readonly TextDelta: "text_delta";
    readonly Thinking: "thinking";
    readonly ToolStart: "tool_start";
    readonly ToolDone: "tool_done";
    readonly PermissionRequest: "permission_request";
    readonly AskUser: "ask_user";
    readonly PlanConfirm: "plan_confirm";
    readonly Progress: "progress";
    readonly StatusKey: "status_key";
    readonly Usage: "usage";
    readonly TurnComplete: "turn_complete";
    readonly Compaction: "compaction";
    readonly Retrieval: "retrieval";
    readonly Steer: "steer";
    readonly SubAgentProgress: "subagent_progress";
    readonly Interrupted: "interrupted";
    readonly Error: "error";
    readonly Done: "done";
    readonly Metadata: "metadata";
};
/** 统一事件信封——所有 SSE data 帧共用，按 type 分发后取对应字段。 */
export interface Envelope {
    /** 连接内单调递增（从 1 起）；跳号 = 丢帧，应走 messages() 回放补齐。 */
    seq: number;
    /** 协议版本。 */
    v: number;
    type: string;
    session_id?: string;
    /** 交互原语（ask_user / permission_request / plan_confirm）。 */
    request_id?: string;
    /** 状态行更新 key（非空 = 原地替换而非追加；text 空 = 清除）。 */
    status_key?: string;
    text?: string;
    thinking?: string;
    tool_name?: string;
    tool_use_id?: string;
    tool_input?: unknown;
    tool_result?: string;
    error?: string;
    usage?: {
        input_tokens: number;
        output_tokens: number;
    };
    /** ask_user 问题文本。 */
    question?: string;
    /** plan_confirm 计划全文。 */
    plan_content?: string;
    /** permission_request 请求的权限级别。 */
    permission?: string;
    /** 结构化交互载荷（确认卡选项等），按 payload.kind 分发渲染。 */
    payload?: Record<string, unknown>;
    /** metadata 帧：run 耗时 / 插话确认。 */
    elapsed_ms?: number;
    steered?: boolean;
    /** subagent_progress 帧。 */
    agent_id?: string;
    agent_desc?: string;
    agent_status?: string;
    agent_activity?: string;
    agent_tool_uses?: number;
    agent_tokens?: number;
}
/** POST /chat 请求体。 */
export interface ChatRequest {
    message: string;
    session_id?: string;
}
/** GET /protocol 响应。 */
export interface Description {
    version: number;
    events: Array<{
        name: string;
        description: string;
    }>;
    endpoints: Array<{
        method: string;
        path: string;
        description: string;
    }>;
}
