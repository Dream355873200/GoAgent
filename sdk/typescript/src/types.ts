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
} as const;

/** 统一事件信封——所有 SSE data 帧共用，按 type 分发后取对应字段。 */
export interface Envelope {
  /** 连接内单调递增（从 1 起），与线上顺序一致；跳号 = 丢帧，应走 messages() 回放补齐。 */
  seq: number;
  /** 协议版本。 */
  v: number;
  type: string;
  session_id?: string;
  /**
   * 会话事件日志序号：会话内单调递增的不透明整数（不从 1 开始）。断线后以最后收到的值
   * 调用 resume(sessionId, { after }) 续传。只属于本连接的帧（插话/排队回执、拒绝、流结束元数据）没有它。
   */
  session_seq?: number;
  /** 一次会话运行的标识（run_start 与该运行的 metadata 携带）。 */
  run_id?: string;

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
  /** tool_done：工具以错误结束，此时 tool_result 是错误信息。 */
  tool_is_error?: boolean;
  error?: string;
  /** input_tokens 不含缓存部分；缓存读写单独计数。 */
  usage?: {
    input_tokens: number;
    output_tokens: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
  };

  /** ask_user 问题文本。 */
  question?: string;
  /** plan_confirm 计划全文。 */
  plan_content?: string;
  /** permission_request 请求的权限级别（ReadOnly / Normal / RequireApproval / Dangerous）。 */
  permission?: string;
  /** 结构化交互载荷（确认卡选项等），按 payload.kind 分发渲染。 */
  payload?: Record<string, unknown>;

  /** metadata 帧：run 耗时 / 插话确认 / 排队回执（排队时 text 为排队项 ID）。 */
  elapsed_ms?: number;
  steered?: boolean;
  queued?: boolean;
  /** interrupted 帧：POST /interrupt 给出的原因。 */
  reason?: string;
  /** resync 帧：服务端缓冲中最旧帧的 session_seq。 */
  oldest?: number;

  /** custom 帧：事件名（<命名空间>.<事件名>，goagent. 保留给框架）与 JSON 值。 */
  name?: string;
  value?: unknown;

  /** subagent_progress 帧。agent_status 取值：running / done / failed / cancelled。 */
  agent_id?: string;
  agent_desc?: string;
  agent_status?: string;
  agent_activity?: string;
  agent_tool_uses?: number;
  agent_tokens?: number;
  /**
   * 帧属于某个子代理（归属路径，如 "toolu_01.toolu_07"）：subagent_progress 描述的子代理；
   * 服务端开启子代理全量转发时的正文 / 思考 / 工具帧；子代理里发起的审批与提问。空 = 主 agent。
   */
  agent_path?: string;
}

/** POST /chat 请求体。 */
export interface ChatRequest {
  message: string;
  session_id?: string;
  /** 空闲唤醒：message 为空时取该会话排队车道的队头作为本轮输入。 */
  resume_queue?: boolean;
}

/** GET /sessions 列表项。 */
export interface SessionSummary {
  id: string;
  state: string;
  turn_count: number;
  /** Unix 毫秒。 */
  created_at: number;
  /** Unix 毫秒。 */
  updated_at: number;
  first_message?: string;
}

/** GET /agents 列表项：一个子代理运行的快照。 */
export interface SubagentInfo {
  /** 父运行中 Agent_* 调用的 tool_use_id。 */
  id: string;
  /** 归属路径：父子代理路径 + "." + id。 */
  path: string;
  name: string;
  status: "running" | "done" | "failed" | "cancelled";
  /** 当前活动（运行中）或错误 / 中断原因。 */
  activity?: string;
  tool_uses: number;
  tokens: number;
  background: boolean;
  /** 后台运行的 bgtask ID。 */
  task_id?: string;
  started_at: string;
  ended_at?: string;
  /** 结束时的最终文本（截断）。 */
  result?: string;
}

/** 排队车道中的一条消息。 */
export interface QueueItem {
  id: string;
  text: string;
}

/** GET /protocol 响应。 */
export interface Description {
  version: number;
  events: Array<{ name: string; description: string }>;
  endpoints: Array<{ method: string; path: string; description: string }>;
  /** 服务端登记了 schema 的自定义事件（App.RegisterEvent）。 */
  custom_events?: Array<{ name: string; schema?: Record<string, unknown> }>;
}
