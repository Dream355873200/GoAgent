# GoAgent 完整参考

> [English](en/reference.md) | 简体中文

> 本文档是 README 的 L2/L3 深度参考：权限、运行模式、会话、内置工具、
> 中间件、Hooks、Observer、子 Agent、Pipeline、内存、压缩、Provider、Prompt。
> 刚入门？先读 [tutorial.md](tutorial.md)（三课渐进式教程）和 [README](../README.md)（核心概念）。

---

## 工具定义

### 推荐方式：InferTool（自动推断）

从函数签名自动生成 JSON Schema 和序列化逻辑：

```go
type SearchInput struct {
    Query string `json:"query" desc:"搜索关键词" required:"true"`
    Limit int    `json:"limit,omitempty" desc:"结果数量限制"`
}

type SearchResult struct {
    Items []string `json:"items"`
    Total int      `json:"total"`
}

// 默认 ReadOnly（自动通过，无需确认）
app.UseTools(goagent.InferTool("search", "搜索互联网获取最新信息", search))

// 指定权限级别
app.UseTools(goagent.InferTool("deploy", "部署服务", deploy, goagent.Normal))
app.UseTools(goagent.InferTool("restart", "重启服务", restart, goagent.Dangerous))
```

函数签名必须是 `func(context.Context, T) (D, error)`，其中：
- `T` — 输入结构体，struct tags 自动生成 JSON Schema
- `D` — 返回给 LLM 的结果。一般用 `string` 即可；如果返回结构体会自动 `json.Marshal` 为 string

### InferTool Options

InferTool 除了权限级别，还支持通过 Option 配置工具行为：

```go
app.UseTools(goagent.InferTool("bash", "执行命令", bashFn, goagent.Normal,
    goagent.WithInterruptMode("block"),  // 中断时等待完成（适合长时间运行的命令）
    goagent.WithMaxResultSize(50000),    // 结果最大字符数
))
app.UseTools(goagent.InferTool("search", "搜索", searchFn,
    goagent.WithConcurrent(),           // 允许与其他工具并行执行
))
```

### 批量注册

```go
app.UseTools(
    goagent.InferTool("deploy", "部署服务", deploy, goagent.Normal),
    goagent.InferTool("status", "查看状态", status),
    goagent.InferTool("restart", "重启服务", restart, goagent.Dangerous),
)
```

### 结构体标签参考

工具的 Input 结构体支持以下标签：

| 标签 | 作用 | 示例 |
|------|------|------|
| `json:"fieldName"` | JSON 字段名 | `json:"name"` |
| `desc:"描述"` | 字段描述，展示给 LLM | `desc:"用户名"` |
| `enum:"a,b,c"` | 枚举值 | `enum:"read,write,delete"` |
| `required:"true"` | 必填字段 | `required:"true"` |
| `json:"name,omitempty"` | 可选字段 | `json:"limit,omitempty"` |

---

## 权限系统

### 权限模式

| 模式 | 只读工具 | 普通操作 | 高风险操作 |
|------|---------|---------|-----------|
| `PermissionDefault` | 放行 | 询问 | 询问 |
| `PermissionAcceptEdits` | 放行 | 编辑类工具（`FileEdit`）放行，其余询问 | 询问 |
| `PermissionAuto` | 放行 | 分类器判定 | 分类器判定 |
| `PermissionBypass` | 放行 | 放行 | 分类器判定 |
| `PermissionPlanOnly` | 放行 | 拒绝 | 拒绝 |
| `PermissionDenyAll` | 放行 | 拒绝（allow 规则除外） | 拒绝（allow 规则除外） |

- deny 规则与 Guard 在所有模式下最先生效；allow 规则命中的调用直接放行，不经分类器。
- auto 模式下 ask 规则仍询问用户；bypass 模式下命中 ask 规则的调用按高风险操作交给分类器。
- 高风险操作（`permission.HighRisk`）：工具声明为 `RequireApproval` / `Dangerous`；递归删除、磁盘与系统操作、提权、破坏性 git 操作（强推、`reset --hard`、`clean -f` 等）、上传本地文件、原始网络连接、发布软件包、内联脚本（`bash -c`、`python -c` 等）、把下载内容交给解释器执行；写入系统目录或裸磁盘；涉及凭据（`~/.ssh`、`~/.aws`、`id_rsa`、`.netrc` 等）、shell 启动文件或 `.git/hooks`。宿主可以用 ask 规则补充自己的高风险操作。
- 需要分类器判定而分类器不可用时（未配置 provider、`WithYoloClassifier(false)`、调用失败），该调用改为询问审批者，没有审批者则拒绝。

```go
// 自主运行：每个非只读调用由分类器把关
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionAuto))

// CI/CD：不询问，高风险操作由分类器把关
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionBypass))

// 交互式开发：只读与编辑类工具自动通过，其余询问
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionAcceptEdits))

// 规划模式：只允许只读操作
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionPlanOnly))

// 拒绝所有写操作
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionDenyAll))
```

### 自定义权限规则

```go
rules := goagent.NewPermissionRules().
    Allow("Read", "").       // 允许所有只读操作
    Allow("Bash", "git *").  // 允许 git 命令
    Deny("Bash", "rm *").    // 禁止 rm 命令
    Deny("Bash", "dd *").    // 禁止 dd 命令
    Ask("Write", "").        // 写操作需要确认

app := goagent.New(goagent.WithPermissionRules(rules))
```

规则优先级：`deny > ask > allow`。

### 异步审批（Web/API 场景）

HTTP 或 WebSocket 模式下，Agent 需要暂停等待前端用户的审批。框架内置 `PermissionHandler`，三步接入：

```go
// 1. 创建并注册
permHandler := goagent.NewPermissionHandler()
app := goagent.New(
    goagent.ProviderConfig{...},
    goagent.WithApprover(permHandler),
)

// 2. 后台 goroutine：监听权限请求，推给前端
go func() {
    for req := range permHandler.Requests() {
        // 通过 SSE / WebSocket 推送给前端
        sendToFrontend(map[string]any{
            "type":        "permission_request",
            "request_id":  req.RequestID,
            "tool_name":   req.ToolName,
            "tool_input":  req.ToolInput,
            "permission":  req.Permission,
        })
    }
}()

// 3. 前端用户点允许/拒绝后，你的 HTTP handler 收到回调
permHandler.Resolve(requestID, true, false, "")         // 允许
permHandler.Resolve(requestID, false, false, "用户拒绝") // 拒绝
```

完整示例见 [examples/web-api/](examples/web-api/)。

### 自定义审批逻辑

如果需要完全自定义审批流程（如接审批流、消息队列），直接实现 `Approver` 接口：

```go
type MyApprover struct{}

func (a *MyApprover) Approve(toolName, input string, perm goagent.Permission) (bool, bool) {
    // 返回 (allow, alwaysAllow)
    // alwaysAllow=true 表示后续同类工具不再询问
    return true, false
}

app := goagent.New(goagent.WithApprover(&MyApprover{}))
```

### YOLO 权限分类器

对齐 Claude Code 的 YOLO 权限分类器，使用模型判断工具调用是否安全、是否符合用户意图。

**何时调用：**
- 有 provider 时默认装配（`WithYoloClassifier(false)` 关闭）
- 只在 auto 模式（每个非只读、非白名单调用）与 bypass 模式（只判定高风险调用）下调用，其他模式没有额外开销
- 安全工具白名单（Read/Grep/Glob/Task 等）跳过分类，直接允许
- 分类器不可用（调用失败、上下文过长）时，该调用改为询问审批者，没有审批者则拒绝

**成本：** 两阶段分类——Stage 1 快速判定（最多 256 tokens），判为危险时再做 Stage 2 复核（最多 4096 tokens）。默认使用主模型，可用 `WithYoloClassifierModel("…")` 指定同一 provider 上更快更便宜的模型。

