# GoAgent 本地变更记录

本仓库（E:\claude-code-rev-study-main\...\goagent）是对 GitHub 上
Dream355873200/GoAgent 的本地增强副本。amobileCreater 的 engine 通过
go.mod `replace` 指向此处。**每次向 GitHub 推送前**：把下面对应条目
整理进正式 commit，然后移除 replace 升级版本号。

## 2026-09-01（⑤ benchmark B2 自迭代武装——一二档判分可靠性）

- **范围决策（用户共识）**：自迭代一二档开放（prompt/skill——声明式
  文本可 diff 可回滚；工具注册本身结果明显用单测，但工具 description
  属 prompt 仍可 bench）；三档（agent 改核心代码）等沙箱 Tier 2 进程
  隔离后再开——Tier 1 防意外不防恶意，三档恰恰制造恶意场景。
- **B2 终态断言族（benchmark/state.go）**：TargetOutput 加 State
  字段（工作区快照 map[path]content，二进制跳过）；四个断言原语——
  file-exists（存在性）、file-not-exists（Preserve 语义：不该动的
  没动，SWE-bench PASS_TO_PASS）、file-contains（单文件内容）、
  state-equals（期望快照精确比对，缺失/内容不同/多余全识别，分母
  取大边——agent 留垃圾终态是脏的）。τ-bench×SWE-bench 交叉结论：
  「对话内容对」和「副作用对」是独立通道，只看输出的断言是纸糊的。
- **GoldReplayer（根包 gold.go）**：DeriveState(ctx, dir, tools,
  calls) 在隔离目录回放参考工具调用序列（走 ToolDef.call 同一咽喉点
  ——WorkDir/沙箱解析与真实 agent 运行一致），返回终态快照。SABER
  教训：手写期望态是标注错误重灾区（τ-bench 被批出 50+ 处，修复后
  airline 涨 14-20 分）；回放派生让期望终态与工具语义永远一致。
  只比终态不比路径——等价路径全通过。附 SnapshotDir（目录快照，
  NUL 字节检测跳过二进制）/SortedPaths。
- **hidden 断言分离（Case.AgentView）**：剥掉 Asserts/Tags/Metadata
  的 agent 视图——答案泄漏是物理问题（SWE-bench 教训），靠实现者
  自觉不读不够，提供不可泄漏的形态才是硬保证。
- **case 入库验证（benchmark/validate.go）**：ValidateCase(c, gold,
  bad) 双端验证——gold 未全过 = 断言无法判定成功；bad 全过 = 恒真
  断言拦不住回归。把「判分器本身有 bug」从运行时污染提前到入库
  拦截（SWE-bench validation 阶段）。
- **AgentTarget.CaptureStateFrom**：Run 结束后快照指定目录为
  TargetOutput.State（终态断言采集端）。快照失败不中断——终态断言
  在空 State 上正常判挂（agent 没产出终态 = 任务失败，非基建错误）。
- **MockProvider 健壮性修复（顺手）**：NewMockProvider() 零响应
  配置下 nextResponse 取 Responses[-1] 直接 panic；改返回空文本响应
  （agent 收到空回复自然退出的合法语义）。
- **测试**：state_test.go（四断言通过/失败/部分分/三类差异/坏 case
  error/AgentView 剥离/ValidateCase 四情形）+ gold_test.go（回放
  派生落盘/未知工具报错/端到端：DeriveState→state-equals→
  CaptureStateFrom→ValidateCase——mock agent 未写文件被终态通道
  判 fail，正是「只看输出会漏掉文件没写」的演示）。全量 9 包
  -race 零失败。

## 2026-09-01（⑤ benchmark B1 真模型层 + loop 并发修复）

