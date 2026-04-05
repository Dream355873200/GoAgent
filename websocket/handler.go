// Package websocket 实现 WebSocket 支持。
//
// 提供双向实时通信能力，支持权限审批和中断指令。
package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // 生产环境应验证 Origin
	},
}

// WSConfig WebSocket 配置。
type WSConfig struct {
	// ReadBufferSize 读取缓冲区大小。
	ReadBufferSize int
	// WriteBufferSize 写入缓冲区大小。
	WriteBufferSize int
	// PingInterval 心跳间隔。
	PingInterval time.Duration
	// PongTimeout PONG 超时。
	PongTimeout time.Duration
	// RequestTimeout 请求超时。
	RequestTimeout time.Duration
}

// DefaultWSConfig 返回默认配置。
func DefaultWSConfig() WSConfig {
	return WSConfig{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		PingInterval:    30 * time.Second,
		PongTimeout:     60 * time.Second,
		RequestTimeout:  60 * time.Second,
	}
}

// Message WebSocket 消息。
type Message struct {
	// Type 消息类型。
	Type string `json:"type"`
	// SessionID 会话 ID。
	SessionID string `json:"session_id,omitempty"`
	// Data 消息数据。
	Data json.RawMessage `json:"data,omitempty"`
	// Timestamp 时间戳。
	Timestamp time.Time `json:"timestamp"`
}

// MessageType 消息类型常量。
const (
	TypeEvent        = "event"         // 服务端事件
	TypeApprove      = "approve"       // 权限审批
	TypeDeny         = "deny"          // 权限拒绝
	TypeInterrupt    = "interrupt"     // 中断执行
	TypeResume       = "resume"        // 恢复执行
	TypePing         = "ping"          // 心跳
	TypePong         = "pong"          // 心跳响应
	TypeError        = "error"         // 错误
	TypeStartSession = "start_session" // 开始会话
	TypeEndSession   = "end_session"   // 结束会话
)

// Handler WebSocket 处理器。
type Handler struct {
	app interface {
		Approve(ctx context.Context, sessionID, requestID string) error
		Deny(ctx context.Context, sessionID, requestID string) error
		Interrupt(ctx context.Context, sessionID string) error
		Resume(ctx context.Context, sessionID, input string) error
	}
	config  WSConfig
	clients map[string]*Client
	mu      sync.RWMutex
}

// NewHandler 创建 WebSocket 处理器。
func NewHandler(app interface {
	Approve(ctx context.Context, sessionID, requestID string) error
	Deny(ctx context.Context, sessionID, requestID string) error
	Interrupt(ctx context.Context, sessionID string) error
	Resume(ctx context.Context, sessionID, input string) error
}, config WSConfig) *Handler {
	if config.PingInterval == 0 {
		config = DefaultWSConfig()
	}
	return &Handler{
		app:     app,
		config:  config,
		clients: make(map[string]*Client),
	}
}

// HandleWebSocket 处理 WebSocket 连接。
func (h *Handler) HandleWebSocket(w http.ResponseWriter, r *http.Request, sessionID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := NewClient(sessionID, conn, h.config)
	h.addClient(sessionID, client)
	defer h.removeClient(sessionID)

	client.Run()
}

// HandleHTTP 处理 HTTP 请求（升级为 WebSocket）。
func (h *Handler) HandleHTTP(w http.ResponseWriter, r *http.Request, sessionID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Failed to upgrade", http.StatusInternalServerError)
		return
	}

	client := NewClient(sessionID, conn, h.config)
	h.addClient(sessionID, client)
	defer h.removeClient(sessionID)

	client.Run()
}

// Send 向指定会话发送消息。
func (h *Handler) Send(sessionID string, msg *Message) error {
	h.mu.RLock()
	client, ok := h.clients[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return client.Send(msg)
}

// Broadcast 向所有会话广播消息。
func (h *Handler) Broadcast(msg *Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, client := range h.clients {
		client.Send(msg)
	}
}

// addClient 添加客户端。
func (h *Handler) addClient(sessionID string, client *Client) {
	h.mu.Lock()
	h.clients[sessionID] = client
	h.mu.Unlock()
}

// removeClient 移除客户端。
func (h *Handler) removeClient(sessionID string) {
	h.mu.Lock()
	delete(h.clients, sessionID)
	h.mu.Unlock()
}

// GetClient 获取指定会话的客户端。
func (h *Handler) GetClient(sessionID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[sessionID]
}