**权限检查流程：**
1. Deny 规则（最高优先级）
2. Guard 链（可拒不可放）
3. Ask 规则（auto 等模式下询问用户；bypass 下交给分类器）
4. 权限模式（auto / bypass 在这里调用分类器）
5. Allow 规则
6. 默认级别检查（ReadOnly/Normal/RequireApproval/Dangerous）
7. 用户审批（Approver）

**Transcript 上下文：** 分类器读取发起本次调用的那段对话的最近 20 条记录，判断操作是否与用户意图一致（同一权限门被 pipeline 多个 worker 共用时，各自用自己的对话）。

**Prompt 自定义：** `WithYoloPromptFile(path)` > 提示词目录中的 `yolo-classifier.prompt.md` > 嵌入的默认值。

### 运行时切换模式

```go
app.SetPermissionMode(goagent.PermissionPlanOnly)          // 全局：所有在跑运行与后续运行
app.SetSessionPermissionMode("sess-1", goagent.PermissionBypass) // 只作用于该会话
app.ClearSessionPermissionMode("sess-1")                   // 撤销会话级设置，回到全局模式
mode := app.SessionPermissionMode("sess-1")                // 会话当前生效的模式
```

切换立即作用于进行中的运行。全局切换不覆盖设置了会话级模式的会话。优先级从高到低：会话级运行时设置 > `WithSessionPermissionMode` 的解析结果 > 全局运行时覆写 > `WithPermissionMode`。多个用户共用一个 App 时，用户自己的模式切换应按会话进行。

### "始终允许"

用户在审批时勾选"始终允许"后，同一会话的后续调用（包括之后的运行）不再询问；授权不跨会话。普通工具按工具名记忆，shell 命令按"工具名 + 命令的前一到两个词"记忆。`app.SessionGrants(id)` 返回会话的授权集合，可以查看（`Keys()`）和撤销（`Clear()`）；`app.CloseSession(id)` 释放会话的全部内存状态，授权随之撤销。

### 改参审批

批准工具调用时可以**替换它的参数**（完整替换）执行：

```go
// 自定义审批者（实现 goagent.ReasonApprover）：批准时给出替换后的参数
func (a *MyApprover) ApproveReason(ctx context.Context, toolName, input string, perm goagent.Permission) (bool, bool, string) {
    if edited := editArgsInUI(toolName, input); edited != nil {   // 你的改参 UI
        if !permission.SetEditedInput(ctx, edited) {              // ctx 不支持改参时返回 false
            return false, false, "该调用不支持修改参数"
        }
        return true, false, ""
    }
    ...
}

// PermissionHandler 的两种入口
req.ApproveWithInput([]byte(`{"path":"b.txt"}`))                  // 拿到请求对象时
handler.ResolveWithInput(sessionID, requestID, []byte(`{"path":"b.txt"}`)) // 按 requestID 处置
```

修改后的参数按工具的入参 schema 校验，不合格时**不执行**、原因作为工具结果交给模型；执行成功时结果末尾注明参数被修改过。改参批准不记为"始终允许"。AG-UI（`editedArgs`）与 v1 `POST /approve`（`input` 字段）的对应入口见「AG-UI 1.0 剖面」节。

---

## 运行模式

### CLI REPL

```go
import "github.com/Dream355873200/GoAgent/cli"

cli.Run(app) // 导入 cli 包后 app.RunCLI() 同样可用
```

终端界面在 `cli` 包中，根包不依赖终端界面库。交互式 TUI 界面，支持：
- 彩色输出和格式化
- 工具执行状态实时显示
- 推理过程（Thinking）展示
- 会话切换（`/sessions`）
- 任务列表（`/tasks`）

### HTTP SSE API

```go
app.RunHTTP(":8080")
```

未配置 `WithHTTPAuthToken` 时只监听 `127.0.0.1`；对外服务配置令牌（或在已受保护的网络中用 `WithHTTPInsecure()`）。浏览器前端跨源访问用 `WithHTTPAllowedOrigins` 放行，未列出的跨站请求返回 403。HTTP 模式不会自动批准工具调用，需要显式配置 `WithApprover`。

**REST 端点：**

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/chat` | SSE 流式传输对话（支持 `session_id` 多轮、`?session_id=` 多会话并行；SSE 断开不取消任务） |
| `POST` | `/interrupt` | 中断当前任务（`session_id` 必填） |
| `GET` | `/sessions` | 会话列表摘要 |
| `GET` | `/sessions/{id}/messages` | 会话历史消息 |
| `POST` | `/execute` | 同步执行，返回结果 |
| `POST` | `/approve` | 权限审批响应（带 `session_id` 时只处置该会话的请求） |
| `GET` | `/pending-approval` | 会话的未决审批（`?session_id=`） |
| `GET` | `/health` | 健康检查 |
| `GET` | `/tools` | 工具列表 |
| `GET` | `/tasks` | 任务列表 |
| `POST` | `/tasks` | 创建任务 |
| `GET` | `/tasks/{id}` | 获取任务详情 |
| `PUT` | `/tasks/{id}` | 更新任务 |
| `DELETE` | `/tasks/{id}` | 删除任务 |
| `GET` | `/plan` | 计划状态 |
| `POST` | `/plan` | 进入计划模式 |
| `DELETE` | `/plan` | 退出计划模式 |
| `GET` | `/bgtasks` | 后台任务列表 |
| `GET` | `/bgtasks/{id}` | 后台任务详情 |
| `POST` | `/bgtasks/{id}/stop` | 停止后台任务 |
| `GET` | `/usage` | Token 使用成本统计 |
| `GET` | `/audit` | 工具执行分析 |
| `GET` | `/mode` | 当前权限模式（`?session_id=` 返回该会话生效的模式） |
| `POST` | `/mode` | 切换权限模式：`{"mode":"auto"}` 切换全局；带 `session_id` 只切换该会话，`mode` 为空表示撤销会话级设置 |
| `GET` / `POST` | `/thinking` | 思考强度档位 |
| `GET` / `POST` | `/model` | 当前模型 / 运行时切换模型 |

**示例：**

```bash
# 创建任务
curl -X POST http://localhost:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{"subject": "实现登录功能", "description": "包含用户名密码和OAuth"}'

# 获取成本统计
curl http://localhost:8080/usage

# 获取分析统计
curl http://localhost:8080/audit
```

### 嵌入式 SDK

四个运行入口只差两个问题：**要不要多轮会话**（同一 sessionID 的历史、授权、权限模式跨请求保留），**历史谁管**（框架的 SessionManager，还是你自己的存储）。

| | 历史交给框架 | 历史你自己存取 |
|------|------|------|
| **多轮会话** | `RunSession(ctx, sessionID, input)` ← 推荐 | `RunWithSessionHistory(ctx, sessionID, history, input)` |
| **一次性** | `Run(ctx, input)` | `RunWithHistory(ctx, history, input)` / `Execute` |

- **`RunSession`**：唯一自动落盘的入口，**前提是配置 `WithSessionManager(mgr)`**（框架按 sessionID 向 mgr 加载/保存历史，未配置时直接报错）。mgr 用内置 `session.NewMemoryStore()` / `session.NewFileStore(dir)`，或实现 `session.StoreInterface`。
- **`RunWithSessionHistory`**：历史存取你写，框架仍给该 sessionID 完整会话身份（工作目录、工具过滤、会话权限模式、跨运行授权）。
- **`Run`**：问一句就走，不留状态。
- **`RunWithHistory`**：同 `RunWithSessionHistory` 但没有会话身份（会话级特性不可用），只适合纯无状态的单次长上下文调用。

```go
// 多轮会话（自动落盘、重载续跑）
for ev := range app.RunSession(ctx, "user-42", "继续上次的工作") { ... }

