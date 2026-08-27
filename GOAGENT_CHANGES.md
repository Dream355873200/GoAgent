# GoAgent 本地变更记录

本仓库（E:\claude-code-rev-study-main\...\goagent）是对 GitHub 上
Dream355873200/GoAgent 的本地增强副本。amobileCreater 的 engine 通过
go.mod `replace` 指向此处。**每次向 GitHub 推送前**：把下面对应条目
整理进正式 commit，然后移除 replace 升级版本号。

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
