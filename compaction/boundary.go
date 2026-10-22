// Package compaction — 对话记录与模型视图。
//
// 对话记录只追加：自动摘要不改写它，而是以摘要记录的形式追加进去（对齐
// Claude Code 在会话记录中写入 compact boundary 与摘要消息的做法）。发给模型
// 的视图由 Project 从记录投影得到：最后一条摘要记录加上它覆盖范围之后的
// 消息。
package compaction

import (
	"fmt"
	"time"

	"github.com/Dream355873200/GoAgent/message"
)

// Project 从对话记录得到发给模型的视图：记录中有摘要记录（自动摘要时由
// agent 循环追加，见 message.Message.IsCompactSummary）时，视图是最后一条
// 摘要记录加上它的 CompactBoundary 起的全部消息（不含其他摘要记录）；没有
// 摘要记录时视图就是记录本身。摘要记录的边界在记录中找不到（记录被截断或
// 改写过）时放弃投影，返回去掉摘要记录的完整记录，由压缩层按需重新摘要。
//
// 返回的切片是新分配的，元素与 transcript 共享内容；transcript 不被修改。
func Project(transcript []message.Message) []message.Message {
	last := -1
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].IsCompactSummary {
			last = i
			break
		}
	}
	if last < 0 {
		return append([]message.Message(nil), transcript...)
	}
	summary := transcript[last]
	start := last + 1
	if summary.CompactBoundary != "" {
		start = -1
		for i, m := range transcript {
			if m.UID == summary.CompactBoundary {
				start = i
				break
			}
		}
		if start < 0 {
			return withoutSummaries(transcript, 0)
		}
	}
	summary.Compacted = true
	return append([]message.Message{summary}, withoutSummaries(transcript, start)...)
}

func withoutSummaries(transcript []message.Message, from int) []message.Message {
	out := make([]message.Message, 0, len(transcript)-from)
	for _, m := range transcript[from:] {
		if !m.IsCompactSummary {
			out = append(out, m)
		}
	}
	return out
}

// CompactBoundaryIndex 返回最后一个压缩边界消息的索引。
// 如果没有找到边界则返回 -1。
//
// Deprecated: 框架不产生 IsCompactBoundary 消息。对话记录到模型视图的投影
// 改用 Project。将在后续版本删除。
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
//
// Deprecated: 框架不产生 IsCompactBoundary 消息。对话记录到模型视图的投影
// 改用 Project。将在后续版本删除。
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
//
// Deprecated: 框架不再使用压缩边界消息；自动摘要以摘要记录的形式进入对话
// 记录（见 Project）。将在后续版本删除。
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