// 历史在宿主自己的存储里
history := loadHistoryFromDB(userID)
for ev := range app.RunWithSessionHistory(ctx, userID, history, "继续上次的工作") {
    if ev.Type == goagent.EventDone {
        // 完整对话记录：传入的历史 + 本轮消息 + 自动摘要时追加的摘要记录。
        // 原样存下、下一轮作为 history 传回即可；展示时过滤 IsMeta 消息。
        saveHistoryToDB(userID, ev.Messages)
    }
}

// 同步一次性调用
result, err := app.Execute(ctx, "写一个 hello world")
```

`RunHTTP` / `RunCLI` 是服务形态：`/chat` 配置了 SessionManager 时内部走 `RunSession`，否则退化为不落盘的 `RunWithSessionHistory`。

---

## 会话管理

### 基本用法

```go
store := session.NewFileStore("./sessions")
mgr := session.NewManager(store)

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionManager(mgr),
    goagent.WithBuiltinTools(),
)

// 第一轮
for ev := range app.RunSession(ctx, "user-123", "帮我创建一个 API") {
    handleEvent(ev)
}

// 第二轮（自动加载历史）
for ev := range app.RunSession(ctx, "user-123", "加上错误处理") {
    handleEvent(ev)
}
```

### 会话存储后端

```go
// 文件存储（默认，适合 CLI）
store := session.NewFileStore(".yume/sessions")

// 内存存储（测试用；多会话并发安全）
store := session.NewMemoryStore()

// 自定义存储：实现 Store 接口
type Store interface {
    Get(ctx context.Context, id string) (*Session, error)
    Save(ctx context.Context, s *Session) error
    AppendMessage(ctx context.Context, sessionID string, msg Message) error
    List(ctx context.Context) ([]SessionSummary, error)
    Delete(ctx context.Context, id string) error
}
```

### 多会话并行（单进程多项目）

`WithSessionWorkDir` 按 sessionID 解析会话专属工作目录，注入 context 流经 loop → executor → 工具：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    // 每个会话扎根自己的项目目录：Bash 的 cmd.Dir、Read/Write/Edit/Glob/Grep
    // 的相对路径全部以此为基准，多会话互不干扰
    goagent.WithSessionWorkDir(func(sessionID string) string {
        return resolveProjectDir(sessionID) // 返回 "" 回退进程 cwd
    }),
    goagent.WithBuiltinTools(),
)
```

工具内通过 `goagent.Context` 感知会话身份：

```go
func myTool(ctx goagent.Context, in MyInput) (string, error) {
    sid := goagent.SessionIDFromContext(ctx)   // 会话 ID（未注入为空串）
    wd := goagent.WorkDirFromContext(ctx)      // 会话工作目录
    // 也可直接读 ctx.SessionID / ctx.WorkDir 字段
    _ = sid; _ = wd
    return "ok", nil
}
```

HTTP 层原生支持多会话并行：`/chat` 传 `session_id` 即路由到对应会话（自动配 FileStore），SSE 事件带 `session_id` 字段供客户端分流。

### Task 按会话隔离

配合 `task.SessionStore`，Task 工具的存储按会话分区——不同会话的任务列表互不可见：

```go
store := task.NewSessionStore(task.SessionStoreConfig{
    DirFn:   func(sid string) string { return filepath.Join(".yume", "tasks") }, // 落盘（重启恢复），nil = 纯内存
    IdleTTL: 30 * time.Minute, // 闲置会话内存回收（惰性逐出，磁盘数据保留），0 = 不回收
})
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithTaskTools(),
    goagent.WithTaskStore(store),
)
```

Issue 工具（IssueReport/IssueResolve）——测试缺陷记录闭环，与 task 同存储（`metadata.kind=issue` 区分），修复后标记完成、历史保留。面向测试驱动场景，独立 Option 启用：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithTaskTools(),  // 可选；同时启用时 Issue 复用同一 store（含会话分区）
    goagent.WithIssueTools(),
)
```

---

## 会话事件总线与断线续传

每个会话有一份追加式事件日志（默认保留最近 2000 帧或 8 MiB）：`/chat` 输出的每一帧都带会话内单调的 `session_seq`，运行帧还带 `run_id`。断线后从任意位置接着读：

```
GET /runs/{session}/stream?after=<已读到的 seq>
```

- `after=N`：先补发缓冲中 seq > N 的帧，再转实时；也可用 `Last-Event-ID` 头（浏览器 `EventSource` 重连时自动带上）
- `after` 已滑出缓冲：首帧返回 `{type:"resync", oldest}`，客户端改用 `/sessions/{id}/messages` 全量重建
- 省略 `after`：从进行中运行的开头读起；`follow=1` 跨运行持续推送
- 会话被 `CloseSession` 关闭：游标读完缓冲后返回 `ErrSessionStreamClosed`

审批与提问经会话日志下发：挂在同一会话上的所有连接都收得到，客户端断开后请求留在日志里、重连即恢复（`/pending-approval`、`/pending-ask` 保留作兼容）。运行被中断时 `interrupted` 帧带 `reason`（`POST /interrupt` 的 body）。会话忙错误可用 `errors.Is(err, session.ErrBusy)` 判断。

进程内不经 HTTP 也可以读日志：

```go
watcher := app.WatchSession(sessionID, goagent.WatchFromActiveRun) // 或 WatchFromNow / 具体 seq
defer watcher.Close()
for {
    ev, err := watcher.Next(ctx) // SessionEvent{Seq, Kind, RunID, Event, Elapsed, Oldest}
    if err != nil {
        break
    }
    ...
}
```

---

## 自定义事件

工具、hook 里把领域信号发给前端，不用再塞进 `ask_user` 的 payload：

```go
// 运行之中：进入所在运行的事件流（子代理里发射同样到达最外层）
ctx.Emit("anime.canvas_command", CanvasCommand{Action: "add_node"})

