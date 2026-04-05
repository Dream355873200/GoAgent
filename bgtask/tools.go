package bgtask

import (
	"encoding/json"
	"fmt"
	"time"
)

// ── TaskStop 工具 ──────────────────────────────────────────────────

// TaskStopInput 是 TaskStop 工具的输入。
type TaskStopInput struct {
	TaskID string `json:"task_id" description:"要停止的后台任务 ID"`
}

// TaskStopResult 是 TaskStop 工具的输出。
type TaskStopResult struct {
	Message  string `json:"message"`
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
	Command  string `json:"command,omitempty"`
}

// ExecuteStop 执行 TaskStop 工具逻辑。
func (m *Manager) ExecuteStop(input json.RawMessage) (string, error) {
	var in TaskStopInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("解析输入失败: %w", err)
	}

	if in.TaskID == "" {
		return "", fmt.Errorf("task_id 不能为空")
	}

	t := m.Get(in.TaskID)
	if t == nil {
		return "", fmt.Errorf("任务 %s 不存在", in.TaskID)
	}

	if t.Status.IsTerminal() {
		result := TaskStopResult{
			Message:  fmt.Sprintf("任务 %s 已处于终态: %s", in.TaskID, t.Status),
			TaskID:   in.TaskID,
			TaskType: string(t.Type),
		}
		b, _ := json.Marshal(result)
		return string(b), nil
	}

	if err := m.Kill(in.TaskID); err != nil {
		return "", err
	}

	desc := t.Description
	if t.Command != "" {
		desc = t.Command
	}

	result := TaskStopResult{
		Message:  fmt.Sprintf("任务 %s 已停止", in.TaskID),
		TaskID:   in.TaskID,
		TaskType: string(t.Type),
		Command:  desc,
	}
	b, _ := json.Marshal(result)
	return string(b), nil
}

// ── TaskOutput 工具 ──────────────────────────────────────────────────

// TaskOutputInput 是 TaskOutput 工具的输入。
type TaskOutputInput struct {
	TaskID  string `json:"task_id" description:"要获取输出的后台任务 ID"`
	Block   *bool  `json:"block,omitempty" description:"是否等待任务完成。默认 true"`
	Timeout *int   `json:"timeout,omitempty" description:"最大等待时间（毫秒）。默认 30000"`
}

// TaskOutputResult 是 TaskOutput 工具的输出。
type TaskOutputResult struct {
	RetrievalStatus string          `json:"retrieval_status"` // "success", "timeout", "not_ready"
	Task            *TaskOutputInfo `json:"task"`
}

// TaskOutputInfo 是任务输出的详细信息。
type TaskOutputInfo struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Output      string `json:"output"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Error       string `json:"error,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Result      string `json:"result,omitempty"`
}

// ExecuteOutput 执行 TaskOutput 工具逻辑。
func (m *Manager) ExecuteOutput(input json.RawMessage) (string, error) {
	var in TaskOutputInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("解析输入失败: %w", err)
	}

	if in.TaskID == "" {
		return "", fmt.Errorf("task_id 不能为空")
	}

	block := true
	if in.Block != nil {
		block = *in.Block
	}
	timeoutMs := 30000
	if in.Timeout != nil {
		timeoutMs = *in.Timeout
	}

	state, output, err := m.GetOutput(in.TaskID, block, time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return "", err
	}

	// 确定 retrieval_status。
	status := "success"
	if state.Status == StatusRunning || state.Status == StatusPending {
		if block {
			status = "timeout"
		} else {
			status = "not_ready"
		}
	}

	info := &TaskOutputInfo{
		TaskID:      state.ID,
		TaskType:    string(state.Type),
		Status:      string(state.Status),
		Description: state.Description,
		Output:      output,
		ExitCode:    state.ExitCode,
		Error:       state.Error,
	}

	// Agent 特有字段。
	if state.Type == TypeAgent {
		info.Prompt = state.Prompt
		info.Result = state.Result
	}

	result := TaskOutputResult{
		RetrievalStatus: status,
		Task:            info,
	}
	b, _ := json.Marshal(result)
	return string(b), nil
}
