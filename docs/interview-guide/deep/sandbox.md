# 深潜 4：沙箱体系（sandbox）

> 位置：`sandbox.go`（契约）+ `sandbox_dir.go` + `sandbox_worktree.go` + builtin 接线
> 回答的问题：不受信的 agent 执行怎么隔离？为什么这么设计？

## 问题定义：隔离发生在哪

**Agent 对世界的全部作用力都经过工具调用**——LLM 本身只会生成 token。
所以隔离的执行面只有两个：**路径解析**（Read/Write/Edit/Glob/Grep）和
**子进程派生**（Bash/GitCommit）。拦住这两处，agent 就被沙箱住了。

```
所有工具调用汇入同一咽喉点 ToolDef.call（tool.go:108）
  → newContextFromStd 组装 ctx 时把 SandboxSession 从 context value
    提升为 ctx.Sandbox 字段
  → 内置工具在路径解析/进程派生处消费它
一处接线 → 单 agent / pipeline 节点 / benchmark trial 三模式全覆盖
```

## 核心设计决策（面试最值得讲的）

### 决策 1：策略层物化进 Context，不做 Execute 包装器

曾考虑：包一层 `SandboxSession.Execute(ctx, fn)` 包装每个工具调用。
推演后否决——**进程内包装在逻辑上拦不住任何东西**：检查发生在函数
执行前，但工具是任意 Go 代码，绕过包装直接调 os.* 毫无阻力。
真实有效的强制点是**路径解析和 exec 派生的那一行代码**——在那里
改写/校验才是真隔离。包装器是"检查后放行"，正确做法是"路径根本
解析不到外面"。

### 决策 2：Tier 0-3 强度阶梯（模块化的另一面是强度模块化）

| Tier | 实现 | 强度 | 状态 |
|------|------|------|------|
| 0 | 不配置（默认） | 无——零开销，行为与历史版本一致 | ✅ |
| 1 | DirSandbox / WorktreeSandbox | 进程内路径白名单+工作副本 | ✅ |
| 2 | Docker/OS 级 | 进程隔离 | ExecSession 接口预留 |
| 3 | WASM（wazero）/goja | 语言级/能力模型 | 接口预留（L4 地基） |

### 决策 3：诚实的威胁模型

**Tier 1 防意外不防恶意**——进程内前缀检查挡不住工具代码里一句
`os.Remove`；符号链接逃逸不防（词法检查 vs 真实路径）。文档明写：
**进程级隔离是宿主部署者的责任**。威胁主体是"能力很强的实习生"
（写错路径/删错文件），不是攻击者——这个区分决定了 Tier 1 的
性价比（零依赖跨平台微秒级 vs Docker 的运维成本）。

## Tier 1 实现

### DirSandbox（目录沙箱）

```
Enter：os.MkdirTemp 建独立根目录
ResolvePath：
  相对路径 → Join(沙箱根, p) + Clean   ← 相对路径物理上出不去
  绝对路径 → 白名单策略检查            ← 最长前缀优先，无匹配=拒绝
  违规 → 返回 LLM 可读中文错误（"路径不在沙箱允许范围内"）——
         模型可自纠改用合法路径，而不是撞墙
Close：RemoveAll（防御性校验根仍在 BaseDir 下才删）
```

`filepath.Clean` 顺带做词法逃逸捕获：`../../../etc/passwd` Clean 后
落到根外，下一步检查拒绝。

### WorktreeSandbox（git worktree 沙箱）

沙箱根 = `git worktree add` 创建的副本——**天然可 diff/可回滚**：
agent 改坏仓库 force remove 即可，产出收割靠 git diff。这是 benchmark
自迭代（防 agent 污染仓库）的首选形态。

**工程细节**：原 worktree 包的 Enter 用**进程全局 os.Chdir + 单 current
模型**——多会话并发会互相踩。拆出 `Create(name)`（不 chdir、不占
current）+ `Remove(wt, force)`，Enter/Exit 行为零变化（交互式工具依赖
旧语义），多会话各自持有 worktree 并发安全。

### 策略（Policy）

```go
Policy{
    FS      []FSRule,     // {Path, Access: FSNone|FSReadOnly|FSReadWrite}
    Net     NetPolicy,    // Tier 1 携带不强制
    Env     []string,     // 白名单，Tier 1 携带不强制
    Timeout time.Duration, // Bash 单命令上限（min 语义）
}
```

白名单语义：**最长前缀优先，无匹配 = 拒绝**（没写到的就是禁止的）。

## 接线细节（体现工程完成度）

- Read/Write/Edit 调用点走沙箱感知的 `resolvePathChecked`（OpRead/OpWrite）
- Glob/Grep 的搜索根默认 = 沙箱根
- Bash：cmd.Dir 优先沙箱 CommandDir + 超时 min(输入, Policy.Timeout) +
  **ExecSession type-assert 钩子**（Tier 2 实现该接口后命令自动路由进
  Docker，上层零改动）
- GitCommit **零改动**——它读 ctx.WorkDir，沙箱根覆盖该字段后提交自动落沙箱
- 优先级规则：沙箱根非空时覆盖 WithSessionWorkDir（仅 opt-in 激活）
- 节点级覆盖：`PipelineNode.Sandbox *Policy`——不受信 fan-out 节点用更强策略

## 测试策略（Windows 无 Docker 全绿）

四件套：契约测试（Noop 透传/前缀匹配/大小写）、DirSandbox 收敛
（根内 OK/根外拒/`..`逃逸拒/ro 写拒/Close 防御）、builtin 接线
（Write 根外拒/Bash cmd.Dir 落根验证/Timeout 上限）、pipeline 注入
（**probeProvider 假 LLM**——按节点名驱动工具调用，不依赖真实模型，
这个 mock 手法本身值得讲）。

## 向 L4/L5 的延伸（路线图，可讲视野）

- **L4 沙箱代码执行**：goja（纯 Go JS 解释器）为主路径——运行时
  不实现 os/fs/net，**能力不存在而非被拦截**（语言级硬隔离）；
  注入的能力必须套 Policy 射程（能力+射程双锁）；goja+子进程为
  保守模式（工程意义零逃逸）。wazero（WASM）留作 L5 常驻运行时
- **L5 工具生成三闸门**：执行体准入（原生路径在协议里物理不存在）/
  接口面净化（防"无害代码+恶意描述"的提示注入）/ 权限低位+探针评估
- 攻击面分析可完整讲述：绕过不需要突破沙箱，只需要不经过沙箱——
  所以防御必须在注册协议层而不是只靠执行层
