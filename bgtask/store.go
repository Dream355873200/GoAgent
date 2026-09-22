// Package bgtask 提供后台任务存储接口。
package bgtask

import (
	"context"
	"encoding/json"

	"github.com/Dream355873200/GoAgent/message"
)

// StoreInterface 是后台任务存储的接口。
// 实现此接口可注入自定义存储后端（如数据库、Redis 等）。
type StoreInterface interface {
	// RegisterAgent 注册一个后台 agent 任务。
	// sessionID 是创建此任务的会话（终态通知回注该会话）。
	RegisterAgent(sessionID, description, prompt, agentType, model, toolUseID string) (taskID string, ctx context.Context, cancel context.CancelFunc)

	// RegisterShell 注册一个后台 shell 任务。
	RegisterShell(sessionID, command, description string, toolUseID string) (taskID string, ctx context.Context, cancel context.CancelFunc)

	// Complete 标记任务成功完成。
	Complete(taskID, result string, messages []message.Message)

	// Fail 标记任务失败。
	Fail(taskID string, err error)

	// Kill 终止任务。
	Kill(taskID string) error

	// Get 获取任务状态。
	Get(taskID string) *TaskState

	// List 返回所有任务。
	List() []*TaskState

	// ListRunning 返回所有运行中的任务。
	ListRunning() []*TaskState

	// ExecuteStop 停止任务（工具包装）。
	ExecuteStop(input json.RawMessage) (string, error)

	// ExecuteOutput 获取任务输出（工具包装）。
	ExecuteOutput(input json.RawMessage) (string, error)
}

// Compile-time check that *Manager implements StoreInterface.
var _ StoreInterface = (*Manager)(nil)