- **B1 能力评测层（judge 评「好不好」，确定性断言评「坏没坏」）**：
  - **benchmark/judge.go**：Judge 接口（JudgeInput 含工具轨迹——
    「有没有真做」和「做对了没」是两个维度）+ JudgeFunc 适配器 +
    LLMJudge（走 provider.Provider.Complete——judge 与被测 agent 可
    不同家模型，降低同源偏好；closedqa 风格 rubric prompt 三纪律：
    只依据准则/宁可 FAIL 不放水/先判断后结论；首行 PASS/FAIL 解析
    + 逐行扫描兜底——比 JSON 结构化输出抖动率低；格式损坏自动重试
    默认 1 次）+ WithJudge/JudgeFromContext context 注入（保持
    AssertFunc 纯函数签名，注册表不持状态）+ "judge" 断言类型
    （Value=rubric 文本或准则数组，多准则并发判分；judge 自身故障
    返回 error → case 落 excluded 不进分母——judge 坏了不惩罚被测
    agent）。Runner 加 Judge 字段：Run 内部自动注入 ctx；validate
    阶段「用了 judge 断言但没配 Judge」整单拒跑（配置错误）
  - **benchmark/passk.go**：Report.PassK(k) → per-(target,case) 的
    pass^k（C(c,k)/C(n,k) 全过概率，τ-bench 可靠性口径——gpt-4o
    retail pass^1≈60% 但 pass^8≈25% 的鸿沟）与 pass@k
    （1−C(n−c,k)/C(n,k) 至少一次过，HumanEval 无偏估计，能力上界）；
    只统计有效 trial，infra/excluded 不进分母；k>n 时 pass^k 退化
    为「全部样本都过」的频率并如实反映样本极限
  - **benchmark/junit.go**：Report.JUnitXML(w)——CI 系统通吃格式
    （GitHub Actions/Jenkins/GitLab 内建解析）；(target,case)=一个
    testcase，AllPass 通过/否则 failure〔带失败断言明细——点开能看
    挂在哪条断言〕/Valid=0 落 error〔基建问题不算红〕
  - **report.go 统计扩展**：CaseSummary 加 ScoreStdDev/ScoreCI95
    （正态近似 1.96σ/√n，注释明示 n<30 偏窄——METR 误差棒 ±2 倍
    教训：看方差本身比看 CI 可靠）+ MeanLatencyMs/LatencyStdDevMs
  - **根包门面**：Judge/JudgeInput/JudgeFunc/LLMJudge/PassKStat
    类型别名 + WithJudge/JudgeFromContext；门面注释补 B1 用法示例
  - **空转实验指南**：docs/benchmark-research.md §6 补操作指南
    （同配置跑两遍看分数差——噪声底座决定 CI 阈值/默认 Repeat/
    「多大 delta 算真信号」；四项健康信号读数表：ScoreStdDev/
    Diff 零迁移/pass^2 比值/judge 与人工抽检一致率）
  - **测试**：judge_test.go（PASS/FAIL/带思考前缀/小写解析、格式
    损坏重试与耗尽、Runner 全链路〔judge 过→case 过/挂→case 挂/
    与确定性断言 AND〕、多准则 Score=0.5、缺 Judge 拒跑、judge 故障
    excluded）+ passk_test.go（binom 数学〔C(20,10)=184756〕/
    pass^k 与 pass@k 公式验证/k 超样本退化/聚合口径/方差 CI 统计/
    junit 结构与回读）+ 根包门面自食（judge+pass^k+junit 全走别名）
- **loop 并发修复（-race 暴露的既有 bug）**：Loop.FinalMessages 与
  run goroutine 的 defer 写存在 data race——EventDone 事件先于 defer
  写 finalMessages 到达消费者，App.run 收到事件立刻读即并发。修：
  Loop 加 RWMutex 保护 finalMessages，三处写点走 setFinalMessages
  setter。连带 TestHTTPParallelSessions/TestWithRetrievalInjectsContext
  的 -race 失败一并消除；测试侧 flakyTarget 计数器改 atomic（Runner
  并发 trial 下闭包计数器必须并发安全）。全量 9 包 -race 零失败。

## 2026-09-01（第二梯队开工：⑤ benchmark B0 底座 + 度量学调研）

