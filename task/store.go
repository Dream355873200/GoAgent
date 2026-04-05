// Package task 提供 Task 存储接口。
package task

// StoreInterface 是 Task 存储的接口。
// 实现此接口可注入自定义存储后端（如数据库、Redis 等）。
type StoreInterface interface {
	// Create 创建一个新任务。
	Create(subject, description, activeForm string, metadata map[string]any) *Task

	// Get 根据 ID 获取任务。
	Get(id string) *Task

	// Update 更新任务的字段。
	Update(id string, patch UpdatePatch) (*Task, error)

	// List 返回所有任务。
	List() []*Task

	// Delete 删除任务。
	Delete(id string) error

	// ListSummaries 返回所有任务的摘要列表（不含 metadata）。
	ListSummaries() []ListSummary
}

// Compile-time check that *Store implements StoreInterface.
var _ StoreInterface = (*Store)(nil)
