# 深潜 5：多会话架构（sessions）

> 位置：`context.go`（会话上下文）+ `http.go`（多会话并行）+ session/ task/ 包
> 回答的问题：一个进程怎么同时服务多个用户/项目而互不干扰？

## 核心机制：会话身份经 context value 流经全链路

```
App.run 启动时：
  workDir = WithSessionWorkDir 的解析函数(sessionID)  // 每会话独立根目录
  ctx = WithSessionContext(ctx, sessionID, workDir)   // 塞进 context value
    ↓ 沿执行链流动
loop → executor → ToolDef.call → newContextFromStd()
    ↓ 从 context value 提升为结构体字段
工具拿到 ctx.SessionID / ctx.WorkDir
    ↓
Bash 用 WorkDir 设 cmd.Dir；Read/Write/Edit 相对路径按 WorkDir 解析；
task 存储按 SessionID 分区路由
```

**为什么这样设计**：会话身份是"横切信息"——每个工具都可能需要，但
不该让每个工具签名都加参数。context value 是 Go 的标准做法，提升为
`goagent.Context` 字段是给工具的便利门面（还能挂 Sandbox/Progress/Store）。

## 三个验证过的能力

### ① HTTP 层多会话并行（集成测试实测）

两个会话同时 /chat，各自两轮 LLM 调用（每轮 300ms mock 延迟）：
并行 626ms vs 串行 1200ms。同时验证工具拿到的 SessionID/WorkDir
按会话正确注入（G1 全链路断言）。

### ② SSE 断开 ≠ 取消

```
请求进后台 ctx 跑任务（与 SSE 连接解耦）：
  网络闪断/用户关页面 → 连接断，任务继续跑完落库
  用户点终止按钮 → POST /interrupt → InterruptHandler 按会话路由 cancel
```

区分"意外断开"和"主动终止"——误杀比慢更糟。被中断的工具会写回
「工具执行被用户中断」的 tool_result（消息序列完整，同会话可续聊）。

### ③ 会话持久化：逐条即时落盘

loop 的 FinalMessages 每出现一条新消息立刻 AppendMessage（JSONL），
而不是整轮结束才写。**为什么**：进程被杀（用户关应用）时已完成的
步骤全部保留——重开从断点恢复对话与执行进度。崩溃窗口的数据损失
从"整轮"缩到"单条"。

配套哲学：`RunWithHistory` —— **历史归业务（存你的数据库），
循环归框架**。AnimeCreater 就是 agent_message 表 + RunWithHistory。

## Task 系统的会话分区

task.SessionStore 按会话隔离任务存储：ForSession 分区路由 + DirFn
落盘（进程重启恢复）+ IdleTTL 闲置回收（惰性逐出+磁盘恢复）。
工具层 TaskCreate/Update/Get/List 经 storeFor(ctx) 自动路由到
会话分区——宿主无感。

## 踩过的坑（都是真实修过的）

1. **session.MemoryStore 并发 race**：多会话并行必现 data race——
   全方法加锁修复。教训：Go 的 map 并发写零容忍，Store 类组件
   从第一天就该加锁
2. **pipeline 节点的会话上下文缺口**：主循环注入 WithSessionContext，
   pipeline 的 runNode 组装节点 ctx 时漏了——节点工具 WorkDir 恒空。
   做沙箱时（沙箱靠同一机制传递）发现。修复：PipelineConfig.SessionID
   + RunPipeline 注入。**教训：多路径共享机制的注入点必须在唯一
   咽喉点**（这是全库反复出现的主题）
3. **per-tab 多账号**（AnimeCreater 侧）：token 迁 sessionStorage、
   per-user 偏好加 uid 前缀、agent 对话缓存按账号隔离——单进程
   多身份的完整实践
