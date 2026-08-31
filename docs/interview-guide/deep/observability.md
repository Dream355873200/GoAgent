# 深潜 8：可观测性与成本（observability）

> 位置：`observer/` + `cost/` + `budget/` + `analytics/` + PipelineConfig.OnEvent
> 回答的问题：agent 黑盒跑起来后，宿主怎么知道它干了什么、花了多少？

## Observer：11 个业务钩子

```go
type Observer interface {
    OnTokenUsage(...)              // token 用量+美元成本（计费直接消费）
    OnToolStart / OnToolDone / OnToolError(ctx, name, ...)  // 工具调用轨迹
    OnPermissionRequest / Granted / Denied  // 权限决策审计
    OnCompaction(...)              // 压缩事件（治理监控）
    OnSessionStart / OnSessionEnd  // 会话生命周期
    OnError(...)                   // 错误
}
```

配套件：`NopObserver`（默认零开销）、`MultiObserver`（广播多消费者）、
`SyncObservable`（并发安全包装）。注入方式：`WithObservers(obs...)`。

**设计哲学：事件由 agent 循环主动 push，不靠 ctx 切面**——和 eino 的
callbacks（组件 OnStart/OnEnd + ctx 传播）是两种流派。我们的钩子偏
**业务语义**（权限/压缩/成本），eino 偏**组件生命周期**。

**已知缺口（TODO 第一梯队，面试诚实讲反而加分）**：
- LLM 调用级钩子缺失——只有 OnTokenUsage 摘要，没有每次模型调用的
  in/out/耗时（做 tracing 的视角里模型调用是黑洞）→ 补 OnLLMStart/Done/Error
- 钩子配对无结构保证——OnToolStart/Done 靠宿主自己配对 → ctx span
  关联（Start 时 ctx 塞调用 ID，Done 取出）

## Pipeline 的 OnEvent：节点事件透出

pipeline 内部对宿主默认是黑盒（runLightweight 只消费文本/工具/错误
事件）。`PipelineConfig.OnEvent` 把每个节点 worker 的 loop 事件转成
PipelineEvent（progress/thinking/error）透出——AnimeCreater 用它做
节点进度条和 429 倒计时显示。

曾修的坑：Progress/Thinking 事件在 runLightweight 的事件 switch 里被
静默丢弃——嵌入方无法展示节点的限流等待。修复后 progress 带 StatusKey
（原地更新语义，同一状态行刷新倒计时不刷屏）。

## 成本与预算

```
cost/    单价表驱动的成本计算（OnTokenUsage 的美元数字来源）
budget/  token 预算：总量/会话级，超限熔断
analytics/ 使用分析（可选）
```

AnimeCreater 侧的实战：生成任务的成本归集（哪个项目/哪个节点烧了
多少钱）、后台监控页的点数流水——observer 是唯一的取数出口，
一套钩子喂业务侧的计费+监控+审计三个系统。

## 落地形态：HTTP/SSE、WebSocket、TUI、CLI

同一内核四种皮——落地层只做"事件格式转换"（SSE 帧/JSON 帧/终端渲染），
引擎零改动。这是事件流架构（见 core-loop.md）的直接回报。

**HTTP 层细节**：`newHTTPMux` 可测试化拆分；sseEvent 带 session_id
（客户端按会话路由事件——多会话并行时事件不串线）；/tasks 支持
?session_id= 路由。
