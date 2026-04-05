package goagent

import (
	"context"
	"errors"
	"sync"
)

// ErrSessionNotFound 表示请求的会话不存在。
var ErrSessionNotFound = errors.New("session not found")

// InterruptHandler 是中断请求处理器。
// 适用于 HTTP/WebSocket 等需要外部中断 agent 执行的场景。
//
// 使用流程：
//  1. 创建 handler: h := goagent.NewInterruptHandler()
//  2. 传入 chat 请求: POST /chat { "message": "...", "interrupt_handler": h }
//  3. 前端请求中断: POST /interrupt { "session_id": "xxx" }
//
// 示例：
//
//	handler := goagent.NewInterruptHandler()
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	)
//
//	// 在 /chat handler 中
//	ctx := handler.WithCancel(context.Background())
//	for ev := range app.Run(ctx, msg) { ... }
//
//	// 在 /interrupt handler 中
//	handler.Interrupt(sessionID, "用户请求中断")
type InterruptHandler struct {
	mu       sync.RWMutex
	sessions sync.Map // sessionID → context.CancelFunc
}

// NewInterruptHandler 创建中断处理器。
func NewInterruptHandler() *InterruptHandler {
	return &InterruptHandler{}
}

// WithCancel 创建一个带取消上下文的会话。
// 返回新的 context 和 cancel 函数。
// cancel 函数应存储在 sessions map 中以便后续中断。
func (h *InterruptHandler) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// RegisterSession 注册一个会话的取消函数。
func (h *InterruptHandler) RegisterSession(sessionID string, cancel context.CancelFunc) {
	h.sessions.Store(sessionID, cancel)
}

// UnregisterSession 注销一个会话。
func (h *InterruptHandler) UnregisterSession(sessionID string) {
	h.sessions.Delete(sessionID)
}

// Interrupt 请求中断指定会话。
// sessionID 为空时中断所有会话。
func (h *InterruptHandler) Interrupt(sessionID string, reason string) error {
	if sessionID != "" {
		// 中断指定会话
		val, ok := h.sessions.Load(sessionID)
		if !ok {
			return ErrSessionNotFound
		}
		cancel := val.(context.CancelFunc)
		cancel()
		h.sessions.Delete(sessionID)
		return nil
	}

	// 中断所有会话
	h.sessions.Range(func(k, v any) bool {
		cancel := v.(context.CancelFunc)
		cancel()
		h.sessions.Delete(k)
		return true
	})
	return nil
}

// HasSession 检查指定会话是否在运行。
func (h *InterruptHandler) HasSession(sessionID string) bool {
	_, ok := h.sessions.Load(sessionID)
	return ok
}

// InterruptHandlerInterface 是 InterruptHandler 的接口。
type InterruptHandlerInterface interface {
	WithCancel(ctx context.Context) (context.Context, context.CancelFunc)
	RegisterSession(sessionID string, cancel context.CancelFunc)
	UnregisterSession(sessionID string)
	Interrupt(sessionID string, reason string) error
	HasSession(sessionID string) bool
}

// Ensure InterruptHandler implements InterruptHandlerInterface
var _ InterruptHandlerInterface = (*InterruptHandler)(nil)