- **调研先行（用户判断「重要但难做好」，四路并行）**：
  SWE-bench 系 / τ-bench（pass^k）/ METR 方法论 / promptfoo·LangSmith·
  Braintrust 工程形态 + Go 生态空白确认（langchaingo 与 eino 均无 eval
  模块）。合成文档 docs/benchmark-research.md——四路独立收敛的结论：
  重复运行是指标地基（METR 误差棒 ±2 倍、τ-bench 要求 4+ trials）、
  变化量是一等公民（单次分数无意义）、判分打分制非布尔制、失败四分类
  （基建抖动不进分母）。通用型定位的库/宿主边界：格式+原语+模板+矩阵
  +diff 内置，golden 数据与领域判分留宿主。
- **⑤ benchmark B0（最小闭环，纯 mock）**：
  - **benchmark 子包**：Case（声明式，断言字段只在评分阶段消费——
    Target 只拿 Input，SWE-bench 答案泄漏教训）+ Assert（Type/Value/
    Weight/Negate/Metric）+ Verdict（Pass/Score 0-1/Reason 打分制）；
    10 个内置确定性原语（contains/contains-any/equals/starts-with/
    regex/is-json/tool-used〔通配〕/tool-sequence/max-latency-ms/
    max-tokens）+ RegisterAssert 轻逃生舱（OpenAI evals 继承类教训）；
    断言评估自身出错（坏正则等）= case 损坏 → excluded 不进分母
    （τ-bench 标注错误教训）；Runner（Repeat 重复运行/四态结果
    pass·fail·infra_error·excluded/infra 自动重试默认 2 负数关闭/
    并发 semaphore/单 run 超时/套件校验——ID 重复·无断言整单拒跑）；
    Report（JSONL：header 行+per-trial 行；SuiteHash 内容哈希按 ID
    排序——顺序无关可比性；Summaries per-(target,case) 聚合 AllPass
    口径；DiffReports 状态迁移 Fixed/Regressed/Unstable + SuiteMismatch）
  - **根包门面 benchmark.go**：类型别名（Result 因根包占用改名
    TrialResult）+ AgentTarget（opts 装配 App，每次 Run 现造——事件流
    聚合为 TargetOutput：文本/工具轨迹/轮次/token/耗时，trajectory
    断言无需后配 tracing）+ NewAgentTargetFromFactory 工厂形态
    （有状态组件如 MockProvider 每 trial 现造——Repeat>1 时共享游标
    跨 trial 污染是实测踩到的坑）+ UseTools 追加工具面
  - **自食测试**：根包（AgentTarget 驱动自建工具全链路/矩阵对比
    good vs bad + Diff 零迁移/infra_error 不进分母/门面别名）+
    builtin 包（Write→Read→Edit 真实文件工具全链路，锁 readstate
    合法路径——根包测试 import builtin 会环，放 builtin 包）
  - **实测修掉的两个真坑**：(a) MockProvider 响应游标并发污染
    （暴露 benchmark 环境隔离纪律：trial 间除外部世界零共享）；
    (b) App.Run 传 sess=nil → sessionID 空 → WithSessionWorkDir
    静默失效（AgentTarget 改走 RunWithHistory 空历史拿 ephemeral
    会话）。另：无人值守评测跑 Normal 级工具需显式
    WithPermissionMode(PermissionBypass)——库不默认放开权限
    （审批交互无人应答会挂起，但放开与否是宿主决策）
- 测试：benchmark 子包 14 个（断言原语/四态/重试/校验/JSONL 往返/
  哈希稳定/Diff）+ 根包 4 个 + builtin 1 个全绿；全量 9 包零失败
- 待做（B1）：Judge 接口 + LLMJudge（promptfoo/OpenAI 验证过的
  factuality/closedqa rubric）+ pass^k 指标 + 方差/CI 呈现 +
  junit.xml + 空转实验（3-5 真实任务 × 10 重复测噪声底座，
  决定所有阈值默认值）

## 2026-09-01（第一梯队收官：④ RAG 四件套 + 包结构约定）

