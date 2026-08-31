# 深潜 9：子 Agent 与落地集成（serving）

> 位置：`agent/`（子 Agent）+ `http.go`（HTTP/SSE）+ `websocket/` + AnimeCreater 集成实践
> 回答的问题：任务太大一个 agent 干不完怎么办？业务系统怎么嵌入？

## 子 Agent：LLM 现场委派

```
主 agent 的工具箱里有一个特殊工具：「启动子agent」
  → LLM 判断这个子任务不归我 / 需要专门角色 → 调工具
  → 子 agent 独立循环（自己的 system prompt、工具集、MaxTurns）
  → 结果返回主 agent 继续
```

Definition：Name + Description（给主 agent LLM 看的"何时找我"）+
SystemPrompt + Tools + MaxTurns。对齐 Claude Code 的子 agent 架构。

**三个概念的分界**（面试易混，分清了很加分）：
```
SubAgent  = 委派行为（运行时，LLM 决定叫谁）—— "老板派活"
AgentTool = 组合零件（构造时，开发者钉位置）—— TODO：把 agent 包成
            ToolDef 的薄适配器，解锁 agent 进 pipeline 当普通节点
Supervisor= 调度协议（路由表+移交语义打包）—— TODO，按 eino 控制流
            模式（transfer=对话控制权移交，非队列推工单）
```

**pipeline vs SubAgent 的判断准则**（README 原话，很成熟）：
依赖关系是**任务固有**的（先提取→再润色→再审核）用 Pipeline；
需要 agent **现场判断**的用 SubAgent。Pipeline 不是更高级，是另一种场景。

## RunWithHistory：嵌入哲学

```go
history := 从数据库加载(sessionID)
for ev := range app.RunWithHistory(ctx, history, input) {
    if ev.Type == EventDone {
        history = ev.Messages   // 本轮完整消息列表（含 assistant 回复）
        存数据库(history)
    }
}
```

**历史归业务，循环归框架**——框架不管持久化格式（你的数据库你做主），
只负责拼接与续跑。对比框架托管会话（自动 JSONL 落盘）：托管省事但
格式绑架业务；自管多两行代码换完全自由。两种都支持，嵌入推荐自管。

## AnimeCreater 的真实嵌入（生产验证故事）

```
后端（Go/Gin）：
  agent_message 表存对话历史 + RunWithHistory 续跑
  WebSocket 推事件 → 前端按类型分发
  三个 AI 入口共用一套事件处理（AI 绘画对话 / 创意工坊 / 剧本创作）
    ——事件流抽象的直接回报：加第三个入口时后端处理逻辑零改动
  pipeline 驱动：剧本→资产提取→分镜生成 的 DAG 流水线
    （supervisor 审核 + 事务回滚 + 批量进度透出）

前端（React/TS）：
  useAgentChat/useCreativeDirector/useScriptWriter 三个 hook
    同构封装（thinking 折叠块 / 429 状态行 / 工具调用展示）
```

**能讲的规模感**：平台全部 LLM 智能体（对话类 + 流水线类）跑在这个
框架上；框架的每次升级（429 状态行、thinking 透出、事件面补齐）
都在生产环境验证。

## MCP 与运行时生态

- **MCP**（mcp/）：标准协议接外部工具服务器（WithMCP 配置）——
  生态互操作，别人的工具服务器直接变 agent 能力
- **cron**：定时任务
- **bgtask**：后台任务（RegisterAgent/RegisterShell + 独立 cancel 链路）
- **filehistory**：文件修改历史（配合 Edit 工具的版本追踪）
- 这些是"运行时生态"论据：对比纯 SDK 框架（eino），我们连运行时
  配套都给了——"整车 vs 引擎"对比的具体支撑
