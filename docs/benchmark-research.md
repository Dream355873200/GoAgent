# Benchmark 子系统设计输入（调研合成）

> 2026-09-01 调研四路合成：SWE-bench 系 / τ-bench（pass^k）/ METR 方法论 /
> promptfoo·LangSmith·Braintrust（工程形态）+ Go 生态空白确认。
> 定位：**通用型 benchmark 库**——评任何用 GoAgent 搭的 agent，业务专属
> 部分留在可插拔边界之外。本文是动手前的度量学设计输入，不是实现方案。

## 0. 一个前置判断：benchmark 是两个系统，不是一个

| | 引擎回归测试 | 能力评测 |
|---|---|---|
| 被测对象 | loop/工具/pipeline 行为 | agent 任务完成度 |
| provider | mock，确定性 | 真模型，有随机性 |
| 评分 | 精确断言 | 规则 + judge + 抽检 |
| 成本 | 免费，随时全量 | 真金白银 |
| 噪声 | 无 | ±2 倍（METR 实测） |

混装是常见错误：用第二层的成本和噪声做第一层的事，或用第一层的
确定性幻觉评第二层。API 必须让两层各自成立（同一套 Runner，不同的
Target/Judge 组合）。

## 1. 四路独立收敛的结论（可信度最高的部分）

这些点被两个以上互不相关的体系独立验证，是整个设计的地基：

### 1.1 重复运行不是可选项，是指标的地基

- METR：误差棒 **±2 倍**，「纠结好 10% 还是 20% 毫无意义」；T=0 方差
  也大，每任务 3-6 次独立 run 是下限
- τ-bench：同一任务跨 trial 成败翻转是常态（gpt-4o retail pass^1≈60%，
  pass^8 跌到 ~25%）；官方把「4+ trials」写进 leaderboard 提交要求
- promptfoo/LangSmith/Braintrust：repeat / num_repetitions 全部内建

**设计推论**：Runner 原生支持 `Repeat`；报告呈现 per-case 方差而非只给
均值；`pass^k` 做成指标包一等公民（`C(c,k)/C(N,k)`，k 次全过，量的是
可靠性；pass@k 量能力上界，两者都要但语义分开）。

### 1.2 变化量是一等公民，单次分数无意义

- METR：趋势对 10 倍绝对测量误差鲁棒，单点不可比；**同套件内相对比较
  误差更小**（性能相关）——这正是「回归 diff 第一公民」的量化依据
- SWE-bench：榜按 run_id+instance_id 缓存，换补丁必须换 run_id
- promptfoo：多 run 对比 / `filterFailing` 只重跑失败项

**设计推论**：只有超出 CI 的 delta 才算真信号；Report 落 JSONL，
`Diff(a, b)` 输出状态迁移清单（pass→fail 列表）；**缓存键必须含输入
内容哈希**（SWE-bench 的坑：键不含 prediction，换补丁吃旧结果）。

### 1.3 打分制不是布尔制

四家工程工具全部返回 `Verdict{Pass, Score 0-1, Reason, Metric}` 而非
bool。加权、聚合、pass^k、方差统计都要连续分。**设计推论**：断言返回
结构化 Verdict，bool 是 Score>threshold 的语法糖。

### 1.4 失败三分类，基建抖动不进分母

- τ-bench：infra 错误单独剔除不计入
- SWE-bench：后补 infra_failure/ambiguous 二级分类（带病运行很久才补）

**设计推论**：结果状态机从第一天就是 `pass / fail / infra_error /
excluded` 四态，infra_error 自动重试不计入回归 diff——否则基建抖动会
淹没真实回归。

## 2. 断言系统（四路给的最密集输入）

### 2.1 双通道 + 双断言（τ-bench × SWE-bench 交叉）

- **τ-bench**：奖励 = 分量乘积，`reward_basis = [DB, COMMUNICATE]`
  ——对话内容对不对（该告知的信息说了没有）和副作用对不对（终态）
  是独立通道
- **SWE-bench**：`FAIL_TO_PASS`（基线不满足、成功后满足——防假阳性）
  × `PASS_TO_PASS`（前后都维持——防顺手破坏别的）——通过判定取 AND

