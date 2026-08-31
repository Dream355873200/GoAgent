# GoAgent 项目全解析（面试准备 · 中心文档）

> 目的：支撑你在秋招面试中**讲清楚这个项目的每一个机制**。
> 本文是地图——大框架 + 每个功能区块的定位 + 一句话要点；
> 具体机制深潜见 `deep/` 目录的引用文档（每个机制一篇）。
>
> 阅读方式：先通读本篇建立全局图 → 面试前按 JD 重点翻对应深潜文档。

---

## 一句话定位（电梯陈述）

**GoAgent 是一个 Go 语言的 AI Agent 框架**——把「LLM 循环 + 工具调用 + 编排」封装成可嵌入的库，让业务系统（如 AI 短剧生成平台）几十行代码就能拥有一个 Claude Code 式的智能体运行时。区别于 LangChain（太底层）/ eino（偏编排 SDK）：**GoAgent 是"整车"——沙箱、权限、压缩、计费、服务化内建，开箱即用**。

面试开场可以这样讲：*"我独立开发了一个 Go 的 Agent 框架，它驱动着我们 AI 短剧生成平台里所有的 LLM 智能体——从剧本创作的对话 agent，到资产提取的 DAG 流水线，到最近做的工具沙箱。核心解决了三个问题：多会话隔离、长任务编排、不受信代码执行。"*

---

## 整体架构：一张图

```
┌────────────────────────────────────────────────────────────┐
│                       宿主应用（AnimeCreater 等）            │
│         HTTP/WS 前端 ←→ 业务层（会话/通知/审批 UI）           │
├────────────────────────────────────────────────────────────┤
│  App（goagent.go）—— 统一入口，Option 模式装配              │
│    ├── Provider 抽象（OpenAI 兼容/Anthropic，可切后备）      │
│    ├── Agent 循环（internal/loop）—— 核心引擎                │
│    │     LLM 调用 → 工具调用 → 结果回灌 → 直到收敛           │
│    ├── 工具体系（tool.go + builtin/）                        │
│    │     InferTool 反射生成 schema；Read/Write/Edit/Bash...  │
│    ├── Executor + 权限门（executor + permission）           │
│    │     PreCheck 链：权限 → Plan → 中间件 → Hooks          │
│    ├── Pipeline（pipeline.go）—— DAG 任务流水线             │
│    ├── 子 Agent（agent/）—— LLM 现场委派                    │
│    ├── 沙箱（sandbox*.go）—— 工具执行层隔离                  │
│    └── 运行时生态：会话/记忆/压缩/预算/观察者/MCP/cron...     │
├────────────────────────────────────────────────────────────┤
│  落地形态（同一内核四种皮）：HTTP+SSE / WebSocket / TUI / CLI │
└────────────────────────────────────────────────────────────┘
```

**架构分层的关键决策**（面试常问"为什么这么分层"）：
- **App 是装配器不是执行器**——Option 模式收集配置，run() 时快照后交给 loop，保证配置变更不影响进行中的执行（读写锁语义）
- **loop 是唯一引擎**——单 agent、子 agent、pipeline 节点最终都跑同一个循环，只是注入的配置不同（复用大于抽象）
- **一切经事件流**——agent 与外界通信只有事件（Event），宿主消费什么、怎么展示全由宿主决定（观测与业务解耦）

---

## 功能区块地图（每个区块 = 面试可讲的独立故事）

### 1. 核心循环与事件流 —— 深潜 [deep/core-loop.md](deep/core-loop.md)

引擎的本体：`LLM 生成 → 需要工具就执行 → 结果喂回 → 直到给出最终回答`。

- **要点**：事件是唯一通信方式（TextDelta/ToolStart/ToolDone/Error/Done）；流式输出靠 TextDelta 增量；同步版 Execute 是事件流的语法糖
- **可讲的细节**：为什么事件流而不是回调（可组合/可中断/可序列化）；429 限流的自动重试与倒计时状态行（StatusKey 原地更新语义）
- **踩过的坑**：消费侧 `=` 赋值丢增量（TextDelta 必须累加）——修过的一个真 bug

### 2. 工具体系 —— 深潜 [deep/tools.md](deep/tools.md)

Agent 的"手"。`InferTool` 从 Go 函数签名**自动生成** JSON Schema。

