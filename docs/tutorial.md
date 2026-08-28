# GoAgent 渐进式教程

三课学完 GoAgent 的核心：从 30 行的最小 agent，到带工具和记忆的多轮对话，再到多节点 DAG 流水线。每课代码完整可运行，学一个用一个。

> 前置要求：Go 1.24+，一个 LLM API Key（OpenAI 兼容端点或 Anthropic）。
> 完整 API 参考见 [README](../README.md)，架构深潜见 [architecture.md](architecture.md)。

---

## 第 1 课：最小 agent —— 30 行跑通

Agent 的本质是一个循环：**LLM 生成 → 需要工具就执行 → 结果喂回 LLM → 直到给出最终回答**。GoAgent 把这个循环封装成 `App`，你只需要提供模型配置和输入。

创建 `main.go`：

```go
package main

import (
	"context"
	"fmt"

	"github.com/Dream355873200/GoAgent"
)

func main() {
	app := goagent.New(
		goagent.ProviderConfig{
			Model:   "deepseek-chat",            // 任何 OpenAI 兼容模型
			APIKey:  "sk-...",
			BaseURL: "https://api.deepseek.com/v1",
		},
	)

	// Run 返回事件流：LLM 的每个动作都是一条事件
	for ev := range app.Run(context.Background(), "用一句话解释什么是 DAG") {
		switch ev.Type {
		case goagent.EventTextDelta:
			fmt.Print(ev.Text) // 流式输出
		case goagent.EventError:
			fmt.Printf("\n错误: %v\n", ev.Error)
		}
	}
}
```

跑起来：

```bash
go mod init myagent && go get github.com/Dream355873200/GoAgent
go run main.go
```

**这一课的三个要点：**

1. **`ProviderConfig` 描述模型**。OpenAI 兼容端点（DeepSeek/Ollama/OpenRouter/vLLM）改 Model+BaseURL 即可；Anthropic 换 `Type: "anthropic"`。
2. **`Run` 返回事件流而非结果**。事件是 agent 与外界通信的唯一方式——`EventTextDelta`（文本增量）、`EventToolStart/Done`（工具调用）、`EventThinking`（思考过程）、`EventError`、`EventDone`。你消费什么、怎么展示，全由你决定。
3. **不想处理事件流？** 用同步版 `Execute`：

```go
result, err := app.Execute(ctx, "用一句话解释什么是 DAG")
fmt.Println(result.FinalText) // 直接拿最终文本
```

---

## 第 2 课：加工具与多轮记忆

没有工具的 agent 只会说话。这一课给 agent 一个查询工具，并让它记住上一轮说过什么。

### 2.1 用 InferTool 定义工具（零样板）

GoAgent 从**函数签名自动推断** JSON Schema——你写一个普通 Go 函数，框架生成 LLM 需要的工具定义：

```go
// 输入结构体：json tag 是字段名，desc tag 是给 LLM 看的说明
type WeatherInput struct {
	City string `json:"city" desc:"城市名" required:"true"`
}

// 函数签名必须是 func(ctx, 输入) (输出, error)
func getWeather(ctx context.Context, in WeatherInput) (string, error) {
	// 这里替换成真实 API 调用；演示用假数据
	return in.City + " 今天晴，25°C", nil
}

app.UseTools(
	goagent.InferTool("get_weather", "查询指定城市的天气", getWeather),
)
```

现在对话中出现“北京天气怎么样”，LLM 会自动调用 `get_weather`，把结果组织进回答。**权限**默认 ReadOnly（自动放行）；写操作类工具传第四个参数 `goagent.Normal`（首次询问）或 `goagent.Dangerous`（每次询问）。

### 2.2 多轮对话：自己管历史（嵌入推荐）

`Run` 每次调用都是独立会话。多轮对话用 `RunWithHistory`——历史由你加载/持久化，框架负责拼接：

```go
var history []message.Message

ask := func(input string) {
	for ev := range app.RunWithHistory(ctx, history, input) {
		if ev.Type == goagent.EventTextDelta {
			fmt.Print(ev.Text)
		}
		// EventDone 事件携带本轮完整消息列表（含 assistant 回复），
		// 收集起来作为下一轮的 history
		if ev.Type == goagent.EventDone {
			history = ev.Messages
		}
	}
	fmt.Println()
}

ask("我叫小明")            // agent 记住了
ask("我叫什么名字？")       // 能答上——历史被带上了
```

> 嵌入 Web/API 应用时这是推荐方式：历史存你的数据库（AnimeCreater 就是这么用的——agent_message 表 + RunWithHistory）。
> 如果想让框架管持久化（自动加载/落盘 JSONL），见 README 的「会话管理」。

### 2.3 完整示例

```go
package main

import (
	"context"
	"fmt"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/message"
)

type WeatherInput struct {
	City string `json:"city" desc:"城市名" required:"true"`
}

func getWeather(ctx context.Context, in WeatherInput) (string, error) {
	return in.City + " 今天晴，25°C", nil
}

func main() {
	ctx := context.Background()
	app := goagent.New(
		goagent.ProviderConfig{Model: "deepseek-chat", APIKey: "sk-...", BaseURL: "https://api.deepseek.com/v1"},
		goagent.WithSystemPrompt("你是一个简洁的助手，用中文回答"),
	)
	app.UseTools(goagent.InferTool("get_weather", "查询城市天气", getWeather))

	var history []message.Message
	ask := func(input string) {
		fmt.Printf("我: %s\nAI: ", input)
		for ev := range app.RunWithHistory(ctx, history, input) {
			switch ev.Type {
			case goagent.EventTextDelta:
				fmt.Print(ev.Text)
			case goagent.EventToolStart:
				fmt.Printf("\n[调用工具 %s]", ev.ToolName)
			case goagent.EventDone:
				history = ev.Messages
			}
		}
		fmt.Println()
	}

	ask("上海天气怎么样？")  // 会调 get_weather
	ask("那北京呢？")        // 也能调——上下文里有"天气"这个话题
}
```

