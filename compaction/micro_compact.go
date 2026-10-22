// Package compaction 实现四层上下文压缩系统。
package compaction

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
)

// imageMarker 内联图片标记前缀。图片结果绝不能 head+tail 截断——截断
// 会产出损坏的 base64，发给端点必 400 且会话永久卡死。压缩策略：历史
// 图片整体回收为占位符（模型产生图片的那一轮已经看过并消化了内容）。
const imageMarker = "[IMAGE"

// 常见的可压缩工具类型（对齐 Claude Code 的 COMPACTABLE_TOOLS）。
// 这些工具的结果会被 microcompact 处理。
var DefaultCompactableTools = []string{
	"Read",      // 文件读取
	"Bash",      // Shell 命令
	"Shell",     // Shell 命令（别名）
	"Grep",      // 文本搜索
	"Glob",      // 文件匹配
	"WebSearch", // 网络搜索
	"WebFetch",  // 网页获取
	"Edit",      // 文件编辑
	"Write",     // 文件写入
}

// MicrocompactConfig 配置微压缩。
type MicrocompactConfig struct {
	// MaxToolResultChars 单条工具结果的最大字符数。
	// 超过此阈值的结果会被截断，保留头尾各 10KB。
	MaxToolResultChars int
	// ProtectedTail 保护最近 N 条消息不被压缩。
	ProtectedTail int
	// CompactableTools 可压缩的工具类型列表。
	// 只有这些工具的结果会被 microcompact 处理。
	// 如果为空，使用 DefaultCompactableTools。
	CompactableTools []string
}

// DefaultMicrocompactConfig 返回默认微压缩配置。
func DefaultMicrocompactConfig() MicrocompactConfig {
	return MicrocompactConfig{
		MaxToolResultChars: 50_000,
		ProtectedTail:      4,
		CompactableTools:   DefaultCompactableTools,
	}
}

// MicrocompactResult 微压缩结果。
type MicrocompactResult struct {
	// Messages 是压缩后的消息。
	Messages []message.Message
	// TokensFreed 是释放的 token 数。
	TokensFreed int
	// NeedsFurther 是否需要进一步压缩（LLM 摘要）。
	NeedsFurther bool
}

// MicrocompactMessages 执行微压缩。
// 对齐 Claude Code 的 microcompactMessages + applyToolResultBudget。
//
// 微压缩包括：
// 1. 按大小裁剪过长的工具结果（仅 CompactableTools 中的工具）
// 2. 移除已被助手消费的工具结果内容（仅 CompactableTools 中的工具）
func MicrocompactMessages(messages []message.Message, cfg MicrocompactConfig) *MicrocompactResult {
	if cfg.MaxToolResultChars == 0 {
		cfg.MaxToolResultChars = 50_000
	}
	if cfg.ProtectedTail == 0 {
		cfg.ProtectedTail = 4
	}
	if len(cfg.CompactableTools) == 0 {
		cfg.CompactableTools = DefaultCompactableTools
	}
	// 确保工具列表已排序（二分查找需要）。
	slices.Sort(cfg.CompactableTools)

	result := make([]message.Message, len(messages))
	copy(result, messages)

	totalFreed := 0

	// Pass 1: 按大小裁剪过长的工具结果（仅指定工具类型）。
	result, freed := applyToolResultBudget(result, cfg.MaxToolResultChars, cfg.CompactableTools)
	totalFreed += freed

	// Pass 2: 移除已被助手消费的工具结果内容（仅指定工具类型）。
	result, freed = removeConsumedToolResults(result, cfg.CompactableTools)
	totalFreed += freed

	// 检查是否需要进一步压缩
	needsFurther := totalFreed > 0

	return &MicrocompactResult{
		Messages:     result,
		TokensFreed:  totalFreed,
		NeedsFurther: needsFurther,
	}
}

// applyToolResultBudget 按大小裁剪过长的工具结果。
// 只处理 CompactableTools 中的工具类型。
func applyToolResultBudget(messages []message.Message, maxChars int, compactableTools []string) ([]message.Message, int) {
	freed := 0
	result := make([]message.Message, len(messages))
	copy(result, messages)

	// 内联图片结果不能 head+tail 截断（会产出损坏 base64，发给端点必 400
	// 且会话永久卡死）：最近 imgTailMessages 条消息内的图片完整保留，更早的
	// 整体回收为占位符（见 imagePlaceholder）。
	names := toolNamesByID(messages)
	cow := cowText{}
	for i, msg := range result {
		for j, block := range msg.Content {
			// 只处理compactable工具的结果
			if block.Type == "tool_result" && isCompactableTool(names[block.ForToolUseID], compactableTools) && len(block.Text) > maxChars {
				original := len(block.Text)
				if strings.Contains(block.Text, imageMarker) {
					if i < len(result)-imgTailMessages {
						text := imagePlaceholder(block.Text)
						cow.set(result, i, j, text)
						freed += (original - len(text)) / 4
					}
					continue
				}
				// 保留头尾各 10KB
				headSize := 10_000
				if headSize > original/2 {
					headSize = original / 2
				}
				tailSize := 10_000
				if tailSize > original/2 {
					tailSize = original / 2
				}
				text := headTail(block.Text, headSize, tailSize,
					fmt.Sprintf("\n\n[... 中间 %d 字符已截断 ...]\n\n", original-headSize-tailSize))
				cow.set(result, i, j, text)
				freed += (original - len(text)) / 4
			}
		}
	}

	return result, freed
}

// isCompactableTool 检查工具是否可压缩。
func isCompactableTool(toolName string, compactableTools []string) bool {
	return slices.Contains(compactableTools, toolName)
}

// toolNamesByID 返回 tool_use ID 到工具名的映射：tool_result 块只带
// ForToolUseID，工具名要从对应的 tool_use 块取。
func toolNamesByID(messages []message.Message) map[string]string {
	names := make(map[string]string)
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.ToolUseID != "" {
				names[block.ToolUseID] = block.ToolName
			}
		}
	}
	return names
}

// removeConsumedToolResults 移除已被助手消费的工具结果内容。
// 只处理 CompactableTools 中的工具类型。
func removeConsumedToolResults(messages []message.Message, compactableTools []string) ([]message.Message, int) {
	freed := 0
	result := make([]message.Message, len(messages))
	copy(result, messages)

	// 追踪哪些 tool_use ID 已被"消费"（后续有助手响应）。
	names := toolNamesByID(messages)
	consumed := make(map[string]bool)

	// 正向扫描：找到已被助手文本跟随的工具结果。
	for i, msg := range messages {
		if msg.Role == message.RoleAssistant {
			// 标记所有前置工具结果为已消费（只记录compactable工具）。
			for j := i - 1; j >= 0; j-- {
				for _, block := range messages[j].Content {
					if block.Type == "tool_result" && isCompactableTool(names[block.ForToolUseID], compactableTools) {
						consumed[block.ForToolUseID] = true
					}
				}
				if messages[j].Role == message.RoleAssistant {
					break
				}
			}
		}
	}

	// 将已消费的工具结果替换为标记（只处理compactable工具）。
	// 图片结果跳过：它们由 Pass 1 的保留策略管理。
	cow := cowText{}
	for i, msg := range result {
		for j, block := range msg.Content {
			if block.Type == "tool_result" && consumed[block.ForToolUseID] && len(block.Text) > 100 {
				if strings.Contains(block.Text, imageMarker) {
					continue
				}
				original := len(block.Text)
				cow.set(result, i, j, fmt.Sprintf("[内容已处理，移除 %d 字符]", original))
				freed += original / 4
			}
		}
	}

	return result, freed
}