- **要点**：结构体 tag 即 schema（`json` 字段名 / `desc` 给 LLM 的说明 / `required`）；工具三档权限（ReadOnly 自动放行 / Normal 首次询问 / Dangerous 每次询问）
- **可讲的细节**：文件工具对标 Claude Code 的细节——未读拒绝编辑（防凭记忆编辑）、覆盖保护（防凭印象覆盖）、诊断式报错（未命中时给相似行线索）、二进制检测、超长行截断、同会话未修改短路
- **为什么**：工具质量决定 agent 上限——LLM 的行为强依赖工具描述与反馈质量

### 3. 多会话架构 —— 深潜 [deep/sessions.md](deep/sessions.md)

单进程服务多用户的地基。

- **要点**：SessionID + WorkDir 经 context value 流经全链路（loop→executor→工具的 `ctx.WorkDir`）；`WithSessionWorkDir(fn)` 按会话解析工作目录——多会话多根目录互不串
- **可讲的细节**：SSE 断开≠取消（后台 ctx 解耦，网络闪断不误杀任务）；HTTP 层两会话并行实测（626ms vs 串行 1200ms）；任务存储按会话分区
- **踩过的坑**：pipeline 节点工具的 WorkDir 恒空（主循环注入了、pipeline 路径漏了）——做沙箱时发现的真实缺陷，补了 PipelineConfig.SessionID

### 4. Pipeline DAG 编排 —— 深潜 [deep/pipeline.md](deep/pipeline.md)

确定性多阶段任务的编排引擎（AnimeCreater 的剧本→资产→分镜流水线）。

- **要点**：节点是 **agent**（不是函数）——"任务流水线"而非"数据变换网"（对照 eino graph 的本质区别）；DependsOn 声明依赖 + 消息队列派活 + Concurrency 并发 worker；拓扑排序调度
- **可讲的细节**：MessageFunc 惰性求值（构造时上游还没跑，注入的资产清单是空的——真实踩坑后设计）；Review 审核打回 + RetryPolicy（Rollback 整批重做 vs Amend 增量补齐的决策依据）；错误严格传播（部分产出也算失败，不留静默残缺）
- **动态 Pipeline（dynpipeline.go）**：LLM 运行时自建 DAG——JSON DSL + 校验矩阵全 LLM 可读（环检测带环上节点序列、工具不存在附可用清单）——这是对 eino 的差异化优势（组件编译期插、LLM 没有手；我们的工具运行时按名选）

### 5. 沙箱体系 —— 深潜 [deep/sandbox.md](deep/sandbox.md)

工具执行层的隔离（近期核心工作，面试重点）。

- **要点**：agent 对世界的全部作用力都经过工具调用——拦住这一个执行面，单 agent/pipeline/benchmark 三种模式全覆盖（都汇入 ToolDef.call 同一咽喉点）；Tier 0-3 强度阶梯
- **可讲的细节**：设计决策——策略层物化进 `Context.Sandbox` 字段而非 Execute 包装器（进程内包装拦不住任何东西，真实执行面是路径解析与子进程派生）；DirSandbox（临时根+词法逃逸捕获）vs WorktreeSandbox（git worktree 天然可回滚，多会话并发安全——拆了原 worktree 包的全局 chdir）；路径白名单最长前缀优先、无匹配拒绝；防意外不防恶意的诚实边界声明
- **关联愿景**：L4 沙箱代码执行（goja 语言级隔离）/ L5 工具生成的三闸门安全设计——都能讲出完整的攻击面分析与防御层次

### 6. 权限与审批 —— 深潜 [deep/permissions.md](deep/permissions.md)

- **要点**：三档权限级别 + 5 种模式 + Approver 可注入（CLI 交互询问 / SDK 自动拒绝 / 业务接前端审批 UI）；PreCheck 链在工具执行前统一拦截
- **可讲的细节**：权限门是链式（权限→Plan 模式→中间件→Hooks）；中断的一等语义规划（EventInterrupted——用户终止与报错分离）

### 7. 上下文治理 —— 深潜 [deep/context-mgmt.md](deep/context-mgmt.md)

长对话不爆 token 的三件套。

- **要点**：**压缩**（四层压缩 + 阈值触发 + Circuit Breaker 防压缩风暴）、**记忆**（跨会话持久化 + 会话内定期提取）、**会话持久化**（逐条即时落盘 JSONL，进程被杀也能从断点恢复——重开续聊）
- **可讲的细节**：为什么逐条即时落盘而不是整轮结束才写（崩溃窗口的数据损失）；429 等待期间每秒推送倒计时的状态行机制复用到审批挂起

