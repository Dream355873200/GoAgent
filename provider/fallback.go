// Package provider 定义 provider 接口。
package provider

import (
	"context"
	"strings"
)

// StreamingFallbackProvider 流式降级包装器。
// 对齐 Claude Code 的 mid-stream fallback 机制。
type StreamingFallbackProvider struct {
	provider   Provider
	fallback   Provider
	conditions []FallbackCondition
}

// FallbackCondition 降级条件函数。
type FallbackCondition func(*Request, error) bool

// DefaultFallbackConditions 默认降级条件。
func DefaultFallbackConditions() []FallbackCondition {
	return []FallbackCondition{
		// 超时降级
		func(req *Request, err error) bool {
			if err != nil && strings.Contains(err.Error(), "timeout") {
				return true
			}
			return false
		},
		// 过载降级
		func(req *Request, err error) bool {
			if _, ok := err.(*OverloadError); ok {
				return true
			}
			return false
		},
		// 特定模型不支持
		func(req *Request, err error) bool {
			if err != nil && strings.Contains(err.Error(), "model") {
				return true
			}
			return false
		},
	}
}

// NewStreamingFallbackProvider 创建流式降级包装器。
func NewStreamingFallbackProvider(primary Provider, fallback Provider, conditions []FallbackCondition) *StreamingFallbackProvider {
	if conditions == nil {
		conditions = DefaultFallbackConditions()
	}
	return &StreamingFallbackProvider{
		provider:   primary,
		fallback:   fallback,
		conditions: conditions,
	}
}

// Stream 实现 Provider 接口，带降级逻辑。
func (f *StreamingFallbackProvider) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	stream, err := f.provider.Stream(ctx, req)
	if err == nil {
		return stream, nil
	}

	// 检查是否需要降级
	for _, cond := range f.conditions {
		if cond(req, err) {
			return f.fallback.Stream(ctx, req)
		}
	}

	return nil, err
}

// Complete 实现 Provider 接口。
func (f *StreamingFallbackProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	resp, err := f.provider.Complete(ctx, req)
	if err == nil {
		return resp, nil
	}

	// 检查是否需要降级
	for _, cond := range f.conditions {
		if cond(req, err) {
			return f.fallback.Complete(ctx, req)
		}
	}

	return nil, err
}

// Capabilities 返回主 provider 能力。
func (f *StreamingFallbackProvider) Capabilities() Capabilities {
	return f.provider.Capabilities()
}

// SetModel 设置模型。
func (f *StreamingFallbackProvider) SetModel(modelID string) {
	if switcher, ok := f.provider.(ModelSwitcher); ok {
		switcher.SetModel(modelID)
	}
	if switcher, ok := f.fallback.(ModelSwitcher); ok {
		switcher.SetModel(modelID)
	}
}

// AddCondition 添加降级条件。
func (f *StreamingFallbackProvider) AddCondition(cond FallbackCondition) {
	f.conditions = append(f.conditions, cond)
}

// RetryStreamProvider 带重试的流式包装器。
type RetryStreamProvider struct {
	provider   Provider
	maxRetries int
	conditions []FallbackCondition
}

// NewRetryStreamProvider 创建带重试的包装器。
func NewRetryStreamProvider(provider Provider, maxRetries int) *RetryStreamProvider {
	return &RetryStreamProvider{
		provider:   provider,
		maxRetries: maxRetries,
		conditions: DefaultFallbackConditions(),
	}
}

// Stream 实现带重试的流式调用。
func (r *RetryStreamProvider) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	var lastErr error

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		stream, err := r.provider.Stream(ctx, req)
		if err == nil {
			return stream, nil
		}

		lastErr = err

		// 检查是否是可重试的错误
		retryable := false
		for _, cond := range r.conditions {
			if cond(req, err) {
				retryable = true
				break
			}
		}

		if !retryable || attempt == r.maxRetries {
			break
		}
	}

	return nil, lastErr
}

// Complete 实现带重试的 Complete 调用。
func (r *RetryStreamProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	var lastErr error

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		resp, err := r.provider.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// 检查是否可重试
		retryable := false
		for _, cond := range r.conditions {
			if cond(req, err) {
				retryable = true
				break
			}
		}

		if !retryable || attempt == r.maxRetries {
			break
		}
	}

	return nil, lastErr
}

// Capabilities 返回 provider 能力。
func (r *RetryStreamProvider) Capabilities() Capabilities {
	return r.provider.Capabilities()
}

// SetModel 设置模型。
func (r *RetryStreamProvider) SetModel(modelID string) {
	if switcher, ok := r.provider.(ModelSwitcher); ok {
		switcher.SetModel(modelID)
	}
}
