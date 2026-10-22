package goagent

import (
	"context"
	"encoding/json"
	"sort"
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

	// SessionID 是发起审批的会话（由运行 ctx 决定）。
	SessionID string `json:"session_id,omitempty"`

	// once 确保只能调用一次 Approve/Deny。
	once sync.Once
	ch   chan permissionResponse
	seq  int64
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
	streams  *streamBus[*PermissionRequest]
	nextID   atomic.Int64
	mu       sync.Mutex
	closed   bool
}

// NewPermissionHandler 创建异步权限处理器。
// bufSize 是请求 channel 的缓冲区大小，默认 16。
func NewPermissionHandler() *PermissionHandler {
	return &PermissionHandler{
		requests: make(chan *PermissionRequest, 16),
		streams:  newStreamBus[*PermissionRequest](),
	}
}

// Requests 返回权限请求 channel。
// 直连消费模式（SDK/嵌入式宿主）持续消费此 channel 接收审批请求。
// HTTP/SSE 宿主应改用 Subscribe：按会话路由到活跃连接。
func (h *PermissionHandler) Requests() <-chan *PermissionRequest {
	return h.requests
}

// Subscribe 把一条连接按会话绑定为审批下发通道（/chat handler 每请求
// 调用一次，defer 注销）。返回的 channel 在注销后关闭——消费 goroutine
// 用 range 消费即可随解绑自动退出，生命周期与连接严格绑定。
func (h *PermissionHandler) Subscribe(sessionID string) (<-chan *PermissionRequest, func()) {
	return h.streams.Subscribe(sessionID)
}

// Resolve 通过 requestID 解决一个待处理的权限请求。
// 这是供 HTTP handler 等外部接口调用的便捷方法。request_id 是随机生成
// 的不可猜测值；需要校验会话归属时用 ResolveForSession。
func (h *PermissionHandler) Resolve(requestID string, allow bool, alwaysAllow bool, reason string) bool {
	return h.resolve(requestID, "", false, allow, alwaysAllow, reason)
}

// ResolveForSession 同 Resolve，但只解决属于 sessionID 的请求：一个会话
// 不能替另一个会话作答。
func (h *PermissionHandler) ResolveForSession(sessionID, requestID string, allow bool, alwaysAllow bool, reason string) bool {
	return h.resolve(requestID, sessionID, true, allow, alwaysAllow, reason)
}

func (h *PermissionHandler) resolve(requestID, sessionID string, checkSession, allow, alwaysAllow bool, reason string) bool {
	val, ok := h.pending.Load(requestID)
	if !ok {
		return false
	}
	req := val.(*PermissionRequest)
	if checkSession && req.SessionID != sessionID {
		return false
	}
	if _, ok := h.pending.LoadAndDelete(requestID); !ok {
		return false
	}
	if allow {
		req.Approve(alwaysAllow)
	} else {
		req.Deny(reason)
	}
	return true
}

// PendingBySession 返回某会话当前未决的审批请求（按发起顺序）。前端重载/
// 重连后据此恢复审批卡；为空表示没有在等审批的工具调用。
func (h *PermissionHandler) PendingBySession(sessionID string) []*PermissionRequest {
	if sessionID == "" {
		return nil
	}
	var out []*PermissionRequest
	h.pending.Range(func(_, v any) bool {
		if req := v.(*PermissionRequest); req.SessionID == sessionID {
			out = append(out, req)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// Close 关闭请求 channel。在会话结束时调用。
func (h *PermissionHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.requests)
	}
}

// Approve 实现 Approver 接口（不带 ctx：无法取消、也拿不到会话）。
// Gate 会优先调用 ApproveWithContext。
func (h *PermissionHandler) Approve(toolName string, input string, perm Permission) (bool, bool) {
	return h.ApproveWithContext(context.Background(), toolName, input, perm)
}

// ApproveWithContext 是带 context 的审批方法（实现 ContextApprover）。
// 请求路由到 ctx 所属会话的活跃连接；没有绑定连接时走共享 channel
// （SDK 直连消费模式）。context 取消时自动拒绝。
func (h *PermissionHandler) ApproveWithContext(ctx context.Context, toolName string, input string, perm Permission) (bool, bool) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false, false
	}
	h.mu.Unlock()

	requestID := newRequestID("perm")
	responseCh := make(chan permissionResponse, 1)
	req := &PermissionRequest{
		RequestID:  requestID,
		ToolName:   toolName,
		ToolInput:  json.RawMessage(input),
		Permission: perm.String(),
		SessionID:  SessionIDFromContext(ctx),
		ch:         responseCh,
		seq:        h.nextID.Add(1),
	}

	// 注册到 pending map。
	h.pending.Store(requestID, req)

	// 优先路由到该会话绑定的活跃连接；未命中再走共享 channel 兜底。
	if h.streams.Send(req.SessionID, req) {
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

	// 兜底：发到共享 channel（直连消费模式）。
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