// 运行之外：推给会话——有进行中的运行随流下发，否则进会话事件日志供续传
app.EmitEvent(sessionID, "amc.device_changed", deviceState)
```

- 名称 `<命名空间>.<事件名>`（字母数字与 `_-`，点号分隔；`goagent.` 前缀保留给框架）
- 值在发射点序列化；失败或不符合登记的 schema 时返回错误，不静默丢弃
- 可选 schema 登记：`app.RegisterEvent(name, schema)`，`GET /protocol` 列出已登记事件
- 线上：v1 帧为 `{type:"custom", name, value, session_seq}`；AG-UI 剖面为 `CUSTOM{name, value}`
- 观察者实现 `observer.CustomEventObserver` 收到；事件是展示层信号，不进会话记录

---

## AG-UI 1.0 剖面

`POST /agui` 用 [AG-UI](https://docs.ag-ui.com) 1.0 协议暴露同一个内核（与 v1 的 `/chat` 互不影响）：请求体是 `RunAgentInput`，响应是 AG-UI 事件流。正文、推理、工具调用、子代理、用量全部映射；扩展只走 `CUSTOM` 事件与 `goagent.` 前缀的 metadata。

人机协同是"两次运行"：本次运行以 `RUN_FINISHED{outcome:{type:"interrupt", interrupts:[…]}}` 结束（内核在审批点继续阻塞），客户端下次请求带 `resume:[{interruptId, status, payload}]` 作答，新流从停车处接着输出、不重发 `TOOL_CALL_START`：

| 中断类型 | reason | resume 载荷 |
|---------|--------|------------|
| 工具审批 | `tool_call`（带 `toolCallId` 与 `responseSchema`） | `{approved:true, editedArgs:{…}, reason?}`——`editedArgs` 替换工具参数执行（见「改参审批」） |
| 提问 | `ask_user`（结构化载荷在 `metadata["goagent.payload"]`） | `{answer:"…"}` 或字符串 |

- **前端工具**：请求 `tools` 里的工具由前端执行——模型调用时本次运行以 `success + pendingToolCallIds` 结束，客户端下次以 `role:"tool"` 消息带回结果（`error` 字段作为工具错误）
- **历史归服务端**：客户端 `messages` 按 id 去重，运行结束发 `MESSAGES_SNAPSHOT` 对齐；流式消息 id 与会话记录的 UID 一致
- 停车默认 30 分钟超时按拒绝处理：`WithAGUI(goagent.AGUIConfig{ParkTimeout})` 可调
- 有待作答的中断时发来无关请求：`RUN_ERROR{code:"pending_interrupts"}`；会话忙为 `thread_busy`；待回答的前端工具未带回结果为 `pending_tool_calls`
- `GET /agui/capabilities` 声明能力（审批、改参、前端工具、子代理、推理）

每个事件都经官方 schema 校验（`protocol/agui/testdata/schema-1.0.json`），官方 `@ag-ui/client` 的 `HttpAgent` 实连验证零告警。

---

## 内置工具

一行开启所有内置工具：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

> **注意**: `WithBuiltinTools()` 包含核心开发工具（Read/Write/Edit/Glob/Grep/Bash/GitCommit/WebSearch/WebFetch）。
> AskUser、Task、Plan、BgTask 等子系统工具默认不启用，需通过对应 Option 显式开启。

| 工具 | 说明 | 权限 |
|------|------|------|
| `Read` | 读取文件，支持 offset/limit 和行号；超长行截断、二进制检测、未修改短路 | ReadOnly |
| `Write` | 创建或覆盖文件；覆盖已存在但本会话未读过的文件会拒绝（防凭印象覆盖） | Normal |
| `Edit` | 精确字符串替换；未读过的文件拒绝编辑，失配时给诊断式报错（空白差异/部分命中/文件已变） | Normal |
| `Glob` | glob 模式文件搜索 | ReadOnly |
| `Grep` | 正则搜索，支持文件类型过滤 | ReadOnly |
| `Bash` | 执行 shell 命令，支持 timeout；cmd.Dir 按会话工作目录 | Normal |
| `GitCommit` | 会话工作目录 git add -A + commit；无变更短路，非仓库明确报错 | Normal |
| `WebSearch` | 搜索互联网 | Normal |
| `WebFetch` | 获取网页内容；默认只允许公网地址（内网用 `WebFetchToolWith`） | ReadOnly |

### 文件工具的读写状态跟踪（readstate）

文件工具内置会话级的读取状态跟踪（路径 → 内容指纹），对齐 Claude Code 的行为：

- **Read 短路**：同会话重复读未变化的文件返回「无需重读」，防止模型循环读取
- **Edit 前置校验**：本会话没读过的文件拒绝编辑（凭记忆编辑必然失配）
- **Write 覆盖保护**：覆盖已存在但未读过的文件拒绝
- **写后保持已读**：Edit/Write 之后文件仍算「已读」（内容 = 模型刚写的，它有最新视图），同批后续 Edit 放行；下次 Read 若无外部改动会短路
- **外部改动检测**：文件被会话外修改后指纹失配，Read 不再短路、Edit 会因 old_string 失配给出诊断

### Read

```go
type ReadInput struct {
    FilePath    string `json:"file_path" desc:"要读取的文件路径" required:"true"`
    Offset      int    `json:"offset,omitempty" desc:"起始行号（1-based）"`
    Limit       int    `json:"limit,omitempty" desc:"最多读取行数"`
    ShowLineNum bool   `json:"show_line_numbers,omitempty" desc:"是否显示行号"`
}
```

### Write

```go
type WriteInput struct {
    FilePath string `json:"file_path" desc:"目标文件路径" required:"true"`
    Content  string `json:"content" desc:"文件内容" required:"true"`
}
```

### Edit

```go
type EditInput struct {
    FilePath   string `json:"file_path" desc:"要修改的文件" required:"true"`
    OldString  string `json:"old_string" desc:"要替换的确切文本" required:"true"`
    NewString  string `json:"new_string" desc:"替换后的文本" required:"true"`
}
```

### Bash

```go
type BashInput struct {
    Command    string `json:"command" desc:"要执行的命令" required:"true"`
    Timeout    int    `json:"timeout,omitempty" desc:"超时时间（秒）"`
    WorkDir    string `json:"workdir,omitempty" desc:"工作目录"`
}
```

### 子系统工具（默认不启用）

以下工具默认不注册，需通过 Option 显式启用：

| Option | 工具 | 说明 |
|--------|------|------|
| `WithAskTools()` | `AskUser` | 向用户提问，获取输入 |
| `WithTaskTools()` | `TaskCreate/Update/Get/List` | 任务管理（依赖、状态） |
| `WithPlanTools()` | `EnterPlanMode/ExitPlanMode` | 规划模式（只读探索 + 计划编写） |
| `WithBgTaskTools()` | `TaskStop/TaskOutput` | 后台任务管理 |

```go
// CLI 应用：启用全部子系统
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithTaskTools(),
    goagent.WithPlanTools(),
    goagent.WithAskTools(),
    goagent.WithBgTaskTools(),
)

// Web 应用：只启用核心工具
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
)
```

---

## ToolKit 工具包

按领域分组的工具集合：

```go
// 只注册文件操作
goagent.WithToolKits(goagent.FileKit())

// 文件 + 搜索
goagent.WithToolKits(goagent.FileKit(), goagent.SearchKit())

// Shell 执行
goagent.WithToolKits(goagent.ShellKit())

// 用户交互
goagent.WithToolKits(goagent.InteractKit())

// 代码开发全套
goagent.WithToolKits(goagent.CodeKit())

// 全部内置工具
goagent.WithToolKits(goagent.AllKit())

// Web 搜索
goagent.WithToolKits(goagent.WebKit())
```

可用 ToolKit：

| ToolKit | 包含工具 |
|---------|---------|
| `FileKit()` | Read, Write, Edit |
| `SearchKit()` | Glob, Grep |
| `ShellKit()` | Bash |
| `WebKit()` | WebSearch, WebFetch |
| `InteractKit()` | AskUser |
| `CodeKit()` | FileKit + SearchKit + ShellKit |
| `AllKit()` | FileKit + SearchKit + ShellKit + WebKit |

---

## MCP：客户端与服务端

### 客户端：连接 MCP 服务器

```go
// 方式一：声明式（New 时连接，发现的工具自动注册）
app := goagent.New(cfg,
    goagent.WithMCP(agent.MCPServerConfig{
        Name:    "fs",
        Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", dir},
        // HTTP 服务器：Name + URL（+ Auth 作为 Authorization 头）
    }),
)

// 方式二：自己管理连接
client, tools, err := mcp.Connect(ctx, mcp.ServerConfig{Name: "fs", Command: "…"})
for _, t := range tools {
    app.Tool(t.Name, goagent.MCPToolDef(t))
}
defer client.Disconnect()
```

底层是官方 Go SDK（`github.com/modelcontextprotocol/go-sdk`），门面（`ServerConfig` / `Connect` / `Client` / `MCPToolDef`）不变。可用能力：调用取消（运行中断时通知服务端）、进度通知（显示为工具状态行）、图片结果（内联交给模型）、工具列表变更（`client.OnToolsChanged`，配合 `app.ReplaceTool` / `app.RemoveTool` 同步注册表）。工具注册名为 `mcp__<服务器名>__<工具名>`（`mcp.ToolName`，服务器名会做净化）。

服务器在工具调用中向用户索取输入（elicitation）时，转成所在会话的一次提问：payload 为 `{kind:"mcp_elicitation", server, mode, message, schema|url, error?}`；回答可以是表单 JSON、`{"action":"decline"}` / `{"action":"cancel"}`、单字段表单的直接文本（按字段类型转换）。`WithMCP` 默认启用；自行 `mcp.Connect` 时给 `ServerConfig.Elicit` 填 `goagent.ElicitViaAskUser`。

### 服务端：把工具与 agent 暴露为 MCP

```go
// Streamable HTTP（挂在宿主自己的路由与鉴权之后）
h, _ := app.MCPHandler(goagent.MCPServerOptions{
    Tools: []string{"search_docs", "create_ticket"},  // 只暴露列出的工具
    Agent: &goagent.MCPAgentTool{KeepSessions: true}, // agent 本身暴露为 run_agent 工具
})

