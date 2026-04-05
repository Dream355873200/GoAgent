// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/sessionmem"
)

// SessionMemoryConfig 配置 SessionMemory 压缩。
type SessionMemoryConfig struct {
	// MemoryDir 是会话记忆文件存放目录。
	MemoryDir string
	// MinTokens 保留消息的最小 token 数。
	MinTokens int
	// MinTextBlockMessages 保留消息的最小文本消息数。
	MinTextBlockMessages int
	// MaxTokens 保留消息的最大 token 数（硬限制）。
	MaxTokens int
	// Provider 用于生成摘要的 LLM 提供者。
	Provider interface {
		Complete(ctx context.Context, req interface {
			GetMessages() []message.Message
			GetSystemPrompt() string
			GetMaxTokens() int
		}) (interface{ GetMessage() message.Message }, error)
	}
	// SessionMemory 是会话记忆管理器（可选，用于获取记忆内容）。
	SessionMemory *sessionmem.SessionMemory
}

// DefaultSessionMemoryConfig 返回默认配置。
func DefaultSessionMemoryConfig() SessionMemoryConfig {
	return SessionMemoryConfig{
		MinTokens:            10_000,
		MinTextBlockMessages: 5,
		MaxTokens:            40_000,
	}
}

// smCompactState 追踪 SessionMemory 压缩状态。
// 用于在多次压缩之间保持状态。
var smCompactState = struct {
	lastSummarizedIndex int
	initialized         bool
}{
	lastSummarizedIndex: -1,
	initialized:         false,
}

// CompactionResult 压缩结果。
// 对齐 Claude Code 的 CompactionResult。
type CompactionResult struct {
	// Messages 是压缩后的消息。
	Messages []message.Message
	// Summary 是生成的摘要。
	Summary string
	// TokensFreed 是释放的 token 数。
	TokensFreed int
	// SessionMemoryUsed 是否使用了 SessionMemory 压缩。
	SessionMemoryUsed bool
	// PreCompactTokens 压缩前的 token 数。
	PreCompactTokens int
	// PostCompactTokens 压缩后的 token 数。
	PostCompactTokens int
	// BoundaryMarkerUsed 是否使用了边界标记。
	BoundaryMarkerUsed bool
	// UserDisplayMessage 用户显示的消息（来自 hooks）。
	UserDisplayMessage string
}

// ResetSMCompactState 重置 SessionMemory 压缩状态（用于测试）。
func ResetSMCompactState() {
	smCompactState.lastSummarizedIndex = -1
	smCompactState.initialized = false
}

// GetLastSummarizedIndex 返回上次压缩后保留消息的起始索引。
func GetLastSummarizedIndex() int {
	return smCompactState.lastSummarizedIndex
}

// TrySessionMemoryCompaction 尝试使用 SessionMemory 机制压缩。
// 如果成功返回结果，失败返回 nil。
// 对齐 Claude Code 的 trySessionMemoryCompaction。
//
// 流程：
// 1. 检查 SessionMemory 是否启用且有内容
// 2. 计算保留消息的起始索引（从 lastSummarizedIndex 开始，满足最小/最大约束）
// 3. 格式化 session memory 内容作为摘要消息
// 4. 构建压缩结果
func TrySessionMemoryCompaction(ctx context.Context, messages []message.Message, cfg SessionMemoryConfig, threshold int) *CompactionResult {
	// 检查是否配置了 session memory
	if cfg.SessionMemory == nil {
		return nil
	}

	// 等待任何进行中的提取完成
	cfg.SessionMemory.WaitForExtraction(0)

	// 获取 session memory 内容
	sessionMemory := cfg.SessionMemory.LoadContent()
	if sessionMemory == "" {
		return nil
	}

	// 检查 session memory 是否为空（只有模板）
	if cfg.SessionMemory.IsEmpty() {
		return nil
	}

	// 计算保留消息的起始索引
	startIndex := calculateMessagesToKeepIndex(messages, cfg)

	// 如果起始索引 >= 消息长度，说明没有消息可保留
	if startIndex >= len(messages) {
		return nil
	}

	// 截断超长 section
	maxSectionChars := 2000 * 4 // ~2000 tokens
	truncatedContent, wasTruncated := sessionmem.TruncateForCompact(sessionMemory, maxSectionChars)

	// 构建摘要消息
	summaryContent := buildSMCompactSummaryMessage(truncatedContent, wasTruncated, cfg.MemoryDir)
	summaryMsg := message.Message{
		Role: message.RoleUser,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: summaryContent,
		}},
		Compacted:        true,
		IsCompactSummary: true,
	}

	// 保留 startIndex 之后的消息
	messagesToKeep := make([]message.Message, len(messages)-startIndex)
	copy(messagesToKeep, messages[startIndex:])

	// 计算 token
	preCompactTokens := estimateMessagesTokens(messages)
	postCompactTokens := estimateMessagesTokens(messagesToKeep) + message.EstimateTokens(summaryMsg)

	// 检查阈值（如果提供了）
	if threshold > 0 && postCompactTokens >= threshold {
		return nil
	}

	// 更新状态
	smCompactState.lastSummarizedIndex = len(messagesToKeep) - 1
	smCompactState.initialized = true

	// 构建结果
	result := &CompactionResult{
		Messages:           append([]message.Message{summaryMsg}, messagesToKeep...),
		Summary:            summaryContent,
		TokensFreed:        preCompactTokens - postCompactTokens,
		SessionMemoryUsed:  true,
		PreCompactTokens:   preCompactTokens,
		PostCompactTokens:  postCompactTokens,
		BoundaryMarkerUsed: true,
	}

	return result
}