- **④ RAG 双形态四件套**（补缺清单第 1 项，讨论定稿的实现）：
  - **retrieval 子包（共享底座）**：Document 类型（Content+Metadata
    贯穿全程，Metadata["source"]/["score"] 是引用审计与融合排序的
    货币）；窄接口四兄弟 Retriever/Embedder/Store/Loader + Transformer/
    Reranker（对齐 eino 接口密度）；FusionRetriever 多路融合
    （Children 并发 + Merge 策略注入，预置 MergeInterleaveByScore——
    只信任路内相对序不信任路间绝对分；任一路失败整体 fail-closed）；
    参考实现 MemoryRetriever（关键词）/VectorStore（进程内余弦）/
    ChunkTransformer（滑窗分块带 chunk_index 定位）/FileLoader
    （.md/.txt 目录递归）；Index 辅助流水线（Loader→Transformer→
    Embedder→Store 一次串成）
  - **根包门面（retrieval.go）**：类型别名 + 形态 1 WithRetrieval
    （App.run 前置 retrieve→rerank→拼 prompt，agent 无感知；含
    ShouldRetrieve 谓词闲聊短路、Timeout、MaxChars 保序截断〔头一条
    超预算硬截且至少透出 1/4 预算的内容——不能给模型留空壳引用块〕、
    检索失败 fail-closed 发 EventError）+ 形态 2 NewRetrieveTool
    （agent 运行时自主调用，研究型任务/多跳问题）+ EventRetrieval
    观测事件（query 摘要/命中数/注入字符数，SSE type=retrieval）
  - 组合模式为设计约定：多路检索走 FusionRetriever 普通 Go 代码组合；
    分块策略独立（Loader/Transformer 解耦）；边界诚实声明（迭代检索/
    中段检索走工具形态）
- **包结构约定**（回答「为什么 pipeline 等散落根目录」）：根目录 = API
  门面（App/Option/Event/ToolDef 契约）；新子系统一律「子包实现 +
  根包门面」（retrieval 模式）或「内核下沉 internal/ + 根包壳」
  （loop 模式）；sandbox 契约留根包做依赖锚点。pipeline/http/cli 的
  下沉（internal/pipeline，~1800 行）留作专项等第一梯队收敛后做
- 测试：retrieval 子包 8 个 + 根包 6 个（注入/谓词短路/零命中不污染/
  fail-closed/截断保序/工具全链路）全绿；全量回归 8 包零失败

## 2026-09-01（环境信息段无条件注入：工作目录对模型可见）

- **修复「能力存在但不可见」**：WithSessionWorkDir 注入的会话工作目录
  只存在于 ctx value（Glob/Read 等工具解析正常），但 system prompt 从
  不告知模型——模型不知道当前目录也不知道平台，会猜路径（如
  /workspace）、用平台不存在的命令（Windows cmd 下跑 pwd），然后自我
  怀疑。对齐 Claude Code 的 Working directory 行为：
  - `buildSystemPrompt(cfg, memMgr, workDir)` 新增第三参数，run() 内
    组装点移到 workDir 定型之后（会话目录 > 沙箱根，沙箱覆盖时模型
    看到的就是沙箱根，不会被旧路径拒之门外）
  - 环境信息段（Platform/Architecture/Shell/Working directory）从
    enableGitStatus 门后改为**无条件注入**；workDir 空时回退进程 cwd
  - enableGitStatus 时的 git 仓库根探测改用会话目录
    （新增 sysprompt.DetectGitRootIn(dir)，原 DetectGitRoot 委托之）——
    多项目会话下 git 状态不再对不上
  - detectShell 探测结果进程级缓存（sync.Once）——环境段每次 run 组装，
    不能每次 fork bash --version
- 测试：envinfo_test.go 三件（会话目录注入 / 空回退 cwd / 沙箱根覆盖）
- 兼容性：默认用户 system prompt 多一段环境信息（对齐 Claude Code），
  其余行为零变化；GetSystemPrompt 不含 run 级会话目录（注释明示）。

## 2026-08-31（第一梯队三件套：EventInterrupted / LLM observer 钩子 / 挂起恢复）

