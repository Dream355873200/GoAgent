package tui

// 本文件实现工具调用的智能渲染。
//
// 对齐 Claude Code 的工具展示风格：
//   - Read  → 显示文件路径
//   - Write → 显示文件路径
//   - Edit  → 显示文件路径
//   - Bash  → 显示命令（截断长命令）
//   - Glob  → 显示搜索模式
//   - Grep  → 显示搜索模式 + 路径
//   - 其他  → 显示截断的 JSON 输入

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// formatToolDone 根据工具名和结果格式化工具完成的显示。
// Claude Code 风格：绿色 ● + 人类可读的一行摘要。
// 例如：● Read 1 file    ● Searched for 1 pattern    ● Bash(go vet ./...)
func formatToolDone(toolName, toolInput, toolResult string) string {
	desc := toolDoneSummary(toolName, toolInput, toolResult)
	return SuccessDotStyle.Render("●") + " " + DimStyle.Render(desc) + "\n\n"
}

// toolDoneSummary 生成 Claude Code 风格的工具完成摘要。
func toolDoneSummary(toolName, toolInput, toolResult string) string {
	detail := extractToolDetail(toolName, toolInput)

	switch toolName {
	case "Read":
		lines := strings.Count(toolResult, "\n")
		if detail != "" {
			return fmt.Sprintf("Read %s (%d 行)", detail, lines)
		}
		return fmt.Sprintf("Read 1 file (%d 行)", lines)

	case "Write":
		contentLines := countWriteLines(toolInput)
		if contentLines == 0 {
			contentLines = strings.Count(toolResult, "\n")
		}
		if detail != "" {
			return fmt.Sprintf("Write %s (%d 行)", detail, contentLines)
		}
		return fmt.Sprintf("Wrote 1 file (%d 行)", contentLines)

	case "Edit":
		if detail != "" {
			return fmt.Sprintf("Edit %s", detail)
		}
		return "Edited 1 file"

	case "Bash":
		cmd := detail
		if cmd == "" {
			cmd = "command"
		}
		output := strings.TrimSpace(toolResult)
		if output == "" {
			return fmt.Sprintf("Bash(%s) (无输出)", cmd)
		}
		// 只取第一行输出。
		if idx := strings.IndexByte(output, '\n'); idx >= 0 {
			output = output[:idx]
		}
		if len(output) > 80 {
			output = output[:80] + "..."
		}
		return fmt.Sprintf("Bash(%s)\n  ⎿  %s", cmd, output)

	case "Glob":
		files := 0
		if toolResult != "" {
			files = strings.Count(toolResult, "\n")
			if !strings.HasSuffix(toolResult, "\n") {
				files++
			}
		}
		pat := detail
		if pat == "" {
			pat = "pattern"
		}
		return fmt.Sprintf("Searched for %s, 匹配 %d 个文件", pat, files)

	case "Grep":
		lines := 0
		if toolResult != "" {
			lines = strings.Count(toolResult, "\n")
			if !strings.HasSuffix(toolResult, "\n") {
				lines++
			}
		}
		pat := detail
		if pat == "" {
			pat = "pattern"
		}
		return fmt.Sprintf("Searched for %s, %d 行匹配", pat, lines)

	case "AskUser":
		return "Asked user a question"

	case "TaskCreate":
		if detail != "" {
			return fmt.Sprintf("Created task: %s", detail)
		}
		return "Created a task"

	case "TaskUpdate":
		if detail != "" {
			return fmt.Sprintf("Updated task %s", detail)
		}
		return "Updated a task"

	default:
		if detail != "" {
			return fmt.Sprintf("%s %s", toolName, detail)
		}
		return fmt.Sprintf("Ran %s", toolName)
	}
}

// extractToolDetail 从工具输入 JSON 提取关键参数摘要。
func extractToolDetail(toolName, toolInput string) string {
	if toolInput == "" {
		return ""
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(toolInput), &raw); err != nil {
		return ""
	}

	getString := func(key string) string {
		v, ok := raw[key]
		if !ok {
			return ""
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return ""
		}
		return s
	}

	switch toolName {
	case "Read":
		fp := getString("file_path")
		if fp == "" {
			return ""
		}
		return shortPath(fp)

	case "Write":
		fp := getString("file_path")
		if fp == "" {
			return ""
		}
		return shortPath(fp)

	case "Edit":
		fp := getString("file_path")
		if fp == "" {
			return ""
		}
		return shortPath(fp)

	case "Bash":
		cmd := getString("command")
		if cmd == "" {
			return ""
		}
		// 只取第一行，截断长命令。
		if idx := strings.IndexByte(cmd, '\n'); idx >= 0 {
			cmd = cmd[:idx] + "..."
		}
		if len(cmd) > 80 {
			cmd = cmd[:80] + "..."
		}
		return cmd

	case "Glob":
		pattern := getString("pattern")
		path := getString("path")
		if pattern == "" {
			return ""
		}
		if path != "" {
			return fmt.Sprintf("%s in %s", pattern, shortPath(path))
		}
		return pattern

	case "Grep":
		pattern := getString("pattern")
		path := getString("path")
		if pattern == "" {
			return ""
		}
		if path != "" {
			return fmt.Sprintf(`"%s" in %s`, pattern, shortPath(path))
		}
		return fmt.Sprintf(`"%s"`, pattern)

	case "AskUser":
		q := getString("question")
		if q == "" {
			return ""
		}
		if len(q) > 60 {
			q = q[:60] + "..."
		}
		return q

	case "TaskCreate":
		return getString("subject")

	case "TaskUpdate":
		id := getString("taskId")
		status := getString("status")
		if id != "" && status != "" {
			return fmt.Sprintf("#%s → %s", id, status)
		}
		return ""

	default:
		// 通用：显示截断的 JSON。
		if len(toolInput) > 100 {
			return toolInput[:100] + "..."
		}
		return toolInput
	}
}