### 8. 可观测性与成本 —— 深潜 [deep/observability.md](deep/observability.md)

- **要点**：Observer 接口 11 钩子（工具/权限/压缩/会话/token 用量）；预算系统（token 上限 + 成本追踪 + 超限熔断）；pipeline 的 OnEvent 把节点级进度/思考/错误透出给宿主
- **待补缺口**（诚实讲也有价值）：LLM 调用级钩子（OnLLMStart/Done）、ctx span 关联——已在 TODO 排第一梯队

### 9. 落地形态与集成 —— 深潜 [deep/serving.md](deep/serving.md)

- **要点**：同一内核四种皮（HTTP+SSE / WebSocket / TUI / CLI）；`RunWithHistory` 的嵌入哲学——**历史归业务（存你的数据库），循环归框架**
- **可讲的细节**：AnimeCreater 怎么嵌的（agent_message 表 + RunWithHistory + WS 推事件 + 前端三入口共用事件处理）

### 10. 路线图与设计思考 —— 深潜 [deep/roadmap.md](deep/roadmap.md)

TODO 区本身就是设计能力的展示。

- **可讲的**：动态 Pipeline L1-L5 阶梯（受限→自愈→完全→沙箱执行→工具生成）；benchmark 自迭代闭环（含防过拟合三防线：benchmark 只增不删/环境隔离/机器可判停止条件）；与 eino/LangGraph 的对比方法论（eino 造引擎 vs 我们造整车；LangGraph = agent 版 Temporal，学零件不学产品）

---

## 高频面试问题速答卡

**Q: 为什么自己造轮子不用 LangChain/eino？**
A: 三层理由——LangChain 太底层（搭个 agent 要拼一堆组件）；eino 是编排框架偏 SDK（服务化/沙箱/权限都要自己搭，且我们遇到过 bug 响应慢）；我们的场景需要的是"运行时"——多会话隔离、长任务编排、文件级工具、服务化内建。选型时判断：Go 生态里"模型无关 + 开箱即用 + 完整运行时"这个生态位是空的。

**Q: 项目里最难的技术决策？**
A: 沙箱的形态选择。一开始想做 Execute 包装器（把工具调用包一层），推演后发现进程内包装在逻辑上拦不住任何东西——真正的执行面是路径解析和子进程派生。改为"策略层物化进 Context"：沙箱会话挂在 context value 上流到每个工具的 ctx.Sandbox 字段，内置工具在路径解析处强制。这个设计让单 agent/pipeline/benchmark 三种模式一处接线全覆盖（它们都汇入同一个 ToolDef.call 咽喉点）。

**Q: 遇过最深的 bug？**
A: 三个可以讲——① pipeline 节点工具的 Context.WorkDir 恒空：主循环注入会话上下文，pipeline 路径漏了，根因是"两条执行路径共享机制但注入点不对称"；② 消费侧把 TextDelta 增量事件当全量赋值，子 agent 的汇总静默残缺；③ GORM 审计 callback 对批量 Create 的 slice 调反射取值直接 panic 杀进程。共同教训：**多路径共享机制时，注入点必须在"唯一咽喉点"而不是各路径分别注入**。

**Q: 并发安全怎么处理的？**
A: App 的配置快照模式（run 启动时持读锁快照，之后无锁读）；会话存储/task store 全方法加锁（曾有多会话并行必现 data race）；pipeline 的节点状态用独立 mutex；日志桥接层防并发写。

**Q: 项目规模和你的角色？**
A: 全部独立设计开发——框架本体 + 文档（教程/架构文档/TODO 路线图）+ 在 AI 短剧平台的生产验证。规模：~40 个包、核心 pipeline.go 单文件 1800 行、测试覆盖沙箱/工具/流水线/HTTP 并行等关键路径。

---

## 使用建议

1. **通读本篇**直到能不看稿讲出电梯陈述和架构图
2. **按 JD 选深潜**：投 Agent/LLM 岗 → 全部；投后端岗 → core-loop/pipeline/sessions/sandbox 四篇够了
3. **面试前一晚**只看速答卡 + 每篇深潜开头的"要点"段
4. 讲述时**带踩坑故事**——"我修过一个 bug"比"我实现了一个功能"可信十倍
