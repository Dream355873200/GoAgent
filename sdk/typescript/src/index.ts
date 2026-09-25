/**
 * goagent-client — GoAgent HTTP/SSE 协议的 TypeScript 客户端。
 *
 * 与 GoAgent 引擎（RunHTTP）通信的零依赖客户端：对话 SSE 流式消费
 * （统一信封 Envelope：字符串 type / 连接内单调 seq / 协议版本 v）、
 * 交互原语回传（审批 / 提问 / 计划确认）、会话 / 任务 / 计划 / 后台
 * 任务 / 用量等 REST 端点的类型化封装。
 *
 * 运行环境：Node 18+（内置 fetch）、Electron 主进程、浏览器均可。
 *
 * 用法：
 * ```ts
 * const agent = new GoAgentClient("http://127.0.0.1:8420");
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
import type { Envelope, ChatRequest, Description } from "./types.js";

export type { Envelope, ChatRequest, Description } from "./types.js";

/** 对话选项。 */
export interface ChatOptions {
  /** 用户消息（必填）。 */
  message: string;
  /** 会话 ID；不传由服务端生成。多轮对话请固定同一个 ID。 */
  sessionId?: string;
  /** 空闲唤醒：message 为空时取该会话排队的队头作为本轮输入（后台任务终态通知等）。 */
  resumeQueue?: boolean;
  /** AbortSignal——断开 SSE 连接。注意：任务在引擎侧继续后台执行， */
  /** 中止任务请改用 interrupt()。 */
  signal?: AbortSignal;
}

/** metadata 末帧（run 结束）。 */
export interface RunMeta extends Envelope {
  type: "metadata";
  elapsed_ms?: number;
  steered?: boolean;
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
  constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    public readonly baseUrl: string,
  ) {}

  // ---------- 对话（SSE 流式） ----------

  /**
   * 流式对话：POST /chat，逐帧 yield 统一信封。
   * 首帧恒为 run_start、末帧恒为 metadata（steered=true 表示消息经
   * 插话通道注入了正在运行的 run，本轮不会有更多事件）。
   * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，
   * 可经 sessions.messages() 回放。
   */
  async *chat(req: ChatOptions): AsyncGenerator<Envelope, void, undefined> {
    const res = await fetch(`${this.baseUrl}/chat`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message: req.message, session_id: req.sessionId, resume_queue: req.resumeQueue || undefined }),
      signal: req.signal,
    });
    if (!res.ok || !res.body) {
      throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    }
    for await (const frame of parseSSE(res.body)) {
      yield frame;
    }
  }

  // ---------- 交互原语回传 ----------

  /** 权限审批决定（ask_user/permission_request 帧的 request_id）。 */
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

  /** 计划确认（plan_confirm 帧）。 */
  async confirmPlan(requestId: string, confirm: boolean, reason?: string): Promise<void> {
    await this.post("/plan/confirm", { request_id: requestId, confirm, reason });
  }

  /** 终止指定会话正在执行的 run。 */
  async interrupt(sessionId: string, reason?: string): Promise<void> {
    await this.post("/interrupt", { session_id: sessionId, reason });
  }

  // ---------- 插话通道 ----------

  /**
   * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；空闲时等价
   * 于普通消息。返回 steered=true 表示已注入当前 run。
   */
  async steer(sessionId: string, message: string): Promise<{ steered: boolean }> {
    const res = await this.post<{ steered?: boolean }>("/chat", { message, session_id: sessionId });
    return { steered: !!res.steered };
  }

  /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回积压数。 */
  async enqueue(sessionId: string, message: string): Promise<number> {
    const res = await this.post<{ pending: number }>("/queue", { message, session_id: sessionId });
    return res.pending;
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

  async sessions(): Promise<Array<{ id: string; title?: string; state: string; updated_at?: string }>> {
    return this.get("/sessions");
  }

  /** 会话完整消息历史（前端重开项目时的回放源）。 */
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

  private async get<T>(path: string): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`);
    if (!res.ok) throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    return res.json() as Promise<T>;
  }

  private async post<T = Record<string, unknown>>(path: string, body: unknown): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
    return res.json() as Promise<T>;
  }
}

/**
 * 解析 SSE 字节流为信封序列。只处理 data: 行（本协议不使用事件名行
 * 与多行 data），容忍跨 chunk 分帧。
 */
async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<Envelope, void, undefined> {
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
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line.startsWith("data: ")) continue;
        try {
          yield JSON.parse(line.slice(6)) as Envelope;
        } catch {
          // 坏帧跳过（半截 JSON / 非协议行）
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