// summarizeToolResult 根据工具名生成结果摘要。
func summarizeToolResult(toolName, toolInput, toolResult string) string {
	switch toolName {
	case "Read":
		// 统计行数。
		lines := strings.Count(toolResult, "\n")
		fp := extractFilePath(toolInput)
		if fp != "" {
			return fmt.Sprintf("✓ 读取 %s (%d 行)", shortPath(fp), lines)
		}
		return fmt.Sprintf("✓ 读取完成 (%d 行)", lines)

	case "Write":
		lines := strings.Count(toolResult, "\n")
		fp := extractFilePath(toolInput)
		if fp != "" {
			// toolResult 通常是成功消息，取写入内容的行数。
			contentLines := countWriteLines(toolInput)
			if contentLines > 0 {
				lines = contentLines
			}
			return fmt.Sprintf("✓ 写入 %s (%d 行)", shortPath(fp), lines)
		}
		return truncateResult(toolResult, 120)

	case "Edit":
		fp := extractFilePath(toolInput)
		if fp != "" {
			return fmt.Sprintf("✓ 编辑 %s", shortPath(fp))
		}
		return truncateResult(toolResult, 120)

	case "Bash":
		// 显示命令输出（截断）。
		return truncateResult(toolResult, 200)

	case "Glob":
		// 统计匹配文件数。
		if toolResult == "" {
			return "✓ 无匹配文件"
		}
		files := strings.Count(toolResult, "\n")
		if !strings.HasSuffix(toolResult, "\n") && toolResult != "" {
			files++
		}
		return fmt.Sprintf("✓ 匹配 %d 个文件", files)

	case "Grep":
		if toolResult == "" {
			return "✓ 无匹配结果"
		}
		lines := strings.Count(toolResult, "\n")
		if !strings.HasSuffix(toolResult, "\n") && toolResult != "" {
			lines++
		}
		return fmt.Sprintf("✓ %d 行匹配", lines)

	default:
		return truncateResult(toolResult, 200)
	}
}

// ── 辅助函数 ────────────────────────────────────────────────────────

// shortPath 缩短文件路径，只保留最后 3 段。
func shortPath(p string) string {
	p = filepath.ToSlash(p)
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-3:], "/")
}

// extractFilePath 从工具输入 JSON 提取 file_path 字段。
func extractFilePath(toolInput string) string {
	if toolInput == "" {
		return ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(toolInput), &raw); err != nil {
		return ""
	}
	v, ok := raw["file_path"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// countWriteLines 从 Write 工具的输入 JSON 中统计 content 字段的行数。
func countWriteLines(toolInput string) int {
	if toolInput == "" {
		return 0
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(toolInput), &raw); err != nil {
		return 0
	}
	v, ok := raw["content"]
	if !ok {
		return 0
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return 0
	}
	lines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") && s != "" {
		lines++
	}
	return lines
}

// truncateResult 截断结果文本。
func truncateResult(s string, maxLen int) string {
	if s == "" {
		return ""
	}
	// 只取前几行。
	lines := strings.SplitN(s, "\n", 6)
	if len(lines) > 5 {
		lines = lines[:5]
		s = strings.Join(lines, "\n") + "\n..."
	}
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// extractAgentDesc 从 Agent_ 工具名和输入 JSON 中提取子 agent 描述。
// 工具名格式：Agent_<name>，输入包含 {"prompt": "..."}。
func extractAgentDesc(toolName, toolInput string) string {
	name := strings.TrimPrefix(toolName, "Agent_")

	// 尝试从输入 JSON 提取 prompt 的前几个词作为描述。
	if toolInput != "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(toolInput), &raw); err == nil {
			if v, ok := raw["prompt"]; ok {
				var prompt string
				if err := json.Unmarshal(v, &prompt); err == nil && prompt != "" {
					// 取前 60 字符作为描述。
					desc := prompt
					if len(desc) > 60 {
						desc = desc[:60] + "…"
					}
					return name + " " + desc
				}
			}
			// 尝试 description 字段。
			if v, ok := raw["description"]; ok {
				var desc string
				if err := json.Unmarshal(v, &desc); err == nil && desc != "" {
					return name + " " + desc
				}
			}
		}
	}

	return name
}
