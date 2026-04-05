package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
)

// Manager 是面向开发者的会话管理器。
// 封装 SessionStore，提供创建/恢复/列出/删除会话的高级 API。
//
// 使用方式：
//
//	// 1. 选择存储后端
//	store := session.NewMemoryStore()       // 内存（测试用）
//	store := session.NewFileStore("./data") // 文件系统
//	store := myRedisStore{}                 // 自定义 Redis
//
//	// 2. 创建 Manager
//	mgr := session.NewManager(store)
//
//	// 3. 注册到 App
//	app := goagent.New(goagent.WithSessionManager(mgr))
//
//	// 4. 在 Gin 路由中使用
//	r.POST("/chat", func(c *gin.Context) {
//	    sessionID := c.Query("session_id")
//	    for ev := range app.RunSession(ctx, sessionID, msg) { ... }
//	})
type Manager struct {
	store SessionStore
	mu    sync.RWMutex
	// active 追踪当前正在运行的会话，防止同一会话并发执行。
	active map[string]bool
}

// NewManager 创建会话管理器。
func NewManager(store SessionStore) *Manager {
	return &Manager{
		store:  store,
		active: make(map[string]bool),
	}
}

// Create 创建一个新会话，返回 sessionID。
// metadata 可以传入自定义键值对（可选）。
func (m *Manager) Create(ctx context.Context, metadata map[string]string) (string, error) {
	sess := NewSession()
	if metadata != nil {
		sess.Metadata.Custom = metadata
	}
	if err := m.store.Create(ctx, sess); err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}
	return sess.ID, nil
}

// Get 获取会话信息（含消息历史）。
// 如果会话不存在返回 nil, nil。
func (m *Manager) Get(ctx context.Context, sessionID string) (*Session, error) {
	return m.store.Get(ctx, sessionID)
}

// List 列出所有会话摘要。
func (m *Manager) List(ctx context.Context) ([]*SessionSummary, error) {
	return m.store.List(ctx)
}

// Delete 删除指定会话。
func (m *Manager) Delete(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	if m.active[sessionID] {
		m.mu.Unlock()
		return fmt.Errorf("会话 %s 正在运行中，无法删除", sessionID)
	}
	m.mu.Unlock()
	return m.store.Delete(ctx, sessionID)
}

// Acquire 标记会话为活跃状态。
// 防止同一会话被并发执行。如果已被占用返回错误。
// 内部使用，由 App.RunSession 调用。
func (m *Manager) Acquire(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[sessionID] {
		return fmt.Errorf("会话 %s 正在运行中", sessionID)
	}
	m.active[sessionID] = true
	return nil
}

// Release 释放会话的活跃状态。
// 内部使用，由 App.RunSession 在结束时调用。
func (m *Manager) Release(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, sessionID)
}

// GetOrCreate 获取会话，如果不存在则创建。
// 适用于前端不区分"新建"和"继续"的场景。
func (m *Manager) GetOrCreate(ctx context.Context, sessionID string, metadata map[string]string) (*Session, error) {
	sess, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess, nil
	}

	// 不存在，创建新会话（使用传入的 ID）。
	sess = NewSessionWithID(sessionID)
	if metadata != nil {
		sess.Metadata.Custom = metadata
	}
	if err := m.store.Create(ctx, sess); err != nil {
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}
	return sess, nil
}

// AppendMessage 向会话追加消息。
// 内部使用，由 agent loop 在每轮结束时调用。
func (m *Manager) AppendMessage(ctx context.Context, sessionID string, msg message.Message) error {
	return m.store.AppendMessage(ctx, sessionID, msg)
}

// UpdateState 更新会话状态。
func (m *Manager) UpdateState(ctx context.Context, sessionID string, state State) error {
	return m.store.UpdateState(ctx, sessionID, state)
}

// Store 返回底层 SessionStore（供高级用户使用）。
func (m *Manager) Store() SessionStore {
	return m.store
}
