// Package session — 会话恢复。
//
// 从 JSONL 文件重建会话状态，包括消息历史和元数据。
// 还修复孤立的 tool_use 块（没有对应 tool_result 的情况）。
//
// 对齐 Claude Code 的会话恢复逻辑。
package session

import (
	"encoding/json"
	"fmt"

	"github.com/Dream355873200/GoAgent/message"
)

// Restore 从存储中恢复指定会话。
func Restore(storage *Storage, sessionID string) (*Session, error) {
	records, err := storage.ReadAll(sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取会话记录失败: %w", err)
	}

	session := NewSessionWithID(sessionID)
	session.StoragePath = storage.sessionPath(sessionID)

	for _, record := range records {
		switch record.Type {
		case RecordMessage:
			var msg message.Message
			if err := json.Unmarshal(record.Data, &msg); err != nil {
				return nil, fmt.Errorf("解析消息记录失败: %w", err)
			}
			session.Messages = append(session.Messages, msg)

		case RecordMetadata:
			var meta Metadata
			if err := json.Unmarshal(record.Data, &meta); err != nil {
				return nil, fmt.Errorf("解析元数据记录失败: %w", err)
			}
			session.Metadata = meta

		case RecordState:
			var stateMap map[string]string
			if err := json.Unmarshal(record.Data, &stateMap); err != nil {
				continue // 跳过无效的状态记录
			}
			switch stateMap["state"] {
			case "idle":
				session.State = StateIdle
			case "running":
				session.State = StateRunning
			case "completed":
				session.State = StateCompleted
			case "suspended":
				session.State = StateSuspended
			}

		case RecordCheckpoint:
			// 挂起点检查记录：只保留最后一条（最新执行位置才是恢复点）。
			var cp Checkpoint
			if err := json.Unmarshal(record.Data, &cp); err == nil {
				session.LastCheckpoint = &cp
			}

		case RecordBoundary:
			// 边界记录作为特殊消息处理。
			var msg message.Message
			if err := json.Unmarshal(record.Data, &msg); err == nil {
				msg.IsCompactBoundary = true
				session.Messages = append(session.Messages, msg)
			}
		}
	}

	// 修复孤立的 tool_use 块。
	session.Messages = EnsureToolResultPairing(session.Messages)

	// 更新时间。
	if len(records) > 0 {
		session.CreatedAt = records[0].Timestamp
		session.UpdatedAt = records[len(records)-1].Timestamp
	}

	return session, nil
}

// EnsureToolResultPairing 修复孤立的 tool_use 块。
// 如果某个 tool_use 没有对应的 tool_result，紧随所属 assistant 消息
// 插入一个合成的错误 result。合成结果必须紧邻 tool_calls 消息——
// OpenAI 系 API 校验 tool 消息与 tool_calls 消息的相邻性，追加到
// 历史末尾照样被拒（HTTP 400 insufficient tool messages）。
// 触发场景不止崩溃恢复：用户中断/运行异常退出时，assistant 消息
// 已提前落盘而工具结果尚未落定，孤立 tool_use 会毒化整个会话——
// 之后每次请求都 400。
//
// 对齐 Claude Code 的 ensureToolResultPairing 逻辑。
func EnsureToolResultPairing(messages []message.Message) []message.Message {
	// 收集所有 tool_use ID 与已有 tool_result 对应的 ID。
	toolUseIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" {
				toolUseIDs[block.ToolUseID] = true
			}
			if block.Type == "tool_result" {
				resultIDs[block.ForToolUseID] = true
			}
		}
	}

	// 无孤立 tool_use → 原样返回（快路径，避免每次运行都拷贝整段历史）。
	orphans := 0
	for id := range toolUseIDs {
		if !resultIDs[id] {
			orphans++
		}
	}
	if orphans == 0 {
		return messages
	}

	// 就地修复：遍历时紧跟所属 assistant 消息插入合成 tool_result，
	// 并把补过的 ID 记入 resultIDs 防止重复插入。
	result := make([]message.Message, 0, len(messages)+orphans)
	for _, msg := range messages {
		result = append(result, msg)
		var missing []string
		for _, block := range msg.Content {
			if block.Type == "tool_use" && !resultIDs[block.ToolUseID] {
				missing = append(missing, block.ToolUseID)
				resultIDs[block.ToolUseID] = true
			}
		}
		for _, id := range missing {
			result = append(result, message.NewToolResultMessage(
				id,
				"会话恢复：此工具调用在执行前被中断",
				true,
			))
		}
	}

	return result
}

// ResumeSession 恢复会话并准备继续使用。
// 将状态设置为 Running 并返回可用于继续对话的消息。
func ResumeSession(storage *Storage, sessionID string) (*Session, error) {
	session, err := Restore(storage, sessionID)
	if err != nil {
		return nil, err
	}

	session.SetState(StateRunning)

	// 写入状态变更。
	if err := storage.WriteState(sessionID, StateRunning); err != nil {
		return nil, fmt.Errorf("写入状态变更失败: %w", err)
	}

	return session, nil
}
