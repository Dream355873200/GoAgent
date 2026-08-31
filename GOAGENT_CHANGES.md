# GoAgent 本地变更记录

本仓库（E:\claude-code-rev-study-main\...\goagent）是对 GitHub 上
Dream355873200/GoAgent 的本地增强副本。amobileCreater 的 engine 通过
go.mod `replace` 指向此处。**每次向 GitHub 推送前**：把下面对应条目
整理进正式 commit，然后移除 replace 升级版本号。

## 2026-08-31（测试模式 WithDebugMode + loop 引擎日志面）

- **起因（生产排查）**：漫剧 pipeline 启动无反应、429 限流日志不打印。
  根因是 loop 引擎**零日志**——所有引擎转折（限流重试/过载切换/截断
  恢复/错误退出）只发 Event 或调 Observer，一个字都不写日志；pipeline
  的 runLightweight 又丢弃 Progress 事件，三条出口（事件/observer/日志）
  全断，撞 429 时外部完全不可见。
- **loop 日志面（Config 新增 Logger/Debug）**：logInfo/logWarn/logError/
  logDebug 四个辅助方法，nil logger 安全。插桩点——启动（model/tools/
  msgs/max_turns）、429 限流（等待时长+attempt+恢复/耗尽）、过载切后备、
  API 错误/流中断/上下文取消/最大轮次/上下文耗尽（全部终止路径）、
  max_tokens 截断恢复两级、thinking-only 恢复、stop hook 阻止、每轮完成
  摘要（tools/input_tokens/msgs）、正常完成（turns/stop/text 长度）。
  debug 细节（logDebug）：逐轮请求参数（msgs/tools/max_tokens/token
  估算）、tool_start/tool_done（id/input_len/content_len/err）。
- **WithDebugMode() Option**：测试模式开关（appConfig.debug）。关键事件
  日志默认输出（不受开关控制）；细节日志仅 debug 开启时输出。级别全部
  走 Info/Warn/Error——**不依赖 slog Debug level**，宿主无需调整 handler
  级别，开启即生效。对齐 README TODO「Agent 迭代测试模式」的信息暴露
  原则（测试模式的中间信息应尽可能可获取）。
- **接线全覆盖**：App.run 主循环（goagent.go）传 Logger=a.logger +
  Debug=a.config.debug；pipeline 节点 loop（buildLightweightLoop）同样
  继承——pipeline 的 429 此前完全不可见，这是本次修复的主场景；
  supervisor 的 subApp/finalApp（每轮审核现造的 App）经
  WithLogger+withDebugModeIf 继承（no-op Option 兜底，off 时零变化）。
- **与 Observer 的分工定稿**：logger 给人（排查/测试文本轨迹），
  Observer 给程序（计费/SSE/benchmark 结构化回调）。429 等**引擎内部
  转折不发 observer**（粒度是业务事件不是引擎转折），所以 observer
  覆盖不了本问题——补日志是正解；未来 benchmark 若需机器消费限流
  数据再加 OnRateLimitRetry 接口。
- **测试**：internal/loop/logging_test.go——关键事件无 debug 也输出/
  debug 细节受开关控制/nil logger 不 panic。全量 build/vet/test 过，
  默认路径（不注入 Logger）零行为变化。

## 2026-08-29（stop hook 双轨合并）

