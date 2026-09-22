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
    constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    baseUrl) {
        this.baseUrl = baseUrl;
    }
    // ---------- 对话（SSE 流式） ----------
    /**
     * 流式对话：POST /chat，逐帧 yield 统一信封。
     * 首帧恒为 run_start、末帧恒为 metadata（steered=true 表示消息经
     * 插话通道注入了正在运行的 run，本轮不会有更多事件）。
     * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，
     * 可经 sessions.messages() 回放。
     */
    async *chat(req) {
        const res = await fetch(`${this.baseUrl}/chat`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ message: req.message, session_id: req.sessionId }),
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
    async approve(requestId, allow, opts) {
        await this.post("/approve", {
            request_id: requestId,
            allow,
            always_allow: opts?.alwaysAllow,
            reason: opts?.reason,
            session_id: opts?.sessionId,
        });
    }
    /** 回答引擎提问（ask_user 帧）。 */
    async answer(requestId, answer) {
        await this.post("/askuser", { request_id: requestId, answer });
    }
    /** 计划确认（plan_confirm 帧）。 */
    async confirmPlan(requestId, confirm, reason) {
        await this.post("/plan/confirm", { request_id: requestId, confirm, reason });
    }
    /** 终止指定会话正在执行的 run。 */
    async interrupt(sessionId, reason) {
        await this.post("/interrupt", { session_id: sessionId, reason });
    }
    // ---------- 插话通道 ----------
    /**
     * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；空闲时等价
     * 于普通消息。返回 steered=true 表示已注入当前 run。
     */
    async steer(sessionId, message) {
        const res = await this.post("/chat", { message, session_id: sessionId });
        return { steered: !!res.steered };
    }
    /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回积压数。 */
    async enqueue(sessionId, message) {
        const res = await this.post("/queue", { message, session_id: sessionId });
        return res.pending;
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
    /** 会话完整消息历史（前端重开项目时的回放源）。 */
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
    async get(path) {
        const res = await fetch(`${this.baseUrl}${path}`);
        if (!res.ok)
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        return res.json();
    }
    async post(path, body) {
        const res = await fetch(`${this.baseUrl}${path}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
        });
        if (!res.ok)
            throw new GoAgentError(await res.text().catch(() => res.statusText), res.status);
        return res.json();
    }
}
/**
 * 解析 SSE 字节流为信封序列。只处理 data: 行（本协议不使用事件名行
 * 与多行 data），容忍跨 chunk 分帧。
 */
async function* parseSSE(body) {
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
                const line = buf.slice(0, nl).trim();
                buf = buf.slice(nl + 1);
                if (!line.startsWith("data: "))
                    continue;
                try {
                    yield JSON.parse(line.slice(6));
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