**设计推论**：`Assertion` 至少四类：
1. `DialogueAssertion`：输出内容（contains/regex/is-json/llm-judge）
2. `StateAssertion`：终态断言（工具副作用、文件系统、task store 状态）
3. `TrajectoryAssertion`：工具调用序列/参数（**我们的差异化牌**——
   promptfoo 的 trajectory 断言要靠后配 tracing，observer 原生就有）
4. `PreserveAssertion`：P2P 等价——不该动的东西没动

### 2.2 期望终态用回放派生，不手写（SABER 教训）

τ-bench 被系统性批出 50+ 处标注错误，修复后 airline 涨 14-20 分——
意味着原版长期把「正确拒绝」的 agent 判错。手写期望态是标注错误的
重灾区。**设计推论**：提供 GoldReplayer——在可复位 mock 环境回放
case 附带的参考工具调用序列，派生期望终态快照；只比终态不比路径
（等价路径全通过）。

### 2.3 case 入库前先验证（SWE-bench validation 阶段）

每条 case 入库前跑双端验证：gold 轨迹得 1 分、故意错误轨迹得 0 分
（τ-bench 调研第 8 条）。**不满足的 case 是坏 case，根本不该进套件**——
这把「判分器本身有 bug」从运行时污染提前到了入库拦截。

### 2.4 断言库形态（promptfoo 样板）

- 内置 8-10 个确定性原语：contains / regex / is-json(+schema) /
  levenshtein / latency / cost / token-usage / tool-used /
  tool-sequence / sandbox-exec（沙箱里编译运行被测产物——其它库
  没有的能力）
- `RegisterAssert("name", fn)` 轻逃生舱——**OpenAI evals 的反面教训**：
  自定义 eval 要继承类写两个方法被骂三年，扩展点必须是一等公民
  逃生舱而不是重插件系统
- Judge 独立接口：`Judge(ctx, Case, Result) Verdict`，内置 LLMJudge
  复用库自身 provider，rubric 模板直接采 promptfoo/OpenAI 验证过的
  factuality / closedqa / select-best

## 3. 库 vs 宿主边界（通用型定位的核心分界线）

四家工程工具收敛到同一条线，直接采纳：

| 库内置 | 宿主自定义 |
|---|---|
| Case/Assert/Judge/Target/Report 类型与加载 | golden 数据本身 |
| 确定性断言原语库 + 注册表 | 领域判分逻辑（RegisterAssert） |
| LLM judge 模板（factuality 等） | rubric 文本 |
| 矩阵运行（多 target × case）| 被测 agent 的装配（Target 实现） |
| 重复/方差/pass^k/CI 指标 | 人工抽检流程 |
| JSONL 落盘 + Diff 报告 + junit.xml | 任务的领域语义 |

Go 生态确认：langchaingo（1492 路径）与 eino + eino-ext（1605 路径）
**均无 eval 模块**——GoAgent 补此位是真空，不是重复造轮。

## 4. 自迭代的防自欺防线（README 三条 + 调研加固）

README 已有：benchmark 修改只增不删、held-out 不可见、人工抽检不可撤。
调研补充四条更硬的：

1. **答案泄漏是物理问题**（SWE-bench 警示）：case 存 agent 可读位置
   = 断言泄漏，agent 能直接翻答案。断言字段默认**不注入 agent 上下文**；
   case 文件提供 hidden 打包态（断言分离存放）；沙箱 case 评分器侧
   校验 agent 未读答案文件
2. **成功 run 也要查作弊**（METR 原文明示「只查失败是有偏过程」）：
   分数涨了必须先排除「学会骗判分器」。LLM 监控器只做 flag、确认走
   独立通道；确认作弊即重判失败
3. **算法判分系统性高估**（METR 实证：测试通过率 38% 的 PR 人工评审
   后 0% 可合并）：hill-climbing 一个会作弊的算法分会放大 reward
   hacking 而不产生真实收益。抽检维度用「可直接用吗」式 rubric
4. **排除规则预注册**：只允许固定类别（任务损坏/缺工具/歧义/作弊/
   污染）排除并留日志，防止「把难的删了刷分」

## 5. 其它直接可抄的工程细节

- 任务集**版本 + 内容哈希**写进报告（tau3 改分后新旧结果不可比）
- 报告带 per-case 产物目录（log/输出/trace 引用），diff 只列状态迁移
- 温度固定 + seed 是共识基线，但不消灭随机性——「低温+重复+方差
   呈现」是标准姿势
