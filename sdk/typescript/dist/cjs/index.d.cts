/**
 * goagent-client — GoAgent HTTP/SSE 协议的 TypeScript 客户端。
 *
 * 与 GoAgent 引擎（RunHTTP）通信的零依赖客户端：对话 SSE 流式消费
 * （统一信封 Envelope：字符串 type / 连接内单调 seq / 协议版本 v）、
 * 断线续传（session_seq + resume() / chat({ autoResume })）、
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
import type { Envelope, Description, QueueItem, SessionSummary, SubagentInfo } from "./types.js";
export { FrameType, PROTOCOL_VERSION } from "./types.js";
export type { Envelope, ChatRequest, Description, QueueItem, SessionSummary, SubagentInfo } from "./types.js";
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
    /**
     * 断线自动续传：连接在收到 metadata 之前中断（网络错误或流意外结束）时，以最后收到的
     * session_seq 调用 resume() 接着读，调用方看到的仍是一条连续的帧序列（期间可能出现 resync 帧）。
     * true 等价于 { retries: 5, delayMs: 1000 }。
     */
    autoResume?: boolean | {
        retries?: number;
        delayMs?: number;
    };
}
/** resume() 选项。 */
export interface ResumeOptions {
    /** 最后收到的 session_seq。省略时从进行中运行的开头读起；0 表示从服务端缓冲中最旧的帧读起。 */
    after?: number;
    /** true 时跨运行持续推送直到 signal 中止；否则会话空闲且已读到最新时以 metadata 结束。 */
    follow?: boolean;
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
export declare class GoAgentError extends Error {
    readonly status: number;
    constructor(message: string, status: number);
}
export declare class GoAgentClient {
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    readonly baseUrl: string;
    private readonly token?;
    private readonly fetchImpl;
    constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    baseUrl: string, opts?: ClientOptions);
    /**
     * 流式对话：POST /chat，逐帧 yield 统一信封。末帧恒为 metadata。
     * 首帧通常是 run_start；会话忙时（插话、排队、拒绝）不开新 run，没有 run_start。
     * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，可经 resume() 续传或 messages() 回放。
     */
    chat(req: ChatOptions): AsyncGenerator<Envelope, void, undefined>;
    /**
     * 会话事件流续传：GET /runs/{session}/stream。先补发 session_seq > after 的帧，再转实时。
     * 续传位置早于服务端缓冲时首帧为 resync。非 follow 模式下会话空闲后以 metadata 结束。
     */
    resume(sessionId: string, opts?: ResumeOptions): AsyncGenerator<Envelope, void, undefined>;
    private openStream;
    /**
     * 权限审批决定（permission_request 帧的 request_id）。
     * 批准时可用 `input` 替换工具参数（完整替换，按工具的入参 schema 校验；不记为始终允许）。
     */
    approve(requestId: string, allow: boolean, opts?: {
        alwaysAllow?: boolean;
        reason?: string;
        sessionId?: string;
        input?: Record<string, unknown>;
    }): Promise<void>;
    /** 回答引擎提问（ask_user 帧）。 */
    answer(requestId: string, answer: string): Promise<void>;
    /** 计划确认（plan_confirm 帧；该帧当前服务端不产生，保留）。 */
    confirmPlan(requestId: string, confirm: boolean, reason?: string): Promise<void>;
    /** 终止指定会话正在执行的 run。 */
    interrupt(sessionId: string, reason?: string): Promise<void>;
    /** 会话当前待处理的审批（断线重连后补齐 permission_request）。 */
    pendingApprovals(sessionId: string): Promise<PendingApproval[]>;
    /** 会话当前待回答的提问（只返回最新一条；断线重连后补齐 ask_user）。 */
    pendingAsk(sessionId: string): Promise<PendingAsk | null>;
    /**
     * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；此刻不可插话时进入排队车道。
     * 会话空闲时这条消息会开启新的一轮 run——本方法读尽该流并返回 steered=false。
     * 需要消费空闲时的流式输出请直接用 chat()。
     */
    steer(sessionId: string, message: string): Promise<SteerResult>;
    /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回排队项 ID 与积压数。 */
    enqueue(sessionId: string, message: string): Promise<{
        id: string;
        pending: number;
    }>;
    /** 查看排队车道（队头在前）。 */
    queue(sessionId: string): Promise<QueueItem[]>;
    /** 撤回一条尚未消费的排队消息。已消费或不存在时抛出 404。 */
    removeQueued(sessionId: string, itemId: string): Promise<string>;
    /** 会话中的子代理（运行中与最近 5 分钟内结束的）。 */
    agents(sessionId: string): Promise<SubagentInfo[]>;
    /** 单个子代理的快照与结束后保留的对话记录。 */
    agent(sessionId: string, agentId: string): Promise<{
        agent: SubagentInfo;
        messages: unknown[];
    }>;
    /** 给运行中的子代理发指示：在它的下一个工具批边界生效。子代理不在运行时抛出 404。 */
    steerAgent(sessionId: string, agentId: string, message: string): Promise<void>;
    /** 只中断该子代理：父运行收到"被用户中断"的结果后继续。子代理不在运行时抛出 404。 */
    interruptAgent(sessionId: string, agentId: string, reason?: string): Promise<void>;
    /** 权限模式（default / accept_edits / auto / bypass / plan / deny_all）。传 sessionId 时只作用于该会话。 */
    getMode(sessionId?: string): Promise<string>;
    /** 设置权限模式；sessionId 非空且 mode 为空时清除该会话的设置。 */
    setMode(mode: string, sessionId?: string): Promise<void>;
    health(): Promise<{
        status: string;
        tools: number;
    }>;
    /** 协议自描述（版本 / 帧类型表 / 端点表），客户端做兼容性校验用。 */
    protocol(): Promise<Description>;
    tools(): Promise<Array<{
        name: string;
        description: string;
        permission: string;
        concurrent: boolean;
    }>>;
    sessions(): Promise<SessionSummary[]>;
    /** 会话完整消息历史（前端重开项目或断线重连时的回放源）。 */
    messages(sessionId: string): Promise<unknown[]>;
    tasks(sessionId?: string): Promise<unknown[]>;
    task(taskId: string, sessionId?: string): Promise<unknown>;
    plan(): Promise<{
        active: boolean;
        state: string;
        file_path: string;
        content: string;
    }>;
    bgTasks(): Promise<unknown[]>;
    stopBgTask(id: string): Promise<void>;
    usage(): Promise<unknown>;
    private headers;
    private get;
    private post;
}
/**
 * 解析 SSE 字节流为信封序列。只处理 data: 行（本协议不使用事件名行
 * 与多行 data），容忍跨 chunk 分帧与 CRLF 行尾；注释行（: ping 心跳）与
 * id 行（续传端点）跳过。
 */
export declare function parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<Envelope, void, undefined>;
