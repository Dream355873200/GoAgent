// Package retry 实现 API 调用的重试和速率限制处理。
//
// 支持指数退避、retry-after header 解析、最大重试次数配置。
// 仅对 429/529/5xx 重试，4xx (非 429) 不重试。
//
// 对齐 Claude Code 的 claude.ts 重试逻辑。
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// Config 是重试器的配置。
type Config struct {
	// MaxRetries 是最大重试次数。默认 3。
	MaxRetries int

	// InitialDelay 是首次重试的延迟。默认 1s。
	InitialDelay time.Duration

	// MaxDelay 是最大延迟。默认 30s。
	MaxDelay time.Duration

	// Multiplier 是延迟倍增因子。默认 2.0。
	Multiplier float64

	// JitterFraction 是抖动比例 (0.0-1.0)。默认 0.1。
	JitterFraction float64
}

// DefaultConfig 返回默认重试配置。
func DefaultConfig() Config {
	return Config{
		MaxRetries:     3,
		InitialDelay:   1 * time.Second,
		MaxDelay:       30 * time.Second,
		Multiplier:     2.0,
		JitterFraction: 0.1,
	}
}

// RateLimitError 表示 API 返回了 429 速率限制错误。
type RateLimitError struct {
	// Message 是错误信息。
	Message string
	// RetryAfter 是建议的重试等待时间。0 表示未指定。
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return e.Message
}

// RetryableError 表示一个可重试的错误。
// 实现此接口的错误会被 Retry 自动重试。
type RetryableError interface {
	error
	IsRetryable() bool
}

// ServerError 表示 5xx 服务器错误。
type ServerError struct {
	StatusCode int
	Message    string
}

func (e *ServerError) Error() string {
	return e.Message
}

// IsRetryable 返回 true，5xx 错误总是可重试的。
func (e *ServerError) IsRetryable() bool {
	return true
}

// OverloadError 表示 529 过载错误。
type OverloadError struct {
	Message string
}

func (e *OverloadError) Error() string {
	return e.Message
}

// IsRetryable 返回 true，过载错误可重试。
func (e *OverloadError) IsRetryable() bool {
	return true
}

// IsRetryable 判断错误是否可重试。
func IsRetryable(err error) bool {
	// RateLimitError 始终可重试。
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return true
	}

	// 实现 RetryableError 接口的错误。
	var re RetryableError
	if errors.As(err, &re) {
		return re.IsRetryable()
	}

	return false
}

// GetRetryAfter 从错误中提取 retry-after 信息。
func GetRetryAfter(err error) time.Duration {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	return 0
}

// Retry 执行带重试的函数。
// fn 返回错误时，根据错误类型和配置决定是否重试。
func Retry[T any](ctx context.Context, cfg Config, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T

	if cfg.MaxRetries <= 0 {
		cfg = DefaultConfig()
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		// 检查上下文取消。
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}

		lastErr = err

		// 不可重试的错误直接返回。
		if !IsRetryable(err) {
			return zero, err
		}

		// 最后一次尝试不等待。
		if attempt == cfg.MaxRetries {
			break
		}

		// 计算等待时间。
		delay := calculateDelay(cfg, attempt, err)

		// 等待或被取消。
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	return zero, lastErr
}

// RetryVoid 执行带重试的无返回值函数。
func RetryVoid(ctx context.Context, cfg Config, fn func(ctx context.Context) error) error {
	_, err := Retry(ctx, cfg, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// calculateDelay 计算第 attempt 次重试的延迟。
func calculateDelay(cfg Config, attempt int, err error) time.Duration {
	// 优先使用 retry-after。
	if retryAfter := GetRetryAfter(err); retryAfter > 0 {
		return retryAfter
	}

	// 指数退避。
	delay := float64(cfg.InitialDelay) * math.Pow(cfg.Multiplier, float64(attempt))

	// 上限。
	if delay > float64(cfg.MaxDelay) {
		delay = float64(cfg.MaxDelay)
	}

	// 添加抖动。
	if cfg.JitterFraction > 0 {
		jitter := delay * cfg.JitterFraction
		delay += (rand.Float64()*2 - 1) * jitter
	}

	return time.Duration(delay)
}

// Attempt 记录单次尝试的信息。
type Attempt struct {
	// Number 是尝试次数（从 0 开始）。
	Number int
	// Error 是此次尝试的错误（nil 表示成功）。
	Error error
	// Delay 是此次尝试后的等待时间。
	Delay time.Duration
}
