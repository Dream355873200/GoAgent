// Package bgtask 实现后台任务框架。
//
// 对齐 Claude Code 的 src/tasks/ + src/utils/task/：
//   - LocalAgentTask: 后台运行子 agent（含进度跟踪、abort、磁盘输出）
//   - LocalShellTask: 后台运行 shell 命令
//   - TaskStop / TaskOutput 工具
//   - 状态机：pending → running → completed/failed/killed
//
// 使用方式：由 AgentTool 和 BashTool 的后台模式创建任务，
// TaskStop/TaskOutput 工具由 LLM 调用以管理这些任务。
package bgtask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropic-community/goagent/message"
)

// Status 是后台任务的状态。
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusKilled    Status = "killed"
)

// IsTerminal 返回状态是否为终态。
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusKilled
}

// TaskType 是后台任务类型。
type TaskType string

const (
	TypeAgent TaskType = "local_agent"
	TypeShell TaskType = "local_bash"
)

// TaskState 是后台任务的状态。
type TaskState struct {
	ID          string    `json:"id"`
	Type        TaskType  `json:"type"`
	Status      Status    `json:"status"`
	Description string    `json:"description"`
	ToolUseID   string    `json:"tool_use_id,omitempty"` // 创建此任务的 tool_use ID
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time,omitempty"`
	OutputFile  string    `json:"output_file"`
	Error       string    `json:"error,omitempty"`

	// Agent 任务特有字段。
	AgentID   string            `json:"agent_id,omitempty"`
	Prompt    string            `json:"prompt,omitempty"`
	AgentType string            `json:"agent_type,omitempty"`
	Model     string            `json:"model,omitempty"`
	Result    string            `json:"result,omitempty"` // 最终输出文本
	Messages  []message.Message `json:"-"`                // 完整消息列表（不序列化）

	// Shell 任务特有字段。
	Command  string `json:"command,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`

	// 进度追踪。
	ToolUseCount int `json:"tool_use_count,omitempty"`
	TokenCount   int `json:"token_count,omitempty"`

	// 内部控制。
	cancel   context.CancelFunc // 取消函数
	notified bool               // 是否已通知主循环
}

// Manager 管理所有后台任务。
type Manager struct {
	mu        sync.RWMutex
	tasks     map[string]*TaskState
	outputDir string // 输出文件目录
	idSeq     int64  // ID 序列号
}

// NewManager 创建一个新的后台任务管理器。
func NewManager(outputDir string) *Manager {
	if outputDir == "" {
		outputDir = filepath.Join(".yume", "tasks")
	}
	return &Manager{
		tasks:     make(map[string]*TaskState),
		outputDir: outputDir,
	}
}

// generateID 生成唯一任务 ID。
// 前缀：a=agent, b=bash。
func (m *Manager) generateID(taskType TaskType) string {
	m.idSeq++
	prefix := "a"
	if taskType == TypeShell {
		prefix = "b"
	}
	return fmt.Sprintf("%s%d%d", prefix, time.Now().UnixMilli()%100000, m.idSeq)
}

// RegisterAgent 注册并启动一个后台 agent 任务。
// 返回任务 ID 和取消用的 context。
func (m *Manager) RegisterAgent(description, prompt, agentType, model string, toolUseID string) (taskID string, ctx context.Context, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskID = m.generateID(TypeAgent)
	ctx, cancel = context.WithCancel(context.Background())

	outputFile := filepath.Join(m.outputDir, taskID+".output")

	state := &TaskState{
		ID:          taskID,
		Type:        TypeAgent,
		Status:      StatusRunning,
		Description: description,
		ToolUseID:   toolUseID,
		StartTime:   time.Now(),
		OutputFile:  outputFile,
		AgentID:     taskID,
		Prompt:      prompt,
		AgentType:   agentType,
		Model:       model,
		cancel:      cancel,
	}
	m.tasks[taskID] = state
	return
}

// RegisterShell 注册并启动一个后台 shell 任务。
func (m *Manager) RegisterShell(command, description, toolUseID string) (taskID string, ctx context.Context, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskID = m.generateID(TypeShell)
	ctx, cancel = context.WithCancel(context.Background())

	outputFile := filepath.Join(m.outputDir, taskID+".output")

	state := &TaskState{
		ID:          taskID,
		Type:        TypeShell,
		Status:      StatusRunning,
		Description: description,
		ToolUseID:   toolUseID,
		StartTime:   time.Now(),
		OutputFile:  outputFile,
		Command:     command,
		cancel:      cancel,
	}
	m.tasks[taskID] = state
	return
}

