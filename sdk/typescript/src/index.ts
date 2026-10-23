/**
 * goagent-client — GoAgent HTTP/SSE 协议的 TypeScript 客户端。
 *
 * 与 GoAgent 引擎（RunHTTP）通信的零依赖客户端：对话 SSE 流式消费
 * （统一信封 Envelope：字符串 type / 连接内单调 seq / 协议版本 v）、
 * 交互原语回传（审批 / 提问 / 计划确认）、插话与排队、会话 / 任务 /
 * 计划 / 后台任务 / 用量等 REST 端点的类型化封装。
 *
 * 运行环境：Node 18+（内置 fetch）、Electron 主进程、浏览器均可。
 *
 * 用法：
 * ```ts
 * const agent = new GoAgentClient("http://127.0.0.1:8420", { token: process.env.GOAGENT_TOKEN });
 * for await (const frame of agent.chat({ message: "做一个记账 app", sessionId: "s1" })) {
 *   switch (frame.type) {
 *     case "text_delta":   process.stdout.write(frame.text ?? ""); break;
 *     case "tool_start":   console.log("工具:", frame.tool_name); break;
 *     case "ask_user":     await agent.answer(frame.request_id!, "好的"); break;
 *     case "permission_request": await agent.approve(frame.request_id!, true); break;
 *   }
 * }
 * ```
 */
import type { Envelope, Description, QueueItem, SessionSummary } from "./types.js";

export { FrameType, PROTOCOL_VERSION } from "./types.js";
export type { Envelope, ChatRequest, Description, QueueItem, SessionSummary } from "./types.js";

/** 客户端选项。 */
export interface ClientOptions {
  /** 鉴权令牌（服务端配置了 WithHTTPAuthToken 时必填），以 Authorization: Bearer 发送。 */
  token?: string;
  /** 自定义 fetch（测试或运行环境没有全局 fetch 时注入）。 */
  fetch?: typeof fetch;
}

/** 对话选项。 */
export interface ChatOptions {
  /** 用户消息；resumeQueue 为 true 时可为空。 */
  message: string;
  /** 会话 ID；不传由服务端生成。多轮对话请固定同一个 ID。 */
  sessionId?: string;
  /** 空闲唤醒：message 为空时取该会话排队的队头作为本轮输入（后台任务终态通知等）。 */
  resumeQueue?: boolean;
  /** AbortSignal——断开 SSE 连接。任务在引擎侧继续后台执行；中止任务请改用 interrupt()。 */
  signal?: AbortSignal;
}

/** metadata 末帧（流结束）。 */
export interface RunMeta extends Envelope {
  type: "metadata";
  elapsed_ms?: number;
  steered?: boolean;
  queued?: boolean;
}

/** steer() 的结果。 */
export interface SteerResult {
  /** 已在最近的工具批边界注入正在运行的 run。 */
  steered: boolean;
  /** 会话忙但此刻不可插话，消息已进入排队车道；值为排队项 ID。 */
  queuedId?: string;
}

/** 待处理的审批请求（GET /pending-approval）。 */
export interface PendingApproval {
  request_id: string;
  tool_name: string;
  tool_input?: unknown;
  permission?: string;
}

/** 待处理的提问（GET /pending-ask）。 */
export interface PendingAsk {
  request_id: string;
  question: string;
  payload?: Record<string, unknown>;
}

export class GoAgentError extends Error {
  constructor(
    message: string,
    public readonly status: number,
  ) {
    super(message);
    this.name = "GoAgentError";
  }
}

export class GoAgentClient {
  private readonly token?: string;
  private readonly fetchImpl: typeof fetch;

  constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    public readonly baseUrl: string,
    opts: ClientOptions = {},
  ) {
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? globalThis.fetch.bind(globalThis);
  }

  // ---------- 对话（SSE 流式） ----------

  /**
   * 流式对话：POST /chat，逐帧 yield 统一信封。末帧恒为 metadata。
   * 首帧通常是 run_start；会话忙时（插话、排队、拒绝）不开新 run，没有 run_start。
   * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，可经 messages() 回放。
   */
  async *chat(req: ChatOptions): AsyncGenerator<Envelope, void, undefined> {
    const res = await this.fetchImpl(`${this.baseUrl}/chat`, {
      method: "POST",
      headers: this.headers({ "Content-Type": "application/json" }),
      body: JSON.stringify({ message: req.message, session_id: req.sessionId, resume_queue: req.resumeQueue || undefined }),
      signal: req.signal,
    });
    if (!res.ok || !res.body) {
      throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    }
    yield* parseSSE(res.body);
  }

  // ---------- 交互原语回传 ----------

  /** 权限审批决定（permission_request 帧的 request_id）。 */
  async approve(requestId: string, allow: boolean, opts?: { alwaysAllow?: boolean; reason?: string; sessionId?: string }): Promise<void> {
    await this.post("/approve", {
      request_id: requestId,
      allow,
      always_allow: opts?.alwaysAllow,
      reason: opts?.reason,
      session_id: opts?.sessionId,
    });
  }

  /** 回答引擎提问（ask_user 帧）。 */
  async answer(requestId: string, answer: string): Promise<void> {
    await this.post("/askuser", { request_id: requestId, answer });
  }

  /** 计划确认（plan_confirm 帧；该帧当前服务端不产生，保留）。 */
  async confirmPlan(requestId: string, confirm: boolean, reason?: string): Promise<void> {
    await this.post("/plan/confirm", { request_id: requestId, confirm, reason });
  }

  /** 终止指定会话正在执行的 run。 */
  async interrupt(sessionId: string, reason?: string): Promise<void> {
    await this.post("/interrupt", { session_id: sessionId, reason });
  }

  /** 会话当前待处理的审批（断线重连后补齐 permission_request）。 */
  async pendingApprovals(sessionId: string): Promise<PendingApproval[]> {
    const res = await this.get<{ pending: boolean; items?: PendingApproval[] }>(`/pending-approval?session_id=${encodeURIComponent(sessionId)}`);
    return res.items ?? [];
  }

  /** 会话当前待回答的提问（只返回最新一条；断线重连后补齐 ask_user）。 */
  async pendingAsk(sessionId: string): Promise<PendingAsk | null> {
    const res = await this.get<{ pending: boolean } & Partial<PendingAsk>>(`/pending-ask?session_id=${encodeURIComponent(sessionId)}`);
    if (!res.pending || !res.request_id) return null;
    return { request_id: res.request_id, question: res.question ?? "", payload: res.payload };
  }

  // ---------- 插话与排队（服务端需 WithSteering） ----------

  /**
   * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；此刻不可插话时进入排队车道。
   * 会话空闲时这条消息会开启新的一轮 run——本方法读尽该流并返回 steered=false。
   * 需要消费空闲时的流式输出请直接用 chat()。
   */
  async steer(sessionId: string, message: string): Promise<SteerResult> {
    let steered = false;
    let queuedId: string | undefined;
    for await (const frame of this.chat({ message, sessionId })) {
      if (frame.type === "metadata") {
        steered = !!frame.steered;
        if (frame.queued) queuedId = frame.text;
      }
    }
    return { steered, queuedId };
  }

  /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回排队项 ID 与积压数。 */
  async enqueue(sessionId: string, message: string): Promise<{ id: string; pending: number }> {
    return this.post<{ id: string; pending: number }>("/queue", { message, session_id: sessionId });
  }

  /** 查看排队车道（队头在前）。 */
  async queue(sessionId: string): Promise<QueueItem[]> {
    const res = await this.get<{ pending: number; items?: QueueItem[] }>(`/queue?session_id=${encodeURIComponent(sessionId)}`);
    return res.items ?? [];
  }

  /** 撤回一条尚未消费的排队消息。已消费或不存在时抛出 404。 */
  async removeQueued(sessionId: string, itemId: string): Promise<string> {
    const res = await this.post<{ text: string }>("/queue/remove", { session_id: sessionId, item_id: itemId });
    return res.text;
  }

  // ---------- 运行时控制 ----------

  /** 权限模式（default / accept_edits / auto / bypass / plan / deny_all）。传 sessionId 时只作用于该会话。 */
  async getMode(sessionId?: string): Promise<string> {
    const q = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : "";
    return (await this.get<{ mode: string }>(`/mode${q}`)).mode;
  }

  /** 设置权限模式；sessionId 非空且 mode 为空时清除该会话的设置。 */
  async setMode(mode: string, sessionId?: string): Promise<void> {
    await this.post("/mode", { mode, session_id: sessionId });
  }

  // ---------- REST ----------

  async health(): Promise<{ status: string; tools: number }> {
    return this.get("/health");
  }

  /** 协议自描述（版本 / 帧类型表 / 端点表），客户端做兼容性校验用。 */
  async protocol(): Promise<Description> {
    return this.get("/protocol");
  }

  async tools(): Promise<Array<{ name: string; description: string; permission: string; concurrent: boolean }>> {
    return this.get("/tools");
  }

  async sessions(): Promise<SessionSummary[]> {
    return this.get("/sessions");
  }

  /** 会话完整消息历史（前端重开项目或断线重连时的回放源）。 */
  async messages(sessionId: string): Promise<unknown[]> {
    return this.get(`/sessions/${encodeURIComponent(sessionId)}/messages`);
  }

  async tasks(sessionId?: string): Promise<unknown[]> {
    return this.get(sessionId ? `/tasks?session_id=${encodeURIComponent(sessionId)}` : "/tasks");
  }

  async task(taskId: string, sessionId?: string): Promise<unknown> {
    const q = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : "";
    return this.get(`/tasks/${encodeURIComponent(taskId)}${q}`);
  }

  async plan(): Promise<{ active: boolean; state: string; file_path: string; content: string }> {
    return this.get("/plan");
  }

  async bgTasks(): Promise<unknown[]> {
    return this.get("/bgtasks");
  }

  async stopBgTask(id: string): Promise<void> {
    await this.post(`/bgtasks/${encodeURIComponent(id)}/stop`, {});
  }

  async usage(): Promise<unknown> {
    return this.get("/usage");
  }

  // ---------- HTTP 底座 ----------

  private headers(extra: Record<string, string> = {}): Record<string, string> {
    return this.token ? { ...extra, Authorization: `Bearer ${this.token}` } : extra;
  }

  private async get<T>(path: string): Promise<T> {
    const res = await this.fetchImpl(`${this.baseUrl}${path}`, { headers: this.headers() });
    if (!res.ok) throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    return res.json() as Promise<T>;
  }

  private async post<T = Record<string, unknown>>(path: string, body: unknown): Promise<T> {
    const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
      method: "POST",
      headers: this.headers({ "Content-Type": "application/json" }),
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    return res.json() as Promise<T>;
  }
}

/**
 * 解析 SSE 字节流为信封序列。只处理 data: 行（本协议不使用事件名行
 * 与多行 data），容忍跨 chunk 分帧与 CRLF 行尾；注释行（: ping 心跳）跳过。
 */
export async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<Envelope, void, undefined> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).replace(/\r$/, "");
        buf = buf.slice(nl + 1);
        if (!line.startsWith("data:")) continue;
        try {
          yield JSON.parse(line.slice(5).trimStart()) as Envelope;
        } catch {
          // 坏帧跳过（半截 JSON / 非协议行）
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
