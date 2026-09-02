package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
)

// SessionStore 是会话持久化的抽象接口。
// 开发者可实现此接口接入 Redis、MySQL、MongoDB 等存储。
// 框架提供默认的文件系统实现（FileStore）。
//
// 示例（自定义 Redis 实现）：
//
//	type RedisStore struct { client *redis.Client }
//
//	func (s *RedisStore) Create(ctx context.Context, sess *Session) error {
//	    data, _ := json.Marshal(sess)
//	    return s.client.Set(ctx, "session:"+sess.ID, data, 24*time.Hour).Err()
//	}
//	// ... 实现其余方法
type SessionStore interface {
	// Create 创建并持久化一个新会话。
	Create(ctx context.Context, sess *Session) error

	// Get 根据 sessionID 获取会话（含消息历史）。
	// 如果会话不存在，返回 nil, nil。
	Get(ctx context.Context, sessionID string) (*Session, error)

	// List 列出所有会话摘要（不含完整消息历史）。
	List(ctx context.Context) ([]*SessionSummary, error)

	// Delete 删除指定会话。
	Delete(ctx context.Context, sessionID string) error

	// AppendMessage 向会话追加一条消息（增量写入）。
	AppendMessage(ctx context.Context, sessionID string, msg message.Message) error

	// UpdateState 更新会话状态。
	UpdateState(ctx context.Context, sessionID string, state State) error
}

// SessionSummary 是会话的简要信息（用于列表展示）。
type SessionSummary struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	TurnCount    int    `json:"turn_count"`
	CreatedAt    int64  `json:"created_at"`              // Unix 毫秒
	UpdatedAt    int64  `json:"updated_at"`              // Unix 毫秒
	FirstMessage string `json:"first_message,omitempty"` // 第一条消息摘要
}

// FileStore 是基于文件系统的 SessionStore 实现。
// 使用 JSONL 格式存储，适用于单机/开发环境。
type FileStore struct {
	storage *Storage
}

// NewFileStore 创建基于文件系统的 SessionStore。
func NewFileStore(dir string) *FileStore {
	return &FileStore{storage: NewStorage(dir)}
}

func (fs *FileStore) Create(ctx context.Context, sess *Session) error {
	// 写入元数据。
	if err := fs.storage.WriteMetadata(sess.ID, sess.Metadata); err != nil {
		return err
	}
	// 写入初始状态。
	return fs.storage.WriteState(sess.ID, sess.State)
}

func (fs *FileStore) Get(ctx context.Context, sessionID string) (*Session, error) {
	if !fs.storage.Exists(sessionID) {
		return nil, nil
	}
	return Restore(fs.storage, sessionID)
}

func (fs *FileStore) List(ctx context.Context) ([]*SessionSummary, error) {
	ids, err := fs.storage.ListSessions()
	if err != nil {
		return nil, err
	}

	summaries := make([]*SessionSummary, 0, len(ids))
	for _, id := range ids {
		sess, err := Restore(fs.storage, id)
		if err != nil {
			continue // 跳过损坏的会话
		}
		firstMsg := ""
		if len(sess.Messages) > 0 {
			text := message.ExtractText(sess.Messages[0])
			if len(text) > 80 {
				text = text[:80] + "..."
			}
			firstMsg = text
		}
		summaries = append(summaries, &SessionSummary{
			ID:           sess.ID,
			State:        sess.State.String(),
			TurnCount:    sess.TurnCount,
			CreatedAt:    sess.CreatedAt.UnixMilli(),
			UpdatedAt:    sess.UpdatedAt.UnixMilli(),
			FirstMessage: firstMsg,
		})
	}
	return summaries, nil
}

func (fs *FileStore) Delete(ctx context.Context, sessionID string) error {
	return fs.storage.Delete(sessionID)
}

func (fs *FileStore) AppendMessage(ctx context.Context, sessionID string, msg message.Message) error {
	return fs.storage.WriteMessage(sessionID, msg)
}

func (fs *FileStore) UpdateState(ctx context.Context, sessionID string, state State) error {
	return fs.storage.WriteState(sessionID, state)
}

// WriteCheckpoint 写入挂起点检查记录（Checkpointer 接口）。
func (fs *FileStore) WriteCheckpoint(ctx context.Context, sessionID string, cp Checkpoint) error {
	return fs.storage.WriteCheckpoint(sessionID, cp)
}

// ReadLastCheckpoint 读取最近一条挂起点检查记录（Checkpointer 接口）。
// 无 checkpoint 返回 nil, nil。
func (fs *FileStore) ReadLastCheckpoint(ctx context.Context, sessionID string) (*Checkpoint, error) {
	return fs.storage.ReadLastCheckpoint(sessionID)
}

// Checkpointer 是可选的挂起点检查能力（SessionStore 的扩展接口）。
// FileStore 已实现；自定义存储按需实现——未实现的 store 上调用
// Manager 的 checkpoint 方法返回 ErrCheckpointUnsupported。
type Checkpointer interface {
	WriteCheckpoint(ctx context.Context, sessionID string, cp Checkpoint) error
	ReadLastCheckpoint(ctx context.Context, sessionID string) (*Checkpoint, error)
}

// ErrCheckpointUnsupported 存储后端不支持挂起点检查。
var ErrCheckpointUnsupported = fmt.Errorf("session store 不支持 checkpoint")

// MemoryStore 是基于内存的 SessionStore 实现。
// 不做持久化，进程重启后数据丢失。
// 适用于测试和临时场景。
// 并发安全：HTTP 层多会话并行时会并发读写，全部方法持锁。
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewMemoryStore 创建基于内存的 SessionStore。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*Session)}
}

func (ms *MemoryStore) Create(ctx context.Context, sess *Session) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.sessions[sess.ID] = sess
	return nil
}

func (ms *MemoryStore) Get(ctx context.Context, sessionID string) (*Session, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	return sess, nil
}

func (ms *MemoryStore) List(ctx context.Context) ([]*SessionSummary, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	summaries := make([]*SessionSummary, 0, len(ms.sessions))
	for _, sess := range ms.sessions {
		firstMsg := ""
		if len(sess.Messages) > 0 {
			text := message.ExtractText(sess.Messages[0])
			if len(text) > 80 {
				text = text[:80] + "..."
			}
			firstMsg = text
		}
		summaries = append(summaries, &SessionSummary{
			ID:           sess.ID,
			State:        sess.State.String(),
			TurnCount:    sess.TurnCount,
			CreatedAt:    sess.CreatedAt.UnixMilli(),
			UpdatedAt:    sess.UpdatedAt.UnixMilli(),
			FirstMessage: firstMsg,
		})
	}
	return summaries, nil
}

func (ms *MemoryStore) Delete(ctx context.Context, sessionID string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	delete(ms.sessions, sessionID)
	return nil
}

func (ms *MemoryStore) AppendMessage(ctx context.Context, sessionID string, msg message.Message) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[sessionID]
	if !ok {
		return nil
	}
	sess.AddMessage(msg)
	return nil
}

func (ms *MemoryStore) UpdateState(ctx context.Context, sessionID string, state State) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[sessionID]
	if !ok {
		return nil
	}
	sess.SetState(state)
	return nil
}
