// Package plan 提供 Plan 存储接口。
package plan

// StoreInterface 是 Plan 存储的接口。
// 实现此接口可注入自定义存储后端（如数据库、Redis 等）。
type StoreInterface interface {
	// Enter 进入计划模式，返回计划文件路径。
	Enter() (string, error)

	// Exit 退出计划模式，返回最终计划文件路径。
	Exit() (string, error)

	// Cancel 取消当前计划。
	Cancel()

	// Complete 标记计划完成。
	Complete()

	// IsActive 返回计划模式是否处于激活状态。
	IsActive() bool

	// GetState 返回当前计划状态。
	GetState() State

	// FilePath 返回当前计划文件路径。
	FilePath() string

	// Content 返回当前计划内容。
	Content() string

	// WritePlan 写入计划内容。
	WritePlan(content string) error

	// ReadPlan 读取计划内容。
	ReadPlan() (string, error)

	// IsToolAllowed 检查指定权限是否在当前计划模式下允许。
	IsToolAllowed(toolPermission string) bool
}

// Compile-time check that *Manager implements StoreInterface.
var _ StoreInterface = (*Manager)(nil)
