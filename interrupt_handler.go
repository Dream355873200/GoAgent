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
	mu  sync.Mutex
	seq uint64
	// sessionID → 登记 token → 取消函数。同一会话可有多个登记（同时在途的
	// 多个 /chat 请求）：各自只注销自己那一条，中断时全部取消——后来的
	// 请求（如忙时撞车被拒）不会覆盖或删掉真正在跑那个任务的 cancel。
	sessions map[string]map[uint64]interruptEntry
}

// interruptEntry 是一条取消登记：cause 非空时以 *InterruptError 为原因取消。
type interruptEntry struct {
	cancel context.CancelFunc
	cause  context.CancelCauseFunc
}

// InterruptError 是 InterruptHandler 取消运行时附带的原因：用 RegisterCause
// 登记的运行可经 context.Cause 取到它，循环据此给 EventInterrupted 填写原因。
type InterruptError struct {
	Reason string
}

func (e *InterruptError) Error() string {
	if e.Reason == "" {
		return "goagent: 用户中断"
	}
	return "goagent: 用户中断: " + e.Reason
}

// InterruptReason 返回中断原因。
func (e *InterruptError) InterruptReason() string { return e.Reason }

// NewInterruptHandler 创建中断处理器。
func NewInterruptHandler() *InterruptHandler {
	return &InterruptHandler{sessions: map[string]map[uint64]interruptEntry{}}
}

// WithCancel 创建一个带取消上下文的会话。
// 返回新的 context 和 cancel 函数。
// cancel 函数应存储在 sessions map 中以便后续中断。
func (h *InterruptHandler) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// Register 为会话追加一条取消登记，返回只注销这一条的函数。
func (h *InterruptHandler) Register(sessionID string, cancel context.CancelFunc) (unregister func()) {
	return h.register(sessionID, interruptEntry{cancel: cancel})
}

// RegisterCause 同 Register，但中断时以 *InterruptError{Reason} 为原因取消：
// 运行中的循环据此在 EventInterrupted 中带上 Interrupt 给出的原因。
func (h *InterruptHandler) RegisterCause(sessionID string, cancel context.CancelCauseFunc) (unregister func()) {
	return h.register(sessionID, interruptEntry{cause: cancel})
}

func (h *InterruptHandler) register(sessionID string, e interruptEntry) (unregister func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]map[uint64]interruptEntry{}
	}
	h.seq++
	token := h.seq
	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = map[uint64]interruptEntry{}
	}
	h.sessions[sessionID][token] = e
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m := h.sessions[sessionID]; m != nil {
			delete(m, token)
			if len(m) == 0 {
				delete(h.sessions, sessionID)
			}
		}
	}
}

// RegisterSession 注册一个会话的取消函数（替换该会话已有的全部登记）。
// 并发请求场景请用 Register。
func (h *InterruptHandler) RegisterSession(sessionID string, cancel context.CancelFunc) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.mu.Unlock()
	h.Register(sessionID, cancel)
}

// UnregisterSession 注销一个会话的全部登记。
func (h *InterruptHandler) UnregisterSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, sessionID)
}

// Interrupt 请求中断指定会话。
// sessionID 为空时中断所有会话。reason 经 RegisterCause 登记的运行可见
// （EventInterrupted 的 InterruptReason）。
func (h *InterruptHandler) Interrupt(sessionID string, reason string) error {
	h.mu.Lock()
	var entries []interruptEntry
	if sessionID != "" {
		m, ok := h.sessions[sessionID]
		if !ok {
			h.mu.Unlock()
			return ErrSessionNotFound
		}
		for _, e := range m {
			entries = append(entries, e)
		}
		delete(h.sessions, sessionID)
	} else {
		for _, m := range h.sessions {
			for _, e := range m {
				entries = append(entries, e)
			}
		}
		h.sessions = map[string]map[uint64]interruptEntry{}
	}
	h.mu.Unlock()
	cause := &InterruptError{Reason: reason}
	for _, e := range entries {
		if e.cause != nil {
			e.cause(cause)
		} else {
			e.cancel()
		}
	}
	return nil
}

// HasSession 检查指定会话是否在运行。
func (h *InterruptHandler) HasSession(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions[sessionID]) > 0
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