// stdio（MCP 客户端以子进程启动宿主；stdout 是协议流，日志写 stderr）
app.ServeMCPStdio(ctx, opts)

// 或拿 SDK 的 *mcp.Server 自选传输、追加提示词/资源
server, _ := app.MCPServer(opts)
```

- 直接调用的工具不经模型、不经审批者（MCP 客户端负责向它的用户确认，工具带 `readOnlyHint` / `destructiveHint` 注解）；deny 规则、Guard 与权限模式照常生效；`Context.Progress` 转成 MCP 进度通知；结果开头的内联图片转成 MCP 图片
- agent 工具（默认名 `run_agent`）：入参 `{task, session_id?}`，跑完返回最终回复；`KeepSessions` 延续会话（省略 session_id 时新建并返回）；客户端取消调用即中断运行；带进度令牌时工具调用与子代理进展以进度通知下发
- agent 运行中的审批与提问经 elicitation 交给客户端的用户；客户端不支持 elicitation 时审批被拒绝（原因交给模型）

---

## 中间件

中间件在工具执行前后拦截，用于日志、审计、限流等：

```go
type LoggingMiddleware struct{}

func (m *LoggingMiddleware) BeforeTool(ctx goagent.Context, name string, input json.RawMessage) *goagent.Dision {
    ctx.Logger.Info("tool called", "name", name)
    return nil // 继续执行
}

func (m *LoggingMiddleware) AfterTool(ctx goagent.Context, name string, result *goagent.Result, err error) {
    if err != nil {
        ctx.Logger.Error("tool failed", "name", name, "error", err)
    } else {
        ctx.Logger.Info("tool completed", "name", name)
    }
}

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithMiddleware(&LoggingMiddleware{}),
)
```

### 内置中间件

```go
// 限流中间件：每个工具每分钟最多调用 10 次
app := goagent.New(
    goagent.WithMiddleware(ratelimit.New(10, time.Minute)),
)

// 审计中间件：记录所有工具调用
app := goagent.New(
    goagent.WithMiddleware(audit.New()),
)
```

---

## Hooks 框架

Hooks 在 agent 循环的关键事件点触发：

```go
// 5 种 Hook 类型
app := goagent.New(
    goagent.WithHooks(
        // 工具执行前
        hooks.PreToolUse(func(ctx context.Context, toolName string, input json.RawMessage) error {
            log.Printf("calling %s", toolName)
            return nil
        }),
        // 工具执行后
        hooks.PostToolUse(func(ctx context.Context, toolName string, input, result string, err error) {
            log.Printf("%s returned: %s", toolName, result)
        }),
        // LLM 生成前
        hooks.PreInference(func(ctx context.Context, req *provider.Request) error {
            return nil
        }),
        // LLM 生成后
        hooks.PostInference(func(ctx context.Context, req *provider.Request, resp *provider.Response) error {
            log.Printf("response: %s", resp.Message.Content[0].Text)
            return nil
        }),
        // 循环退出前
        hooks.OnStop(func(ctx context.Context) error {
            log.Println("agent stopping")
            return nil
        }),
    ),
)
```

### 预置 Hooks

```go
hooks.Log()          // 结构化日志
hooks.Audit()        // 审计日志
hooks.Metrics()      // Prometheus 指标
hooks.RateLimit(10)  // 限流
```

---

## Observer 可观测性

Observer 是后推送式的可观测性接口，与 Hooks 的预拦截互补：

- **Hooks**：预拦截，适合权限控制和流程干预
- **Observer**：后推送，适合监控、计费和审计

### 快速开始

```go
// 启用成本追踪
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithCostTracking(),
)

// 启用使用分析
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithAnalytics(),
)

// 注册自定义 Observer
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithObservers(myPrometheusObserver, myAuditLogger),
)
```

### 内置 Observer

| 选项 | 说明 |
|------|------|
| `WithCostTracking()` | 启用 Token 用量和 USD 成本追踪 |
| `WithAnalytics()` | 启用工具执行分析（调用次数、耗时、错误率） |

### 自定义 Observer

实现 `observer.Observer` 接口：

```go
type MyObserver struct{}

func (o *MyObserver) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
    prometheus.Record("token_usage", float64(usage.InputTokens+usage.OutputTokens))
}

func (o *MyObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
    log.Printf("tool started: %s", toolName)
}

func (o *MyObserver) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
    log.Printf("tool done: %s (%v)", toolName, duration)
}

func (o *MyObserver) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
    log.Printf("tool error: %s - %v", toolName, err)
}

func (o *MyObserver) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
    audit.Log("permission_granted", toolName, permission)
}

func (o *MyObserver) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
    audit.Log("permission_denied", toolName, permission, reason)
}

func (o *MyObserver) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
    log.Printf("compaction: freed %d tokens, reason: %s", tokensFreed, reason)
}

func (o *MyObserver) OnSessionStart(ctx context.Context, sessionID string) {}
func (o *MyObserver) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {}
func (o *MyObserver) OnError(ctx context.Context, err error) {}
```

### 获取追踪数据

```go
// 获取成本统计
summary := app.Usage()
fmt.Printf("Total cost: $%.4f\n", summary.TotalCostUSD)

// 获取分析统计
analytics := app.Analytics()
fmt.Printf("Tool calls: %d, Errors: %d\n", analytics.TotalCalls, analytics.TotalErrors)
```

### HTTP 端点

通过 HTTP API 访问追踪数据：

```bash
curl http://localhost:8080/usage
curl http://localhost:8080/audit
```

---

## OpenTelemetry 导出

`observer/oteltrace` 把运行导出为 OpenTelemetry span，名称与属性遵循 [gen_ai.* 语义约定](https://opentelemetry.io/docs/specs/semconv/gen-ai/)：

```
invoke_agent goagent                     一次运行（gen_ai.conversation.id、goagent.turns、goagent.cost_usd）
├── chat claude-sonnet-4-6               模型请求（请求/响应模型、用量含缓存、结束原因；失败带 error.type）
└── execute_tool Bash                    工具调用（goagent.duration_ms；失败为 Error 状态）
    └── chat / execute_tool …            子代理的请求与调用，挂在派发它的 Agent_* 调用下
```

```go
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(otlpExporter)) // 宿主自己的导出管线
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithObservers(oteltrace.New(tp, oteltrace.Options{
        AgentName:    "engine",    // gen_ai.agent.name
        ProviderName: "openai",    // gen_ai.provider.name
    })),
)
```

- span 随 ctx 传递（观察者可选接口 `observer.ContextObserver`）：工具内部用 otelhttp 包装的 HTTP 客户端、带追踪的数据库驱动自动挂到对应 `execute_tool` 的下面
- 权限决定、压缩、自定义事件记为所在 span 的事件；用户中断记为 `goagent.cancelled` 事件，不算失败
- 默认不记录提示词与回复内容（语义约定把内容采集定为可选项）
- 运行 ctx 里已有 span（如 HTTP 中间件的请求 span）时，`invoke_agent` 挂在它下面

每次 `OnLLMStart` 恰好对应一次 `OnLLMDone` 或 `OnLLMError`：换后备模型、压缩后重试、流中断恢复都会先结束上一次请求的 span。

---

## 测试工具

### 脚本化模型（provider/scripted）

按脚本应答的 `provider.Provider`，确定性测试多轮循环：每次模型请求按顺序消费一个 Step，并记录请求供断言。

```go
m := scripted.New(
    scripted.Tool("Read", map[string]any{"path": "a.go"}),      // 发起工具调用
    scripted.Text("读完了，没问题。").WithUsage(120, 30),          // 流式正文 + 用量
)
m.Route(scripted.SystemContains("你是代码探索助手"),             // 按请求分流（子代理与主 agent 共用模型时）
    scripted.Tool("Grep", map[string]any{"pattern": "TODO"}),
    scripted.Text("找到 3 处"))

