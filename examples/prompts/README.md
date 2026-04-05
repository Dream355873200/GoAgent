# 自定义提示词示例

这个目录包含示例提示词文件，演示如何自定义 goagent 的提示词系统。

## 文件说明

| 文件 | 说明 | 用途 |
|------|------|------|
| `example-identity.prompt.md` | 身份定义示例 | 自定义 Agent 的基本角色定位 |
| `README.md` | 本文件 | 说明如何使用这些示例 |

## 快速开始

### 方式1：完全自定义身份

```go
app := goagent.New(
    goagent.WithPromptConfig(goagent.PromptConfig{
        IdentityText: `你是一个客服助手。
- 专注于帮助用户解决问题
- 保持友好和专业的语气
- 复杂问题需要先确认细节再回答`,
    }),
)
```

### 方式2：从文件加载

```go
app := goagent.New(
    goagent.WithPromptConfig(goagent.PromptConfig{
        Identity: "prompts/my-identity.prompt.md",
    }),
)
```

### 方式3：追加到内置提示词

```go
app := goagent.New(
    goagent.WithPromptConfig(goagent.PromptConfig{
        // 追加到身份定义之后
        AppendIdentity: "\n\n额外要求：你必须使用中文回答。",
        // 追加到提醒之后
        AppendReminder: "\n\n重要：不要修改生产环境代码。",
    }),
)
```

## 查看当前提示词

```go
// 打印当前使用的完整 system prompt
fmt.Println(app.GetSystemPrompt())
```

## 提示词组成

goagent 的提示词系统由以下部分组成（按顺序）：

1. **Identity** - Agent 身份定义
2. **DoingTasks** - 执行任务指令
3. **Actions** - 谨慎执行操作
4. **UsingTools** - 工具使用策略
5. **ToneStyle** - 语气和风格
6. **OutputEfficiency** - 输出效率
7. **Reminder** - 系统提醒
8. **Custom** - 用户自定义提示词（如果有）

## 优先级

每个部分支持三种配置方式，优先级如下：

1. `*Text`（直接文本） - 最高优先级
2. `*`（文件路径） - 次高优先级
3. 内置提示词 - 默认使用内置提示词

追加字段（`Append*`）始终添加到对应部分之后。
