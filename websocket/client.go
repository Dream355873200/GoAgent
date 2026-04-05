// Package websocket 实现 WebSocket 支持。
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gorilla/websocket"
)

// ErrNotConnected 未连接错误。
var ErrNotConnected = errors.New("not connected")

// Client WebSocket 客户端。
type Client struct {
	sessionID string
	conn      *websocket.Conn
	config    WSConfig
	sendCh    chan *Message
	doneCh    chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewClient 创建 WebSocket 客户端。
func NewClient(sessionID string, conn *websocket.Conn, config WSConfig) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		sessionID: sessionID,
		conn:      conn,
		config:    config,
		sendCh:    make(chan *Message, 256),
		doneCh:    make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Run 启动客户端处理循环。
func (c *Client) Run() {
	go c.readLoop()
	go c.writeLoop()
	go c.pingLoop()
}

// readLoop 读取循环。
func (c *Client) readLoop() {
	defer close(c.doneCh)
	defer c.conn.Close()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			c.sendError("invalid message format")
			continue
		}

		c.handleMessage(&msg)
	}
}

// writeLoop 写入循环。
func (c *Client) writeLoop() {
	for {
		select {
		case msg := <-c.sendCh:
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-c.doneCh:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

// pingLoop 心跳循环。
func (c *Client) pingLoop() {
	ticker := time.NewTicker(c.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.config.PongTimeout)); err != nil {
				return
			}
		case <-c.doneCh:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

// handleMessage 处理收到的消息。
func (c *Client) handleMessage(msg *Message) {
	switch msg.Type {
	case TypePing:
		c.send(&Message{
			Type:      TypePong,
			Timestamp: time.Now(),
		})
	case TypeApprove:
		c.handleApprove(msg)
	case TypeDeny:
		c.handleDeny(msg)
	case TypeInterrupt:
		c.handleInterrupt(msg)
	case TypeResume:
		c.handleResume(msg)
	}
}

// handleApprove 处理权限审批。
func (c *Client) handleApprove(msg *Message) {
	var data struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		c.sendError("invalid approve data")
		return
	}
	// 通知 App 处理
	c.send(&Message{
		Type:      TypeEvent,
		SessionID: c.sessionID,
		Data:      json.RawMessage(`{"action":"approved","request_id":"` + data.RequestID + `"}`),
		Timestamp: time.Now(),
	})
}

// handleDeny 处理权限拒绝。
func (c *Client) handleDeny(msg *Message) {
	var data struct {
		RequestID string `json:"request_id"`
		Reason    string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		c.sendError("invalid deny data")
		return
	}
	c.send(&Message{
		Type:      TypeEvent,
		SessionID: c.sessionID,
		Data:      json.RawMessage(`{"action":"denied","request_id":"` + data.RequestID + `"}`),
		Timestamp: time.Now(),
	})
}

// handleInterrupt 处理中断指令。
func (c *Client) handleInterrupt(msg *Message) {
	c.send(&Message{
		Type:      TypeEvent,
		SessionID: c.sessionID,
		Data:      json.RawMessage(`{"action":"interrupted"}`),
		Timestamp: time.Now(),
	})
}

// handleResume 处理恢复指令。
func (c *Client) handleResume(msg *Message) {
	var data struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		c.sendError("invalid resume data")
		return
	}
	c.send(&Message{
		Type:      TypeEvent,
		SessionID: c.sessionID,
		Data:      json.RawMessage(`{"action":"resumed"}`),
		Timestamp: time.Now(),
	})
}

// Send 发送消息。
func (c *Client) Send(msg *Message) error {
	select {
	case c.sendCh <- msg:
		return nil
	case <-c.doneCh:
		return ErrNotConnected
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// send 内部发送方法。
func (c *Client) send(msg *Message) {
	select {
	case c.sendCh <- msg:
	case <-c.doneCh:
	case <-c.ctx.Done():
	}
}

// sendError 发送错误消息。
func (c *Client) sendError(errMsg string) {
	c.send(&Message{
		Type:      TypeError,
		SessionID: c.sessionID,
		Data:      json.RawMessage(`{"error":"` + errMsg + `"}`),
		Timestamp: time.Now(),
	})
}

// Close 关闭客户端。
func (c *Client) Close() error {
	c.cancel()
	close(c.sendCh)
	return c.conn.Close()
}

// SessionID 返回会话 ID。
func (c *Client) SessionID() string {
	return c.sessionID
}

// IsConnected 返回是否已连接。
func (c *Client) IsConnected() bool {
	select {
	case <-c.doneCh:
		return false
	default:
		return true
	}
}
