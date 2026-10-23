# GoAgent Progressive Tutorial

> [简体中文](../tutorial.md) | English

Learn GoAgent's core in three lessons: from a 30-line minimal agent, to multi-turn conversations with tools and memory, to a multi-node DAG pipeline. Every lesson's code is complete and runnable.

> Prerequisites: Go 1.25+, an LLM API key (OpenAI-compatible endpoint or Anthropic).
> Full API reference: [README](../../README.md). Architecture deep-dives: [architecture.md](architecture.md).

---

## Lesson 1: Minimal agent — running in 30 lines

An agent is, at its core, a loop: **the LLM generates → executes tools when needed → feeds results back → until it produces a final answer**. GoAgent wraps this loop in an `App`; you only supply the model config and the input.

Create `main.go`:

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
			Model:   "deepseek-chat",            // any OpenAI-compatible model
			APIKey:  "sk-...",
			BaseURL: "https://api.deepseek.com/v1",
		},
	)

	// Run returns an event stream: every action the LLM takes is an event
	for ev := range app.Run(context.Background(), "Explain what a DAG is in one sentence") {
		switch ev.Type {
		case goagent.EventTextDelta:
			fmt.Print(ev.Text) // streamed output
		case goagent.EventError:
			fmt.Printf("\nerror: %v\n", ev.Error)
		}
	}
}
```

Run it:

```bash
go mod init myagent && go get github.com/Dream355873200/GoAgent
go run main.go
```

**Three takeaways from this lesson:**

1. **`ProviderConfig` describes the model.** OpenAI-compatible endpoints (DeepSeek/Ollama/OpenRouter/vLLM) only need Model + BaseURL; for Anthropic set `Type: "anthropic"`.
2. **`Run` returns an event stream, not a result.** Events are the only way the agent communicates with the outside: `EventTextDelta` (text increments), `EventToolStart/Done` (tool calls), `EventThinking` (reasoning), `EventError`, `EventDone`. What you consume and how you present it is entirely up to you.
3. **Don't want to handle the event stream?** Use the synchronous `Execute`:

```go
result, err := app.Execute(ctx, "Explain what a DAG is in one sentence")
fmt.Println(result.FinalText) // the final text, directly
```

---

## Lesson 2: Adding tools and multi-turn memory

An agent without tools can only talk. This lesson gives the agent a query tool and makes it remember the previous turn.

### 2.1 Define tools with InferTool (zero boilerplate)

GoAgent **generates the JSON Schema automatically** from a Go function signature — you write an ordinary function, the framework produces the tool definition the LLM needs:

```go
// Input struct: json tags are field names, desc tags are explanations for the LLM
type WeatherInput struct {
	City string `json:"city" desc:"city name" required:"true"`
}

// The signature must be func(ctx, input) (output, error)
func getWeather(ctx context.Context, in WeatherInput) (string, error) {
	// replace with a real API call; fake data for the demo
	return in.City + ": sunny, 25°C", nil
}

app.UseTools(
	goagent.InferTool("get_weather", "Look up the weather for a city", getWeather),
)
```

Now "What's the weather in Shanghai?" makes the LLM call `get_weather` and weave the result into its answer. **Permissions** default to ReadOnly (auto-approved); for write operations pass a fourth argument `goagent.Normal` (asks once) or `goagent.Dangerous` (always asks).

### 2.2 Multi-turn: managing history yourself (to understand the mechanics)

`Run` starts an isolated session every call. For multi-turn conversations use `RunWithHistory` — you load/persist the history, the framework stitches it together:

```go
var history []message.Message

ask := func(input string) {
	for ev := range app.RunWithHistory(ctx, history, input) {
		if ev.Type == goagent.EventTextDelta {
			fmt.Print(ev.Text)
		}
		// EventDone carries this turn's full message list (including the
		// assistant reply) — collect it as the next round's history
		if ev.Type == goagent.EventDone {
			history = ev.Messages
		}
	}
	fmt.Println()
}

ask("My name is Kim")     // the agent remembers
ask("What's my name?")    // answers correctly — history was attached
```

> This lesson uses `RunWithHistory` so you can see how history works. **For production prefer `RunSession`**: configure `WithSessionManager(mgr)` and the framework loads/persists history by sessionID automatically, keeping "always allow" grants and the session permission mode across runs. When history already lives in your own database, use `RunWithSessionHistory` (full session identity, history still yours). See "Which run entry point?" in the README.

### 2.3 Complete example

```go
package main

import (
	"context"
	"fmt"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/message"
)

type WeatherInput struct {
	City string `json:"city" desc:"city name" required:"true"`
}

func getWeather(ctx context.Context, in WeatherInput) (string, error) {
	return in.City + ": sunny, 25°C", nil
}

