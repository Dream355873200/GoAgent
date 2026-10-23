package goagent

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
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

	// AgentPath 非空表示审批来自该子代理（归属路径）。
	AgentPath string `json:"agent_path,omitempty"`

	// ToolUseID 是等待审批的工具调用 ID。
	ToolUseID string `json:"tool_use_id,omitempty"`

	// once 确保只能调用一次 Approve/Deny。
	once sync.Once
	ch   chan permissionResponse
	seq  int64
}

type permissionResponse struct {
	allow  bool
	always bool
	reason string
	input  json.RawMessage // 批准时替换工具参数（改参审批）
}

// Approve 批准此工具调用。
// alwaysAllow=true 表示后续同类工具调用不再询问。
func (r *PermissionRequest) Approve(alwaysAllow bool) {
	r.once.Do(func() {
		r.ch <- permissionResponse{allow: true, always: alwaysAllow}
	})
}

// ApproveWithInput 批准此工具调用，并以 input 替换它的参数（完整替换）执行。
// 修改后的参数按工具的入参 schema 校验，交给模型的工具结果会注明参数被修改过。
// 不会记为"始终允许"。
func (r *PermissionRequest) ApproveWithInput(input json.RawMessage) {
	r.once.Do(func() {
		r.ch <- permissionResponse{allow: true, input: append(json.RawMessage(nil), input...)}
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
	// router 把请求发布到会话事件日志（HTTP 模式安装，见 installSessionRouting）：
	// 没有显式 Subscribe 的会话经它到达所有挂在该会话上的连接。
	router func(*PermissionRequest) bool
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

// ResolveWithInput 批准属于 sessionID 的请求，并以 input 替换工具参数（见
// PermissionRequest.ApproveWithInput）。sessionID 为空时不校验会话归属。
func (h *PermissionHandler) ResolveWithInput(sessionID, requestID string, input json.RawMessage) bool {
	req := h.take(requestID, sessionID, sessionID != "")
	if req == nil {
		return false
	}
	req.ApproveWithInput(input)
	return true
}

func (h *PermissionHandler) resolve(requestID, sessionID string, checkSession, allow, alwaysAllow bool, reason string) bool {
	req := h.take(requestID, sessionID, checkSession)
	if req == nil {
		return false
	}
	if allow {
		req.Approve(alwaysAllow)
	} else {
		req.Deny(reason)
	}
	return true
}

// take 取走待处理的请求（checkSession 时只取属于 sessionID 的）。
func (h *PermissionHandler) take(requestID, sessionID string, checkSession bool) *PermissionRequest {
	val, ok := h.pending.Load(requestID)
	if !ok {
		return nil
	}
	req := val.(*PermissionRequest)
	if checkSession && req.SessionID != sessionID {
		return nil
	}
	if _, ok := h.pending.LoadAndDelete(requestID); !ok {
		return nil
	}
	return req
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
// 请求按 ctx 所属会话路由：先找该会话的显式订阅（Subscribe），再发布到会话
// 事件日志（HTTP 模式），都没有时走共享 channel（SDK 直连消费模式）。
// context 取消时自动拒绝。
func (h *PermissionHandler) ApproveWithContext(ctx context.Context, toolName string, input string, perm Permission) (bool, bool) {
	allow, always, _ := h.ApproveWithReason(ctx, toolName, input, perm)
	return allow, always
}

// ApproveWithReason 同 ApproveWithContext，并返回拒绝原因（实现 ReasonApprover）：
// Resolve / ResolveForSession 给出的 reason 由此写进交给模型的工具结果。
func (h *PermissionHandler) ApproveWithReason(ctx context.Context, toolName string, input string, perm Permission) (bool, bool, string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false, false, ""
	}
	router := h.router
	h.mu.Unlock()

	requestID := newRequestID("perm")
	responseCh := make(chan permissionResponse, 1)
	req := &PermissionRequest{
		RequestID:  requestID,
		ToolName:   toolName,
		ToolInput:  json.RawMessage(input),
		Permission: perm.String(),
		SessionID:  SessionIDFromContext(ctx),
		AgentPath:  AgentPathFromContext(ctx),
		ToolUseID:  observer.ToolCallIDFromContext(ctx),
		ch:         responseCh,
		seq:        h.nextID.Add(1),
	}

	// 注册到 pending map。
	h.pending.Store(requestID, req)

	if h.streams.Send(req.SessionID, req) || (router != nil && router(req)) {
		return h.wait(ctx, requestID, responseCh)
	}

	// 兜底：发到共享 channel（直连消费模式）。
	select {
	case h.requests <- req:
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return false, false, ""
	}
	return h.wait(ctx, requestID, responseCh)
}

// wait 等待前端回复，ctx 取消时按拒绝处理。
func (h *PermissionHandler) wait(ctx context.Context, requestID string, responseCh chan permissionResponse) (bool, bool, string) {
	select {
	case resp := <-responseCh:
		h.pending.Delete(requestID)
		if resp.allow && resp.input != nil && !permission.SetEditedInput(ctx, resp.input) {
			return false, false, "这次调用不支持修改参数后执行"
		}
		return resp.allow, resp.always, resp.reason
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return false, false, ""
	}
}

// setRouter 安装会话事件日志路由（nil 移除）。
func (h *PermissionHandler) setRouter(fn func(*PermissionRequest) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.router = fn
}
