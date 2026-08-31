# 深潜 3：Pipeline DAG 编排

> 位置：`pipeline.go`（1782 行，全库最大单文件）+ `dynpipeline.go`
> 回答的问题：多阶段任务怎么编排？和别家的 graph 有什么本质不同？

## 心智模型：任务流水线，不是数据变换网

这是面试最重要的对比点。pipeline 节点是 **agent**（带 Instruction/Tools
的 LLM 循环），不是函数组件：

```
eino graph：节点 = 函数（数据→数据），拼「数据变换网」（RxJS 同类）
LangGraph：图 = 控制流状态机（agent⇄tools 环 + checkpoint）
GoAgent pipeline：节点 = agent（任务→自主执行），拼「任务流水线」（Temporal 同类）
```

AnimeCreater 实例：剧本解析 → 资产提取 → 分镜生成 → 审查，每个节点是
一个有职责的 agent，工序间传工单。

## 核心结构

```go
PipelineConfig{
    Nodes: []PipelineNode{{
        Name: "extract",
        Agent: &PipelineAgentDef{Name, Instruction, Tools, Provider}, // 劳动力
        DependsOn:   []string{"parse"},   // 依赖：parse 全部完成才启动
        Injects:     []string{"enrich"},  // 可以往 enrich 的队列推任务
        Concurrency: 3,                   // 3 个并行 worker 消费队列
        Message:     "...",               // 无上游生产者时的初始任务
        MessageFunc: func(ctx) string,    // 惰性求值的 Message
        Review:      true,                // supervisor 审核打回
        MaxRetries:  3, RetryPolicy: ...,
    }},
    Supervisor:        ...,  // 上帝节点（审核）
    TransactionFactory: ..., // 打回时的事务回滚
    OnEvent:           ...,  // 节点事件透出（progress/thinking/error）
}
```

**每个节点都建消息队列**——上游用工具（`GetMessageQueue(ctx, "downstream")`
→ `q.Push(task)`）往下游队列推任务，worker 从队列消费。**边静态、流动态**：
拓扑 run 前定死，哪个边上有多少消息是运行时决定的。

## 四个深挖点（每个都是踩坑换来的）

### ① MessageFunc 惰性求值

**坑**：Message 在构造 PipelineConfig 时固化，而内容依赖上游产出——
构造时上游还没跑，注入的「资产清单」是空的，下游每个 worker 白花一轮
LLM 调用去重查。

**解**：MessageFunc 在节点真正被调度那一刻（DependsOn 全满足、worker
启动前）求值——拿到的是上游产出后的真实状态。DAG 里 Message 是
**启动前快照**这个认知本身就是踩坑结晶。

### ② Review 审核与 RetryPolicy

supervisor 打回（reject）时怎么处置本轮已产出的数据？

```
RetryRollback（默认）：事务回滚，整批从零重做
  → 适合「产出是整体结构」：分集划分的边界是一个整体决策
RetryAmend：保留已产出，审核意见推回增量补齐
  → 适合「产出是可累积集合」：资产提取打回「漏了X」，整批销毁去补X
    是负期望重试——LLM 重跑不确定，这次补上X可能漏掉Y
```

面试点：这个选择**不该全局一刀切**，按节点产出性质逐个声明——展示
"重试不是免费的"的工程认知。

### ③ 错误严格传播

早期版本：节点 LLM 报错但有部分工具产出时**静默当成功**——残缺产出
流向下游且无日志。改为一律 recordNodeError → 取消整条 pipeline →
RunPipeline 返回错误。原则：**错误一律传播，不留静默残缺**；上游已落库
的部分由业务事务/补偿逻辑负责。

### ④ 节点工具的会话上下文缺口（真实 bug）

主循环在 App.run 注入 `WithSessionContext(ctx, sessionID, workDir)`，
pipeline 的节点 context 组装**漏了这步**——节点工具的 `ctx.WorkDir/
SessionID` 恒空。做沙箱时（沙箱靠同一 context 机制传递）发现，补
`PipelineConfig.SessionID` 修复。教训：**多路径共享机制的注入点必须在
唯一咽喉点，不能各路径分别注入**。

## 动态 Pipeline（dynpipeline.go）——差异化优势

LLM 运行时自建 DAG：`create_pipeline` 工具收 JSON DSL → BuildDynPipeline
校验 → 执行。

**校验矩阵全 LLM 可读**（这是设计核心——拼图人是 LLM，报错要能自纠）：

```
空 nodes → "nodes 不能为空"
节点数 >12 → "任务该拆得更粗，或改用子 agent"（LLM 易犯的错不是
             拓扑错误，是拆太碎——每节点一次 LLM 调用，成本爆炸）
工具不存在 → 附【可用工具清单】
depends_on 悬空 → 指名道姓哪个节点引用了谁
循环依赖 → DFS 环检测，报出环上节点序列 "a → b → c → a"
```

对照 eino：组件编译期插（LLM 没有手），我们运行时按名选（LLM 现场建图）。
L1 已实现；L2 自愈（失败重规划）/L3 完全（自建 tool）在路线图。

## Supervisor 设计定稿（TODO，可讲思考过程）

移交给**控制流**模式（对齐 eino）：transfer = 对话控制权移交（完整消息
历史+身份切换，落点在 loop 的消息列表交接）。v1 曾设计成队列数据流
（推工单），评审废弃——**为架构统一牺牲语义正确性是框架设计的经典陷阱**
（协作的主流是接力不是工单分发，baggage 摘要补丁的存在说明模型选错了）。
队列数据流保留给 pipeline 节点间任务分发 + 路由节点（分拣员：看任务
类型决定进哪条传送带）。

**分工一句话**：协作（接力推进同一件事）走控制流 transfer；分发（批量
任务按类型奔向 worker）走数据流路由节点。
