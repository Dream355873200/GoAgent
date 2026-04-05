// Package session 实现会话管理，包括会话状态、持久化和恢复。
//
// 会话是 agent 循环的单次交互上下文，包含消息历史、元数据和状态。
// 使用 JSONL 格式持久化，支持增量写入和完整恢复。
//
// 对齐 Claude Code 的 session 管理逻辑。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Dream355873200/GoAgent/message"
)

// State 表示会话的当前状态。
type State int

const (
	// StateIdle 会话空闲，等待用户输入。
	StateIdle State = iota
	// StateRunning 会话正在运行中（agent 循环活跃）。
	StateRunning
	// StateRequiresAction 会话等待用户操作（如权限审批）。
	StateRequiresAction
	// StateSuspended 会话已暂停（可恢复）。
	StateSuspended
	// StateCompleted 会话已完成。
	StateCompleted
)

// String 返回状态名称。
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRunning:
		return "running"
	case StateRequiresAction:
		return "requires_action"
	case StateSuspended:
		return "suspended"
	case StateCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// Metadata 包含会话的元数据。
type Metadata struct {
	// Model 是使用的模型名称。
	Model string `json:"model,omitempty"`
	// SystemPrompt 是系统提示。
	SystemPrompt string `json:"system_prompt,omitempty"`
	// WorkingDir 是工作目录。
	WorkingDir string `json:"working_dir,omitempty"`
	// Tags 是用户定义的标签。
	Tags []string `json:"tags,omitempty"`
	// Custom 是自定义键值对。
	Custom map[string]string `json:"custom,omitempty"`
}

// Session 表示一个 agent 会话。
type Session struct {
	// ID 是会话的唯一标识符。
	ID string `json:"id"`

	// State 是会话的当前状态。
	State State `json:"state"`

	// Messages 是会话的消息历史。
	Messages []message.Message `json:"-"` // 通过 JSONL 存储，不直接序列化

	// Metadata 是会话的元数据。
	Metadata Metadata `json:"metadata"`

	// CreatedAt 是会话的创建时间。
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt 是会话的最后更新时间。
	UpdatedAt time.Time `json:"updated_at"`

	// StoragePath 是会话数据的存储路径。
	StoragePath string `json:"storage_path,omitempty"`

	// TurnCount 是当前的轮次计数。
	TurnCount int `json:"turn_count"`
}

// NewSession 创建一个新会话。
func NewSession() *Session {
	id := generateID()
	now := time.Now()
	return &Session{
		ID:        id,
		State:     StateIdle,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewSessionWithID 创建一个指定 ID 的新会话。
func NewSessionWithID(id string) *Session {
	now := time.Now()
	return &Session{
		ID:        id,
		State:     StateIdle,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// AddMessage 向会话添加一条消息。
func (s *Session) AddMessage(msg message.Message) {
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = time.Now()
}

// SetState 更新会话状态。
func (s *Session) SetState(state State) {
	s.State = state
	s.UpdatedAt = time.Now()
}

// LastMessage 返回会话中的最后一条消息，如果为空返回 nil。
func (s *Session) LastMessage() *message.Message {
	if len(s.Messages) == 0 {
		return nil
	}
	return &s.Messages[len(s.Messages)-1]
}

// Summary 返回会话的简短摘要。
func (s *Session) Summary() string {
	firstMsg := ""
	if len(s.Messages) > 0 {
		text := message.ExtractText(s.Messages[0])
		if len(text) > 80 {
			text = text[:80] + "..."
		}
		firstMsg = text
	}
	return fmt.Sprintf("[%s] %s (%d 条消息, %d 轮)",
		s.ID[:8], firstMsg, len(s.Messages), s.TurnCount)
}

// generateID 生成一个随机的会话 ID。
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
