# 深潜 2：工具体系（tools）

> 位置：`tool.go`（InferTool/ToolDef）+ `builtin/`（内置工具）+ `toolkit.go`
> 回答的问题：agent 的"手"是怎么定义、怎么被调度的？

## InferTool：零样板定义工具

```go
type WeatherInput struct {
    City string `json:"city" desc:"城市名" required:"true"`
}

tool := goagent.InferTool("get_weather", "查询天气", func(ctx context.Context, in WeatherInput) (string, error) {
    return in.City + " 晴", nil
})
```

**反射生成 schema 的机制**：函数签名 `func(ctx, T) (D, error)` 被解析——
T 的结构体 tag 转 JSON Schema（`json` = 字段名、`desc` = 给 LLM 的字段说明、
`required` = 必填），输出类型 D 也自动推断。开发者写一个普通 Go 函数，
框架产出 LLM 需要的完整工具定义。

面试点：**为什么 schema 这么重要**——LLM 调工具全靠 schema 描述理解
参数语义。`desc` tag 是写给模型看的文档，描述质量直接决定调用准确率。
手写 JSON Schema 的样板代码是 LangChain 时代最烦的部分，反射推断消掉了它。

## 工具调用的完整路径（含咽喉点）

```
LLM 输出 tool_use
  → loop 组装 executor.ToolCall{Execute, PreCheck}
  → StreamingExecutor 调度（支持并发执行 Concurrent 工具）
  → 执行前 PreCheck 链：权限门 → Plan 检查 → 中间件 → Hooks
  → ToolDef.call()（tool.go:108）★ 全库唯一咽喉点
      json.Unmarshal 输入 → newContextFromStd(ctx) 组装 goagent.Context
      （SessionID/WorkDir/Sandbox 从 context value 提升为字段）
      → 反射调用用户的 Execute(ctx, in)
  → 结果序列化为 tool_result 消息回灌 LLM
```

**咽喉点概念是理解全库的钥匙**：单 agent、pipeline 节点、子 agent 的
工具调用全部汇入 ToolDef.call——沙箱、会话隔离都挂在这一个点实现，
一处接线全模式覆盖。

## executor 包三层（读代码认知）

```
Executor          —— 基础执行器：串并分区（Concurrent 工具连续并行、
                    非并发自成批次串行）+ 信号量限流（默认 10）
StreamingExecutor —— 流式执行器（loop 实际用的）：LLM 还在吐 token 时
                    解析出 tool_use 就立即开始执行——工具执行与 token
                    生成的尾部时间重叠，指令级流水线的同构
TrackedExecutor   —— 生命周期状态机（queued/executing/completed/
                    aborted/yielded）+ 兄弟中止 + 合成 tool_result。
                    当前未接线（死代码）——loop 用更简单的方式内联合成
                    中断 tool_result。保留价值：工具级 interrupt/resume
                    的现成地基；也是"抄架构先抄主链路"的教训样本
```

## stop hook：模型提议终止，宿主持有否决权（合并后现状）

agent 说"做完了"退出前，hooks 包的 EventStop 跑一道**宿主代码的
确定性复核**（不调 LLM）——防"模型自我报告不可信"的场景（截断/
提前收工/产物缺失）。曾经历双轨（internal StopHookRunner 与 hooks
包 EventStop 并存），已合并为 hooks 包单轨：

- `RunStop(ctx, sessionID, stopReason, lastText, messages)`——现场
  信息全透传（合并的核心：签名从只有 ctx 升级到带完整现场）
- `NewStopHook(name, fn)`——函数直接注册成 stop hook
- 内置 `MaxOutputTokensStopHook(n)`——数 ``` 奇偶的截断兜底
  （引擎级 stopReason 恢复在 loop 主循环，优先于此；本 hook 覆盖
  provider 不报截断的场景）
- 三层职责分立：引擎自愈（主循环，改请求参数提 max_tokens——只能
  住引擎里）/ 内置启发式（hooks 包）/ 用户扩展（WithHooks 注册）

面试抽象：**stop hook 是给"循环终止条件"加了第二签名权**——凡是
LLM 自我报告不可信的系统都需要这个模式（另一个例子：token 计数
不能信模型自报，按 API usage 算）。

## 权限三档

| 级别 | 行为 | 适用 |
|------|------|------|
| ReadOnly | 自动放行 | 读操作（Read/Glob/Grep） |
| Normal | 首次询问（Approver） | 写操作（Write/Edit/Bash） |
| Dangerous | 每次询问 | 高危（rm 等） |

Approver 可注入：CLI 交互询问 / SDK 默认拒绝 / 业务接前端审批 UI。

## 文件工具对标 Claude Code 的细节（面试富矿）

每个都是真实工程判断，能讲出"为什么"：

1. **未读拒绝编辑**：本会话没 Read 过的文件拒绝 Edit——模型没建立文件
   最新视图就编辑，old_string 大概率失配。前置拒绝比执行失败快一轮 LLM
2. **覆盖保护**：Write 覆盖已存在但未读过的文件拒绝——防凭印象覆盖
   丢改动。新建文件无需先读
3. **诊断式报错**：old_string 未命中时不裸报「未找到」，而是扫描出
   最相似的行给线索（可能差在空白/多处命中/文件已变）——LLM 拿线索
   修正比盲试快
4. **超长行截断**：>2000 字符的行中间挖空（保首尾）——minified JS
   一次读入会撑爆上下文
5. **二进制检测**：采样 NUL/控制字符比例，返回类型提示而非乱码
6. **未修改短路**：同会话重复读未变化的文件返回「无需重读」+ 指纹
   （内容哈希）跟踪——防模型循环重读同一文件烧 token
7. **写后保持已读**：Write/Edit 后指纹更新为写入内容，同批后续 Edit
   放行（内容=模型刚写的，它有最新视图）

## ToolKit：按领域打包

`WithToolKits(FileKit, ShellKit...)` 把一组工具按领域注册——嵌入方
按需启用整包，而不是逐个挑。

## 已知边界

- 工具是进程内 Go 函数——不受沙箱约束的 os.* 调用拦不住（Tier 1 边界，
  见 sandbox.md）
- schema 推断不支持 union 类型等复杂形态（够用，LLM 侧也理解不好）
