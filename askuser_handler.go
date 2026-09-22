package goagent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// AskUserRequest 是发送给前端的提问请求。
// 开发者通过 AskUserHandler 的 channel 接收此请求，
// 展示给用户后调用 Respond() 完成问答。
type AskUserRequest struct {
	// RequestID 是此次提问请求的唯一 ID。
	RequestID string `json:"request_id"`

	// Question 是向用户提问的问题。
	Question string `json:"question"`

	// Payload 结构化交互载荷（可选）。通用库不感知领域语义——宿主工具
	// 经 AskStructured 附加任意 JSON 对象（如确认卡的选项列表），随 SSE
	// ask_user 帧原样下发，客户端按 payload 里的 kind 字段分发渲染。
	Payload map[string]any `json:"payload,omitempty"`

	// once 确保只能调用一次 Respond。
	once sync.Once
	ch   chan string
}

// Respond 提交用户回答。
func (r *AskUserRequest) Respond(answer string) {
	r.once.Do(func() {
		r.ch <- answer
	})
}

// AskUserHandler 是异步提问处理器。
// 适用于 HTTP/WebSocket/SDK 等需要前端参与问答的场景。
//
// 使用流程：
//  1. 创建 handler: h := goagent.NewAskUserHandler()
//  2. 注册到 App: app.SetAskUserHandler(h)
//  3. 前端监听: for req := range h.Requests() { ... }
//  4. 用户回答后: req.Respond("用户回答的内容")
//
// 示例（HTTP SSE 模式）：
//
//	handler := goagent.NewAskUserHandler()
//	app.SetAskUserHandler(handler)
//
//	// 在 SSE 流中监听提问请求
//	go func() {
//	    for req := range handler.Requests() {
//	        // 通过 SSE 推送给前端
//	        sendSSE("ask_user", req)
//	    }
//	}()
//
//	// 前端返回回答时
//	http.HandleFunc("/askuser", func(w http.ResponseWriter, r *http.Request) {
//	    var resp AskUserHTTPRequest
//	    json.NewDecoder(r.Body).Decode(&resp)
//	    handler.Resolve(resp.RequestID, resp.Answer)
//	})
type AskUserHandler struct {
	requests chan *AskUserRequest
	pending  sync.Map // requestID → *AskUserRequest
	streams  *streamBus[*AskUserRequest]
	nextID   atomic.Int64
	mu       sync.Mutex
	closed   bool
}

// NewAskUserHandler 创建异步提问处理器。
// bufSize 是请求 channel 的缓冲区大小，默认 16。
func NewAskUserHandler() *AskUserHandler {
	return &AskUserHandler{
		requests: make(chan *AskUserRequest, 16),
		streams:  newStreamBus[*AskUserRequest](),
	}
}

// Requests 返回提问请求 channel。
// 直连消费模式（SDK/嵌入式宿主）持续消费此 channel 接收提问。
// HTTP/SSE 宿主应改用 Subscribe：按会话把请求精确路由到发起 run 的
// 活跃连接，避免共享 channel 的多消费者抢帧与 goroutine 泄漏。
func (h *AskUserHandler) Requests() <-chan *AskUserRequest {
	return h.requests
}

// Subscribe 把一条连接按会话绑定为提问下发通道（/chat handler 每请求
// 调用一次，defer 注销）。返回的 channel 在注销后关闭——消费 goroutine
// 用 range 消费即可随解绑自动退出，生命周期与连接严格绑定。
func (h *AskUserHandler) Subscribe(sessionID string) (<-chan *AskUserRequest, func()) {
	return h.streams.Subscribe(sessionID)
}

// Resolve 通过 requestID 解决一个待处理的提问请求。
// 这是供 HTTP handler 等外部接口调用的便捷方法。
func (h *AskUserHandler) Resolve(requestID string, answer string) bool {
	val, ok := h.pending.LoadAndDelete(requestID)
	if !ok {
		return false
	}
	req := val.(*AskUserRequest)
	req.Respond(answer)
	return true
}

