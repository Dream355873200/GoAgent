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
import type { Envelope, Description } from "./types.js";
export type { Envelope, ChatRequest, Description } from "./types.js";
/** 对话选项。 */
export interface ChatOptions {
    /** 用户消息（必填）。 */
    message: string;
    /** 会话 ID；不传由服务端生成。多轮对话请固定同一个 ID。 */
    sessionId?: string;
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
export declare class GoAgentError extends Error {
    readonly status: number;
    constructor(message: string, status: number);
}
export declare class GoAgentClient {
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    readonly baseUrl: string;
    constructor(
    /** 引擎地址（如 http://127.0.0.1:8420）。 */
    baseUrl: string);
    /**
     * 流式对话：POST /chat，逐帧 yield 统一信封。
     * 首帧恒为 run_start、末帧恒为 metadata（steered=true 表示消息经
     * 插话通道注入了正在运行的 run，本轮不会有更多事件）。
     * 消费中断（abort/异常）不会终止引擎侧任务——任务继续跑完落盘，
     * 可经 sessions.messages() 回放。
     */
    chat(req: ChatOptions): AsyncGenerator<Envelope, void, undefined>;
    /** 权限审批决定（ask_user/permission_request 帧的 request_id）。 */
    approve(requestId: string, allow: boolean, opts?: {
        alwaysAllow?: boolean;
        reason?: string;
        sessionId?: string;
    }): Promise<void>;
    /** 回答引擎提问（ask_user 帧）。 */
    answer(requestId: string, answer: string): Promise<void>;
    /** 计划确认（plan_confirm 帧）。 */
    confirmPlan(requestId: string, confirm: boolean, reason?: string): Promise<void>;
    /** 终止指定会话正在执行的 run。 */
    interrupt(sessionId: string, reason?: string): Promise<void>;
    /**
     * 插话：会话忙时消息在最近的工具批结束边界注入当前 run；空闲时等价
     * 于普通消息。返回 steered=true 表示已注入当前 run。
     */
    steer(sessionId: string, message: string): Promise<{
        steered: boolean;
    }>;
    /** 排队消息：当前 run 结束后自动作为下一条输入续跑。返回积压数。 */
    enqueue(sessionId: string, message: string): Promise<number>;
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
    sessions(): Promise<Array<{
        id: string;
        title?: string;
        state: string;
        updated_at?: string;
    }>>;
    /** 会话完整消息历史（前端重开项目时的回放源）。 */
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
    private get;
    private post;
}