app := goagent.New(goagent.WithProvider(m), ...)
// ... 运行 ...
m.AssertDone(t)          // 脚本恰好用完（比脚本多跑会报错，少了会残留）
```

Step 支持：思考（`WithThinking`）、分块流出（`WithChunkSize`）、多个并行工具调用（`Tools`）、请求直接失败（`Fail`，如 `&provider.RateLimitError{}` 测重试）、流出正文后断流（`StreamErr`，测锚点恢复）、请求断言（`Expect`）。`Requests()` 返回收到的全部请求。

### HTTP 录制回放（httprr）

第一次对真实服务录制（模型端点、MCP 服务器），之后离线回放——没有网络、没有密钥也能跑同一轮对话：

```go
rr, err := httprr.Open("testdata/chat.httprr", http.DefaultTransport)
if errors.Is(err, fs.ErrNotExist) {
    t.Skip("没有录制：设置 GOAGENT_HTTPRR=record 并提供密钥后录制")
}
defer rr.Close()
prov := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"), anthropic.WithHTTPClient(rr.Client()))
```

- 模式由环境变量 `GOAGENT_HTTPRR` 决定：`record` 录制、未设置回放、其他值按正则匹配文件路径
- 匹配键是方法 + URL + 请求体（请求头不参与——SDK 的 User-Agent、X-Stainless-* 随版本变化）；同一请求录到多次按顺序回放（先 429 后成功的重试）
- 录制自动清洗：`Authorization`、`X-Api-Key`、`Cookie`、`User-Agent`、`X-Stainless-*`、URL 的 `key` 参数、响应的 `Set-Cookie` 都不会进录制文件；`ScrubReq` / `ScrubResp` 追加自定义清洗（同时作用于回放的匹配键）
- `openai.Config{HTTPClient: rr.Client()}` 同理

---

## 子 Agent 系统

注册子 agent 供主 agent 调用：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSubAgents(
        agent.Definition{
            Name:        "researcher",
            Description: "专门做代码搜索和分析",
            SystemPrompt: "你是一个代码研究助手...",
            MaxTurns:    5,
        },
        agent.Definition{
            Name:        "reviewer",
            Description: "代码审查专家",
            SystemPrompt: "你是一个代码审查专家...",
            MaxTurns:    3,
        },
    ),
)
```

LLM 可以通过 `Agent_researcher`、`Agent_reviewer` 等工具调用子 agent。子 agent 的结果通过 `EventSubAgentProgress` 事件实时推送给主循环。

子 agent 与主循环使用同一个循环实现：有界并发、工具 panic 恢复为错误结果、上下文压缩、输出截断续写、限流重试。在某次运行之中调用时（包括宿主工具里直接用 `agent.NewRunner(...).Run`），子 agent 的工具调用继承该运行的检查链（plan 模式、中间件、hooks、权限门）与观测出口，按工具声明的级别检查：

```go
tools, _ := app.AgentTools("Read", "Grep", "Bash") // 级别与并发声明取自注册时
app.Tool("Agent_explore", app.AgentTool(agent.Definition{
    Name: "explore", SystemPrompt: "…", Tools: tools, MaxTurns: 20,
}))

// 直接构造的工具：标上权限级别；Serial 表示不与同轮其他调用并行
agent.ToolDef{Name: "deploy", InputSchema: schema, Permission: permission.LevelNormal, Serial: true, Execute: deploy}
```

### 子代理治理

每个 `Agent_*` 调用（同步或后台）以它的 tool_use_id 登记为可寻址的子代理：主 agent 与宿主都能查状态、发指示、单独中断，父运行不受影响。

```go
app.Subagents(sessionID)                        // 运行中 + 最近结束的子代理
app.Subagent(sessionID, agentID)                // 快照与结束后保留的对话记录
app.SteerAgent(sessionID, agentID, "先只看 go 文件") // 向运行中的子代理插话（找不到返回 ErrAgentNotFound）
app.InterruptAgent(sessionID, agentID, "方向不对")   // 只中断它，父运行继续
```

- 子代理的过程事件并入父运行的事件流：默认摘要（状态/活动/工具次数/token），`WithSubagentStream(goagent.SubagentStreamFull)` 全量转发文本与思考；每个事件带 `agent_path` 归属
- 模型自己也能用 `agent_send` / `agent_status` 工具协作（`WithSubAgents` 自动注册；`WithSessionToolFilter` 可过滤）
- 审批与提问带 `agent_path`，前端知道卡片属于哪个子代理
- HTTP：`GET /agents?session_id=`、`GET /agents/{id}`；`/chat`、`/queue`、`/interrupt` 都接受 `agent_id`
- 每会话同时运行的子代理默认上限 8（`WithMaxSubagents(n)`）；超出时 `Agent_*` 返回错误让模型串行化
- 后台子代理的启动回执带 `agent_id`，`TaskOutput` / `TaskStop` 也接受 agent_id；会话关闭时运行中的子代理被中断

---

## Pipeline DAG 编排

Pipeline 是基于 DAG（有向无环图）的多 Agent 编排引擎，支持拓扑调度、并行 Worker、Supervisor 审核。

### 核心概念

| 概念 | 说明 |
|------|------|
| **Node** | DAG 中的一个 Agent 实例 |
| **DependsOn** | 节点依赖，拓扑排序决定执行顺序 |
| **Concurrency** | Worker 数量，>1 时自动创建消息队列 |
| **Injects** | 队列注入权限，控制节点间数据流 |
| **Supervisor** | 可选的审核节点，审批/拒绝 Worker 结果 |
| **Review** | 标记结果需要 Supervisor 审核 |

### 节点参数

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `Name` | string | 必填 | 节点唯一标识 |
| `Agent` | *PipelineAgentDef | 必填 | Agent 定义（Name/Instruction/Tools） |
| `Concurrency` | int | 1 | Worker 数量，>1 自动创建消息队列 |
| `DependsOn` | []string | 无 | 依赖节点列表 |
| `Message` | string | 无 | 初始消息（Concurrency=1 时使用） |
| `Injects` | []string | 无 | 可注入队列的目标节点名称 |
| `MessageType` | any | string | 队列消息元素零值（如 `Task{}`） |
| `ResultType` | any | string | 队列结果元素零值（如 `Result{}`） |
| `Review` | bool | false | 是否需要 Supervisor 审核 |
| `ReviewBatch` | int | 1 | 累积多少条结果后触发审核 |
| `MaxRetries` | int | 3 | 最大拒绝次数（超限自动通过） |
| `OnResult` | func | 无 | 结果回调（审核通过后或无需审核时自动调用） |
| `QueueSize` | int | 64 | 队列缓冲区大小 |
| `OnEvent` | func | 无 | 节点运行事件回调（progress/thinking/error 透出，含 429 状态行） |
| `SharedData` | map | 无 | 全局共享数据（tool 通过 GetPipelineData 读取） |

### 线性 Pipeline

