// Package compaction — 压缩边界管理。
//
// 压缩边界标记消息历史中发生压缩的位置。边界之前的消息在构建 prompt 时被排除，
// 以保持 prompt cache 的有效性。
//
// 对齐 Claude Code 的 getMessagesAfterCompactBoundary() 和
// createCompactBoundaryMessage() 逻辑。
package compaction

import (
	"fmt"
	"time"

	"github.com/Dream355873200/GoAgent/message"
)

// CompactBoundaryIndex 返回最后一个压缩边界消息的索引。
// 如果没有找到边界则返回 -1。
func CompactBoundaryIndex(messages []message.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].IsCompactBoundary {
			return i
		}
	}
	return -1
}

// MessagesAfterCompactBoundary 仅返回最后一个压缩边界之后的消息。
// 如果不存在边界，返回所有消息。
// 通过排除压缩前的消息来保持 prompt cache 有效性。
func MessagesAfterCompactBoundary(messages []message.Message) []message.Message {
	idx := CompactBoundaryIndex(messages)
	if idx < 0 {
		return messages
	}
	// 包含边界消息本身（它包含压缩标记）。
	return messages[idx:]
}

// BoundaryMetadata 保存一次压缩事件的信息。
type BoundaryMetadata struct {
	Trigger          string    // "auto" 或 "manual"
	PreCompactTokens int       // 压缩前的 token 数
	Timestamp        time.Time // 压缩发生的时间
}

// MarkCompactBoundary 创建带元数据的压缩边界消息。
// 此消息被注入对话中，标记压缩发生的位置。
func MarkCompactBoundary(trigger string, preCompactTokens int) message.Message {
	text := fmt.Sprintf(
		"[Context compacted (%s) at %s. Pre-compact tokens: ~%d]",
		trigger,
		time.Now().Format(time.RFC3339),
		preCompactTokens,
	)
	msg := message.Message{
		Role: message.RoleSystem,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: text,
		}},
		IsCompactBoundary: true,
		Compacted:         true,
	}
	return msg
}