- task family 结构：family + ≥4 参数化变体，分析粒度到 family（METR
  「瓶颈多样性」论证）；难度从秒级到分钟级铺开防饱和
- Metrics 包独立：PassHatK / PassAtK / 均值方差 / bootstrap CI 都是
  纯函数，吃 (trials, successes) 不吃运行时

## 6. 分期落地建议

- **B0（最小闭环，纯 mock）**：Case/Assert 类型 + 确定性断言原语 +
  Runner(Repeat/四态结果) + Report(JSONL) + Diff + GoAgent 自食
  （用 benchmark 测 goagent 自己的文件工具——引擎回归层先行）
- **B1（真模型层）**：Judge 接口 + LLMJudge + pass^k 指标 + 方差/
  CI 呈现 + junit.xml；空转实验在此层做（见下）
- **B2（自迭代武装）**：GoldReplayer + StateAssertion + hidden 断言
  分离 + 作弊 flag 流水线 + Issue 联动（回归→IssueReport→修复→复跑）
- **B3（远期）**：用户模拟器（τ-bench 形态，建在 MockProvider 上）、
  任务族参数化生成器

**空转实验的位置**：B1 完成后立即做——挑 3-5 个真实 agent 任务 ×
10 次重复，测分数分布宽度。这个数据决定 CI 阈值、默认 Repeat 数、
「多大 delta 算真信号」的默认值。没有这个数据，后面所有阈值都是拍的。

### 空转实验操作指南（B1 已具备全部工具）

空转实验（null experiment）：**什么都不改**，同一套配置跑两遍，看
分数差多少。差值就是测量噪声的底座——任何小于它的「改进」都是噪声。

```go
// 1. 真实 provider（与被测不同家的模型做 judge，降低同源偏好）
agentP := openai.New(...)
judgeP := anthropic.New(...)

target := goagent.NewAgentTarget("baseline", goagent.WithProvider(agentP))
run := func() *goagent.Report {
    r := &goagent.Runner{
        Targets: []goagent.Target{target},
        Cases:   suite,          // 3-5 个真实任务，各带 2-3 条 rubric
        Repeat:  10,             // τ-bench 口径：4 是下限，10 才看得了分布
        Judge:   &goagent.LLMJudge{Provider: judgeP},
    }
    rep, _ := r.Run(ctx)
    return rep
}

a, b := run(), run()                       // 两遍，中间什么都不改
d := goagent.DiffReports(a, b)             // 零迁移之外还有多少 unstable？
for _, s := range a.Summaries() {
    // 看 ScoreStdDev / ScoreCI95：单 case 方差有多大
}
for _, pk := range a.PassK(2) {            // pass^2 掉到 pass rate 的几成
}
```

读数方法（METR 的基线纪律）：

| 观察项 | 健康信号 | 噪声警报 |
|---|---|---|
| 同 case 10 trial 的 ScoreStdDev | < 0.15 | > 0.25 → Repeat 加倍或 rubric 收紧 |
| 两遍 run 的 Diff | 零迁移 | regressed>0 → 阈值必须 > 该噪声 |
| pass^2 / pass rate 比值 | > 0.7 | < 0.4 → 可靠性问题，不是评测问题 |
| judge 分数 vs 人工抽检 20% | 一致率 > 85% | 偏低 → judge prompt 校准或换模型 |

结论固化：把测出的噪声底座写进 CI 配置（如「MeanScore delta <
2×噪声底座不判回归」），并回填 Runner.Repeat / Timeout 的默认值
建议。这个实验每个新 case 套件上线前跑一次，每季度复跑一次。

## 7. 已知分歧点（需要时再决策）

- METR「判分一律算法化、LLM 只做初筛」vs promptfoo「llm-rubric 是
  主力断言」——本质是「能力评测」vs「prompt 调优」场景差异。我们
  两层系统各取所需：回归层纯算法，能力层 judge 为主 + 抽检兜底
- SWE-bench 冻结发布集 vs 持续收割新 case——我们是自迭代工具不是
  学术榜，明确选**不冻结、可收割**：case 带 created_at/难度元数据，
  支持淘汰，但淘汰走预注册规则
