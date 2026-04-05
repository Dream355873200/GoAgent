// Package cost 提供成本追踪的 Observer 实现。
//
// 通过 observer.Observer 接口，Tracker 可以作为 Agent 循环的观察者，
// 自动记录每次 LLM 调用的 Token 用量和 USD 成本。
//
// 使用方式：
//
//	app := goagent.New(
//	    goagent.WithCostTracking(),
//	)
//
// 这等价于手动设置：
//
//	costObs := &cost.Observer{Tracker: cost.NewTracker()}
//
//	app := goagent.New(
//	    goagent.WithObservers(costObs),
//	)
package cost

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anthropic-community/goagent/observer"
	"github.com/anthropic-community/goagent/provider"
)

// Observer 将 cost.Tracker 包装为 observer.Observer。
// 当 OnTokenUsage 被调用时，自动记录到 Tracker 中。
type Observer struct {
	// Tracker 是用于记录成本的追踪器。如果为 nil，则不记录。
	Tracker *Tracker

	// Model 是当前使用的模型名称。
	// 用于按模型聚合成本。
	Model string
}

// Verify that Observer implements the Observer interface.
var _ observer.Observer = (*Observer)(nil)

// OnTokenUsage 在每次 LLM 调用返回用量信息时记录成本。
func (o *Observer) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
	if o == nil || o.Tracker == nil {
		return
	}
	// 如果 Observer 未指定 Model，使用传入的 model 参数
	if o.Model != "" {
		model = o.Model
	}
	o.Tracker.Record(model,
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheReadTokens,
		usage.CacheCreateTokens,
	)
}

// OnToolStart 无操作。成本追踪只关心 Token 用量。
func (o *Observer) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {}

// OnToolDone 无操作。
func (o *Observer) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
}

// OnToolError 无操作。
func (o *Observer) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
}

// OnPermissionRequest 无操作。
func (o *Observer) OnPermissionRequest(ctx context.Context, toolName string, input string, permission string) {
}

// OnPermissionGranted 无操作。
func (o *Observer) OnPermissionGranted(ctx context.Context, toolName string, permission string) {}

// OnPermissionDenied 无操作。
func (o *Observer) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
}

// OnCompaction 无操作。
func (o *Observer) OnCompaction(ctx context.Context, tokensFreed int, reason string) {}

// OnSessionStart 无操作。
func (o *Observer) OnSessionStart(ctx context.Context, sessionID string) {}

// OnSessionEnd 无操作。
func (o *Observer) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {}

// OnError 无操作。
func (o *Observer) OnError(ctx context.Context, err error) {}