```go
// 定义任务和结果的类型
type Task struct {
    ID   string `json:"id"`
    Data string `json:"data"`
}

type Result struct {
    TaskID    string `json:"task_id"`
    Processed string `json:"processed"`
}

app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "将以下任务拆分为 3 个子任务...",
            Injects:     []string{"worker"},
            Agent: &goagent.PipelineAgentDef{
                Name:        "splitter",
                Instruction: "将大任务拆分为小任务，通过 send_task 工具发送到 worker",
                Tools:       splitTools,
            },
        },
        {
            Name:        "worker",
            Concurrency: 3,
            DependsOn:   []string{"splitter"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent: &goagent.PipelineAgentDef{
                Name:        "worker",
                Instruction: "处理单个子任务",
                Tools:       processTools,
            },
            OnResult: func(result any) {
                res := result.(Result)
                fmt.Printf("处理完成 %s: %s\n", res.TaskID, res.Processed)
            },
        },
    },
})

app.RunPipeline(context.Background())
```

`splitter` 的工具通过 `GetMessageQueue` 向 `worker` 队列推送任务（见[队列注入](#队列注入)）。
`worker` 处理完毕后，结果按 `ResultDefault` 类型传递给 `OnResult` 回调。

### 并行扇出 + 合并

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // 阶段 1：拆分
        {
            Name:        "split",
            Concurrency: 1,
            Message:     "分析以下需求，拆分到 3 个专业方向...",
            Injects:     []string{"a", "b", "c"},
            Agent:       splitAgent,
        },
        // 阶段 2：并行处理（自动并行，Concurrency>1 时创建队列）
        {
            Name:        "a",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentA,
        },
        {
            Name:        "b",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentB,
        },
        {
            Name:        "c",
            Concurrency: 3,
            DependsOn:   []string{"split"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent:       agentC,
        },
        // 阶段 3：合并（等 a/b/c 全部完成）
        {
            Name:        "merge",
            Concurrency: 1,
            DependsOn:   []string{"a", "b", "c"},
            Agent:       mergeAgent,
        },
    },
})
```

### Supervisor 审核

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "generator",
            Concurrency: 3,
            Message:     "生成 5 个创意方案，每个方案通过 submit_idea 工具提交",
            MessageType: Idea{},
            ResultType:  Idea{},
            Review:      true,        // 结果需要 Supervisor 审核
            ReviewBatch: 2,           // 每 2 条触发一次审核
            MaxRetries:  2,           // 最多拒绝 2 次后自动通过
            Agent:       ideationAgent,
            OnResult: func(result any) {
                idea := result.(Idea)
                log.Printf("审核通过: %s — %s", idea.Title, idea.Summary)
            },
        },
    },
    Supervisor: &goagent.PipelineAgentDef{
        Name:        "supervisor",
        Instruction: "审核方案：有创意且可行的调用 approve_result 批准，否则调用 reject_result 给出改进建议",
        // Supervisor 自动获得 approve_result / reject_result 工具（按 index 逐条处置）
    },
})
```

`Review=true` 标记节点产出的结果需要 Supervisor 审核。Supervisor 是独立于 DAG 的"上帝节点"，不参与拓扑调度；每批结果开一轮独立对话，自动获得两个审核工具：
- `approve_result` — 批准第 index 条结果，以真实结果触发 `OnTaskResult`；本轮没有处置的结果在轮末自动批准
- `reject_result` — 拒绝并附带改进建议，结果重新入队（超过 `MaxRetries` 后自动通过）

### 并行独立节点 + SharedData 注入

多个无依赖的节点自动并行执行，适合同一数据源的不同维度处理：

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // splitter：拆剧本，下游注入到 enrichers 和 storyboard
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "请读取项目剧本，拆分为各集，并识别角色、场景和道具。",
            Review:      true,
            Injects:     []string{"character_enricher", "scene_enricher", "storyboard"},
            Agent:       splitterAgent,
        },
        // worldview：与 splitter 并行，直接注入剧本内容提取世界观
        // 无 Injects/CloseQueues（无下游），独立完成
        {
            Name:        "worldview",
            Concurrency: 1,
            Message:     buildWorldviewMessage(ctx, services, projectID, userID),
            Review:      true,
            Agent:       worldviewAgent,
        },
        // enrichers 依赖 splitter...
    },
    SharedData: map[string]any{
        "project_id": projectID,
        "user_id":    userID,
    },
    Supervisor: supervisorAgent,
})
```

工具通过 `GetPipelineDataInt64` 从 SharedData 获取注入的 project_id：

```go
func NewCreateWorldviewEntryTool(svc *service.WorldviewService, userID int64) ga.NamedTool {
    return ga.InferTool("create_worldview_entry", "创建世界观条目",
        func(ctx context.Context, input CreateWorldviewEntryInput) (*CreateWorldviewEntryOutput, error) {
            // project_id 从 SharedData 自动获取，无需 LLM 传入
            pid, ok := ga.GetPipelineDataInt64(ctx, "project_id")
            if !ok || pid == 0 {
                return nil, fmt.Errorf("无法获取 project_id")
            }
            return svc.CreateWorldview(ctx, pid, userID, input)
        })
}
```

### 队列注入

工具内通过 `GetMessageQueue` 向下游节点推送任务：

```go
func processorTool(ctx goagent.Context, in SomeInput) (string, error) {
    downstream := goagent.GetMessageQueue(ctx, "next_stage")
    if downstream != nil {
        // Push 的类型需匹配下游节点的 MessageDefault
        downstream.Push(Task{ID: "1", Data: "处理这个任务"})
        downstream.Push(Task{ID: "2", Data: "处理那个任务"})
    }
    return "已推送 2 个任务到下游", nil
}
```

只有在当前节点的 `Injects` 中声明了目标节点名称，才能获取到队列。

---

## 内存与记忆

### CLAUDE.md

在项目根目录创建 `CLAUDE.md`，自动注入到每次会话的 system prompt：

```markdown
# 项目说明

这是一个 Go 微服务项目，使用 Gin 框架。

## 常用命令

- 启动服务：`go run ./cmd/server`
- 运行测试：`go test ./...`
- 构建：`go build -o server ./cmd/server`

## 代码规范

- 使用 golangci-lint 进行代码检查
- 提交前必须通过所有测试
```

通过 `WithProjectContext()` 也可以指定多个上下文文件：

```go
app := goagent.New(
    goagent.WithProjectContext("PROJECT.md"),
    goagent.WithProjectContext("ARCHITECTURE.md"),
)
```

### Auto Memory

自动记忆：对话结束后从历史中提取重要信息，保存到 memory 文件：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithMemoryDir(".goagent/memory"),
)
```

记忆文件结构：
- `MEMORY.md` — 主记忆文件
- `topics/` — 按话题分类的子记忆文件

### SessionMemory

长对话中定期提取关键信息，防止在上下文压缩时丢失：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionMemory(sessionmem.Config{
        MinTokensToInit:        10000,   // 首次提取的 token 阈值
        MinTokensBetweenUpdate: 5000,    // 两次提取之间的最小 token 差
        MemoryDir:              ".yume/.session-memory",
    }),
)
```

---

## 上下文压缩

每次请求前，压缩层按顺序处理发给模型的视图：

1. **预算截断（L0）**：超过 `MaxResultSize` 的工具结果保留首尾、截断中间（含内联图片的结果不截断，较早的图片回收为占位符）
2. **裁剪（L1）**：截断较早消息里的长文本
3. **微压缩（L2）**：上下文接近窗口上限时，剥离已被模型消费过的工具结果
4. **折叠（L3）**：把中间轮次折叠为摘要视图
5. **自动摘要（L4）**：仍超过 `AutoCompactThreshold` 时请求模型摘要，保留最近一轮；连续 3 次失败后熔断，不再尝试

L0–L3 只改视图。L4 的摘要以**摘要记录**追加进对话记录（`IsCompactSummary`、`CompactBoundary`，同时是元消息），随会话存储落盘；重新加载会话、排队续跑、`RunWithHistory` 传回历史时，循环用 `compaction.Project` 直接还原视图，不会再请求模型摘要一遍。`EventDone.Messages` 与 `WithTranscriptSink` 给出的是完整对话记录。

```go
app := goagent.New(
    goagent.WithCompaction(goagent.CompactionConfig{
        AutoCompactThreshold: 0.8,  // 上下文达到 80% 时触发
        MaxResultSize:        50_000, // 单个结果最大字符数
    }),
)
```

---

## Provider 配置

### 方式一：直接使用 ProviderConfig（推荐）

```go
// 连接本地 Ollama（无需 API Key）
app := goagent.New(goagent.ProviderConfig{
    Model:   "qwen2.5:7b",
    BaseURL: "http://localhost:11434/v1",
})

