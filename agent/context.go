// Package agent — 子 agent 上下文管理。
//
// AgentContext 为子 agent 提供隔离的上下文，
// 包括消息历史和工具状态的独立管理。
package agent

import (
	"context"
	"sync"

	"github.com/anthropic-community/goagent/message"
)

// AgentContext 是子 agent 的运行时上下文。
type AgentContext struct {
	// ctx 是 Go 标准上下文。
	ctx context.Context

	// Definition 是子 agent 的定义。
	Definition Definition

	// ParentSessionID 是父 agent 的会话 ID。
	ParentSessionID string

	mu       sync.Mutex
	messages []message.Message
	metadata map[string]any
}

// NewAgentContext 创建一个新的子 agent 上下文。
func NewAgentContext(ctx context.Context, def Definition, parentSessionID string) *AgentContext {
	return &AgentContext{
		ctx:             ctx,
		Definition:      def,
		ParentSessionID: parentSessionID,
		metadata:        make(map[string]any),
	}
}

// Context 返回底层的 Go 标准上下文。
func (ac *AgentContext) Context() context.Context {
	return ac.ctx
}

// AddMessage 向子 agent 添加一条消息。
func (ac *AgentContext) AddMessage(msg message.Message) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.messages = append(ac.messages, msg)
}

// Messages 返回当前的消息历史。
func (ac *AgentContext) Messages() []message.Message {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	result := make([]message.Message, len(ac.messages))
	copy(result, ac.messages)
	return result
}

// SetMetadata 设置元数据。
func (ac *AgentContext) SetMetadata(key string, value any) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.metadata[key] = value
}

// GetMetadata 获取元数据。
func (ac *AgentContext) GetMetadata(key string) (any, bool) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	v, ok := ac.metadata[key]
	return v, ok
}
