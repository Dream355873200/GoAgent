export { FrameType, PROTOCOL_VERSION } from "./types.js";
export class GoAgentError extends Error {
    status;
    constructor(message, status) {
        super(message);
        this.status = status;
        this.name = "GoAgentError";
    }
}
export class GoAgentClient {
    baseUrl;
    token;
    fetchImpl;
    constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    baseUrl, opts = {}) {
        this.baseUrl = baseUrl;
        this.token = opts.token;
        this.fetchImpl = opts.fetch ?? globalThis.fetch.bind(globalThis);
    }
    // ---------- 对话（SSE 流式） ----------
    /**
     * 流式对话：POST /chat，逐帧 yield 统一信封。末帧恒为 metadata。
     * 首帧通常是 run_start；会话忙时（插话、排队、拒绝）不开新 run，没有 run_start。
     * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，可经 resume() 续传或 messages() 回放。
     */
    async *chat(req) {
        const res = await this.fetchImpl(`${this.baseUrl}/chat`, {
            method: "POST",
            headers: this.headers({ "Content-Type": "application/json" }),
            body: JSON.stringify({
                message: req.message,
                session_id: req.sessionId,
                resume_queue: req.resumeQueue || undefined,
                tools: req.tools,
            }),
            signal: req.signal,
        });
        if (!res.ok || !res.body) {
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        }
        if (!req.autoResume) {
            yield* parseSSE(res.body);
            return;
        }
        const { retries = 5, delayMs = 1000 } = req.autoResume === true ? {} : req.autoResume;
        let sessionId = req.sessionId;
        let last;
        let body = res.body;
        for (let attempt = 0;;) {
            try {
                for await (const frame of parseSSE(body)) {
                    if (frame.session_id)
                        sessionId = frame.session_id;
                    if (frame.session_seq)
                        last = frame.session_seq;
                    attempt = 0;
                    yield frame;
                    if (frame.type === "metadata")
                        return;
                }
            }
            catch (err) {
                if (req.signal?.aborted)
                    throw err;
            }
            // 没收到 metadata 就断了：以最后的 session_seq 续传。
            if (!sessionId || last === undefined)
                return;
            for (;;) {
                if (attempt++ >= retries)
                    throw new GoAgentError("SSE 连接中断且续传失败", 0);
                await sleep(delayMs, req.signal);
                try {
                    body = await this.openStream(sessionId, { after: last, signal: req.signal });
                    break;
                }
                catch (err) {
                    if (req.signal?.aborted)
                        throw err;
                }
            }
        }
    }
    /**
     * 会话事件流续传：GET /runs/{session}/stream。先补发 session_seq > after 的帧，再转实时。
     * 续传位置早于服务端缓冲时首帧为 resync。非 follow 模式下会话空闲后以 metadata 结束。
     */
    async *resume(sessionId, opts = {}) {
        yield* parseSSE(await this.openStream(sessionId, opts));
    }
    async openStream(sessionId, opts) {
        const q = new URLSearchParams();
        if (opts.after !== undefined)
            q.set("after", String(opts.after));
        if (opts.follow)
            q.set("follow", "1");
        const qs = q.toString();
        const res = await this.fetchImpl(`${this.baseUrl}/runs/${encodeURIComponent(sessionId)}/stream${qs ? `?${qs}` : ""}`, {
            headers: this.headers(),
            signal: opts.signal,
        });
        if (!res.ok || !res.body) {
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        }
        return res.body;
    }
    // ---------- 交互原语回传 ----------
    /**
     * 权限审批决定（permission_request 帧的 request_id）。
     * 批准时可用 `input` 替换工具参数（完整替换，按工具的入参 schema 校验；不记为始终允许）。
     */
    async approve(requestId, allow, opts) {
        await this.post("/approve", {
            request_id: requestId,
            allow,
            always_allow: opts?.alwaysAllow,
            reason: opts?.reason,
            session_id: opts?.sessionId,
            input: opts?.input,
        });
    }
    /** 回答引擎提问（ask_user 帧）。 */
    async answer(requestId, answer) {
        await this.post("/askuser", { request_id: requestId, answer });
    }
    /**
     * 回传前端工具的执行结果（chat({tools}) 声明的工具，收到对应 tool_start 帧后调用）。
     * error 非空表示执行失败（作为工具错误交给模型）；等待中的运行即刻继续。
     */
    async toolResult(sessionId, toolCallId, content, error) {
        await this.post("/chat/tool-result", { session_id: sessionId, tool_call_id: toolCallId, content, error });
    }
    /** 会话中等待前端执行结果的工具调用（断线重连后补齐）。 */
    async pendingTools(sessionId) {
        const res = await this.fetchImpl(`${this.baseUrl}/pending-tools?session_id=${encodeURIComponent(sessionId)}`, {
            headers: this.headers(),
        });
        if (!res.ok)
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        const body = (await res.json());
        return body.tools ?? [];
    }
    /** 计划确认（plan_confirm 帧；该帧当前服务端不产生，保留）。 */
    async confirmPlan(requestId, confirm, reason) {
        await this.post("/plan/confirm", { request_id: requestId, confirm, reason });
    }
    /** 终止指定会话正在执行的 run。 */
    async interrupt(sessionId, reason) {
        await this.post("/interrupt", { session_id: sessionId, reason });
    }
    /** 会话当前待处理的审批（断线重连后补齐 permission_request）。 */
    async pendingApprovals(sessionId) {
        const res = await this.get(`/pending-approval?session_id=${encodeURIComponent(sessionId)}`);
        return res.items ?? [];
    }
    /** 会话当前待回答的提问（只返回最新一条；断线重连后补齐 ask_user）。 */
    async pendingAsk(sessionId) {
        const res = await this.get(`/pending-ask?session_id=${encodeURIComponent(sessionId)}`);
        if (!res.pending || !res.request_id)
            return null;
        return { request_id: res.request_id, question: res.question ?? "", payload: res.payload };
    }
    // ---------- 插话与排队（服务端需 WithSteering） ----------
    /**
     * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；此刻不可插话时进入排队车道。
     * 会话空闲时这条消息会开启新的一轮 run——本方法读尽该流并返回 steered=false。
     * 需要消费空闲时的流式输出请直接用 chat()。
     */
    async steer(sessionId, message) {
        let steered = false;
        let queuedId;
        for await (const frame of this.chat({ message, sessionId })) {
            if (frame.type === "metadata") {
                steered = !!frame.steered;
                if (frame.queued)
                    queuedId = frame.text;
            }
        }
        return { steered, queuedId };
    }
    /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回排队项 ID 与积压数。 */
    async enqueue(sessionId, message) {
        return this.post("/queue", { message, session_id: sessionId });
    }
    /** 查看排队车道（队头在前）。 */
    async queue(sessionId) {
        const res = await this.get(`/queue?session_id=${encodeURIComponent(sessionId)}`);
        return res.items ?? [];
    }
    /** 撤回一条尚未消费的排队消息。已消费或不存在时抛出 404。 */
    async removeQueued(sessionId, itemId) {
        const res = await this.post("/queue/remove", { session_id: sessionId, item_id: itemId });
        return res.text;
    }
    // ---------- 子代理 ----------
    /** 会话中的子代理（运行中与最近 5 分钟内结束的）。 */
    async agents(sessionId) {
        return this.get(`/agents?session_id=${encodeURIComponent(sessionId)}`);
    }
    /** 单个子代理的快照与结束后保留的对话记录。 */
    async agent(sessionId, agentId) {
        return this.get(`/agents/${encodeURIComponent(agentId)}?session_id=${encodeURIComponent(sessionId)}`);
    }
    /** 给运行中的子代理发指示：在它的下一个工具批边界生效。子代理不在运行时抛出 404。 */
    async steerAgent(sessionId, agentId, message) {
        await this.post("/queue", { session_id: sessionId, agent_id: agentId, message });
    }
    /** 只中断该子代理：父运行收到"被用户中断"的结果后继续。子代理不在运行时抛出 404。 */
    async interruptAgent(sessionId, agentId, reason) {
        await this.post("/interrupt", { session_id: sessionId, agent_id: agentId, reason });
    }
    // ---------- 运行时控制 ----------
    /** 权限模式（default / accept_edits / auto / bypass / plan / deny_all）。传 sessionId 时只作用于该会话。 */
    async getMode(sessionId) {
        const q = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : "";
        return (await this.get(`/mode${q}`)).mode;
    }
    /** 设置权限模式；sessionId 非空且 mode 为空时清除该会话的设置。 */
    async setMode(mode, sessionId) {
        await this.post("/mode", { mode, session_id: sessionId });
    }
    // ---------- REST ----------
    async health() {
        return this.get("/health");
    }
    /** 协议自描述（版本 / 帧类型表 / 端点表），客户端做兼容性校验用。 */
    async protocol() {
        return this.get("/protocol");
    }
    async tools() {
        return this.get("/tools");
    }
    async sessions() {
        return this.get("/sessions");
    }
    /** 会话完整消息历史（前端重开项目或断线重连时的回放源）。 */
    async messages(sessionId) {
        return this.get(`/sessions/${encodeURIComponent(sessionId)}/messages`);
    }
    async tasks(sessionId) {
        return this.get(sessionId ? `/tasks?session_id=${encodeURIComponent(sessionId)}` : "/tasks");
    }
    async task(taskId, sessionId) {
        const q = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : "";
        return this.get(`/tasks/${encodeURIComponent(taskId)}${q}`);
    }
    async plan() {
        return this.get("/plan");
    }
    async bgTasks() {
        return this.get("/bgtasks");
    }
    async stopBgTask(id) {
        await this.post(`/bgtasks/${encodeURIComponent(id)}/stop`, {});
    }
    async usage() {
        return this.get("/usage");
    }
    // ---------- HTTP 底座 ----------
    headers(extra = {}) {
        return this.token ? { ...extra, Authorization: `Bearer ${this.token}` } : extra;
    }
    async get(path) {
        const res = await this.fetchImpl(`${this.baseUrl}${path}`, { headers: this.headers() });
        if (!res.ok)
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        return res.json();
    }
    async post(path, body) {
        const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
            method: "POST",
            headers: this.headers({ "Content-Type": "application/json" }),
            body: JSON.stringify(body),
        });
        if (!res.ok)
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        return res.json();
    }
}
/**
 * 解析 SSE 字节流为信封序列。只处理 data: 行（本协议不使用事件名行
 * 与多行 data），容忍跨 chunk 分帧与 CRLF 行尾；注释行（: ping 心跳）与
 * id 行（续传端点）跳过。
 */
export async function* parseSSE(body) {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
        while (true) {
            const { done, value } = await reader.read();
            if (done)
                break;
            buf += decoder.decode(value, { stream: true });
            let nl;
            while ((nl = buf.indexOf("\n")) >= 0) {
                const line = buf.slice(0, nl).replace(/\r$/, "");
                buf = buf.slice(nl + 1);
                if (!line.startsWith("data:"))
                    continue;
                try {
                    yield JSON.parse(line.slice(5).trimStart());
                }
                catch {
                    // 坏帧跳过（半截 JSON / 非协议行）
                }
            }
        }
    }
    finally {
        reader.releaseLock();
    }
}
/** 可中止的等待（续传重试间隔）。 */
function sleep(ms, signal) {
    return new Promise((resolve, reject) => {
        if (signal?.aborted)
            return reject(signal.reason);
        const t = setTimeout(resolve, ms);
        signal?.addEventListener("abort", () => {
            clearTimeout(t);
            reject(signal.reason);
        }, { once: true });
    });
}
