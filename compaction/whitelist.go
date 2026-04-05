// Package compaction — 可压缩工具白名单。
//
// 并非所有工具结果都能安全压缩。此白名单定义哪些工具产生的结果
// 可以安全地进行微压缩（在模型处理后移除）。
//
// 对齐 Claude Code 的 microCompact.ts 中的 COMPACTABLE_TOOLS 集合。
package compaction

// CompactableTools 是结果可以被微压缩的工具名称集合。
// 这些工具的输出是信息性的，在模型响应后不需要持久保留在对话中。
var CompactableTools = map[string]bool{
	"Read":       true,
	"Bash":       true,
	"Grep":       true,
	"Glob":       true,
	"WebSearch":  true,
	"WebFetch":   true,
	"Agent":      true,
	"TaskOutput": true,
}

// IsCompactable 返回指定工具的结果是否可以被微压缩。
func IsCompactable(toolName string) bool {
	return CompactableTools[toolName]
}

// RegisterCompactableTool 将工具名称添加到可压缩白名单。
// 允许用户将自定义工具标记为可压缩。
func RegisterCompactableTool(name string) {
	CompactableTools[name] = true
}
