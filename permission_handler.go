package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// PermissionRequest 是发送给前端的权限审批请求。
// 开发者通过 PermissionHandler 的 channel 接收此请求，
// 展示给用户后调用 Approve() 或 Deny() 完成审批。
type PermissionRequest struct {
	// RequestID 是此次审批请求的唯一 ID。
	RequestID string `json:"request_id"`

	// ToolName 是请求执行的工具名称。
	ToolName string `json:"tool_name"`

	// ToolInput 是工具的输入参数（JSON）。
	ToolInput json.RawMessage `json:"tool_input"`

	// Permission 是工具声明的权限级别。
	Permission string `json:"permission"`

	// Description 是工具调用的简要描述。
	Description string `json:"description,omitempty"`

	// once 确保只能调用一次 Approve/Deny。
	once sync.Once
	ch   chan permissionResponse
}

type permissionResponse struct {
	allow  bool
	always bool
	reason string
}

// Approve 批准此工具调用。
// alwaysAllow=true 表示后续同类工具调用不再询问。
func (r *PermissionRequest) Approve(alwaysAllow bool) {
	r.once.Do(func() {
		r.ch <- permissionResponse{allow: true, always: alwaysAllow}
	})
}

// Deny 拒绝此工具调用。
func (r *PermissionRequest) Deny(reason string) {
	r.once.Do(func() {
		r.ch <- permissionResponse{allow: false, reason: reason}
	})
}

// PermissionHandler 是异步权限审批处理器。
// 适用于 HTTP/WebSocket/SDK 等需要前端参与审批的场景。
//
// 使用流程：
//  1. 创建 handler: h := goagent.NewPermissionHandler()
//  2. 注册到 App: goagent.WithApprover(h)
//  3. 前端监听: for req := range h.Requests() { ... }
//  4. 用户决定后: req.Approve(false) 或 req.Deny("用户拒绝")
//
// 示例（HTTP SSE 模式）：
//
//	handler := goagent.NewPermissionHandler()
//	app := goagent.New(
//	    goagent.WithProvider(provider),
//	    goagent.WithApprover(handler),
//	)
//
//	// 在 SSE 流中监听权限请求
//	go func() {
//	    for req := range handler.Requests() {
//	        // 通过 SSE 推送给前端
//	        sendSSE("permission_request", req)
//	    }
//	}()
//
//	// 前端返回审批结果时
//	http.HandleFunc("/approve", func(w http.ResponseWriter, r *http.Request) {
//	    var resp ApproveRequest
//	    json.NewDecoder(r.Body).Decode(&resp)
//	    handler.Resolve(resp.RequestID, resp.Allow, resp.AlwaysAllow, resp.Reason)
//	})
type PermissionHandler struct {
	requests chan *PermissionRequest
	pending  sync.Map // requestID → *PermissionRequest
	nextID   atomic.Int64
	closed   atomic.Bool
}

// NewPermissionHandler 创建异步权限处理器。
// bufSize 是请求 channel 的缓冲区大小，默认 16。
func NewPermissionHandler() *PermissionHandler {
	return &PermissionHandler{
		requests: make(chan *PermissionRequest, 16),
	}
}

// Requests 返回权限请求 channel。
// 前端应持续消费此 channel 以接收需要审批的工具调用。
func (h *PermissionHandler) Requests() <-chan *PermissionRequest {
	return h.requests
}

// Resolve 通过 requestID 解决一个待处理的权限请求。
// 这是供 HTTP handler 等外部接口调用的便捷方法。
func (h *PermissionHandler) Resolve(requestID string, allow bool, alwaysAllow bool, reason string) bool {
	val, ok := h.pending.LoadAndDelete(requestID)
	if !ok {
		return false
	}
	req := val.(*PermissionRequest)
	if allow {
		req.Approve(alwaysAllow)
	} else {
		req.Deny(reason)
	}
	return true
}

// Close 关闭请求 channel。在会话结束时调用。
func (h *PermissionHandler) Close() {
	if h.closed.CompareAndSwap(false, true) {
		close(h.requests)
	}
}

// Approve 实现 Approver 接口。
// 当 permission.Gate 需要用户审批时调用此方法。
// 它会创建一个 PermissionRequest 并通过 channel 发送给前端，
// 然后阻塞等待前端的回复。
func (h *PermissionHandler) Approve(toolName string, input string, perm Permission) (bool, bool) {
	return h.ApproveWithContext(context.Background(), toolName, input, perm)
}

// ApproveWithContext 是带 context 的审批方法。
// context 取消时自动拒绝。
func (h *PermissionHandler) ApproveWithContext(ctx context.Context, toolName string, input string, perm Permission) (bool, bool) {
	if h.closed.Load() {
		return false, false
	}

	id := h.nextID.Add(1)
	requestID := fmt.Sprintf("perm-%d", id)

	responseCh := make(chan permissionResponse, 1)
	req := &PermissionRequest{
		RequestID:  requestID,
		ToolName:   toolName,
		ToolInput:  json.RawMessage(input),
		Permission: perm.String(),
		ch:         responseCh,
	}

	// 注册到 pending map。
	h.pending.Store(requestID, req)

	// 发送到前端。
	select {
	case h.requests <- req:
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return false, false
	}

	// 等待前端回复。
	select {
	case resp := <-responseCh:
		h.pending.Delete(requestID)
		return resp.allow, resp.always
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return false, false
	}
}