// Close 关闭请求 channel。在会话结束时调用。
func (h *AskUserHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.requests)
	}
}

// Ask 实现 AskUserHandlerInterface 的同步提问回调（无会话信息）。
// 当 builtin AskUser 工具被调用时，此方法被调用。
// 它会创建一个 AskUserRequest 并通过 channel 发送给前端，然后阻塞等待回复。
func (h *AskUserHandler) Ask(question string) (string, error) {
	return h.AskSession("", question, nil)
}

// AskStructured 带 struct 化载荷的提问：与 Ask 同一管道，额外携带
// payload 供客户端分发渲染。领域工具（确认卡、单选/多选）用它替代
// 旧的「文本前缀内嵌 JSON」协议。多会话宿主建议用 AskSession。
func (h *AskUserHandler) AskStructured(question string, payload map[string]any) (string, error) {
	return h.AskSession("", question, payload)
}

// AskSession 按会话路由的提问：请求优先发往该会话绑定的活跃连接
// （AskStructured / Ask 在无会话信息时落到最近绑定的连接），没有任何
// 绑定连接时退回共享 channel（SDK 直连消费模式）。
func (h *AskUserHandler) AskSession(sessionID string, question string, payload map[string]any) (string, error) {
	return h.AskSessionCtx(context.Background(), sessionID, question, payload)
}

// AskSessionCtx 在 AskSession 基础上感知取消：run 被中断（ctx.Done）时，
// 等待用户回答的阻塞立即解除并清理 pending——否则提问会永久卡住整个
// run（前端「提问期间点终止无效」的根因）。ctx 取消后迟到的回答被丢弃
// （pending 已清理，Resolve 返回 false）。
func (h *AskUserHandler) AskSessionCtx(ctx context.Context, sessionID string, question string, payload map[string]any) (string, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return "", fmt.Errorf("ask handler 已关闭")
	}
	h.mu.Unlock()

	id := h.nextID.Add(1)
	requestID := fmt.Sprintf("ask-%d", id)

	responseCh := make(chan string, 1)
	req := &AskUserRequest{
		RequestID: requestID,
		Question:  question,
		Payload:   payload,
		ch:        responseCh,
	}

	// 注册到 pending map。
	h.pending.Store(requestID, req)

	// 优先路由到活跃连接（提问与回答它的流在同一连接上）。
	if h.streams.Send(sessionID, req) {
		return h.waitAnswer(ctx, requestID, responseCh)
	}

	// 兜底：无绑定连接（SDK 直连消费模式）。
	select {
	case h.requests <- req:
		// 等待前端回复。
		return h.waitAnswer(ctx, requestID, responseCh)
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return "", fmt.Errorf("提问随 run 中断取消: %w", ctx.Err())
	default:
		// Channel 满了，返回错误
		h.pending.Delete(requestID)
		return "", fmt.Errorf("ask 请求队列已满")
	}
}

// waitAnswer 等待前端回答，ctx 取消（run 被中断）时立即解除阻塞。
func (h *AskUserHandler) waitAnswer(ctx context.Context, requestID string, responseCh chan string) (string, error) {
	select {
	case answer := <-responseCh:
		h.pending.Delete(requestID)
		return answer, nil
	case <-ctx.Done():
		h.pending.Delete(requestID)
		return "", fmt.Errorf("提问随 run 中断取消: %w", ctx.Err())
	}
}

// AskUserHTTPRequest 是 HTTP /askuser 端点的请求格式。
type AskUserHTTPRequest struct {
	RequestID string `json:"request_id"`
	Answer    string `json:"answer"`
}

// AskUserHandlerInterface 是 AskUserHandler 的接口，用于 App 配置。
type AskUserHandlerInterface interface {
	Requests() <-chan *AskUserRequest
	Resolve(requestID string, answer string) bool
	Close()
	Ask(question string) (string, error)
}

// Ensure AskUserHandler implements AskUserHandlerInterface
var _ AskUserHandlerInterface = (*AskUserHandler)(nil)
