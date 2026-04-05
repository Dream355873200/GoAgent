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

	"github.com/anthropic-community/goagent/message"
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
// 如果某个 tool_use 没有对应的 tool_result，插入一个合成的错误 result。
// 这在会话恢复时很重要，因为崩溃可能导致 tool_use 没有对应的 result。
//
// 对齐 Claude Code 的 ensureToolResultPairing 逻辑。
func EnsureToolResultPairing(messages []message.Message) []message.Message {
	// 收集所有 tool_use ID。
	toolUseIDs := make(map[string]bool)
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" {
				toolUseIDs[block.ToolUseID] = true
			}
		}
	}

	// 收集所有 tool_result 对应的 ID。
	resultIDs := make(map[string]bool)
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == "tool_result" {
				resultIDs[block.ForToolUseID] = true
			}
		}
	}

	// 找出孤立的 tool_use ID。
	var orphanIDs []string
	for id := range toolUseIDs {
		if !resultIDs[id] {
			orphanIDs = append(orphanIDs, id)
		}
	}

	if len(orphanIDs) == 0 {
		return messages
	}

	// 为每个孤立的 tool_use 插入合成的 tool_result。
	result := make([]message.Message, len(messages))
	copy(result, messages)

	for _, orphanID := range orphanIDs {
		syntheticResult := message.NewToolResultMessage(
			orphanID,
			"会话恢复：此工具调用在执行前被中断",
			true,
		)
		result = append(result, syntheticResult)
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