// calculateMessagesToKeepIndex 计算保留消息的起始索引。
// 对齐 Claude Code 的 calculateMessagesToKeepIndex。
//
// 从 lastSummarizedIndex 开始，尝试保留满足以下条件的最少消息：
// - 至少 MinTokens 个 token
// - 至少 MinTextBlockMessages 条包含文本的消息
// - 不超过 MaxTokens 个 token
func calculateMessagesToKeepIndex(messages []message.Message, cfg SessionMemoryConfig) int {
	if len(messages) == 0 {
		return 0
	}

	// 确保有默认值
	minTokens := cfg.MinTokens
	if minTokens == 0 {
		minTokens = 10_000
	}
	minTextBlockMessages := cfg.MinTextBlockMessages
	if minTextBlockMessages == 0 {
		minTextBlockMessages = 5
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 40_000
	}

	// 确定起始索引
	var startIndex int
	if smCompactState.lastSummarizedIndex >= 0 && smCompactState.lastSummarizedIndex < len(messages)-1 {
		startIndex = smCompactState.lastSummarizedIndex + 1
	} else {
		// 如果没有之前的压缩记录，从头开始
		startIndex = 0
	}

	// 计算从 startIndex 到结尾的 token 数和文本消息数
	totalTokens := 0
	textBlockCount := 0
	for i := startIndex; i < len(messages); i++ {
		totalTokens += message.EstimateTokens(messages[i])
		if hasTextBlocks(messages[i]) {
			textBlockCount++
		}
	}

	// 如果已经达到最小要求且不超过最大限制，直接返回
	if totalTokens >= minTokens && textBlockCount >= minTextBlockMessages {
		if totalTokens <= maxTokens {
			return adjustIndexToPreserveAPIInvariants(messages, startIndex)
		}
	}

	// 向前扩展直到满足最小要求或达到最大限制
	// 找到最后一个 boundary marker，作为扩展的下限
	floor := findLastBoundaryIndex(messages)
	if floor == -1 {
		floor = 0
	} else {
		floor++ // 从 boundary 之后开始
	}

	for i := startIndex - 1; i >= floor; i-- {
		totalTokens += message.EstimateTokens(messages[i])
		if hasTextBlocks(messages[i]) {
			textBlockCount++
		}
		startIndex = i

		if totalTokens >= minTokens && textBlockCount >= minTextBlockMessages {
			break
		}
		if totalTokens >= maxTokens {
			break
		}
	}

	return adjustIndexToPreserveAPIInvariants(messages, startIndex)
}

// hasTextBlocks 检查消息是否包含文本块。
func hasTextBlocks(msg message.Message) bool {
	for _, block := range msg.Content {
		if block.Type == "text" && block.Text != "" {
			return true
		}
	}
	return false
}

// findLastBoundaryIndex 找到最后一个 compact boundary marker 的索引。
func findLastBoundaryIndex(messages []message.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, block := range messages[i].Content {
			if block.Type == "text" {
				if strings.Contains(block.Text, "[对话已压缩]") ||
					strings.Contains(block.Text, "[Compacted]") {
					return i
				}
			}
		}
	}
	return -1
}

// adjustIndexToPreserveAPIInvariants 调整索引以保证 API 调用不出错。
// 对齐 Claude Code 的 adjustIndexToPreserveAPIInvariants。
//
// 确保不切断 tool_use/tool_result 对。
func adjustIndexToPreserveAPIInvariants(messages []message.Message, startIndex int) int {
	if startIndex <= 0 || startIndex >= len(messages) {
		return startIndex
	}

	// 收集 startIndex 之后的所有 tool_result ID
	toolResultIDs := make(map[string]bool)
	for i := startIndex; i < len(messages); i++ {
		for _, block := range messages[i].Content {
			if block.Type == "tool_result" {
				toolResultIDs[block.ForToolUseID] = true
			}
		}
	}

	if len(toolResultIDs) == 0 {
		return startIndex
	}

	// 向前查找缺少的 tool_use
	adjusted := startIndex
	for i := startIndex - 1; i >= 0; i-- {
		for _, block := range messages[i].Content {
			if block.Type == "tool_use" && toolResultIDs[block.ToolUseID] {
				adjusted = i
				delete(toolResultIDs, block.ToolUseID)
				if len(toolResultIDs) == 0 {
					return adjusted
				}
			}
		}
	}

	return adjusted
}

// buildSMCompactSummaryMessage 构建 SessionMemory 压缩的摘要消息。
func buildSMCompactSummaryMessage(content string, wasTruncated bool, memoryPath string) string {
	var sb strings.Builder
	sb.WriteString("[对话已压缩 - Session Memory]\n\n")
	sb.WriteString(content)

	if wasTruncated && memoryPath != "" {
		sb.WriteString("\n\n")
		sb.WriteString(fmt.Sprintf("注意：部分会话记忆因长度限制被截断。完整内容可查看: %s/MEMORY.md", memoryPath))
	}

	return sb.String()
}
