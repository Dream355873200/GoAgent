package goagent

import (
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
	nextID   atomic.Int64
	closed   atomic.Bool
}

// NewAskUserHandler 创建异步提问处理器。
// bufSize 是请求 channel 的缓冲区大小，默认 16。
func NewAskUserHandler() *AskUserHandler {
	return &AskUserHandler{
		requests: make(chan *AskUserRequest, 16),
	}
}

// Requests 返回提问请求 channel。
// 前端应持续消费此 channel 以接收需要回答的提问。
func (h *AskUserHandler) Requests() <-chan *AskUserRequest {
	return h.requests
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
	if h.closed.CompareAndSwap(false, true) {
		close(h.requests)
	}
}

// Ask 实现同步的 Ask 回调接口。
// 当 builtin AskUser 工具被调用时，此方法被调用。
// 它会创建一个 AskUserRequest 并通过 channel 发送给前端，然后阻塞等待回复。
func (h *AskUserHandler) Ask(question string) (string, error) {
	if h.closed.Load() {
		return "", fmt.Errorf("ask handler 已关闭")
	}

	id := h.nextID.Add(1)
	requestID := fmt.Sprintf("ask-%d", id)

	responseCh := make(chan string, 1)
	req := &AskUserRequest{
		RequestID: requestID,
		Question:  question,
		ch:        responseCh,
	}

	// 注册到 pending map。
	h.pending.Store(requestID, req)

	// 发送到前端。
	select {
	case h.requests <- req:
		// 等待前端回复。
		answer := <-responseCh
		h.pending.Delete(requestID)
		return answer, nil
	default:
		// Channel 满了，返回错误
		h.pending.Delete(requestID)
		return "", fmt.Errorf("ask 请求队列已满")
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
