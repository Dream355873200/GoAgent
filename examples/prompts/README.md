# 自定义提示词示例

这个目录包含示例提示词文件，演示如何自定义 goagent 的提示词系统。

## 文件说明

| 文件 | 说明 | 用途 |
|------|------|------|
| `example-identity.prompt.md` | 身份定义示例 | 自定义 Agent 的基本角色定位 |
| `README.md` | 本文件 | 说明如何使用这些示例 |

## 快速开始

### 方式一：使用外部目录覆盖

将修改后的 prompt 文件放到一个目录中，通过 `WithPromptDir` 加载：
找不到的文件会自动 fallback 到嵌入的默认值。

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithPromptDir("./prompts"),
)
```

文件名必须与内置文件名一致，例如 `system-identity.prompt.md`。

可使用 `prompts.ExportDefaults(dir)` 一键导出所有默认 prompt 到目录：

```go
import "github.com/Dream355873200/GoAgent/prompts"

files, _ := prompts.ExportDefaults("./my-prompts")
fmt.Println("已导出:", files)
// 修改 ./my-prompts/system-identity.prompt.md 后重新启动即可生效
```

### 方式二：纯字符串系统提示词

不需要 prompt 体系时，直接传入自定义字符串：

```go
app := goagent.New(
    goagent.WithSystemPrompt("你是一个客服助手。\n- 保持友好专业的语气\n- 复杂问题先确认细节再回答"),
)
```

### 方式三：默认模式

不传任何 prompt Option，自动加载嵌入的 7 个 prompt 文件（对齐 Claude Code）：

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
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
