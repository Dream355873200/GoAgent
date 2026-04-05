// Package task 实现 Task/Todo V2 系统。
//
// 对齐 Claude Code 的 TaskCreate/TaskUpdate/TaskList/TaskGet 工具。
// 支持依赖管理（blocks/blockedBy）、状态跟踪、自增 ID。
//
// 所有数据仅保留在内存中，随 session 结束清除。
package task

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// Status 是任务的状态。
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusDeleted    Status = "deleted"
)

// Task 表示一个任务。
type Task struct {
	// ID 是任务的唯一标识符（自增字符串）。
	ID string `json:"id"`

	// Subject 是任务的简短标题（祈使句）。
	Subject string `json:"subject"`

	// Description 是任务的详细描述。
	Description string `json:"description"`

	// Status 是任务的当前状态。
	Status Status `json:"status"`

	// Owner 是任务的负责人（agent 名称）。
	Owner string `json:"owner,omitempty"`

	// ActiveForm 是任务进行中时显示的动名词形式。
	ActiveForm string `json:"activeForm,omitempty"`

	// Metadata 是任务的自定义元数据。
	Metadata map[string]any `json:"metadata,omitempty"`

	// BlockedBy 是阻塞此任务的其他任务 ID 列表。
	BlockedBy []string `json:"blockedBy,omitempty"`

	// Blocks 是被此任务阻塞的其他任务 ID 列表。
	Blocks []string `json:"blocks,omitempty"`
}

// IsBlocked 返回任务是否被其他未完成任务阻塞。
func (t *Task) IsBlocked(store *Store) bool {
	for _, id := range t.BlockedBy {
		dep := store.getUnsafe(id)
		if dep != nil && dep.Status != StatusCompleted && dep.Status != StatusDeleted {
			return true
		}
	}
	return false
}

// Store 是任务的内存存储。
type Store struct {
	mu     sync.RWMutex
	tasks  map[string]*Task
	nextID atomic.Int64
}

// NewStore 创建一个新的任务存储。
func NewStore() *Store {
	s := &Store{
		tasks: make(map[string]*Task),
	}
	return s
}

// Create 创建一个新任务。
func (s *Store) Create(subject, description, activeForm string, metadata map[string]any) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := fmt.Sprintf("%d", s.nextID.Add(1))
	task := &Task{
		ID:          id,
		Subject:     subject,
		Description: description,
		Status:      StatusPending,
		ActiveForm:  activeForm,
		Metadata:    metadata,
	}
	s.tasks[id] = task
	return task
}

// Get 获取指定 ID 的任务。
func (s *Store) Get(id string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getUnsafe(id)
}

// getUnsafe 不加锁获取任务（内部使用）。
func (s *Store) getUnsafe(id string) *Task {
	t, ok := s.tasks[id]
	if !ok || t.Status == StatusDeleted {
		return nil
	}
	return t
}

// Update 更新任务的字段。
// 使用 JSON patch 语义：只更新传入的非零字段。
func (s *Store) Update(id string, patch UpdatePatch) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("任务 %s 不存在", id)
	}

	if patch.Subject != "" {
		t.Subject = patch.Subject
	}
	if patch.Description != "" {
		t.Description = patch.Description
	}
	if patch.ActiveForm != "" {
		t.ActiveForm = patch.ActiveForm
	}
	if patch.Owner != "" {
		t.Owner = patch.Owner
	}
	if patch.Status != "" {
		t.Status = patch.Status
	}

	// 合并 metadata。
	if patch.Metadata != nil {
		if t.Metadata == nil {
			t.Metadata = make(map[string]any)
		}
		for k, v := range patch.Metadata {
			if v == nil {
				delete(t.Metadata, k)
			} else {
				t.Metadata[k] = v
			}
		}
	}

	// 添加依赖关系。
	if len(patch.AddBlockedBy) > 0 {
		t.BlockedBy = appendUnique(t.BlockedBy, patch.AddBlockedBy...)
		// 双向链接。
		for _, depID := range patch.AddBlockedBy {
			if dep, ok := s.tasks[depID]; ok {
				dep.Blocks = appendUnique(dep.Blocks, id)
			}
		}
	}
	if len(patch.AddBlocks) > 0 {
		t.Blocks = appendUnique(t.Blocks, patch.AddBlocks...)
		// 双向链接。
		for _, depID := range patch.AddBlocks {
			if dep, ok := s.tasks[depID]; ok {
				dep.BlockedBy = appendUnique(dep.BlockedBy, id)
			}
		}
	}

	return t, nil
}

// UpdatePatch 是任务更新的参数。
type UpdatePatch struct {
	Subject      string         `json:"subject,omitempty"`
	Description  string         `json:"description,omitempty"`
	ActiveForm   string         `json:"activeForm,omitempty"`
	Owner        string         `json:"owner,omitempty"`
	Status       Status         `json:"status,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	AddBlockedBy []string       `json:"addBlockedBy,omitempty"`
	AddBlocks    []string       `json:"addBlocks,omitempty"`
}

// List 返回所有非删除状态的任务。
func (s *Store) List() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Task
	for _, t := range s.tasks {
		if t.Status != StatusDeleted {
			result = append(result, t)
		}
	}
	return result
}

// Delete 删除指定 ID 的任务。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("任务 %s 不存在", id)
	}
	t.Status = StatusDeleted
	return nil
}

// ToJSON 将任务序列化为 JSON 字符串。
func (t *Task) ToJSON() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// appendUnique 向切片追加不重复的元素。
func appendUnique(slice []string, items ...string) []string {
	existing := make(map[string]bool, len(slice))
	for _, s := range slice {
		existing[s] = true
	}
	for _, item := range items {
		if !existing[item] {
			slice = append(slice, item)
			existing[item] = true
		}
	}
	return slice
}

// OpenBlockedBy 返回任务的未完成阻塞依赖 ID 列表。
func (t *Task) OpenBlockedBy(store *Store) []string {
	var open []string
	for _, id := range t.BlockedBy {
		dep := store.Get(id)
		if dep != nil && dep.Status != StatusCompleted && dep.Status != StatusDeleted {
			open = append(open, id)
		}
	}
	return open
}

// ListSummary 返回所有任务的摘要信息。
type ListSummary struct {
	ID        string   `json:"id"`
	Subject   string   `json:"subject"`
	Status    Status   `json:"status"`
	Owner     string   `json:"owner,omitempty"`
	BlockedBy []string `json:"blockedBy,omitempty"`
}

// ListSummaries 返回所有非删除任务的摘要。
func (s *Store) ListSummaries() []ListSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ListSummary
	for _, t := range s.tasks {
		if t.Status == StatusDeleted {
			continue
		}
		// 计算未完成的阻塞依赖。
		var openBlocked []string
		for _, id := range t.BlockedBy {
			if dep, ok := s.tasks[id]; ok && dep.Status != StatusCompleted && dep.Status != StatusDeleted {
				openBlocked = append(openBlocked, id)
			}
		}
		result = append(result, ListSummary{
			ID:        t.ID,
			Subject:   t.Subject,
			Status:    t.Status,
			Owner:     t.Owner,
			BlockedBy: openBlocked,
		})
	}
	return result
}