// Complete 标记任务为成功完成。
func (m *Manager) Complete(taskID, result string, messages []message.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[taskID]
	if !ok || t.Status.IsTerminal() {
		return
	}
	t.Status = StatusCompleted
	t.EndTime = time.Now()
	t.Result = result
	t.Messages = messages
}

// Fail 标记任务为失败。
func (m *Manager) Fail(taskID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[taskID]
	if !ok || t.Status.IsTerminal() {
		return
	}
	t.Status = StatusFailed
	t.EndTime = time.Now()
	if err != nil {
		t.Error = err.Error()
	}
}

// CompleteShell 标记 shell 任务完成并记录退出码。
func (m *Manager) CompleteShell(taskID string, exitCode int, output string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[taskID]
	if !ok || t.Status.IsTerminal() {
		return
	}
	t.ExitCode = &exitCode
	t.Result = output
	t.EndTime = time.Now()
	if exitCode == 0 {
		t.Status = StatusCompleted
	} else {
		t.Status = StatusFailed
		t.Error = fmt.Sprintf("进程退出码: %d", exitCode)
	}
}

// Kill 停止一个正在运行的任务。
func (m *Manager) Kill(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}
	if t.Status.IsTerminal() {
		return fmt.Errorf("任务 %s 已处于终态: %s", taskID, t.Status)
	}

	// 调用 cancel 发出中止信号。
	if t.cancel != nil {
		t.cancel()
	}
	t.Status = StatusKilled
	t.EndTime = time.Now()
	return nil
}

// UpdateProgress 更新任务的进度信息。
func (m *Manager) UpdateProgress(taskID string, toolUseCount, tokenCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[taskID]
	if !ok || t.Status.IsTerminal() {
		return
	}
	t.ToolUseCount = toolUseCount
	t.TokenCount = tokenCount
}

// AppendOutput 向任务的输出文件追加内容。
func (m *Manager) AppendOutput(taskID, content string) error {
	m.mu.RLock()
	t, ok := m.tasks[taskID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}

	// 确保输出目录存在。
	dir := filepath.Dir(t.OutputFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(t.OutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// Get 获取任务状态。
func (m *Manager) Get(taskID string) *TaskState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[taskID]
}

// GetOutput 获取任务输出。
// block=true 时阻塞等待完成（最多 timeout）。
func (m *Manager) GetOutput(taskID string, block bool, timeout time.Duration) (*TaskState, string, error) {
	m.mu.RLock()
	t, ok := m.tasks[taskID]
	m.mu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("任务 %s 不存在", taskID)
	}

	if !block || t.Status.IsTerminal() {
		output := readOutputFile(t.OutputFile)
		return t, output, nil
	}

	// 阻塞等待。
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		m.mu.RLock()
		t = m.tasks[taskID]
		m.mu.RUnlock()

		if t.Status.IsTerminal() || time.Now().After(deadline) {
			output := readOutputFile(t.OutputFile)
			return t, output, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// List 返回所有后台任务。
func (m *Manager) List() []*TaskState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*TaskState, 0, len(m.tasks))
	for _, t := range m.tasks {
		result = append(result, t)
	}
	return result
}

// ListRunning 返回所有正在运行的任务。
func (m *Manager) ListRunning() []*TaskState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*TaskState
	for _, t := range m.tasks {
		if t.Status == StatusRunning {
			result = append(result, t)
		}
	}
	return result
}

// KillAll 停止所有正在运行的任务（用于会话清理）。
func (m *Manager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, t := range m.tasks {
		if !t.Status.IsTerminal() && t.cancel != nil {
			t.cancel()
			t.Status = StatusKilled
			t.EndTime = time.Now()
		}
	}
}

// readOutputFile 读取输出文件（最多 8MB）。
func readOutputFile(path string) string {
	const maxSize = 8 * 1024 * 1024
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > maxSize {
		return string(data[len(data)-maxSize:])
	}
	return string(data)
}