// 连接 OpenAI
app := goagent.New(goagent.ProviderConfig{
    Model:  "gpt-4o",
    APIKey: "sk-...",
})

// 连接 OpenRouter
app := goagent.New(goagent.ProviderConfig{
    Model:   "anthropic/claude-3.5-sonnet",
    APIKey:  "sk-or-...",
    BaseURL: "https://openrouter.ai/api/v1",
})

// 连接 vLLM
app := goagent.New(goagent.ProviderConfig{
    Model:   "meta-llama/Llama-3-8b",
    BaseURL: "http://localhost:8000/v1",
})
```

### 方式二：使用 Anthropic

```go
app := goagent.New(goagent.ProviderConfig{
    Type:   "anthropic",
    Model:  "claude-opus-4-6",
    APIKey: "sk-ant-...",
})
```

### 方式三：环境变量（WithOpenAI / WithAnthropic）

全部参数从环境变量读取，适合 `.env` 配置：

```go
// OpenAI 兼容：读取 OPENAI_MODEL, OPENAI_BASE_URL, OPENAI_API_KEY
app := goagent.New(goagent.WithOpenAI())

// Anthropic：读取 ANTHROPIC_MODEL, ANTHROPIC_API_KEY
app := goagent.New(goagent.WithAnthropic())
```

### 混合使用

`ProviderConfig` 和 `With*` Option 可以混用：

```go
app := goagent.New(
    goagent.ProviderConfig{
        Model:   "deepseek-chat",
        APIKey:  "sk-...",
        BaseURL: "https://api.deepseek.com/v1",
    },
    goagent.WithSystemPrompt("你是一个 AI 助手"),
    goagent.WithMaxTurns(50),
    goagent.WithBuiltinTools(),
)
```

### 备用 Provider

当主 Provider 负载高时自动切换到备用：

```go
app := goagent.New(
    goagent.WithProvider(primaryProvider),
    goagent.WithFallback(fallbackProvider),
)
```

### 运行时模型切换

```go
app.SetModel("gpt-4-turbo")  // 返回 false 表示不支持
```

---

## Prompt 配置

GoAgent 内置了一套对齐 Claude Code 的中文提示词体系，同时支持用户自定义。

### 方式一：使用内置提示词（默认行为）

不传任何 prompt Option，自动加载嵌入的 7 个 prompt 文件（对齐 Claude Code）：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

内置提示词包含以下组件（位于 `prompts/` 目录），对齐 Claude Code：

| 文件 | 用途 | 状态 |
|------|------|------|
| `system-identity.prompt.md` | Agent 身份定义 | ✅ 已集成 |
| `system-doing-tasks.prompt.md` | 执行任务指令 | ✅ 已集成 |
| `system-actions.prompt.md` | 谨慎执行操作 | ✅ 已集成 |
| `system-using-tools.prompt.md` | 工具使用策略 | ✅ 已集成 |
| `system-tone-style.prompt.md` | 语气和风格 | ✅ 已集成 |
| `system-output-efficiency.prompt.md` | 输出效率 | ✅ 已集成 |
| `system-reminder.prompt.md` | 系统提醒 | ✅ 已集成 |
| `compact.prompt.md` | 上下文压缩提示词 | ✅ 已集成 |

### 方式二：完全自定义

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithSystemPrompt("你是一个专注于代码审查的 AI 助手..."),
)
```

### 方式三：外部目录覆盖

通过 `WithPromptDir` 从外部目录加载 prompt 文件，找不到的文件会自动 fallback 到嵌入默认值：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithPromptDir("./my-prompts"),
)
```

可使用 `prompts.ExportDefaults(dir)` 一键导出所有默认 prompt 到指定目录，然后修改文件后重启生效。

### 提示词变量

内置提示词支持以下变量自动替换：

| 变量 | 说明 |
|------|------|
| `$cwd` | 当前工作目录 |
| `$boolean` | 是否为 git 仓库 |
| `$OS` | 操作系统 |
| `$OS_version` | 系统版本 |
| `$date` | 当前日期 |
| `$knowledge_cutoff` | 模型知识截止日期 |
| `$gitStatus` | Git 状态摘要 |

### 自定义提示词文件

从自定义文件加载提示词：

```go
import "github.com/Dream355873200/GoAgent/prompts"

// 加载内置提示词
identity := prompts.MustLoad(prompts.Identity)

// 带变量替换
content := prompts.LoadWithVars(prompts.Compact, map[string]string{
    "cwd":     "/path/to/project",
    "date":    "2024-01-15",
})
```

### WithPromptDir 外部目录

通过 `WithPromptDir(dir)` 指定外部 prompt 目录，框架优先从此目录加载 prompt 文件，找不到的文件会自动 fallback 到嵌入的默认值。

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithPromptDir("./my-prompts"),
)
```

**一键导出并修改**：

```go
import "github.com/Dream355873200/GoAgent/prompts"

// 导出所有默认 prompt 到目录（已存在的不覆盖）
files, _ := prompts.ExportDefaults("./my-prompts")
fmt.Println("已导出:", files)
// 修改 ./my-prompts/system-identity.prompt.md 后重启生效
```

**文件名对应**：

| 文件名 | 说明 |
|--------|------|
| `system-identity.prompt.md` | Agent 身份定义 |
| `system-doing-tasks.prompt.md` | 执行任务指令 |
| `system-actions.prompt.md` | 谨慎执行操作 |
| `system-using-tools.prompt.md` | 工具使用策略 |
| `system-tone-style.prompt.md` | 语气和风格 |
| `system-output-efficiency.prompt.md` | 输出效率 |
| `system-reminder.prompt.md` | 系统提醒 |

### 项目上下文（CLAUDE.md）

推荐使用 `CLAUDE.md` 注入项目特定知识：

```go
app := goagent.New(
    goagent.WithProjectContext("CLAUDE.md"),  // 项目根目录
    goagent.WithProjectContext("docs/ARCHITECTURE.md"),  // 额外上下文
)
```

`CLAUDE.md` 示例：

```markdown
# 项目说明

这是一个 Go 微服务项目，使用 Gin 框架。

## 代码规范

- 使用 golangci-lint 进行代码检查
- 提交前必须通过所有测试

## 常用命令

- 启动服务：`go run ./cmd/server`
- 运行测试：`go test ./...`
```

---

## CLI 命令

在 TUI REPL 中可用以下命令：

| 命令 | 说明 |
|------|------|
| `/model <id>` | 切换模型 |
| `/sessions` | 列出所有会话 |
| `/session <id>` | 切换到指定会话 |
| `/clear` | 清空当前会话，创建新会话 |
| `/tasks` | 显示任务列表 |
| `/cost` | 显示当前会话费用统计 |
| `/permissions` | 显示当前权限模式 |
| `/help` | 显示帮助 |

---