func main() {
	ctx := context.Background()
	app := goagent.New(
		goagent.ProviderConfig{Model: "deepseek-chat", APIKey: "sk-...", BaseURL: "https://api.deepseek.com/v1"},
		goagent.WithSystemPrompt("You are a concise assistant."),
	)
	app.UseTools(goagent.InferTool("get_weather", "Look up the weather for a city", getWeather))

	var history []message.Message
	ask := func(input string) {
		fmt.Printf("me: %s\nai: ", input)
		for ev := range app.RunWithHistory(ctx, history, input) {
			switch ev.Type {
			case goagent.EventTextDelta:
				fmt.Print(ev.Text)
			case goagent.EventToolStart:
				fmt.Printf("\n[calling tool %s]", ev.ToolName)
			case goagent.EventDone:
				history = ev.Messages
			}
		}
		fmt.Println()
	}

	ask("What's the weather in Shanghai?")  // calls get_weather
	ask("And in Beijing?")                  // also calls it — "weather" is in context
}
```

**Three takeaways from this lesson:**

1. `InferTool` generates the tool definition from the function signature; after `UseTools` the LLM sees it automatically
2. `RunWithHistory` supports multi-turn; `EventDone`'s `ev.Messages` is your history-collection hook
3. Tool functions can access `ctx` (context passthrough) — pass request-scoped data (user ID, etc.) through ctx

---

## Lesson 3: Running a multi-node pipeline

A single agent is great for conversation; **deterministic multi-stage tasks** (split first → process in parallel → aggregate) call for a Pipeline. It's a DAG: nodes are agents, edges are dependencies, data flows through queues.

### 3.1 A two-node pipeline

First define the tool the splitter uses to push tasks (`send_task` — inside the tool, `GetMessageQueue` fetches the downstream queue):

```go
type Task struct {   // a task pushed from upstream to downstream
	ID   string `json:"id"`
	Text string `json:"text"`
}

type SendTaskInput struct {
	ID   string `json:"id" desc:"task ID" required:"true"`
	Text string `json:"text" desc:"task content" required:"true"`
}

sendTaskTool := goagent.InferTool("send_task", "Send one subtask downstream",
	func(ctx context.Context, in SendTaskInput) (string, error) {
		q := goagent.GetMessageQueue(ctx, "worker") // the queue declared via Injects
		if q == nil {
			return "", fmt.Errorf("worker queue unavailable")
		}
		q.Push(Task{ID: in.ID, Text: in.Text})
		return "sent: " + in.ID, nil
	})
```

Then assemble the DAG:

```go
app.UsePipeline(goagent.PipelineConfig{
	Nodes: []goagent.PipelineNode{
		{
			Name:    "splitter",
			Message: "Split 'deploy the new service, fix the login bug, write the weekly report' into 3 independent tasks, send each with the send_task tool",
			Injects: []string{"worker"}, // declare: I can push into worker's queue
			Agent: &goagent.PipelineAgentDef{
				Name:        "splitter",
				Instruction: "You are a task splitter; use the send_task tool to send each subtask downstream",
				Tools:       []goagent.NamedTool{sendTaskTool},
			},
		},
		{
			Name:        "worker",
			Concurrency: 3, // 3 parallel workers consuming the queue
			DependsOn:   []string{"splitter"},
			MessageType: Task{},
			Agent: &goagent.PipelineAgentDef{
				Name:        "worker",
				Instruction: "Handle the tasks you receive; propose a solution in one sentence",
			},
		},
	},
})

err := app.RunPipeline(ctx) // blocks until every node finishes
```

**Key concepts:**

| Concept | Role |
|---------|------|
| `DependsOn` | Dependencies: worker starts only after splitter fully completes |
| `Injects` + queues | Data flow: splitter's tools push tasks via `GetMessageQueue(ctx, "worker")` |
| `Concurrency` | Worker count: >1 auto-creates a message queue, tasks consumed in parallel |
| `MessageType` | Queue message type (used via reflection; gives serialization its shape) |

### 3.2 Event surfacing: seeing what nodes are doing

Pipeline internals are a black box for embedders by default. Set `OnEvent` to receive each node's runtime events (progress/thinking/error, including 429 rate-limit countdowns):

```go
app.UsePipeline(goagent.PipelineConfig{
	Nodes:  ...,
	OnEvent: func(ev goagent.PipelineEvent) {
		fmt.Printf("[%s] %s: %s\n", ev.Node, ev.Type, ev.Text)
	},
})
```

### 3.3 Failure semantics

A node whose LLM errors (including empty responses) triggers `recordNodeError` → the entire pipeline is cancelled → `RunPipeline` returns the error. **Errors always propagate; no silent partial output** — partial data already persisted upstream is your transaction/compensation logic's responsibility.

Deeper topics (supervisor review, transaction rollback, dynamic LLM-built graphs) are covered in the README's "Pipeline DAG orchestration" and "Dynamic pipelines" sections.

---

## What's next

| Goal | Where to go |
|------|------------|
| Production multi-turn sessions | README "Which run entry point?" + reference.md "Session management" (Chinese) |
| Embedding into a web service | reference.md "Run modes", "AG-UI 1.0 profile" |
| Delegating big tasks to subagents | reference.md "Sub-agent system" (incl. subagent governance) |
| Agent-built DAGs at runtime | README "Dynamic pipelines" (L1 implemented) |
| Architecture principles | [architecture.md](architecture.md) / [mechanisms.md](mechanisms.md) |

One rule of thumb to remember: **when dependencies are inherent to the task (extract → polish → review) use a Pipeline; when the agent must decide at runtime, use a subagent or a single agent + tools.** A pipeline isn't "more advanced" — it's a different shape of problem.