- **① EventInterrupted 一等事件**：用户主动终止此前走
  EvtError(context.Canceled)，与 provider 超时/工具崩溃混在同一通道，
  前端要靠 errors.Is 自己判。现在 loop 五处取消检查点（轮次起点 ctx
  检查 / Stream 请求阶段取消 / 流中 EventError 携带 Canceled / 流后
  中止检查 / 429 等待中取消）识别 Canceled 时发独立事件
  EvtInterrupted=15（对齐公共 EventInterrupted，SubAgentProgress=11~
  Interrupt=14 之后的下一个槽位）；DeadlineExceeded 等仍走 EvtError。
  HTTP 层 SSE 映射 {"type":"interrupted"}；TUI EvInterrupted 渲染
  「[已停止]」而非报错样式；pipeline runLightweight 透出
  interrupted 事件类型（区别于 error）。
- **② LLM 调用级 observer 钩子（LLMObserver 可选接口）**：Observer
  主接口保持稳定（analytics/cost 等现有实现零改动），新增可选接口
  LLMObserver{OnLLMStart/OnLLMDone/OnLLMError}——loop 在 Stream 建立
  前触发 Start（LLMCallInfo：model/turn/msgs/tools/max_tokens/token
  估算），流消费完毕触发 Done（LLMResult：duration/stop_reason/
  text_len/thinking_len/tool_calls/usage），失败路径触发 Error。tracing
  视角里模型调用不再是黑洞。
- **② ctx 三优化**：(a) span 关联——loop 调 OnToolStart/OnToolDone/
  OnToolError 时传 observer.WithToolCallID(ctx, id) 派生 ctx，宿主
  observer 实现经 ToolCallIDFromContext 直接拿配对 ID，无需自行配对
  （零 API 变化）；(b) 沙箱会话的 ctx 覆盖契约成文——WithSandboxSession
  文档约定「后注入覆盖先注入（子覆盖父）」，benchmark trial 嵌套
  pipeline 的父子沙箱语义明确；(c) observer 边界注入——
  observer.IntoContext(ctx, o) 把采集 observer 挂到 ctx，
  ResolveObserver(ctx, base) 合并广播（App.run 与 pipeline
  buildLightweightLoop 都已接线），benchmark 每 trial 塞独立采集
  observer 不用构造新 App。
- **③ interrupt 挂起/恢复（三语义分开）**：终止（已有，ctx cancel）/
  挂起（等人，非取消）/ 恢复（挂起点续跑）彻底分开。
  loop.SuspendGate 门闩（Wait 阻塞 / Resume 唤醒 / Terminate 终止 /
  IsWaiting 探测 / ctx 取消走统一中断路径），挂起检查点在每轮轮次
  起点（IsWaiting 才进），机制复用 429 等待地基（select 等 channel）。
  WithSuspend() Option + App.SuspendGate() 取引用。session 包新增
  RecordCheckpoint 记录类型（Checkpoint{Reason/Turn/PendingStep/
  CreatedAt}）——消息历史本就逐条落盘 JSONL，checkpoint 只补「执行
  位置 + 挂起原因」小块；FileStore 实现 Checkpointer 接口，
  Manager.WriteCheckpoint/ReadLastCheckpoint 做能力探测（MemoryStore
  未实现返回 ErrCheckpointUnsupported）；Restore 回填
  Session.LastCheckpoint。resume = RunWithHistory(历史) + 从 checkpoint
  执行位置继续。
- **测试**：internal/loop/interrupted_test.go（4 个：数值对齐/流中
  Canceled/轮前取消/Deadline 仍走 Error）、observer/observer_context_test.go
  （6 个：Nop 零感知/边界广播/无注入原样返回/链式注入/ToolCallID 往返/
  Usage 携带）、internal/loop/llm_hooks_test.go（3 个：Start→Done 序列/
  错误路径/Nop 无影响）、internal/loop/suspend_test.go（6 个：Resume
  唤醒/Terminate/ctx 取消/未挂起 Resume/多轮/loop 集成）、
  session/checkpoint_test.go（5 个：往返/Restore 回填/Manager 能力
  探测/不支持报错/混排）。全量 go build + go vet + go test 通过。

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
