# GoAgent v0.13 升级指南：架构收敛

本文面向使用 GoAgent 的项目，说明从 v0.12 升级到 v0.13 时需要知道的一切：哪些行为变了、要改哪些代码、新增了哪些 API。

v0.13 完成了 [v0.12 升级指南第 5 节](UPGRADE-v0.12.md#5-已知限制与后续路线不在本次升级范围内)列出的架构路线：

- 子运行（pipeline、团队、子 agent）统一使用主循环实现，并从所在运行结构性地继承检查链；
- 对话记录与发给模型的视图分开；
- 会话级状态收拢到会话运行时；
- 清理死代码与业务耦合；
- 拆分包结构的第一步。

清理过程中还发现并修复了几处缺陷（见 [1.7](#17-其他行为变化缺陷修复)）。

还没有升级到 v0.12 的项目，先读 [UPGRADE-v0.12.md](UPGRADE-v0.12.md)，再读本文。**需要宿主确认或改代码的变化集中在第 1 节**，先看第 0 节的速查表判断你是否受影响。

---

## 目录

- [0. 三分钟速查](#0-三分钟速查)
- [1. 需要处理的行为变化](#1-需要处理的行为变化)
- [2. 新增 API](#2-新增-api)
- [3. 验证情况与建议的宿主侧验证](#3-验证情况与建议的宿主侧验证)
- [4. 后续路线](#4-后续路线)

---

## 0. 三分钟速查

| 如果你的项目…… | 需要做什么 | 详见 |
|---|---|---|
| 调用 `app.RunCLI()` | 改为 `cli.Run(app)`（导入 `github.com/Dream355873200/GoAgent/cli`），或保留 `RunCLI` 并加上 `import _ ".../cli"` | [1.1](#11-终端界面移到-cli-包) |
| 自己持久化 `EventDone.Messages` 或 `WithTranscriptSink` 收到的列表 | 不用改代码；列表现在是完整对话记录（含摘要记录），展示时过滤 `IsMeta` | [1.2](#12-对话记录与模型视图分开) |
| 自己读会话存储展示历史 | 过滤 `IsMeta` 消息（新的摘要记录也是元消息） | [1.2](#12-对话记录与模型视图分开) |
| 在宿主工具里直接用 `agent.NewRunner(...).Run` 派生子 agent | 需要审批的工具标上 `agent.ToolDef.Permission`；核对继承语义 | [1.3](#13-子运行统一使用主循环实现并继承检查链) |
| 用 `App.AgentTools` 给子 agent 配工具，且依赖这些工具全部并行 | 注册时标 `Concurrent: true`，否则在子 agent 里串行执行 | [1.3](#13-子运行统一使用主循环实现并继承检查链) |
| 自定义 Observer 统计会话数或用量 | 子运行不再发会话开始/结束事件；子运行的工具与用量事件现在会到达 | [1.3](#13-子运行统一使用主循环实现并继承检查链) |
| 依赖"始终允许"只在当前一轮有效 | 现在在同一会话内持续有效；撤销用 `SessionGrants(id).Clear()` 或 `CloseSession` | [1.4](#14-会话运行时会话级状态集中管理) |
| 多个用户共用一个 App，并用 `SetPermissionMode` / `POST /mode` 切换模式 | 只切换某个会话时改用 `SetSessionPermissionMode` / `POST /mode` 带 `session_id` | [1.4](#14-会话运行时会话级状态集中管理) |
| 会删除会话 | 删除时同时调用 `app.CloseSession(id)` | [1.4](#14-会话运行时会话级状态集中管理) |
| 依赖多次 `Run` / `RunWithHistory` 之间共享 `Context.Store` 的数据 | 改用真实会话（`RunWithSessionHistory` / `RunSession`） | [1.4](#14-会话运行时会话级状态集中管理) |
| 想把 `.yume` 换成自己的目录名 | `goagent.WithStateDir(".myapp")` | [1.5](#15-状态目录名可配置) |
| 用到了本版已删除或标为 Deprecated 的 API | 已删除的见 1.6 第一张表（直接删除，编译报错即说明有残留调用）；仍保留 Deprecated 的见第二张表 | [1.6](#16-已删除的-api与仍保留的废弃-api) |
| 用过 `app.Group(...).Use(mw)` | 分组中间件现在真正生效（此前从未执行），确认行为符合预期 | [1.7](#17-其他行为变化缺陷修复) |

不在表中的项目一般无需改动，直接升级即可。

---

## 1. 需要处理的行为变化

### 1.1 终端界面移到 cli 包

- **之前**：根包 `goagent` 引入 `tui` 与 Bubble Tea。所有嵌入方，包括只用 HTTP 或 SDK 的服务，都会链接终端界面库并执行它们的初始化。
- **现在**：终端界面在 `github.com/Dream355873200/GoAgent/cli`，根包不再依赖 bubbletea、bubbles、lipgloss（有测试防止回退）。`App.RunCLI()` 仍然保留：导入 cli 包后照常工作；没有导入时，在标准错误输出提示后立即返回。
- **迁移**：

```go
import (
    "github.com/Dream355873200/GoAgent"
    "github.com/Dream355873200/GoAgent/cli"
)

func main() {
    app := goagent.New(...)
    cli.Run(app) // 替代 app.RunCLI()
}
```

也可以保留 `app.RunCLI()`，只加一行 `import _ "github.com/Dream355873200/GoAgent/cli"`。

cli 包只使用 App 的公开 API。为此 App 新增了 `SetApprover`、`SetSessionManager`、`MemoryDir`，以及包级函数 `SetAskUserCallback`，宿主同样可以使用。

### 1.2 对话记录与模型视图分开

- **之前**：循环用同一个消息列表既做完整对话记录，又做发给模型的视图，自动摘要直接替换它。这带来两个问题：
  - `EventDone.Messages` 和 `WithTranscriptSink` 收到的是压缩后的视图。自己管存储的宿主把它存回去，原始对话就永久丢失了。
  - 使用会话存储（`RunSession`）的宿主每次从存储加载完整记录，第一轮都会因为超过阈值，再请求一次模型摘要。长会话里，每条用户消息都会多一次摘要调用。
- **现在**：
  - 循环维护只追加的完整对话记录。自动摘要不改写它，而是追加一条**摘要记录**，和其他消息一样经 `OnMessage` 落盘。摘要记录的字段：`IsCompactSummary`；`CompactBoundary`，指向摘要之后保留的第一条消息；同时是元消息 `IsMeta`。
  - 发给模型的视图由 `compaction.Project(记录)` 得到：最后一条摘要记录，加上它边界之后的全部消息。重新加载会话、同一事件流里的排队续跑、`RunWithHistory` 传回历史，都直接还原视图，不再请求模型摘要。
  - `EventDone.Messages`、`WithTranscriptSink` 收到的列表都是完整对话记录（含摘要记录）。
- **迁移**：
  - 自己持久化这些列表的宿主不用改代码：存下来，下一轮原样传回即可。注意存下的列表比以前长（不再被压缩）。
  - 展示历史时过滤 `IsMeta` 消息。`GET /sessions/{id}/messages` 已经过滤。
  - 会话存储的 JSON 新增 `is_compact_summary`、`compact_boundary` 两个字段。旧数据里没有摘要记录，行为与之前一致：需要时摘要一次，之后记录下来。
- **限制**：压缩后重注入的提醒（`WithPostCompactReminder`）只出现在发生压缩的那次运行里，不随摘要记录保存，重新加载时不会重放。

### 1.3 子运行统一使用主循环实现，并继承检查链

- **之前**：主循环、pipeline 节点、团队成员、子 agent 是四套各自装配的执行实现。
  - 子 agent（`agent.Runner`）是一套手写循环。工具在裸 goroutine 里并发执行，没有上限，一次工具 panic 会带崩整个进程。它也没有上下文压缩和输出截断续写，不尊重工具的并发声明。
  - 团队成员的循环没有 observer、日志与 debug，也不认 `WithToolInputValidation`。
  - 权限门、plan 模式、hooks、中间件要在每一处手动接线。宿主工具里直接用 `agent.Runner` 派生的子 agent，工具调用不经任何检查。
- **现在**：
  - 每个循环把自己的作用域随 ctx 交给每一次工具执行。作用域包括：
    - 检查链：权限门、plan 模式、hooks、中间件；
    - 入参校验开关；
    - 观测出口：observer、日志、debug、会话 ID；
    - 主 provider 与后备。

    在工具里派生的子循环，包括 pipeline 节点与 supervisor、团队队长与成员、子 agent，经同一个入口继承这个作用域。子运行只能和父运行一样严格，继承由结构保证，不再靠逐处接线。
  - `agent.Runner` 改用主循环实现，公开 API 不变。它获得了：有界并发（默认 10）、工具 panic 恢复为错误结果、微压缩加自动摘要、max_tokens 截断续写、限流重试；与所在运行使用同一个 provider 时，还沿用它的后备模型。
  - `agent.ToolDef` 新增两个字段：`Permission`（权限级别，零值为只读）和 `Serial`（不与同一轮的其他调用并行，默认并行）。`App.AgentTools` 自动填入工具注册时的权限级别与并发声明。
  - 子运行的工具调用和模型用量现在进入 App 的 observer，成本统计包含子运行。子运行不再发 `OnSessionStart` / `OnSessionEnd`，因为它们不是会话。
  - 子 agent 里的工具经 `Context.Progress` 上报的状态行，以及其中再派生的子 agent 的进度，会转发到所在运行的事件流（与旧实现一样可见）。
  - 主循环兼容只发完整消息、不发增量事件的 provider（例如把非流式接口包装成 Stream 的实现）：从完整消息补出文本，工具调用照常派发。此前这类 provider 的工具调用会被忽略。
- **迁移与注意**：
  - 宿主工具里直接构造 `agent.ToolDef` 的子 agent，在某次运行之内调用时，现在会过该运行的检查链。没标 `Permission` 的工具按只读处理，默认模式下自动放行，但 deny 规则、Guard 和显式 ask 规则同样作用于它们。需要审批的工具请标上 `Permission`。
  - 经 `App.AgentTools` 取得的工具，注册时没有 `Concurrent: true` 的，在子 agent 里改为串行执行（与主循环一致；此前全部并行）。不经 `agent.Runner` 直接调用它们的 `Execute` 时，只做入参校验，不过检查链。
  - `Definition.Provider` 与 `NewRunner` 的默认 provider 都为空时，子 agent 沿用所在运行的 provider（此前报错）。
  - 子 agent 达到 `MaxTurns` 时仍然返回已有结果，不算失败（与之前一致）。失败时的错误信息由"API 调用失败"改为"运行失败"。
  - v0.12 给 `teams.Config` 新增的 `Permission` / `PlanChecker` / `Hooks` / `Middlewares` 字段已经移除，团队的检查链改为从 ctx 继承。

### 1.4 会话运行时：会话级状态集中管理

- **之前**：会话级状态分散在 App 的多个 `sync.Map` 上，权限门每次运行新建。这导致：
  - "始终允许"只在当前这一次运行（一条用户消息）内有效；
  - `SetPermissionMode`、`AddPermissionGuard`、`RemovePermissionGuard` 只作用于最近启动的那一次运行，多个会话并发时，其他在跑的运行不受影响；
  - 没有会话身份的运行（`Run`、`RunWithHistory`）共享同一份工具键值存储，一次运行通过 `Context.Store` 写入的数据，另一次运行能读到；
  - 会话结束后，这些状态永远不释放。
- **现在**：每个会话有一个运行时，集中保存工具键值存储、压缩管理器、"始终允许"授权与会话级权限模式。
  - "始终允许"在同一会话的后续运行中持续有效，不跨会话。`app.SessionGrants(id)` 可以查看（`Keys`）和撤销（`Clear`），对进行中的运行立即生效。
  - App 登记所有进行中运行的权限门。全局 `SetPermissionMode` 作用于所有在跑运行（模式来自会话级设置的除外）；Guard 的增删作用于所有在跑运行。
  - 新增会话级权限模式：`SetSessionPermissionMode(id, mode)`、`ClearSessionPermissionMode(id)`、`SessionPermissionMode(id)`。优先级从高到低：会话级运行时设置 > `WithSessionPermissionMode` 的解析结果 > 全局运行时覆写（`SetPermissionMode`）> `WithPermissionMode`。
  - HTTP：`POST /mode` 支持 `session_id` 字段，只切换该会话；带 `session_id` 而 `mode` 为空，表示撤销会话级设置。`GET /mode?session_id=` 返回该会话当前生效的模式。
  - 没有会话身份的运行各自使用独立的运行时，彼此隔离。
  - `app.CloseSession(id)` 释放会话在内存中的状态（对话历史在会话存储里，不受影响）。
- **迁移**：
  - 宿主删除会话时调用 `app.CloseSession(id)`。
  - 多个用户共用一个 App 的服务，把"用户切换模式"改为按会话切换（`POST /mode` 带 `session_id`）。全局切换会影响所有用户的运行。
  - 依赖多次 `Run` 之间共享 `Context.Store` 数据的宿主，改用真实会话。

### 1.5 状态目录名可配置

- `.yume` 此前硬编码在约 10 处。现在统一由新的 `statedir` 包管理，默认仍是 `.yume`，用 `goagent.WithStateDir(".myapp")` 或 `statedir.Set` 修改。
- 这是进程级设置：内置工具、skill 发现、worktree 等子系统不经过 App 配置，同样按它寻址。同一进程中的多个 App 应使用相同的值。名字必须是单个路径段，不合法时 `New` 会 panic。
- 位于这个目录下的内容：
  - 会话存储（`RunCLI` / `RunHTTP` 自动创建时）；
  - 计划文件、后台任务输出；
  - 项目 skill（`<目录>/commands`）、worktree；
  - 会话记忆、动态 pipeline 快照。
- 数据已在 `.yume` 下的项目，不改配置即可。

### 1.6 已删除的 API 与仍保留的废弃 API

**已删除（本版直接删）**。下列公开 API 要么没有接入任何执行路径（调用不产生任何效果），要么已被更正确的实现取代。两个宿主项目（amobileCreater engine、AnimeCreater）经验证均未使用：

| 删除的 API | 原因与替代方案 |
|---|---|
| `permission.ParseToolPattern`、`MatchToolCall`、`ToolPattern` | 被活的 `RuleSet` 取代（按从入参提取的命令、路径匹配）。旧函数按入参 JSON 原文做前缀匹配，根本匹配不上 |
| `permission.AIClassifier`、`NewAIClassifier`、`AIClassifierConfig`、`ClassificationResult` | 接不进权限门；实际分类走内置 YOLO 分类器（`PermissionAuto` 模式，或 bypass 模式下的高风险调用，`WithYoloClassifier` 配置） |
| `compaction.CompactableTools`（registry 变量）、`IsCompactable`、`RegisterCompactableTool` | 注册不产生任何效果；微压缩的工具范围见活字段 `MicrocompactConfig.CompactableTools`（默认 `DefaultCompactableTools`） |
| `compaction.CompactBoundaryIndex`、`MessagesAfterCompactBoundary`、`MarkCompactBoundary`、`BoundaryMetadata` | 框架已不产生 `IsCompactBoundary` 消息；改用 `compaction.Project`（摘要记录 + UID 边界投影） |
| `agent.AgentContext` | 未被框架使用，子 agent 上下文由 Runner 内部管理 |
| `goagent.InternalPlanConfirmRequest` | 未被使用；计划确认请求见 `PlanConfirmRequest` |
| `tools` 包（工具自动降级） | 从未接入执行路径，且其中间件接口与 `goagent.Middleware` 不兼容，恢复需重写而非去注释 |

**保留废弃（标 `Deprecated`，不删）**。下列公开 API 独立可用，只是框架自身不消费；哪天需要，去掉注释即可恢复：

| 保留废弃的 API | 替代方案 |
|---|---|
| `provider.StreamingFallbackProvider`、`RetryStreamProvider`、`DefaultFallbackConditions`、`FallbackCondition` | `goagent.WithFallback`（仅整请求切换；流中已输出内容后失败的无缝切换是本族独特能力） |
| `provider.FastModeProvider` 及其配置 | 无（轻任务自动切小模型） |
| `executor.TrackedExecutor`、`TrackedTool`、`ToolState` | `executor.StreamingExecutor`（合成 tool_result 与有界并发已被 loop 内建吸收；剩余独特价值是外部观测工具状态机） |
| `agent.QueryTracker`、`QueryContext` 等查询追踪类型 | observer 包 |
| `compaction.RunForkedAgent`、`DefaultForkedAgent` 等 | `App.AgentTool` / `agent.Runner` |
| `compaction.FileTracker` | `WithPostCompactReminder` |
| `compaction.GenerateToolUseSummary` 等工具调用摘要函数 | 无 |

另外删除了 internal 包里没有接线的代码，外部代码本来就无法使用，不受影响：
- 已被 `SuspendGate` 取代的 `ResumableLoop` / `PauseManager`；
- `Withholder`、`Preprocessor`、`DetachToolSink`；
- 只写不读的 `Transition` 状态（含 `TransMaxOutputEscalate`）；
- 若干未使用的非导出函数。

### 1.7 其他行为变化（缺陷修复）

清理死代码时发现了以下缺陷，一并修复：

- **`app.Group(names...).Use(mw)` 的中间件此前从未执行**：分组没有登记到 App，也没有任何代码读取它。现在 `Use` 等同 `app.Use(mw, WithTools(names...))`。如果你用过分组，这些中间件现在会真正生效，请确认它们的行为符合预期。
- **benchmark 的 `LLMJudge` 此前没有发送裁判 system prompt**：判分纪律（只依据准则、拿不准判 FAIL）和"首行只能是 PASS/FAIL"的输出格式，都没有交给模型。现在会发送，判分可能比之前更严格。
- **手动压缩（`compaction.Compact` / `Manager.Compact`）的微压缩此前什么都没做**：它按 tool_result 块上的工具名判断"可压缩工具"，而 tool_result 块不带工具名。现在按对应的 tool_use 反查工具名。同时改为写时复制，不再就地改写调用方的消息。
- **图片结果回收为占位符的几处问题**：
  - 路径提取此前只认 Windows 盘符路径，现在也支持 POSIX 路径；
  - 只在图片标记之后的正文里查找路径，base64 数据里的 "/" 不会被误当成路径；
  - 占位文案不再引用宿主特有的 `vision_ask` 工具。

### 1.8 builtin 系工具缺导入从静默失效改为显式警告

builtin 系工具的实现由 `builtin` 包的 `init()` 向框架注册（打破 `goagent → builtin → goagent` 循环依赖）。
此前宿主若忘了导入 builtin，`WithBuiltinTools` / `WithTaskTools` / `WithIssueTools` / `WithPlanTools` /
`WithBgTaskTools` / `WithAskTools` 全部静默无效——开关"看起来开了"，工具一个都没注册，运行期没有任何提示。

现在这 6 个开关在 provider 缺失时，`New()` 会通过注入的 logger 发出 Warn 日志，内容包含修复指引
（加空白导入 `_ "github.com/Dream355873200/GoAgent/builtin"`）。回归测试见根包 `builtinwarn_fix_test.go`
（缺 provider 时 6 条警告齐全、provider 在场时无误报）。

文档同步：README 两个入口示例补上了缺失的 builtin 空白导入（最简示例此前照抄会静默无工具）；
配置表标注了 ★ 最小集（10 个高频选项）与 ⚠️ builtin 系标注；新增「运行入口怎么选」决策表。

---

## 2. 新增 API

| API | 位置 | 说明 |
|---|---|---|
| `cli.Run(app)` | `cli` | 交互式终端界面（原 `App.RunCLI` 的实现） |
| `RegisterCLIRunner` | `goagent` | 供 cli 包注册 `RunCLI` 的实现 |
| `App.SetApprover`、`App.SetSessionManager`、`App.MemoryDir`、`SetAskUserCallback` | `goagent` | 运行时设置审批者、会话管理器；读取记忆目录；设置内置 AskUser 工具的提问回调 |
| `App.CloseSession`、`App.SessionGrants` | `goagent` | 释放会话的内存状态；查看与撤销会话的"始终允许"授权 |
| `App.SetSessionPermissionMode`、`App.ClearSessionPermissionMode`、`App.SessionPermissionMode` | `goagent` | 会话级权限模式 |
| `POST /mode` 的 `session_id`、`GET /mode?session_id=` | HTTP | 按会话切换与查询权限模式 |
| `WithStateDir` | `goagent` | 设置状态目录名 |
| `statedir.Default`、`Name`、`Set`、`Join` | `statedir` | 状态目录名的统一出口 |
| `permission.Grants`、`NewGrants`、`Gate.SetGrants`、`Gate.Grants`、`Gate.Mode` | `permission` | 可共享的"始终允许"授权集合；读取权限门的当前模式 |
| `agent.ToolDef.Permission`、`agent.ToolDef.Serial` | `agent` | 子 agent 工具的权限级别与并发声明 |
| `compaction.Project` | `compaction` | 从对话记录得到模型视图 |
| `message.Message.CompactBoundary`；`IsCompactSummary` 改为序列化 | `message` | 摘要记录的字段 |

---

## 3. 验证情况与建议的宿主侧验证

框架侧（本次升级已完成）：

- `go build ./...`、`go vet ./...` 通过，包括 `examples/` 下的示例与新的 cli 包。
- `go test -race ./...` 全部通过，无数据竞争。
- 新增的回归测试：
  - 根包：`cleanup_fix_test.go`、`runner_fix_test.go`、`sessionruntime_fix_test.go`、`transcript_fix_test.go`、`cli_fix_test.go`；
  - 其他包：`agent/runner_test.go`、`internal/loop/projection_test.go`、`compaction/cleanup_fix_test.go`、`benchmark/judge_fix_test.go`、`statedir/statedir_test.go`。
- 以下关键测试做过反向验证（把修复改回旧逻辑后测试失败）：子循环的结构性继承、会话内共享授权、启动时的投影、分组中间件、裁判 system prompt。
- `staticcheck` 的未使用代码检查（U1000）已清零，只剩一处有意保留并加了说明的测试字段。

建议宿主升级后检查：

1. **长会话**：让会话发生一次自动摘要，重启服务后再发一条消息。确认没有额外的摘要请求（日志里只有一次模型请求），历史回放仍然完整。
2. **子 agent**：在宿主工具里派生子 agent 的，确认需要审批的工具出现了审批请求；用 `AgentTools` 配工具的，确认并发行为符合预期。
3. **多用户服务**：按会话切换模式（`POST /mode` 带 `session_id`），确认其他会话不受影响；删除会话时调用了 `CloseSession`。
4. **CLI**：改为 `cli.Run(app)`（或导入 cli 包）后，确认启动正常。
5. **用量报表**：成本统计现在包含子运行的用量，与之前的数字比较时注意口径变化。

---

## 4. 后续路线

- **继续拆分包结构**：把 HTTP 服务（`http.go` 与各个 handler）、pipeline / teams 编排逐步移入子包，根包只保留 App 核心。HTTP 服务依赖大量 App 内部状态，需要先在 App 上整理出稳定的扩展接口。
- **压缩记录的覆盖范围**：摘要记录目前只覆盖 L4 自动摘要。L0–L3（预算截断、裁剪、微压缩、折叠）是确定性的，每次运行从记录重新计算。压缩后重注入的提醒暂不持久化。
- **会话运行时的空闲回收**：目前由宿主调用 `CloseSession`，后续可增加按空闲时间自动回收。
- **可插拔的权限分类器接口**：在 `permission.Gate` 上开放分类器注入点（宿主可换自己的小模型/prompt），
  吸收已删除的 `ClassificationResult`（allow/deny/ask + 理由）结构设计——这是删除 `permission.AIClassifier` 时唯一值得回收的概念。
  "宿主自定义可压缩工具"无需新增机制，活字段 `MicrocompactConfig.CompactableTools` 已覆盖。
- **删除仍保留 Deprecated 的 provider / executor / compaction / agent API**（见 1.6 第二张表），或确认某族有真实需求后接线。
- **目录沙箱仍不是安全边界**（同 v0.12）：需要真正的隔离时，等待容器级的 Tier 2，或在部署层面隔离进程。
