// Package analytics 提供使用分析的 Observer 实现。
//
// 通过 observer.Observer 接口，Tracker 可以作为 Agent 循环的观察者，
// 自动记录工具调用、轮次耗时、压缩事件、错误率等分析数据。
//
// 使用方式：
//
//	app := goagent.New(
//	    goagent.WithAnalytics(),
//	)
//
// 这等价于手动设置：
//
//	analyticsObs := &analytics.Observer{Tracker: analytics.NewTracker()}
//
//	app := goagent.New(
//	    goagent.WithObservers(analyticsObs),
//	)
package analytics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anthropic-community/goagent/observer"
	"github.com/anthropic-community/goagent/provider"
)

// Observer 将 analytics.Tracker 包装为 observer.Observer。
// 将 Observer 事件映射到 Tracker 的记录方法。
type Observer struct {
	// Tracker 是用于记录分析数据的追踪器。如果为 nil，则不记录。
	Tracker *Tracker
}

// Verify that Observer implements the Observer interface.
var _ observer.Observer = (*Observer)(nil)

// OnTokenUsage 无操作。分析追踪不关心 Token 用量（由 cost 负责）。
func (o *Observer) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
}

// OnToolStart 在工具开始执行时记录调用。
func (o *Observer) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	if o == nil || o.Tracker == nil {
		return
	}
	o.Tracker.RecordToolCall(toolName)
}

// OnToolDone 在工具成功完成时记录结果和耗时。
func (o *Observer) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
	if o == nil || o.Tracker == nil {
		return
	}
	o.Tracker.RecordToolResult(toolName, duration, false)
}

// OnToolError 在工具执行出错时记录错误。
func (o *Observer) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
	if o == nil || o.Tracker == nil {
		return
	}
	o.Tracker.RecordToolResult(toolName, duration, true)
	if err != nil {
		o.Tracker.RecordError(err)
	}
}

// OnPermissionRequest 无操作。
func (o *Observer) OnPermissionRequest(ctx context.Context, toolName string, input string, permission string) {
}

// OnPermissionGranted 无操作。
func (o *Observer) OnPermissionGranted(ctx context.Context, toolName string, permission string) {}

// OnPermissionDenied 无操作。
func (o *Observer) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
}

// OnCompaction 在上下文压缩发生时记录。
func (o *Observer) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
	if o == nil || o.Tracker == nil {
		return
	}
	o.Tracker.RecordCompaction(tokensFreed)
}

// OnSessionStart 无操作。
func (o *Observer) OnSessionStart(ctx context.Context, sessionID string) {}

// OnSessionEnd 无操作。会话级别的统计由 Tracker 内部维护。
func (o *Observer) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {}

// OnError 在发生错误时记录。
func (o *Observer) OnError(ctx context.Context, err error) {
	if o == nil || o.Tracker == nil || err == nil {
		return
	}
	o.Tracker.RecordError(err)
}