- **hooks 包成为 stop 扩展点唯一入口（接口演化债清理）**：原
  internal/loop/stophook.go（StopHookRunner，老轨道）与 hooks 包
  EventStop（新框架）双轨并存、串联执行——成因是通用 hook 框架
  出现时未吸收老机制签名能力，老轨道因内置截断 hook 在用未删。
  合并内容：
  - HookContext 加 StopReason/LastAssistantText/Messages 字段
    （EventStop 专用现场信息——签名决定 hook 能做什么）
  - Manager.RunStop 签名升级：RunStop(ctx, sessionID, stopReason,
    lastText, messages)——loop 的退出路径两段串联合成一段
  - NewStopHook(name, fn) 函数式包装；内置 MaxOutputTokensStopHook
    （数 ``` 奇偶的截断兜底，引擎 stopReason 恢复优先于此）迁入
    hooks/stophook_builtin.go
  - 删除 internal/loop/stophook.go（老轨道零外部引用，安全删除）
  - 三层职责定稿：引擎自愈（loop 主循环，提 max_tokens 上限/续写
    提示——需改请求参数只能住引擎）/ 内置启发式（hooks 包）/
    用户扩展（WithHooks）
- **测试**：hooks/stophook_test.go——现场信息透传/block 生效/
  截断检测奇偶判断/恢复上限防死循环。全量 build/vet/test 过，
  AnimeCreater 零影响（双轨本来都未注册使用）。
- **文档**：interview-guide/deep/tools.md 补 executor 三层（含
  TrackedExecutor 死代码的诚实说明）+ stop hook 合并后三层职责。
  README TODO 的合并条目同步完成（原计划与 EventInterrupted 一起
  做，用户指示提前单独完成）。

## 2026-08-28（沙箱层 Tier 0/1 + pipeline 会话上下文缺口修复）

- **沙箱契约（sandbox.go 新增）**：`Sandbox`/`SandboxSession`/`Policy`
  （FS 白名单最长前缀优先、无匹配拒绝、Net/Env/Timeout 携带）/
  `NoopSandbox` 透传 / `ExecSession` 可选接口（Tier 2 Docker 路由预留，
  Bash type-assert）。设计定位：策略层物化进 `Context.Sandbox` 字段
  而非 Execute 包装器——进程内包装拦不住任何东西，真实执行面是
  路径解析与子进程派发。单 agent / pipeline 节点 / benchmark trial
  都汇入 ToolDef.call 同一咽喉点，一处接线全覆盖。
- **DirSandbox（sandbox_dir.go 新增）**：每次 Enter 在 BaseDir 下建
  MkdirTemp 独立根；相对路径按根解析 + Clean 词法捕获 `..` 逃逸；
  空策略归一化为根读写；Close 防御性校验根仍在 BaseDir 下才删。
- **WorktreeSandbox（sandbox_worktree.go 新增）**：git worktree 作
  沙箱根（无全局 chdir、不占 single-current，多会话并发安全），
  KeepOnClose 控制收割/即抛即弃。依赖 worktree 包新拆的 Create/Remove。
- **worktree 包拆分**：Enter 机械拆出 `Create(name)`（git worktree add
  不赋 current 不 chdir）与 `Remove(wt, force)`；Enter/Exit 行为零变化
  （builtin 管理工具依赖旧语义）。多会话可并发持有 worktree。
- **内置工具接线（builtin）**：Read/Write/Edit 调用点走
  `resolvePathChecked`（沙箱在场→ResolvePath+策略检查，违规返回 LLM
  可读中文错误）；Glob/Grep 的 `resolveRootChecked` 默认根 = 沙箱根；
  Bash cmd.Dir 优先沙箱 CommandDir、超时 min(输入, Policy.Timeout)、
  ExecSession 断言钩子。`sandboxOf` 双路提取（结构体字段 + context
  value）让手工构造 Context 的调用方也能接线。
- **WithSandbox(sb, policy) Option + App.run 接线**：每次 run Enter
  独立会话，Enter 失败发 EventError 干净返回（不崩宿主）；沙箱根
  非空时覆盖 WithSessionWorkDir 的 workDir（优先级规则，仅 opt-in 激活）。
- **pipeline 会话上下文缺口修复（独立 bug）**：PipelineConfig 新增
  `SessionID` 字段，RunPipeline 注入 `WithSessionContext`——此前
  pipeline 节点工具的 Context.WorkDir/SessionID 恒为空（主循环在
  App.run 注入，pipeline 路径漏了）。未配 sessionWorkDirFn 时行为不变。
- **PipelineNode.Sandbox *Policy 节点级覆盖**：runNode 期间单独 Enter
  会话（生命周期横跨全部 reject 重试）；RunPipeline 创建 run 级会话
  覆盖 supervisor + 全部节点。
- **文档**：README 新增「沙箱（Sandbox）：工具执行层的隔离」节
  （Tier 0-3 阶梯表、代码示例、优先级规则、Tier 1 边界声明——
  进程级隔离是宿主部署者责任）；L4 TODO 节标注 WorktreeSandbox 即
  其 Tier 1 地基。
- **测试**：sandbox_test（Noop 透传/最长前缀/DirSandbox 收敛/Close
  防御/大小写）、sandbox_worktree_test（无 git skip 守卫/无 chdir 断言/
  KeepOnClose）、builtin/sandbox_test（Write 根外拒/Read-Edit 链路/
  Bash cmd.Dir + Timeout 上限）、sandbox_pipeline_test（probeProvider
  假 LLM 驱动工具调用：缺口修复/run 级注入/节点级覆盖/Enter 失败）。
  全量 go build/vet/test 通过，默认路径（不配沙箱）行为零变化。
- **L4 选型定稿（README TODO 细化）**：goja（JS 解释器）为主路径（语言级
  硬隔离——运行时不实现 os/fs/net，能力不存在而非被拦截；LLM 产出 JS +
  拿异常栈修正是最熟的反馈回路）；goja+子进程为保守模式（工程意义零逃逸）；
  wazero 留给 L5 常驻运行时（能力注入接口同构）；Docker 不进库。
  安全三层账：零注入删不了宿主文件 / 注入能力必须套 Policy 射程双锁 /
  解释器 bug 残余风险爆炸半径=宿主用户权限。落地顺序 L4-α（执行器+
  资源限额）→ L4-β（确定性模式+子进程保守模式）→ L5（wazero）。
- **L5 实现骨架 + 三闸门安全设计（README TODO 细化，讨论定稿）**：
  实现骨架——ToolSpec DSL（name/description/input_schema/impl/
  capabilities）→ register_tool 注册（复用现有 app.Tool()）→ 装配零新
  代码（BuildDynPipeline 按名解析自同一注册表，同会话先注册后装配）。
  三闸门：① 执行体准入——只允许 primitive_chain（受审原语白名单组合，
  Bash 禁入）和 code（goja/wasm 沙箱）两种形态，「原生执行体」在协议
  里物理不存在；② 接口面净化——description/schema 过注入检测（执行体
  在沙箱里但「嘴」在系统里，防持久化提示注入借宿主权限行凶）；③ 权限
  低位+注册前沙箱探针评估+能力只减不增+依赖变更自动降级。
  权限三层全景：进程权限层（开发者手写，agent 只可用不可造）/ 受裁射程
  层（沙箱能力+Policy 逐 tool 裁决）/ 零权限层（纯计算）。新增系统能力
  的唯一通道 = agent 提案、开发者写原语（「提案-实现」工作流）。
- **pipeline 审批三情况 + spec 可读性三视图（README TODO 细化）**：
  pipeline 里同步审批会卡死 DAG/并发风暴/无人值守不可用——审批前移到
  装配时刻为主方案（BuildDynPipeline 校验「引用 untrusted 且未授权 →
  拒绝」，授权走 PipelineConfig.TrustedTools 或 App 级白名单，一次审批
  覆盖整 run）；supervisor 只审产出质量不碰权限（LLM 没资格给 LLM
  放行系统权限）；宽射程工具走异步审批逃生舱（pending_approval +
  节点暂挂 + SSE，复用 429 状态行机制）。可读性：DSL 是给机器的账本，
  人看账单——审批卡片（DAG 图 + provenance 徽章 + 射程摘要，嵌入方
  复用画布组件）/ spec+自然语言说明成对提交并交叉验证（零成本，
  对齐 plan mode 模式）/ Pulumi 式代码视图（远期可不做）。
- **eino 对照盘点 + 补缺清单（README 新 TODO 节）**：定位「eino 造引擎、
  GoAgent 造整车，全自研无 fork」。补缺按重要性：RAG 组件层（高）、
  带 checkpoint 的 interrupt/resume（高——根包现在只有 cancel）、
  agent 模式库（中，prebuilt 配方）、组件级 callbacks 切面（中）、
  通用 Graph 编排（低，刻意不做项）。ctx 三优化：observer 事件 span
  关联、沙箱 ctx 覆盖契约成文、observer.FromContext 单 run 注入
  （benchmark 每 trial 独立采集的直接前置）。
  注：早前探查报告声称 goagent/agent 是 eino adk 的 fork——已证伪
  （agent/ 下 5 个自研文件 1109 行，子 agent 系统，与 adk 文件零对应），
  对比结论已按「全自研」修正。
- **eino 对比深挖定稿（README 差异定位节大扩充）**：
  graph vs pipeline 范式对比（函数组件数据变换网 vs agent 任务流水线，
  六维分野表 + 「拓扑是代码还是数据」判断公式）；动态拓扑三场景分解
  （A 数据分支不需要动态/B LLM 建图我们领先 dynpipeline/C 热重载是
  基础设施职责）——「补 graph」论证闭环为不做。RAG 双形态四件套：
  Document+窄接口四兄弟为共享底座，WithRetrieval 嵌入管道为主
  （含 ShouldRetrieve 谓词）+ retrieve 工具为辅；多路检索走
  FusionRetriever 组合模式（框架不穷举组合）；边界诚实声明。agent
  模式库逐个判断：Plan-Execute 做（零件 70%）、AgentTool 转接头顺手做
  （SubAgent=运行时委派 vs AgentTool=构造时组合，同机制两朝向）、
  Supervisor 缓但设计定稿（路由节点复用队列消息=边静态流动态，
  transfer 四问答案：prompt 候选清单/消费队列不接收/无兄弟不注册/
  LLM 判断非知道；合法移交图=DAG 子图（Injects 声明）；AllowRevive
  唤醒逃生舱；完整规则表）、DeepAgent 不做（SubAgent 就是它）。
  Pipeline Registry 升级：prompt 是 agent 时代真正难做的（分享配方
  价值密度>分享组件），生态三步顺序（RAG 层→格式→社区）。领先面补
  agent 自建 DAG（eino 组件编译期插、LLM 没有手）。
- **Supervisor 模式改判（用户裁决）**：移交语义放弃队列数据流方案，
  按 eino 控制流模式做——transfer = 对话控制权移交（完整消息历史 +
  身份切换，落点在 loop 消息列表交接），子 agent 协作的主流形态是
  接力而非工单分发；队列消息流只在 pipeline 节点间保留。v1 队列方案
  废弃原因记录在案（为架构统一牺牲语义正确性=框架设计经典陷阱）。
  同时 LangGraph 对比入档（控制流状态机路线，与 eino 数据变换网、
  我们任务流水线三分）；组件 callbacks 缺口校准为两处——LLM 调用级
  钩子（OnLLMStart/Done/Error）+ ctx span 配对（工具级钩子已有）。
- **概念澄清后的三项 TODO 增补**：① 路由节点（RouterNode）补回——
  pipeline 内数据流路由（任务按类型分奔 worker），与 transfer 互补
  而非替代（协作走控制流接力 / 分发走数据流路由，两种移交语义对应
  两类场景不互相冒充）；② 装配期类型校验——PipelineNode.MessageType
  升级为上下游类型一致性校验（eino「编译期验证」的等价物，与 L3
  动态类型 schema 校验汇流）；③ LangGraph 应用场景入档（长时有状态
  agent / human-in-the-loop interrupt-resume / 多 agent 状态机，
  定位「agent 版 Temporal」，学零件不学产品）。
- **interrupt/resume 补缺 #2 定稿（README TODO 重写）**：对标 LangGraph
  学零件不学产品。终止能力审计（已全）：/interrupt 端点、按会话路由
  cancel、被中断工具写回 tool_result（消息序列完整同会话可续聊）、
  历史落盘保留、SSE 断开≠取消。缺三件：EventInterrupted 一等事件
  （~20 行，用户终止与报错分离）；进程内挂起/恢复（goroutine 挂起
  非取消，复用 429 机制，服务分钟级审批）；挂起点 checkpoint 落盘
  （审批周期常超进程生命周期；成本核算：消息历史落盘已有+
  RunWithHistory 已是历史喂回，新增仅「执行位置+挂起原因」小块；
  resume=RunWithHistory+断点续跑，与「状态归业务循环归框架」哲学
  同构）。不做任意点回放/时间旅行。终止/挂起/恢复三语义分离。
  （此条历经两轮用户纠偏：先误判持久化不必做——忽视审批周期超
  进程生命周期；再被追问终止能力——实测发现终止链路已全。）
- **TODO 执行顺序标定（内容零删减，用户裁决「不瘦身，模块化即可」）**：
  TODO 区顶部加三梯队开工序——第一梯队 AnimeCreater 直接收益
  （EventInterrupted / observer+ctx / interrupt 挂起恢复 / RAG 四件套）、
  第二梯队 benchmark 自迭代最短路径（评测闭环 → L4-α）、第三梯队真空
  需求（模式库/组合/Registry/L2-L5 等，届时按真实需求密度重排）。
  不排日期；顺序即需求过滤器（前两梯队会自然长出或证伪第三梯队
  的需求）。

## 2026-08-28（动态 Pipeline L1 + 嵌入者体验）

- **动态 Pipeline L1（DynPipelineSpec / BuildDynPipeline / create_pipeline 工具）**：
  LLM 运行时自建 DAG——JSON DSL（节点/依赖/注入/工具按名选）→ PipelineConfig。
  校验矩阵全 LLM 可读（空 nodes/节点数上限 12/重复名/工具不存在附可用清单/
  depends_on+injects 悬空/DFS 环检测带环上节点序列）。失败即终止（重规划是 L2，
  见 README TODO「动态 Pipeline」三层级规划）。
- **WithLogger(l *slog.Logger)**：库内日志注入 API。pipeline 的 37 处
  stdlib log.Printf 全部改为 slog（recordNodeError 现场→Error、WARN/错误
  相关→Warn、轨迹→Info），不再污染宿主日志体系。默认 slog.Default()
  （宿主 slog.SetDefault 可全局接管）。
- **pipeline 错误严格传播**：runLightweight 此前仅在零产出时才算失败——
  LLM 中途报错但已有部分工具产出时静默当成功，残缺产出流向下游且无日志。
  现在一律 recordNodeError，错误信息带部分产出上下文（工具计数/文本长度）。
- **PipelineConfig.OnEvent**：节点运行事件透出回调（PipelineEvent：
  progress/thinking/error）。progress 含 429 状态行的 StatusKey 原地更新
  语义——此前 EvtProgress/EvtThinking 在 runLightweight 里被静默丢弃。
- **WithIssueTools()**：Issue 工具独立 Option（测试驱动场景专用，不与
  WithTaskTools 绑定）；同时启用时复用同一 store（含会话分区）。
- **nil provider 守卫**：loop 对未配置 provider 直接发 EvtError 可诊断
  错误——此前在 goroutine 里 nil pointer panic，整个进程崩掉。
- **清理 agent/swarm.go**（206 行死代码）：职责已被 SubAgent/Pipeline 覆盖。
- **readstate 写后保持已读**（4fca27e）：Edit/Write 后指纹更新为写入内容，
  同批后续 Edit 放行、Read 无外部改动时短路。

- **WithLogger(l *slog.Logger)**：库内日志注入 API。pipeline 的 37 处
  stdlib log.Printf 全部改为 slog（recordNodeError 现场→Error、WARN/错误
  相关→Warn、轨迹→Info），不再污染宿主日志体系。默认 slog.Default()
  （宿主 slog.SetDefault 可全局接管）。
- **pipeline 错误严格传播**：runLightweight 此前仅在零产出时才算失败——
  LLM 中途报错但已有部分工具产出时静默当成功，残缺产出流向下游且无日志。
  现在一律 recordNodeError，错误信息带部分产出上下文（工具计数/文本长度）。
- **PipelineConfig.OnEvent**：节点运行事件透出回调（PipelineEvent：
  progress/thinking/error）。progress 含 429 状态行的 StatusKey 原地更新
  语义——此前 EvtProgress/EvtThinking 在 runLightweight 里被静默丢弃。
- **WithIssueTools()**：Issue 工具独立 Option（测试驱动场景专用，不与
  WithTaskTools 绑定）；同时启用时复用同一 store（含会话分区）。
- **nil provider 守卫**：loop 对未配置 provider 直接发 EvtError 可诊断
  错误——此前在 goroutine 里 nil pointer panic，整个进程崩掉。
- **清理 agent/swarm.go**（206 行死代码）：职责已被 SubAgent/Pipeline 覆盖。
- **readstate 写后保持已读**（4fca27e）：Edit/Write 后指纹更新为写入内容，
  同批后续 Edit 放行、Read 无外部改动时短路。

## 2026-08-27（多会话架构 + 工具质量对标）

### 会话体系（多项目并行的地基）
- **WithSessionWorkDir(fn)**：新 Option。按 sessionID 解析工作目录，
  注入 ctx（context value）流经 loop→executor→工具。返回 "" 回退进程 cwd。
- **WithSessionContext / SessionIDFromContext / WorkDirFromContext**：
  context.go 公开 API。App.run 自动注入会话值。
- **newContextFromStd 透出 SessionID/WorkDir**：工具的 goagent.Context
  字段从此真正有值（此前恒为空串——设备锁等按会话隔离的功能全部失效）。
- **HTTP 层多会话并行**：http.go 拆出 newHTTPMux（可测试）；
  sseEvent 增加 session_id 字段（客户端按会话路由事件）。
  集成测试 TestHTTPParallelSessions 验证两会话并行 626ms（串行需 1200ms）。
- **session.MemoryStore 并发安全**：全部方法加锁（此前多会话并行必现
  data race）。

### task 系统（task 跟随 session）
- **task.SessionStore**：按会话隔离的任务存储。ForSession 分区路由；
  DirFn 落盘（JSON，进程重启恢复）；IdleTTL 闲置回收（惰性逐出+磁盘恢复）；
  SessionRouter 接口供工具层路由。
- **task 工具按会话路由**：TaskCreate/Update/Get/List 经 storeFor(ctx, store)
  路由到会话分区（存储实现 SessionRouter 时）。
- **HTTP /tasks 支持 ?session_id=** 路由。
- **ListSummary 增加 Metadata 字段**（issue 标记等自定义分流）。
- **IssueReport / IssueResolve 工具**：测试缺陷记录闭环（存 task store，
  metadata.kind=issue 区分；修复后标记完成，历史保留）。

### 工具质量（对标 Claude Code 文件工具）
- **readstate.go（新增）**：会话级文件读取状态跟踪（路径→内容指纹）。
- **Read 强化**：超长行截断（>2000 字符中间挖空）；二进制检测
  （NUL/控制字符采样，返回类型提示而非乱码）；未修改短路
  （同会话指纹一致返回「无需重读」，防循环读取）。
- **Edit 前置校验**：本会话未 Read 过的文件拒绝编辑（凭记忆编辑必失配）；
  old_string 未命中时给诊断式报错（空白差异/部分命中/文件已变）。
- **Write 覆盖保护**：覆盖已存在但未读过的文件拒绝（防凭印象覆盖）。
- **Bash**：cmd.Dir 按会话工作目录；描述加「兜底不是首选」指引。
- **工具路径解析**：Read/Write/Edit/Glob/Grep 相对路径解析到会话工作目录
  （resolvePath/resolveRoot，nil-ctx 安全）。
- **SkillTool 强化**：描述内嵌可用 skill 清单（模型看得见才知道能调）；
  skill=list 动作（运行时合并项目目录+全局注册表，项目版覆盖同名全局）；
  打错名报错附可用清单；readProjectSkill 按会话 WorkDir 现读项目
  .yume/commands/（多会话多项目互不串）。
- **GitCommit 工具（新增，进 CoreTools）**：会话工作目录 git add -A +
  commit；无变更短路；非仓库明确报错。应用层用它做版本快照纪律。
- **ReplaceTool(name, def)**：App 级 API，覆盖注册单个工具
  （应用层包装内置工具加领域禁令等策略）。

### provider / 稳定性
- **429 语义区分**：openai provider 的 HTTP 429 从 OverloadError 改为
  新增的 provider.RateLimitError（速率限制切后备模型无济于事，必须等待）。
- **loop 429 重试**：最多 10 次，等待对齐 TPM 窗口（15s 起步递增至 60s），
  等待期间每秒推送带 StatusKey 的倒计时状态事件（前端原地更新闪烁，
  对齐 Claude Code 的「✻ 429 · Retrying in Xs · attempt N/10」）；
  Event 增加 StatusKey 字段（非空=状态行替换语义，空 Text=清除）。
- **loop 提前落盘**：assistant 消息完成（工具执行前）即同步 finalMessages
  ——工具跑几十秒的窗口里前端重连也能看到最新轮次。
- **tool_result 内联图片**：openai provider 把工具结果里的
  [IMAGE <media> <base64>] 前缀块转多模态 content（vision_ask 用）。

### 测试
- context_test.go：会话注入/提取/空值契约
- http_parallel_test.go：HTTP 并行 + G1 全链路（工具看到 SessionID/WorkDir）
- task/sessionstore_test.go：隔离/落盘恢复/闲置回收/路径消毒
- builtin/task_routing_test.go：Task 工具会话路由
- builtin/issue_test.go：Issue 记录/关闭/会话隔离
- builtin/git_commit_test.go：真实仓库提交闭环
- builtin/skill_list_test.go：清单合并/项目覆盖/描述内嵌
- builtin/filetools_test.go：Read 截断/二进制/短路、Edit 前置校验/诊断、
  Write 覆盖保护、会话隔离（7 用例全过）