**这一课的三个要点：**

1. `InferTool` 从函数签名生成工具定义，`UseTools` 注册后 LLM 自动可见
2. `RunWithHistory` 支持多轮；`EventDone` 的 `ev.Messages` 是收集历史的钩子
3. 工具函数里可以访问 `ctx`（context 传透），需要请求级数据（用户 ID 等）时经 ctx 传入

---

## 第 3 课：跑一个多节点 Pipeline

单 agent 适合对话；**确定性多阶段任务**（先拆分 → 并行处理 → 汇总）用 Pipeline。它是一个 DAG：节点是 agent，边是依赖，数据经队列流动。

### 3.1 两节点流水线

先定义 splitter 用来推送任务的工具（`send_task`——工具内通过 `GetMessageQueue` 拿到下游队列）：

```go
type Task struct {   // 上游推给下游的任务
	ID   string `json:"id"`
	Text string `json:"text"`
}

type SendTaskInput struct {
	ID   string `json:"id" desc:"任务ID" required:"true"`
	Text string `json:"text" desc:"任务内容" required:"true"`
}

sendTaskTool := goagent.InferTool("send_task", "把一个子任务发给下游处理",
	func(ctx context.Context, in SendTaskInput) (string, error) {
		q := goagent.GetMessageQueue(ctx, "worker") // 拿 Injects 声明的下游队列
		if q == nil {
			return "", fmt.Errorf("worker 队列不可用")
		}
		q.Push(Task{ID: in.ID, Text: in.Text})
		return "已发送: " + in.ID, nil
	})
```

然后组装 DAG：

```go
app.UsePipeline(goagent.PipelineConfig{
	Nodes: []goagent.PipelineNode{
		{
			Name:    "splitter",
			Message: "把「部署新服务、修复登录bug、写周报」拆成 3 个独立任务，每个用 send_task 工具发送",
			Injects: []string{"worker"}, // 声明：我可以往 worker 的队列推任务
			Agent: &goagent.PipelineAgentDef{
				Name:        "splitter",
				Instruction: "你是任务拆分器，用 send_task 工具把每个子任务发给下游",
				Tools:       []goagent.NamedTool{sendTaskTool},
			},
		},
		{
			Name:        "worker",
			Concurrency: 3, // 3 个并行 worker 同时消费队列
			DependsOn:   []string{"splitter"},
			MessageType: Task{},
			Agent: &goagent.PipelineAgentDef{
				Name:        "worker",
				Instruction: "处理收到的任务，一句话给出方案",
			},
		},
	},
})

err := app.RunPipeline(ctx) // 阻塞直到所有节点完成
```

**关键概念：**

| 概念 | 作用 |
|------|------|
| `DependsOn` | 依赖：splitter 全部完成后 worker 才启动 |
| `Injects` + 队列 | 数据流：splitter 的工具用 `GetMessageQueue(ctx, "worker")` 推任务 |
| `Concurrency` | worker 数：>1 自动建消息队列，任务并行消费 |
| `MessageType` | 队列消息类型（反射用，给序列化定形） |

### 3.2 事件透出：看节点在干什么

Pipeline 内部对嵌入方默认是黑盒。设 `OnEvent` 拿到每个节点的运行事件（进度/思考/错误，含 429 限流倒计时）：

```go
app.UsePipeline(goagent.PipelineConfig{
	Nodes:  ...,
	OnEvent: func(ev goagent.PipelineEvent) {
		fmt.Printf("[%s] %s: %s\n", ev.Node, ev.Type, ev.Text)
	},
})
```

### 3.3 失败语义

节点 LLM 报错（含空响应）会 `recordNodeError` → 取消整条 pipeline → `RunPipeline` 返回错误。**错误一律传播，不留静默残缺产出**——上游已落库的部分数据由你的事务/补偿逻辑负责。

更深的（Supervisor 审核、事务回滚、动态 LLM 建图）见 README 的「Pipeline DAG 编排」和「动态 Pipeline」。

---

## 学完了，接下来看什么

| 想做什么 | 去哪 |
|---------|------|
| 嵌入 Web 服务（多用户/多会话） | README「会话管理」+「多会话并行」 |
| 长对话上下文不够 | README「上下文压缩」 |
| 大任务委派给子 agent | README「子 Agent 系统」 |
| agent 运行时自建 DAG | README「动态 Pipeline」（L1 已实现） |
| 定时任务 / git worktree 隔离 | README「Cron 调度」「Worktree 隔离」 |
| 架构设计原理 | [architecture.md](architecture.md) / [mechanisms.md](mechanisms.md) |

## 学习路径总结

```
第 1 课（核心循环 + 事件流）
   └─ 第 2 课（工具 + 多轮）← 90% 的应用到此够用
         └─ 第 3 课（Pipeline）← 确定性多阶段任务才需要
               └─ README 进阶章节按需取用
```

记住一个判断准则：**依赖关系是任务固有的（提取→润色→审核）用 Pipeline；需要 agent 现场判断的用子 Agent 或单 agent + 工具**。Pipeline 不是更高级，是另一种场景。
