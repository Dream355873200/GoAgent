// Package permission — 工具调用模式匹配器。
//
// 支持 glob 和前缀匹配工具名称和输入内容。
//
// 模式格式：
//   - "Bash"           — 精确匹配工具名
//   - "Bash(git *)"    — 匹配工具名 + 输入前缀
//   - "*"              — 匹配所有工具
//   - "Read*"          — 前缀匹配
//
// 对齐 Claude Code 的 ParseToolPattern / MatchToolCall 逻辑。
package permission

import "strings"

// ToolPattern 解析后的工具模式。
type ToolPattern struct {
	// Name 是工具名称模式。
	Name string
	// InputPrefix 是可选的输入前缀模式（从 "Bash(git *)" 提取 "git "）。
	InputPrefix string
	// IsWildcard 标识名称模式是否为通配符 "*"。
	IsWildcard bool
}

// ParseToolPattern 解析工具模式字符串。
//
// 示例：
//
//	"Bash"         → ToolPattern{Name: "Bash"}
//	"Bash(git *)"  → ToolPattern{Name: "Bash", InputPrefix: "git "}
//	"*"            → ToolPattern{Name: "*", IsWildcard: true}
//	"Read*"        → ToolPattern{Name: "Read", IsWildcard: true}  // 前缀通配
func ParseToolPattern(pattern string) ToolPattern {
	tp := ToolPattern{}

	// 检查是否有括号中的输入模式。
	if idx := strings.Index(pattern, "("); idx >= 0 {
		tp.Name = pattern[:idx]
		// 提取括号内容并移除尾部的 ")" 和 "*"。
		inner := pattern[idx+1:]
		inner = strings.TrimSuffix(inner, ")")
		inner = strings.TrimSuffix(inner, "*")
		inner = strings.TrimSpace(inner)
		tp.InputPrefix = inner
		return tp
	}

	// 检查通配符。
	if pattern == "*" {
		tp.IsWildcard = true
		tp.Name = "*"
		return tp
	}

	// 检查前缀通配符（如 "Read*"）。
	if strings.HasSuffix(pattern, "*") {
		tp.Name = strings.TrimSuffix(pattern, "*")
		tp.IsWildcard = true
		return tp
	}

	tp.Name = pattern
	return tp
}

// MatchToolCall 检查工具调用是否匹配模式。
func MatchToolCall(pattern ToolPattern, toolName, input string) bool {
	// 匹配工具名称。
	if pattern.IsWildcard {
		if pattern.Name != "*" {
			// 前缀匹配。
			if !strings.HasPrefix(toolName, pattern.Name) {
				return false
			}
		}
		// "*" 匹配所有。
	} else {
		if toolName != pattern.Name {
			return false
		}
	}

	// 匹配输入前缀。
	if pattern.InputPrefix != "" {
		if !strings.Contains(input, pattern.InputPrefix) {
			return false
		}
	}

	return true
}

// matchToolCall 是内部便捷函数，解析模式并匹配。
func matchToolCall(toolPattern, inputPattern, toolName, input string) bool {
	// 匹配工具名称。
	tp := ParseToolPattern(toolPattern)
	if !MatchToolCall(tp, toolName, input) {
		return false
	}

	// 额外的输入模式匹配（独立于工具模式中的括号）。
	if inputPattern != "" {
		if !strings.Contains(input, inputPattern) {
			return false
		}
	}

	return true
}
