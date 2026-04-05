// Package driver 定义 Driver 接口，解耦 HTTP/CLI 与 App。
package driver

import (
	"context"

	"github.com/Dream355873200/GoAgent/analytics"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/cost"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/plan"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/task"
)

// Driver 是 agent 的驱动接口。
// HTTP Server 和 CLI 都通过此接口与 Agent 交互。
//
// 实现此接口可将 Agent 嵌入到任意外部系统（Kubernetes、CronJob 等）。
type Driver interface {
	// Run 在新会话中运行 agent。
	Run(ctx context.Context, input string) <-chan Event

	// RunWithHistory 带历史消息运行 agent。
	RunWithHistory(ctx context.Context, history []message.Message, input string) <-chan Event

	// Execute 同步执行并返回最终结果。
	Execute(ctx context.Context, input string) (*ExecuteResult, error)

	// ModelID 返回当前模型 ID。
	ModelID() string

	// ToolNames 返回已注册工具名称列表。
	ToolNames() []string

	// ToolDescription 返回指定工具的描述。
	ToolDescription(name string) string

	// TaskStore 返回 Task 存储接口。
	TaskStore() task.StoreInterface

	// PlanStore 返回 Plan 存储接口。
	PlanStore() plan.StoreInterface

	// BgTaskStore 返回后台任务存储接口。
	BgTaskStore() bgtask.StoreInterface

	// Usage 返回成本追踪摘要（可为 nil）。
	Usage() *cost.CostSummary

	// Analytics 返回使用分析摘要（可为 nil）。
	Analytics() analytics.Summary
}

// Event 是 Driver 运行过程中产生的事件。
type Event struct {
	Type       EventType
	Text       string
	Thinking   string
	ToolName   string
	ToolInput  any
	ToolResult string
	Usage      *provider.Usage
	Error      error
}

// EventType 是事件的类型。
type EventType string

const (
	EventError         EventType = "error"
	EventTextDelta     EventType = "text_delta"
	EventText          EventType = "text"
	EventThinkingDelta EventType = "thinking_delta"
	EventToolUse       EventType = "tool_use"
	EventToolResult    EventType = "tool_result"
	EventUsageUpdate   EventType = "usage_update"
	EventDone          EventType = "done"
	EventMessage       EventType = "message"
)

// ExecuteResult 是 Execute 的执行结果。
type ExecuteResult struct {
	FinalText  string
	TotalUsage provider.Usage
}
