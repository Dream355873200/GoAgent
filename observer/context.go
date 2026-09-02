package observer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
)

// ctxObserverKey 是边界注入 observer 的 context 键（不导出防碰撞）。
type ctxObserverKey struct{}

// combinedObserver 把「App 级 observer」与「ctx 边界注入的 observer」
// 合并为一个视图：两类事件都广播。loop 只拿一个 Observer 引用，
// 边界注入对循环完全透明。
type combinedObserver struct {
	base   Observer
	border []Observer
}

func (c *combinedObserver) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
	c.base.OnTokenUsage(ctx, model, usage, costUSD)
	for _, o := range c.border {
		o.OnTokenUsage(ctx, model, usage, costUSD)
	}
}

func (c *combinedObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
	c.base.OnToolStart(ctx, toolName, input)
	for _, o := range c.border {
		o.OnToolStart(ctx, toolName, input)
	}
}

func (c *combinedObserver) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
	c.base.OnToolDone(ctx, toolName, input, result, duration)
	for _, o := range c.border {
		o.OnToolDone(ctx, toolName, input, result, duration)
	}
}

func (c *combinedObserver) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
	c.base.OnToolError(ctx, toolName, input, err, duration)
	for _, o := range c.border {
		o.OnToolError(ctx, toolName, input, err, duration)
	}
}

func (c *combinedObserver) OnPermissionRequest(ctx context.Context, toolName string, input string, permission string) {
	c.base.OnPermissionRequest(ctx, toolName, input, permission)
	for _, o := range c.border {
		o.OnPermissionRequest(ctx, toolName, input, permission)
	}
}

func (c *combinedObserver) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
	c.base.OnPermissionGranted(ctx, toolName, permission)
	for _, o := range c.border {
		o.OnPermissionGranted(ctx, toolName, permission)
	}
}

func (c *combinedObserver) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
	c.base.OnPermissionDenied(ctx, toolName, permission, reason)
	for _, o := range c.border {
		o.OnPermissionDenied(ctx, toolName, permission, reason)
	}
}

func (c *combinedObserver) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
	c.base.OnCompaction(ctx, tokensFreed, reason)
	for _, o := range c.border {
		o.OnCompaction(ctx, tokensFreed, reason)
	}
}

func (c *combinedObserver) OnSessionStart(ctx context.Context, sessionID string) {
	c.base.OnSessionStart(ctx, sessionID)
	for _, o := range c.border {
		o.OnSessionStart(ctx, sessionID)
	}
}

func (c *combinedObserver) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {
	c.base.OnSessionEnd(ctx, sessionID, totalTurns)
	for _, o := range c.border {
		o.OnSessionEnd(ctx, sessionID, totalTurns)
	}
}

func (c *combinedObserver) OnError(ctx context.Context, err error) {
	c.base.OnError(ctx, err)
	for _, o := range c.border {
		o.OnError(ctx, err)
	}
}

// LLM 调用钩子（可选接口）：base 和边界 observer 各自按需实现。

func (c *combinedObserver) OnLLMStart(ctx context.Context, info *LLMCallInfo) {
	if o, ok := c.base.(LLMObserver); ok {
		o.OnLLMStart(ctx, info)
	}
	for _, ob := range c.border {
		if o, ok := ob.(LLMObserver); ok {
			o.OnLLMStart(ctx, info)
		}
	}
}

func (c *combinedObserver) OnLLMDone(ctx context.Context, info *LLMCallInfo, result *LLMResult) {
	if o, ok := c.base.(LLMObserver); ok {
		o.OnLLMDone(ctx, info, result)
	}
	for _, ob := range c.border {
		if o, ok := ob.(LLMObserver); ok {
			o.OnLLMDone(ctx, info, result)
		}
	}
}

func (c *combinedObserver) OnLLMError(ctx context.Context, info *LLMCallInfo, err error) {
	if o, ok := c.base.(LLMObserver); ok {
		o.OnLLMError(ctx, info, err)
	}
	for _, ob := range c.border {
		if o, ok := ob.(LLMObserver); ok {
			o.OnLLMError(ctx, info, err)
		}
	}
}

// IntoContext 把 observer 注入 ctx：此后经 ResolveObserver 拿到的视图
// 会把注入的 observer 与 base 合并广播。
//
// 典型用法（benchmark / 单次 run 临时采集）：
//
//	ctx = observer.IntoContext(ctx, trialCollector)
//	app.Run(ctx, "…") // 循环事件同时流经 App 级和 trialCollector
//
// 可多次注入：后注入的追加在链上（都广播）。base 为 nil 时注入的
// observer 直接生效。
func IntoContext(ctx context.Context, base Observer, o Observer) context.Context {
	// 合并已有链：取出旧链追加，保持单一 key。
	var chain []Observer
	if prev, ok := ctx.Value(ctxObserverKey{}).([]Observer); ok {
		chain = append(chain, prev...)
	}
	chain = append(chain, o)
	_ = base // base 由 ResolveObserver 时显式传入，此处不绑定
	return context.WithValue(ctx, ctxObserverKey{}, chain)
}

// ResolveObserver 解析 ctx 边界注入的 observer 链。
// 无注入时返回 base 原样（可能为 nil）。有注入时返回合并视图。
func ResolveObserver(ctx context.Context, base Observer) Observer {
	chain, ok := ctx.Value(ctxObserverKey{}).([]Observer)
	if !ok || len(chain) == 0 {
		return base
	}
	if base == nil {
		base = NopObserver{}
	}
	return &combinedObserver{base: base, border: chain}
}
