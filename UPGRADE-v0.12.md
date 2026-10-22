# GoAgent v0.12 升级指南：稳定性与安全加固

本文面向使用 GoAgent 的项目，说明从 v0.11（及更早版本）升级到 v0.12 时需要知道的一切：哪些行为变了、要改哪些配置、新增了哪些 API、修复了哪些问题。

v0.12 是一次集中修复，覆盖核心循环、工具执行、会话持久化、上下文压缩、权限与审批、HTTP 服务、Pipeline / 团队 / 子 agent、Provider、记忆系统。绝大多数修复对调用方透明，升级后直接生效；**需要宿主确认或改配置的行为变化集中在第 1 节**，先看第 0 节的速查表判断你是否受影响。

---

## 目录

- [0. 三分钟速查](#0-三分钟速查)
- [1. 需要处理的行为变化](#1-需要处理的行为变化)
- [2. 新增 API](#2-新增-api)
- [3. 修复清单（按模块）](#3-修复清单按模块)
- [4. 验证情况与建议的宿主侧验证](#4-验证情况与建议的宿主侧验证)
- [5. 已知限制与后续路线（不在本次升级范围内）](#5-已知限制与后续路线不在本次升级范围内)

---

## 0. 三分钟速查

| 如果你的项目…… | 需要做什么 | 详见 |
|---|---|---|
| 用 `RunHTTP`，且没有配置 `WithApprover` | 显式配置审批者，否则需要审批的工具调用一律被拒绝 | [1.1](#11-runhttp-不再自动批准所有工具) |
| 用 `RunHTTP` 对外（非本机）提供服务 | 配置 `WithHTTPAuthToken`，或在已受保护的网络中使用 `WithHTTPInsecure()` | [1.2](#12-runhttp-监听地址鉴权与跨站防护) |
| 浏览器前端跨源访问 `RunHTTP`（如 Vite :5173 → :8080） | 配置 `WithHTTPAllowedOrigins(...)` | [1.2](#12-runhttp-监听地址鉴权与跨站防护) |
| 调用 `POST /interrupt` 时不带 `session_id` | 带上 `session_id` | [1.3](#13-runhttp-端点语义) |
| 在宿主工具内部调用 `RunPipeline`，节点使用 Normal 级工具 | 这些调用现在会经过审批；宿主固定的工具集可设 `TrustedTools: true` | [1.4](#14-子运行继承父运行的权限门) |
| 使用 Pipeline 的 `Review` + `Supervisor` | 核对审核按条处置的新语义；依赖收尾轮的设置 `FinalizePrompt` | [1.6](#16-pipeline-语义修正) |
| 依赖 `approve_result` 后 `OnTaskResult(nil)` 被调用 | 改为接收真实结果 | [1.6](#16-pipeline-语义修正) |
| 使用 bypass 模式 | 高风险操作（递归删除、强推、提权、写系统目录等）现在先经 YOLO 分类器判定，会产生模型调用；分类器不可用时改为询问，没有审批者则拒绝 | [1.7](#17-权限模式新增-autobypass-不再完全不检查) |
| 希望 agent 自主运行、每一步由分类器把关 | 使用新增的 `PermissionAuto` | [1.7](#17-权限模式新增-autobypass-不再完全不检查) |
| 使用 AcceptEdits 模式，且有自定义的写文件工具 | 给这些工具标 `FileEdit: true` | [1.7](#17-权限模式新增-autobypass-不再完全不检查) |
| 写过带入参的权限规则（如 `Bash(git *)`）或依赖"始终允许" | 核对新的匹配语义 | [1.8](#18-权限规则与始终允许) |
| `RunWithHistory` / `RunWithSessionHistory` 配合 `WithSessionManager` 使用 | 这两个入口不再自动落盘；需要落盘用 `RunSession` | [1.9](#19-会话存储) |
| 会话 ID 含 `/`、`\`、`..`、控制字符等 | 换成安全的 ID，否则被拒绝 | [1.9](#19-会话存储) |
| 需要让 WebFetch 访问内网 / 本机地址 | 改用 `WebFetchToolWith(WebFetchConfig{AllowPrivateNetwork: true})` | [1.10](#110-webfetch-默认只访问公网) |
| 有工具实际接受 schema 之外的入参 | 修正 schema（`omitempty` / `required`），或 `WithToolInputValidation(false)` | [1.5](#15-工具入参按-schema-校验) |
| 按 `Usage.InputTokens` 统计上下文大小或计费 | `InputTokens` 现在统一不含缓存；上下文大小用 `ContextTokens()` | [1.11](#111-用量与计费口径) |
| 自己实现了 `provider.Provider` | 按新的用量口径上报 | [1.11](#111-用量与计费口径) |

不在表中的项目一般无需改动，直接升级即可。

---

## 1. 需要处理的行为变化

### 1.1 RunHTTP 不再自动批准所有工具

- **之前**：`RunHTTP` 发现没有配置审批者时，自动套用 `AutoApprover()`。任何能连上端口的人都能让 agent 执行 Bash、写文件。
- **现在**：保持未配置——Normal 及以上级别的工具调用被拒绝，启动时打印警告。
- **迁移**：

```go
// 可信环境（本机开发、已有网关鉴权）：保持旧行为
goagent.WithApprover(goagent.AutoApprover())

// 需要人工审批：前端收到 permission_request 帧后 POST /approve
goagent.WithApprover(goagent.NewPermissionHandler())
```

### 1.2 RunHTTP 监听地址、鉴权与跨站防护

| 项目 | 之前 | 现在 |
|---|---|---|
| 监听地址 | 按传入地址监听（`":8080"` 即所有网卡） | 未配置令牌时只监听回环：`":8080"` 收紧为 `127.0.0.1:8080`；显式的非回环地址（如 `0.0.0.0:8080`）直接返回错误 |
| 鉴权 | 无 | `WithHTTPAuthToken(token)`：请求带 `Authorization: Bearer <token>`，或 `X-GoAgent-Token` 头，或 `?token=` 查询参数（供无法设置请求头的 EventSource 使用） |
| 无令牌对外监听 | — | `WithHTTPInsecure()`：只在网关鉴权、私有网络等已有保护的环境中使用 |
| 跨站请求 | 不拦截（任意网页可借用户浏览器向本机服务下指令） | 浏览器标记为跨站的请求返回 403；需要跨源访问的前端用 `WithHTTPAllowedOrigins("http://localhost:5173")` 放行，框架为这些源返回 CORS 响应头 |
| DNS 重绑定 | 不防护 | 只监听回环时校验 `Host` 头必须是回环名或允许的源 |
| 请求体 | 无上限 | 32 MiB |
| 超时 | 无 | `ReadHeaderTimeout` 10s、`IdleTimeout` 120s；SSE 单次写超时 30s（客户端卡住不再拖住任务） |

```go
// 对外服务的典型配置
app := goagent.New(
    cfg,
    goagent.WithApprover(goagent.NewPermissionHandler()),
    goagent.WithHTTPAuthToken(os.Getenv("GOAGENT_TOKEN")),
    goagent.WithHTTPAllowedOrigins("https://app.example.com"),
)
app.RunHTTP("0.0.0.0:8080")
```

### 1.3 RunHTTP 端点语义

- `POST /interrupt` 必须带 `session_id`，缺失返回 400。`RunHTTP` 会自动创建中断处理器，不再需要手动 `SetInterruptHandler`。
- `POST /approve` 带 `session_id` 时只能处置该会话的审批请求，一个会话不能替另一个会话作答。
- 新增 `GET /pending-approval?session_id=...`：查询会话的未决审批（页面刷新后恢复审批卡片）。
- 审批、提问、计划确认的 `request_id` 改为随机值（此前是递增序号，配合无鉴权的作答端点可被枚举）。
- 审批与提问只推送给所属会话的连接；该会话没有连接时不再回落到"最近连接的客户端"（那会把 A 会话的完整工具入参推给 B，并允许 B 代答）。
- `session_id` 必须是安全的文件名（见 [1.9](#19-会话存储)），否则返回 400。
- 未配置会话管理器时，`/chat` 也以真实会话 ID 运行，按会话路由的机制（插话、审批路由、工具 `Context.SessionID`）都可用。

### 1.4 子运行继承父运行的权限门

**问题**：pipeline 节点、团队成员、子 agent 的工具此前一律按只读处理、不经权限门。模型只要让用户批准一次 `create_pipeline`（或在 AcceptEdits 模式下自动通过），就能在节点里调用任意已注册工具，绕开对 Bash、写文件等的审批。

**现在的规则**：

| 子运行 | 在 agent 运行之中启动 | 宿主代码直接启动（不在任何运行中） |
|---|---|---|
| `create_pipeline` / `RunDynPipeline` | 继承该运行的权限策略 | 按 App 配置新建一套权限策略 |
| `run_team` | 继承 | 按 App 配置新建 |
| `RunPipeline`（`UsePipeline` 配置的） | 继承；`PipelineConfig.TrustedTools: true` 可关闭 | 不检查（与旧版一致） |
| `App.AgentTools(...)` 交给子 agent 的工具 | 继承，并校验入参 | 只校验入参 |
| 直接构造的 `agent.ToolDef` | 不检查（宿主负责） | 不检查 |

"继承"指：同一个权限门（模式、规则、Guard、审批者、"始终允许"授权）、plan 模式、hooks 与中间件，工具按其声明的 `Permission` 级别检查；审批请求带着父运行的会话 ID 路由到同一个用户。由模型编排的子运行（动态 pipeline、团队）无论从哪里启动都要过权限门。

**迁移**：宿主工具内部调用 `RunPipeline`、节点使用 Normal 级工具（例如写数据库）时，这些调用现在会进入审批。按情况选择：

```go
// 节点工具由宿主固定、安全由宿主负责：恢复旧行为
app.UsePipeline(goagent.PipelineConfig{
    TrustedTools: true,
    Nodes: nodes,
})
```

或者把确实只读的工具声明为 `goagent.ReadOnly`，或让该会话运行在 AcceptEdits / Bypass 模式下。

**嵌套深度**：新增 `MaxSubRunDepth = 3`。主对话为第 0 层，pipeline 里再建 pipeline / 团队 / 子 agent 超过 3 层时返回错误，防止模型无限递归派生子任务。

### 1.5 工具入参按 schema 校验

- **之前**：入参只经 `json.Unmarshal`。缺少必填字段的调用以零值静默执行；MCP 工具的入参完全不检查。
- **现在**：每次工具调用执行前，按工具的入参 schema 检查类型、必填字段、枚举值等。不合规的调用不执行、也不进入审批，错误说明作为工具结果返回给模型修正。覆盖主循环、pipeline 节点、团队成员与 `AgentTools()`。
- **注意**：`schema.Generate` 把**没有 `omitempty` 的字段视为必填**（模型看到的 schema 一直如此）。如果你的工具实际允许省略某个字段，给它加 `omitempty` 或 `required:"false"`。
- **关闭**：`goagent.WithToolInputValidation(false)`。

### 1.6 Pipeline 语义修正

**调度**

- `DependsOn` 真正生效：节点在依赖全部**完成**后才启动。此前没有 supervisor 时，所有节点一开始就被标记为"审核完成"，任何一个节点结束都会连带启动所有尚未启动的节点。
- `OnNodeComplete` 在下游节点启动（以及下游 `MessageFunc` 求值）之前执行完毕。
- 队列改为无界缓冲：`Push` 从不阻塞，`QueueSize` 只影响内部通道容量。此前上游推送超过 `QueueSize`、下游又在等上游结束时会永久死锁。
- 环检测覆盖队列关系：依赖边与 `Injects` / `CloseQueues` 组成的环、节点向自己的队列推送，都在启动前报错。节点名为空、重名、没有节点也会报错。

**审核（Review + Supervisor）**

- `approve_result` / `reject_result` 按 `index` 处置**单条**结果；同一条不能处置两次。本轮审核中没有被处置的结果在本轮结束时自动批准——因此只调用一次 `approve_result` 的旧式提示词，效果与旧版"批准整批"相同。
- `OnTaskResult` 收到被批准的**真实结果**。此前 `approve_result` 调用的是 `OnTaskResult(nil)`。
- 审核节点在**所有结果都处置完**之后才算完成，下游也等到这时才启动。此前第一次批准就关闭了队列：之后被打回的任务重新入队时被静默丢弃，下游在其余结果还在审核时就已启动。
- 审核工具按只读级别登记，不会触发用户审批。
- supervisor 每轮审核改为独立的轻量循环：系统提示词只有 `Supervisor.Instruction`，工具为 `Supervisor.Tools` 加审核工具，最大轮次取 `Supervisor.MaxTurns`（未设置时审核 10、收尾 5）。此前是一个使用 `AutoApprover` 的嵌套 App（附带默认系统提示词，且绕过权限门）。
- **收尾轮改为可选**：此前全部节点完成后，supervisor 总会以一段写死的提示词（要求调用 `progress_update(event_type="enrichment_complete")`）再跑一轮。现在只有设置了 `PipelineConfig.FinalizePrompt` 才运行。依赖旧行为的项目：

```go
goagent.PipelineConfig{
    FinalizePrompt: `所有 DAG 节点已完成审核。请执行收尾操作：调用 progress_update(event_type="enrichment_complete") 通知前端。`,
    // ...
}
```

- `ReviewEvent.Done` 字段保留但框架不再发送 Done 事件。

**回调与生命周期**

- `OnTaskResult`、`OnNodeComplete`、`OnEvent`、`MessageFunc` 串行调用，回调内部无需加锁。此前多个 worker 并发调用回调，宿主在回调里写 map 会触发 `concurrent map writes` 崩溃（内置的 `create_pipeline` 就有这个问题）。
- `OnTaskResult` / `OnNodeComplete` / `MessageFunc` 中的 panic 记为节点错误、终止 pipeline，不再让进程崩溃；`OnEvent` 中的 panic 只记日志。
- 节点执行失败：本轮事务回滚，结果不进入审核与回调。
- 运行取消或失败时，仍在审核中的结果所关联的事务被回滚（只有被批准的结果才生效）；`Run` 返回 ctx 的错误，而不是误导性的"LLM 返回空响应"；被中断的节点不会调用 `OnNodeComplete`。
- `Run` 返回后不再有任何回调，所有队列协程退出。

**实例与上下文**

- `UsePipeline` 只保存配置，每次 `RunPipeline` 新建运行实例：可以重复运行，也可以并发运行。此前第二次运行同一实例会出错。
- `create_pipeline` / `RunDynPipeline` 使用独立实例，**不再覆盖**宿主用 `UsePipeline` 配置的 pipeline（此前会被覆盖，多会话并发调用时还会互相串台）。
- `PipelineConfig.SessionID` 为空时沿用 ctx 中已有的会话与工作目录。此前会把它们清空，在会话中启动的动态 pipeline，其节点工具落到进程当前目录。
- ctx 已处于沙箱会话中（由所在的 agent 运行进入）时沿用，不再另开一个。
- 动态 pipeline 的节点名必须是安全的文件名（会成为轨迹文件名），轨迹文件名统一净化。
- 内部字段 `App.pipeline` 改为 `App.pipelineCfg`（只影响同包测试）。

### 1.7 权限模式：新增 auto，bypass 不再完全不检查

六种权限模式的行为：

| 模式 | 只读工具 | 普通操作 | 高风险操作 |
|---|---|---|---|
| `PermissionDefault` | 放行 | 询问 | 询问 |
| `PermissionAcceptEdits` | 放行 | 编辑类工具（`FileEdit`）放行，其余询问 | 询问 |
| `PermissionAuto`（新增） | 放行 | 分类器判定 | 分类器判定 |
| `PermissionBypass` | 放行 | 放行 | 分类器判定 |
| `PermissionPlanOnly` | 放行 | 拒绝 | 拒绝 |
| `PermissionDenyAll` | 放行 | 拒绝（allow 规则除外） | 拒绝（allow 规则除外） |

所有模式共同的规则：deny 规则与 Guard 最先生效；allow 规则命中的调用直接放行，不经分类器；需要分类器判定而分类器不可用时（未配置 provider、`WithYoloClassifier(false)`、调用失败），该调用改为询问审批者，没有审批者则拒绝。

**bypass 模式**

- **之前**：除 deny 规则与 Guard 外完全不检查。App 虽然总会装配 YOLO 分类器，但 bypass 分支在调用它之前就已放行，分类器实际从未运行。
- **现在**：仍然不询问用户，普通操作直接放行；**高风险操作**先经分类器判定，判为危险的拒绝。命中 ask 规则的调用也按高风险操作处理——宿主可以用 ask 规则补充自己的高风险操作。

高风险操作（`permission.HighRisk`）包括：

- 工具声明为 `RequireApproval` 或 `Dangerous` 级别；
- shell 命令（任一子命令）：递归删除（`rm -r`、`find -delete`、`Remove-Item -Recurse`、`rd /s`）；磁盘与系统操作（`dd of=`、`mkfs`、分区工具、关机重启、停止系统服务、防火墙、系统账户）；提权（`sudo`、`su`、`runas` 等）；破坏性 git 操作（强推、删除远端分支、`reset --hard`、`clean -f`、`branch -D`、`checkout .`、`stash drop/clear`）；上传本地文件（`curl -T`、`curl -F x=@file`、`scp`、远程 `rsync`）；原始网络连接（`nc`、`socat` 等）；发布软件包（`npm publish` 等）；删除集群资源、销毁基础设施；`eval` 与内联脚本（`bash -c`、`python -c`、`node -e`、`pwsh -Command`）；把下载内容交给解释器执行（`curl … | sh`）；
- 文件工具的路径、重定向目标，或修改文件的命令（`cp`、`mv`、`rm`、`chmod`、`tee`、`sed -i` 等）的参数指向系统目录或裸磁盘；
- 任何命令或文件工具涉及凭据与密钥（`~/.ssh`、`~/.aws`、`~/.kube`、`id_rsa`、`.netrc` 等）、shell 启动文件（`.bashrc`、`.zshrc`、`.profile`）、`sudoers` 或 `.git/hooks`。

识别规则是启发式的，漏判时还有 deny 规则与 Guard 兜底。

**auto 模式（新增）**

每个非只读、非白名单的工具调用都交给分类器判定，判为危险的拒绝。白名单为 Read、Grep、Glob、Task 系列、AskUserQuestion、进出计划模式。ask 规则命中的调用仍询问用户，allow 规则命中的直接放行。HTTP 的 `POST /mode` 接受 `"auto"`。

**AcceptEdits 模式**

- **之前**：自动放行所有 Normal 级工具——包括 Bash、run_js、GitCommit。
- **现在**：只自动放行只读工具和标记了 `FileEdit: true` 的工具，且**高风险的编辑**（写入系统目录、凭据、shell 启动文件、`.git/hooks`）仍需确认。内置的 Write、Edit、NotebookEdit、NotebookAddCell 已标记；shell、代码执行、网络等工具在该模式下仍需确认。
- **迁移**：自定义的写文件工具加上标记：

```go
app.Tool("SaveDraft", goagent.ToolDef{
    Input:      SaveDraftInput{},
    Permission: goagent.Normal,
    FileEdit:   true,
    Execute:    saveDraft,
})
```

**分类器的配置与成本**

- 有 provider 时默认装配，只在 auto 模式与 bypass 模式的高风险操作上被调用，其他模式没有额外开销。
- 每次判定先做一次快速判定（最多 256 token），判为危险时再做一次复核（最多 4096 token），带上发起调用的那段对话的最近 20 条记录。同一权限门被 pipeline 多个 worker 共用时，各自用自己的对话判定。
- 默认使用主模型。可以指定同一 provider 上更快更便宜的模型：`WithYoloClassifierModel("…")`。
- 提示词：`WithYoloPromptFile(path)` > 提示词目录中的 `yolo-classifier.prompt.md` > 嵌入的默认值。
- 关闭：`WithYoloClassifier(false)`。关闭后，auto 模式与 bypass 模式的高风险操作改为询问审批者（没有审批者则拒绝），不会退回"完全不检查"。

```go
// CI/CD：不询问；高风险操作由小模型把关
app := goagent.New(cfg,
    goagent.WithPermissionMode(goagent.PermissionBypass),
    goagent.WithYoloClassifierModel("gpt-4o-mini"),
)

// 某些高风险操作在这个宿主里是常规操作：用 allow 规则明确放行（不再经分类器）
rules := goagent.NewPermissionRules().Allow("Bash", "git push --force-with-lease *")
```

### 1.8 权限规则与"始终允许"

**规则匹配**

- 规则的入参模式匹配的是从入参中**提取出的值**——命令（`command` / `cmd` / `script` 字段）、路径（`file_path` / `path` / `notebook_path`）、URL、`pattern`、`query`——不再是整段 JSON 文本。
- shell 命令按控制操作符（`;` `&&` `||` `|` `&` 换行）拆成子命令，引号内不拆；`$(...)`、反引号、`<(...)` 内部的命令也作为子命令参与匹配。
  - **deny / ask**：任一子命令命中即生效；不带 `*` 的模式还对整条命令做子串匹配（宁可多拦）。
  - **allow**：每个子命令都要被某条 allow 规则覆盖才放行。含命令替换、输出重定向的命令不会被带入参模式的 allow 规则放行。
- 不带 `*` 的模式按**词前缀**匹配：`rm` 匹配 `rm -rf x`，不匹配 `rmdir x`；`git *` 也匹配单独的 `git`。
- 显式 ask 规则对只读工具、AcceptEdits 与 auto 模式同样生效；bypass 模式下命中 ask 规则的调用交给分类器判定（见 [1.7](#17-权限模式新增-autobypass-不再完全不检查)）。

**"始终允许"**

- **之前**：用户对任意一条 Bash 命令选择"始终允许"，之后**所有** Bash 命令都自动放行。
- **现在**：普通工具仍按工具名记忆；shell 命令按"工具名 + 命令的前一到两个词"记忆。批准过 `ls -la` 后 `ls src` 自动放行；批准过 `git status` 后 `git push` 仍需确认。复合命令与含命令替换、重定向的命令不会被记忆。

### 1.9 会话存储

- 会话 ID 必须能安全地作为文件名：非空、不含 `..`、路径分隔符、`:*?"<>|`、控制字符，不以空白开头或结尾、不以点结尾，不是 Windows 保留设备名，不超过 128 个字符。不合规的 ID 被拒绝（此前可借此读写会话目录之外的文件）。
- 只有 `RunSession` 自动落盘。`RunWithHistory` 与 `RunWithSessionHistory` 不再写入会话存储——此前配置了会话管理器时，所有 `RunWithHistory` 调用的消息都被追加进同一个名为 `ephemeral` 的会话文件，`RunWithSessionHistory` 则会在宿主自有存储之外再存一份。
- 配置了会话管理器时，`RunWithSessionHistory` 与 `RunSession` 一样拒绝同一会话的并发运行。
- 落盘时机改为每条消息进入对话时立即同步写入（先于任何压缩改写），逐条 `fsync`。进程被杀时已发生的步骤全部保留；读取时跳过写了一半的末行，不再让整个会话不可读。
- 内存存储（`MemoryStore`）返回会话的副本，调用方修改不会影响存储内容。

### 1.10 WebFetch 默认只访问公网

- **之前**：WebFetch 是只读级工具（不经审批），可以访问任何地址，包括 `http://169.254.169.254/`（云元数据）、`localhost`、内网服务。网页中的注入指令可诱导模型抓取这些地址并把内容带回对话（SSRF）。
- **现在**：`builtin.WebFetchTool()` 在建立连接时检查目标 IP，拒绝回环、私有网段、链路本地、运营商级 NAT、组播、保留地址，以及内嵌这些地址的 IPv6 形式（IPv4 映射、NAT64、6to4）；重定向与 DNS 重绑定同样受检查。经 `HTTP_PROXY` 代理时改为请求前解析检查。
- **迁移**：确实需要访问内网文档时：

```go
app.Tool("WebFetch", builtin.WebFetchToolWith(builtin.WebFetchConfig{AllowPrivateNetwork: true}))
```

### 1.11 用量与计费口径

`provider.Usage` 的语义统一为（所有 Provider 实现都按此归一）：

- `InputTokens` **不含**缓存读写部分（Anthropic 原生口径）。OpenAI 兼容实现会从 `prompt_tokens` 中扣除 `cached_tokens`。
- 每条模型响应只上报一次终值；消费方按字段取最大值合并（`provider.MergeUsage`），不做累加。此前流式响应的用量会被重复累计。
- 本次请求占用的上下文大小用 `usage.ContextTokens()`（含缓存读写）。
- 未知模型的费用记为 0，不再按 Sonnet 价格猜测；查表容忍 `-v1`、`-v1:0`、日期等后缀（`cost.LookupPricing`）。
- 自定义 Provider：流式实现可以多次发送 `EventUsage`，但后发的必须是累计终值。
- OpenAI 兼容 Provider 的流式请求默认携带 `stream_options.include_usage`（官方 API 只有带上它才返回用量）。个别不认识该字段的后端可设置 `openai.Config.DisableStreamUsage = true`。

### 1.12 其他较小的行为变化

- **工具注册**：`Execute` 签名在注册时校验（必须是 `func(goagent.Context, T) (string, error)`，`T` 与 `Input` 类型一致），不合规时注册即 panic，而不是在调用时才失败。`WithBuiltinTools`、`WithToolKits`、`WithSubAgents` 等自动装配遇到重名工具时跳过并告警，不再在 `New()` 中 panic。
- **工具 panic**：被执行器恢复为错误结果返回给模型，堆栈记入日志，不再带走进程。
- **取消**：ctx 取消后不再启动新的工具；已在运行的工具最多等待 `executor.CancelGrace`（默认 3 秒）收尾，超时未返回的按"被中断"合成结果。中断时已完成的工具结果会被记录进对话。
- **输出截断**：模型输出被 `max_tokens` 截断时注入续写提示（最多 3 次），不再尝试提高 `max_tokens` 重试。
- **默认轮次**：`loop.Config.MaxTurns <= 0` 时取 100（此前为 0 时第一轮就报"已达最大轮次"）。
- **系统提示词**：每次请求都携带 `SystemPrompt`；支持缓存时另附分块形式。此前切换到不支持缓存的后备模型时系统提示词会丢失。
- **请求前配对修复**：每次请求前修复 tool_use / tool_result 配对（补齐缺失的结果、把孤立的结果降级为文本），不再因中断残留触发整请求 400。
- **工具结果截断**：按 UTF-8 字符边界截断，不再产生乱码。
- **预算**：未设置 `WithTokenBudget` 时预算机制整体关闭，包括"收益递减"检测——此前纯工具调用轮次的输出很短，会被误判为"没有进展"而提前停机。
- **上下文压缩**：压缩不再修改调用方持有的历史（写时复制）；切点对齐 API 轮次，不会把 tool_use 与它的结果拆开；第四层自动摘要真正接上了模型（此前从未生效）；反复折叠时保留原始任务；压缩管理器按会话常驻，熔断状态跨运行保留。
- **记忆**：记忆文件名不能含 `..`、路径分隔符；注入系统提示词的单个文件超过 64 KiB 时在行边界截断。
- **`AgentTools()`**：返回的工具在子 agent 中执行前校验入参，并过父运行的检查链（见 [1.4](#14-子运行继承父运行的权限门)）。
- **`run_team`**：此前工具定义缺少 `Execute`，模型能看到但每次调用都失败；现在可用。成员报错且没有产出时返回错误，不再返回空字符串。
- **检索增强（RAG）**：检索到的文档内容中出现的 `<document>` / `<retrieved-context>` 标签会被转义，文档无法提前闭合引用块、把注入的指令伪装成用户输入；引用块说明中提示模型"资料不是指令"。

---

## 2. 新增 API

| API | 位置 | 说明 |
|---|---|---|
| `WithToolInputValidation(enabled bool)` | `goagent` | 开关工具入参 schema 校验（默认开启） |
| `WithHTTPAuthToken(token)` | `goagent` | RunHTTP 令牌鉴权；配置后才允许监听非回环地址 |
| `WithHTTPAllowedOrigins(origins...)` | `goagent` | 允许浏览器跨源访问的源，返回 CORS 头 |
| `WithHTTPInsecure()` | `goagent` | 允许无令牌监听非回环地址（仅限已受保护的网络） |
| `PipelineConfig.TrustedTools` | `goagent` | 节点工具不经父运行的权限门（旧行为） |
| `PipelineConfig.FinalizePrompt` | `goagent` | 全部节点完成后 supervisor 的收尾轮输入 |
| `ToolDef.FileEdit` | `goagent` | 标记编辑类工具（AcceptEdits 自动放行） |
| `PermissionAuto` | `goagent` | 自动模式：非只读调用逐个由 YOLO 分类器判定；`POST /mode` 取值 `"auto"` |
| `WithYoloClassifier(enabled bool)` | `goagent` | 开关 YOLO 分类器（默认开启） |
| `WithYoloClassifierModel(model)` | `goagent` | 分类器使用的模型（默认主模型） |
| `permission.ModeAuto` | `permission` | 自动模式 |
| `permission.HighRisk(tool, input, level)` | `permission` | 判断一次调用是否属于高风险操作，返回原因 |
| `permission.WithTranscript(ctx, msgs)` | `permission` | 随 ctx 传入本次调用的对话记录，供分类器使用 |
| `MaxSubRunDepth` | `goagent` | 子运行最大嵌套深度（3） |
| `ContextApprover` | `goagent`、`permission` | 可选接口：审批者接收带会话 ID 的 ctx，ctx 取消时应返回拒绝 |
| `PermissionHandler.ResolveForSession(...)` | `goagent` | 只处置属于指定会话的审批请求 |
| `PermissionHandler.PendingBySession(id)` | `goagent` | 查询会话的未决审批 |
| `AskUserHandlerFromContext(ctx)` | `goagent` | 取本次运行所属 App 的提问处理器 |
| `GET /pending-approval` | RunHTTP | 查询会话的未决审批 |
| `builtin.WebFetchConfig` / `builtin.WebFetchToolWith(cfg)` | `builtin` | 可配置的 WebFetch（是否允许内网） |
| `permission.Gate.CheckContext(ctx, ...)` | `permission` | 在 ctx 下做权限检查（审批路由、取消即拒绝） |
| `permission.Gate.SetEditTools(names)` | `permission` | 设置 AcceptEdits 自动放行的编辑类工具 |
| `message.Message.UID` | `message` | 消息唯一 ID（逐条落盘去重用；空表示派生视图消息） |
| `message.ContentBlock.Signature` | `message` | thinking 块签名（Anthropic 要求原样回传） |
| `message.NewUID()`、`Message.CloneContent()`、`IsToolResultMessage()` | `message` | 辅助函数 |
| `message.SanitizeForRequest(msgs)`、`ValidatePairing(msgs)` | `message` | tool_use / tool_result 配对修复与校验 |
| `message.EstimateTextTokens(s, charsPerToken)` | `message` | 文本 token 估算（中文按字计） |
| `provider.Usage.ContextTokens()`、`provider.MergeUsage(a, b)` | `provider` | 用量口径辅助 |
| `provider.RateLimitError.RetryAfter` | `provider` | 服务端建议的等待时长 |
| `provider.MockResponse.Usage` | `provider` | Mock 响应可携带用量 |
| `openai.Config.DisableStreamUsage` | `provider/openai` | 不在流式请求中携带 `stream_options` |
| `cost.LookupPricing(model)`、`cost.Estimate(model, usage)` | `cost` | 单一定价表查询与估算 |
| `schema.Validate(schema, input)`、`schema.AsMap(s)` | `schema` | 入参校验 |
| `executor.CancelGrace`、`executor.Config.PanicHandler` | `executor` | 取消收尾时长、工具 panic 回调 |
| `loop.Config.OnMessage`、`loop.Config.SkipInputValidation`、`loop.DefaultMaxTurns` | `internal/loop` | 逐条消息回调、关闭校验、默认轮次 |
| `teams.Config.Permission` / `PlanChecker` / `Hooks` / `Middlewares` | `teams` | 团队工具调用的检查链（v0.13 移除，改为从 ctx 继承，见 [UPGRADE-v0.13.md](UPGRADE-v0.13.md)） |

---

## 3. 修复清单（按模块）

### 核心循环（internal/loop）

- 流式响应期间已执行完的工具结果被丢弃——工具在模型输出未结束时就开始执行，提前完成的结果此前在轮末丢失，模型看到"没有结果"。
- 输出截断：`max_tokens` 截断后续写恢复（最多 3 次）；工具结果截断按 UTF-8 边界进行。
- 切换到后备模型时系统提示词丢失。
- 用户中断时丢弃已完成的工具结果、助手消息未落盘。
- `Executor`、`MaxTurns` 未设置时的空指针与"第 0 轮即超限"。
- 流式用量重复累计；每次请求只记录一次用量与费用。
- 预算未设置时"收益递减"误停机。
- thinking / redacted_thinking 块连同签名完整回传。

### 工具执行（executor）

- 工具 panic 带走整个进程 → 恢复为错误结果。
- ctx 取消后仍启动排队中的工具。
- 等待在跑工具没有上限 → `CancelGrace` 有界等待。
- `MockProvider` 超过 7 个工具调用时死锁（测试基建）。

### 会话持久化（session）

- 按消息 UID 逐条同步落盘，替代"运行结束后按位置补齐"（压缩改写过的视图会被写进存储、中途被杀丢失整轮）。
- `RunWithHistory` 的消息被写进共享的 `ephemeral` 会话。
- 会话 ID 路径穿越。
- 半行损坏让整个会话不可读；写入不 fsync。
- `MemoryStore` 返回内部对象，调用方修改即篡改存储并构成数据竞争。

### 上下文压缩（compaction）

- 压缩就地改写调用方的历史与已落盘消息（写时复制修复）。
- 切点拆开 tool_use 与 tool_result，请求 400。
- 第四层自动摘要从未接上模型。
- 反复折叠后原始任务描述消失；摘要输入截断时丢掉开头的任务。
- 压缩管理器按运行新建，熔断状态每条消息被重置。

### 权限与审批（permission、PermissionHandler、streambus）

- 规则按整段 JSON 文本匹配，`Bash(git *)` 等规则形同虚设；复合命令 `git status && rm -rf /` 可被 `git` 的 allow 规则放行。
- "始终允许"按工具名记忆，批准一次 `ls` 即放行所有 Bash 命令。
- AcceptEdits 自动放行 Bash 等非编辑类工具。
- 审批不感知 ctx：用户中断后审批等待无法结束；审批请求不按会话路由。
- 请求 ID 递增可枚举；会话间可互相代答。
- 运行时修改 Guard 链就地改写进行中运行的切片。
- YOLO 分类器装配了却从未被调用（bypass 分支提前放行），bypass 模式实际上完全不检查；现在 bypass 模式的高风险操作经分类器判定，新增的 auto 模式由分类器判定每个非只读调用。
- 分类器使用共享的对话记录，并发循环共用权限门时可能拿别人的对话判定；现在随 ctx 传入本次调用的对话。

### HTTP 服务

- 默认自动批准、默认监听所有网卡、无鉴权、无 CSRF / DNS 重绑定防护、无请求体上限与超时、SSE 写入无超时。
- 准入顺序：忙碌会话的判断先于订阅，避免排队请求占用审批/提问通道。
- `/interrupt` 未带会话时静默无效。

### Pipeline / 团队 / 子 agent

- DependsOn 不生效；推送超过队列容量死锁；队列关系成环死锁。
- `create_pipeline` 覆盖宿主 pipeline、并发调用串台；同一实例不可重复运行。
- 回调并发导致宿主崩溃。
- 审核：`OnTaskResult(nil)`；首次批准后关闭队列导致打回任务丢失；未处置结果挂起；supervisor 退出后发送方永久阻塞；`reviewApproved` 重复关闭 panic。
- 节点失败与取消时事务不回滚；取消被报告为"空响应"。
- 子运行绕过权限门；无嵌套深度限制；会话工作目录被清空。
- `run_team` 缺少 Execute；团队与成员的错误被吞掉。
- 节点名、子 agent 轨迹文件名路径穿越。
- 动态 pipeline 节点产出按字节截断产生乱码。

### Provider

- Anthropic：thinking 签名与 redacted_thinking 丢失导致多轮思考请求被拒；用量重复上报；错误未分类（429 / 529 / 5xx / 超长）导致不重试、不切后备、不压缩；`Retry-After` 未使用；SDK 错误在缺少请求/响应时 panic；`SetModel` 与进行中请求的数据竞争；思考预算超过 `max_tokens` 时直接 400；模型能力查表不容忍版本后缀。
- OpenAI 兼容：流式工具调用未按 index 排序；重复的 `finish_reason` 导致工具被重复派发；不请求流式用量；缓存命中未从输入中扣除；HTTP 错误分类与超长识别覆盖更多后端；`Request.Model` 未覆盖配置模型；图片编码自适应在并发失败时来回翻转；失败请求转储文件权限过宽（现为 0600）。

### 记忆、schema、计量与其他

- 记忆：嵌套读锁死锁（`SaveMain` 等）；记忆名路径穿越；超大文件整读进内存；截断切坏 UTF-8 与半行。
- schema：递归类型无限展开导致栈溢出；`time.Time`、`json.RawMessage`、`any` 的 schema 不正确。
- 计量：多张定价表不一致；未知模型按 Sonnet 价格计费；`Execute` 的用量缺少缓存部分。
- 工具：签名错误到调用时才暴露；自动装配重名 panic；`Context.Store` / `Load` 未实现（现在按会话共享）。
- 安全：WebFetch SSRF；RAG 文档标签注入。

---

## 4. 验证情况与建议的宿主侧验证

框架侧（本次升级已完成）：

- `go build ./...`、`go vet ./...` 通过（含 `examples/` 下的 7 个示例）。
- `go test -race ./...`：21 个有测试的包全部通过，无数据竞争。
- 每项修复都有对应的回归测试，集中在各包的 `*_fix_test.go`、`fix_regression_test.go`、`subrun_fix_test.go`、`retrieval_fix_test.go`、`builtin/netguard_test.go`；部分关键测试已做反向验证（把修复改回旧逻辑后测试失败）。

建议宿主升级后检查：

1. **审批链路**：在默认权限模式下触发一次 Normal 级工具调用，确认审批请求到达正确的会话界面，`/approve` 后工具执行。
2. **Pipeline**：跑一遍带 `Review` 的流水线，确认 `OnTaskResult` 收到的是结果内容（不再是 `nil`），下游在审核结束后才启动；依赖收尾轮的设置 `FinalizePrompt`。
3. **宿主工具内的 RunPipeline**：确认节点工具是否出现了新的审批请求，按 [1.4](#14-子运行继承父运行的权限门) 决定是否设置 `TrustedTools`。
4. **前端**：跨源访问的前端配置 `WithHTTPAllowedOrigins`；`/interrupt` 请求带 `session_id`。
5. **用量报表**：按新口径核对 `InputTokens` 与缓存字段。
6. **bypass / auto 模式**：跑一条高风险命令（如 `git push --force`），确认先经分类器判定；如果分类器的额外调用不可接受，用 `WithYoloClassifierModel` 换小模型，或用 allow 规则放行宿主的常规操作。

---

## 5. 已知限制与后续路线（不在本次升级范围内）

**已知限制**

- **目录沙箱不是安全边界**：Tier 1 的 `DirSandbox` / `WorktreeSandbox` 只约束文件类工具的路径解析（词法前缀白名单），不跟随符号链接；Bash、run_js 等启动的子进程只是把工作目录设在沙箱根内，仍可访问根外的绝对路径。需要真正隔离时等待 Tier 2（容器级，`ExecSession` 接口已预留），或在部署层面隔离进程。
- **用户审批是最后一道防线**：权限规则的命令拆分是静态分析，无法理解所有 shell 语法（如 `eval`、别名、脚本文件内容）。对不可信输入驱动的 agent，建议 Bash 类工具保持"每次询问"，而不是依赖 allow 规则。

**后续路线**

> 下列各项已在 v0.13 完成（拆分包结构完成第一步：终端界面移到 `cli` 包），见 [UPGRADE-v0.13.md](UPGRADE-v0.13.md)。

以下是架构层面的改进方向，本次只修复问题、不做结构调整，留待后续版本：

- **统一 Runner**：主循环、pipeline 节点、团队成员、子 agent（`agent.Runner`）目前是四套相似的执行实现。目标是收敛为同一个 Runner，权限、校验、观测、压缩只实现一次，子运行的策略继承成为结构保证而不是逐处接线。
- **Transcript + Projection**：把"完整对话记录"（只追加、按 UID 落盘）与"发给模型的视图"（压缩、配对修复后的投影）在类型上分开，压缩永远只产生视图，从根上杜绝改写历史。
- **SessionRuntime**：把会话级的常驻状态（压缩管理器、键值存储、权限授权、插话车道、并发锁）收拢到一个会话运行时对象，替代目前分散在 App 上的多个 `sync.Map`。
- **清理死代码与业务耦合**：移除已不再使用的机制（如 `TransMaxOutputEscalate`），把宿主业务相关的默认值（`.yume` 目录名等）改为配置项。
- **拆分包结构**：`goagent` 根包承担了 App、HTTP、pipeline、团队、RAG 等过多职责，计划拆分为独立子包，降低嵌入方的依赖面。
