package agent

import (
	"encoding/json"
	"strings"
)

// activityKeys 工具入参中最能概括「在做什么」的字段，按优先级。
var activityKeys = []string{"description", "file_path", "path", "pattern", "command", "url", "query", "task", "prompt"}

// ToolActivity 把一次工具调用概括成一行活动描述（如 "Read main.go"）。
func ToolActivity(name string, input json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(input, &args) == nil {
		for _, k := range activityKeys {
			if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
				return name + " " + clip(firstLine(s), 80)
			}
		}
	}
	return name
}

// firstLine 取首个非空行。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return clip(line, 120)
		}
	}
	return ""
}

// clip 按 rune 截断，超长加省略号。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
