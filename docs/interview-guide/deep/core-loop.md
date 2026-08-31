# 深潜 1：核心循环与事件流（core loop）

> 位置：`internal/loop/loop.go` + `goagent.go` 的 App.run
> 回答的问题：agent 引擎到底是怎么转的？

## 执行链全貌

```
app.Run(ctx, input)
  → App.run()（goagent.go:746）
      持读锁快照配置（prompt/tools/middlewares）——之后无锁
      组装：system prompt、权限门、压缩管理器、记忆、observer...
      注入会话上下文（SessionID + WorkDir → context value）
  → loop.New(cfg).Run(ctx, input)（loop.go）
      返回 event channel，循环跑：
      ┌─────────────────────────────────────────┐
      │  1. 组装消息（历史 + system + 本轮输入）  │
      │  2. 调 Provider（流式）                   │
      │  3. 解析响应：纯文本 → 结束；工具调用 → 4 │
      │  4. 逐个执行工具（经 Executor + PreCheck）│
      │  5. 工具结果回灌为 tool_result 消息       │
      │  6. 回到 1，直到收敛或达到 MaxTurns       │
      └─────────────────────────────────────────┘
  → 每一步都发事件到 channel（唯一的对外通信方式）
```

## 事件类型（event.go）

| 事件 | 语义 | 消费注意 |
|------|------|---------|
| TextDelta | 文本增量（一个 chunk） | **必须 `+=` 累加**——当全量赋值会丢内容（修过的真 bug） |
| ToolStart / ToolDone | 工具调用边界 | 配对算耗时（待做：ctx span 关联） |
| Thinking | 思考模型推理过程 | 前端渲染折叠块 |
| Progress | 进度/状态行 | StatusKey 非空 = 原地更新语义 |
| Error / Done | 终态 | Done 携带完整消息列表（历史收集钩子） |

## 两个值得讲的机制细节

### ① 429 限流的自动重试与状态行

```
Provider 返回 HTTP 429
  → 识别为 RateLimitError（区别于过载 OverloadError——
    速率限制切备用模型无济于事，必须等待）
  → 重试最多 10 次，等待对齐 TPM 窗口（15s 起步递增至 60s）
  → 等待期间【每秒】推送带 StatusKey 的 Progress 事件
  → 前端按 StatusKey 原地更新："✻ 429 · Retrying in 23s · attempt 3/10"
```

面试点：**状态行原地更新语义**——不是每次推送新消息（会把对话刷爆），而是按 key 替换同一条状态。对齐 Claude Code 的 429 显示。

### ② 同步 Execute 是事件流的语法糖

`app.Execute(ctx, input)` 内部还是跑 Run，只是帮你收集 TextDelta 拼成 FinalText。说明架构上事件流是底层真相，同步接口是便利层——这个分层让"嵌入式消费事件"和"脚本式拿结果"两种用法共用一套引擎。

## 为什么用事件流而不是回调（高频追问）

- **可组合**：channel 可以被中间层拦截、过滤、再分发（HTTP 层转 SSE、WS 层转 JSON 帧都是消费方式差异）
- **可中断**：ctx cancel 后循环自然退出，事件流尾巴携带终态
- **可序列化**：事件是数据，落盘回放（benchmark 的事件全量落盘设计建立在这上面）
- 对比回调：回调把控制流交给了注册者，事件流保持"生产者-消费者"解耦

## 已知的边界（诚实清单）

- 无 provider 时发可诊断 EvtError（曾有 nil pointer panic 崩进程的 bug，加了守卫）
- 用户主动终止目前走 Error 通道（待做 EventInterrupted 一等事件——TODO 第一梯队第一项）
