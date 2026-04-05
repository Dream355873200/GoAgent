package goagent

import (
	"sync"
	"sync/atomic"
)

// PlanConfirmRequest 是发送给前端的计划确认请求。
// 开发者通过 PlanConfirmHandler 的 channel 接收此请求，
// 展示给用户后调用 Confirm() 或 Reject() 完成确认。
type PlanConfirmRequest struct {
	// RequestID 是此次确认请求的唯一 ID。
	RequestID string `json:"request_id"`

	// PlanContent 是待确认的计划内容。
	PlanContent string `json:"plan_content"`

	// once 确保只能调用一次 Confirm/Reject。
	once   sync.Once
	result chan bool
}

// Confirm 确认计划。
func (r *PlanConfirmRequest) Confirm() {
	r.once.Do(func() {
		r.result <- true
	})
}

// Reject 拒绝计划。
func (r *PlanConfirmRequest) Reject() {
	r.once.Do(func() {
		r.result <- false
	})
}

// PlanConfirmHandler 是异步计划确认处理器。
// 适用于 HTTP/WebSocket/SDK 等需要前端参与计划确认的场景。
//
// 使用流程：
//  1. 创建 handler: h := goagent.NewPlanConfirmHandler()
//  2. 注册到 App: app.SetPlanConfirmHandler(h)
//  3. 前端监听: for req := range h.Requests() { ... }
//  4. 用户决定后: req.Confirm() 或 req.Reject()
//
// 示例（HTTP SSE 模式）：
//
//	handler := goagent.NewPlanConfirmHandler()
//	app.SetPlanConfirmHandler(handler)
//
//	// 在 SSE 流中监听确认请求
//	go func() {
//	    for req := range handler.Requests() {
//	        // 通过 SSE 推送给前端
//	        sendSSE("plan_confirm", req)
//	    }
//	}()
//
//	// 前端返回确认结果时
//	http.HandleFunc("/plan/confirm", func(w http.ResponseWriter, r *http.Request) {
//	    var resp PlanConfirmHTTPRequest
//	    json.NewDecoder(r.Body).Decode(&resp)
//	    if resp.Confirm {
//	        handler.Confirm(resp.RequestID)
//	    } else {
//	        handler.Reject(resp.RequestID)
//	    }
//	})
type PlanConfirmHandler struct {
	requests chan *PlanConfirmRequest
	pending  sync.Map // requestID → *PlanConfirmRequest
	nextID   atomic.Int64
	closed   atomic.Bool
}

// NewPlanConfirmHandler 创建异步计划确认处理器。
// bufSize 是请求 channel 的缓冲区大小，默认 16。
func NewPlanConfirmHandler() *PlanConfirmHandler {
	return &PlanConfirmHandler{
		requests: make(chan *PlanConfirmRequest, 16),
	}
}

// Requests 返回确认请求 channel。
// 前端应持续消费此 channel 以接收需要确认的计划。
func (h *PlanConfirmHandler) Requests() <-chan *PlanConfirmRequest {
	return h.requests
}

// Resolve 通过 requestID 解决一个待处理的确认请求。
// confirm=true 表示确认，false 表示拒绝。
// 这是供 HTTP handler 调用的便捷方法。
func (h *PlanConfirmHandler) Resolve(requestID string, confirm bool) bool {
	val, ok := h.pending.LoadAndDelete(requestID)
	if !ok {
		return false
	}
	req := val.(*PlanConfirmRequest)
	if confirm {
		req.Confirm()
	} else {
		req.Reject()
	}
	return true
}

// Close 关闭请求 channel。在会话结束时调用。
func (h *PlanConfirmHandler) Close() {
	if h.closed.CompareAndSwap(false, true) {
		close(h.requests)
	}
}

// ConfirmRequest 发起一个计划确认请求，阻塞等待用户决定。
// 当 agent 需要用户确认计划时调用此方法。
func (h *PlanConfirmHandler) ConfirmRequest(requestID string) bool {
	val, ok := h.pending.Load(requestID)
	if !ok {
		return false
	}
	req := val.(*PlanConfirmRequest)
	confirmed := <-req.result
	h.pending.Delete(requestID)
	return confirmed
}

// PlanConfirmHTTPRequest 是 HTTP /plan/confirm 端点的请求格式。
type PlanConfirmHTTPRequest struct {
	RequestID string `json:"request_id"`
	Confirm   bool   `json:"confirm"`
	Reason    string `json:"reason,omitempty"`
}

// PlanConfirmHandlerInterface 是 PlanConfirmHandler 的接口，用于 App 配置。
type PlanConfirmHandlerInterface interface {
	Requests() <-chan *PlanConfirmRequest
	Resolve(requestID string, confirm bool) bool
	Close()
	ConfirmRequest(requestID string) bool
}

// Ensure PlanConfirmHandler implements PlanConfirmHandlerInterface
var _ PlanConfirmHandlerInterface = (*PlanConfirmHandler)(nil)

// InternalPlanConfirmRequest 内部使用的计划确认请求。
// 包含 requestID 和 result channel。
type InternalPlanConfirmRequest struct {
	RequestID   string
	PlanContent string
	result      chan bool
}
