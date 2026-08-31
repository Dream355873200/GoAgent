# 深潜 6：权限与审批（permissions）

> 位置：`permission/` 包 + `permission_handler.go` + executor 的 PreCheck 链
> 回答的问题：agent 想干危险的事，谁点头？

## PreCheck 链：执行前的统一拦截

工具真正执行前，executor 跑一条检查链（buildPreCheck 组装）：

```
工具调用请求
  → 权限门（Permission Gate）★ 本篇主角
  → Plan 检查器（Plan 模式下拦写操作，规划阶段不许动手）
  → 中间件链（宿主自定义拦截/改写/审计）
  → Hooks（PreToolUse 等）
  → 全过 → ToolDef.call 真正执行
```

链式设计的好处：每层职责单一（权限只管放行、Plan 只管阶段、中间件
管业务横切），且顺序可控——权限在最前是因为它最便宜（查表）。

## 权限门的三层结构

```
① 级别（工具自带，编译期）：ReadOnly 自动过 / Normal 首次询问 / Dangerous 每次询问
② 模式（宿主全局设定）：
     Default    —— 正常三档语义
     Bypass     —— 全放（可信环境的便捷档）
     AcceptEdits —— 文件编辑自动过（对齐 Claude Code）
     PlanOnly   —— 只读模式（写全拒）
     DenyAll    —— 全拒（审计/演示）
③ 规则（RuleSet，最细粒度）：Allow/Deny/Ask 按工具名+参数模式匹配
   —— 例：允许 Read 一切、Bash 要 Ask、rm -rf 永远 Deny
```

优先级：Deny 规则 > Allow 规则 > 模式 > 级别默认。

## Approver：询问的归宿是可注入的

Normal/Dangerous 触发"询问"时，问题抛给 Approver 接口——**框架不
假设 UI 形态**：

| 形态 | 行为 |
|------|------|
| CLI（默认） | 终端交互式 y/n |
| SDK（默认） | 自动拒绝（安全默认——嵌入代码没人回答时宁拒不放） |
| 业务注入 | AnimeCreater 接前端：SSE 推审批请求 → 用户点按钮 → 回调恢复 |

## YOLO 分类器（智能降摩擦）

全问打断心流，全放不安全——折中方案：**LLM 分类器**给请求分级。
两阶段（fast → thinking 模型二次确认只处理模糊样本），prompt 可外部
覆盖。对齐 Claude Code 的 YOLO 机制。

面试点：这是"用 LLM 治理 LLM"的边界案例——分类器只影响**摩擦程度**
（要不要问），不影响**安全底线**（Deny 规则永远优先于分类器）。
低风险决策交给模型，高风险决策规则写死。

## 终止/中断语义（与权限联动的完整图景）

```
终止（用户点停止）—— 已有：POST /interrupt → InterruptHandler 按会话
  cancel → loop 每轮检查 ctx → 被中断工具写回 tool_result → 
  历史落盘。SSE 断开≠取消（防误杀）
挂起（等人审批再继续）—— TODO 第一梯队：goroutine 挂起非取消 +
  挂起点 checkpoint（复用 429 等待的机制地基 + 已有的逐条落盘）
三语义分离：终止（立即杀）/ 挂起（等人）/ 恢复（断点续跑）
```

## 为什么不是"白名单工具集"就够了（可能的追问）

工具集控制**能调什么**，权限控制**调的时候要不要人点头**——两个
维度正交。Bash 在工具集里（能调）但 Normal 级（每次问）；Read 在
集里且 ReadOnly（不问）。真实安全需求是二维的：能力面 × 每次执行的
确认粒度。
