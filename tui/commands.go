package tui

// 本文件实现 TUI 层的斜杠命令处理。
//
// userinput.Processor 处理基本命令（/help, /clear, /compact, /status）。
// 以下命令需要 app 上下文，由 TUI 层补充处理：
//   - /tools  — 列出已注册工具
//   - /cost   — 显示 token 用量
//   - /tasks  — 列出任务
//   - /bg     — 列出后台任务
//   - /exit   — 退出 TUI

import (
	"context"
	"fmt"
	"strings"
)

// AppInfo 是 TUI 命令处理器对 App 的最小接口。
// 避免 tui 包直接 import goagent（循环依赖）。
type AppInfo interface {
	// ToolNames 返回已注册工具名称列表。
	ToolNames() []string
	// ToolDescription 返回工具描述。
	ToolDescription(name string) string
	// TaskSummaryLines 返回任务摘要的格式化文本行。
	TaskSummaryLines() []string
	// BgTaskLines 返回后台任务摘要的格式化文本行。
	BgTaskLines() []string
	// ModelID 返回当前使用的模型 ID。
	ModelID() string
	// SetModel 动态切换当前使用的模型。返回 false 表示不支持切换。
	SetModel(modelID string) bool
}

// UsageInfo 是 token 用量信息。
type UsageInfo struct {
	InputTokens  int
	OutputTokens int
}

// commandHandler 是 TUI 层命令处理器。
type commandHandler struct {
	app     AppInfo
	usage   *UsageInfo
	sessLst SessionLister // 可选，支持 /sessions 和 /resume
}

// newCommandHandler 创建命令处理器。
func newCommandHandler(app AppInfo, usage *UsageInfo, sessLst SessionLister) *commandHandler {
	return &commandHandler{app: app, usage: usage, sessLst: sessLst}
}

// Handle 处理 TUI 层命令。
// 返回 (output, handled)。作为 userinput.BuiltinCommandHandler 注册。
func (h *commandHandler) Handle(_ context.Context, command, args string) (string, bool) {
	switch command {
	case "tools":
		return h.handleTools(), true
	case "cost":
		return h.handleCost(), true
	case "tasks":
		return h.handleTasks(), true
	case "bg":
		return h.handleBgTasks(), true
	case "model":
		return h.handleModel(args), true
	case "sessions":
		return h.handleSessions(), true
	case "resume":
		return h.handleResume(args), true
	case "exit", "quit":
		return "[退出]", true
	default:
		return "", false
	}
}

// handleTools 列出已注册的工具。
func (h *commandHandler) handleTools() string {
	names := h.app.ToolNames()
	if len(names) == 0 {
		return "（无已注册工具）"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("已注册 %d 个工具:\n", len(names)))
	for i, name := range names {
		sb.WriteString(fmt.Sprintf("  %d. %s", i+1, name))
		if desc := h.app.ToolDescription(name); desc != "" {
			if len(desc) > 60 {
				desc = desc[:60] + "..."
			}
			sb.WriteString(" — " + desc)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// handleCost 显示 token 用量统计。
func (h *commandHandler) handleCost() string {
	if h.usage == nil || (h.usage.InputTokens == 0 && h.usage.OutputTokens == 0) {
		return "token 用量: 暂无数据"
	}
	return fmt.Sprintf("token 用量:\n  输入: %d tokens\n  输出: %d tokens\n  合计: %d tokens",
		h.usage.InputTokens,
		h.usage.OutputTokens,
		h.usage.InputTokens+h.usage.OutputTokens)
}

// handleTasks 列出任务。
func (h *commandHandler) handleTasks() string {
	lines := h.app.TaskSummaryLines()
	if len(lines) == 0 {
		return "（无任务）"
	}
	return strings.Join(lines, "\n")
}

// handleBgTasks 列出后台任务。
func (h *commandHandler) handleBgTasks() string {
	lines := h.app.BgTaskLines()
	if len(lines) == 0 {
		return "（无后台任务）"
	}
	return strings.Join(lines, "\n")
}

// handleModel 处理 /model 命令：查看或切换模型。
func (h *commandHandler) handleModel(args string) string {
	if args == "" {
		current := h.app.ModelID()
		if current == "" {
			return "当前模型: （未设置）\n用法: /model <model_name>"
		}
		return fmt.Sprintf("当前模型: %s\n用法: /model <model_name>", current)
	}
	if h.app.SetModel(args) {
		return fmt.Sprintf("模型已切换为: %s", args)
	}
	return "当前 provider 不支持运行时切换模型"
}

// handleSessions 列出所有可恢复的会话。
func (h *commandHandler) handleSessions() string {
	if h.sessLst == nil {
		return "会话持久化未启用"
	}
	sessions := h.sessLst.ListSessions()
	if len(sessions) == 0 {
		return "（无历史会话）"
	}

	currentID := h.sessLst.CurrentSessionID()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("共 %d 个会话:\n", len(sessions)))
	for i, s := range sessions {
		marker := "  "
		if s.ID == currentID {
			marker = "▸ "
		}
		idShort := s.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		msg := s.FirstMessage
		if msg == "" {
			msg = "（空）"
		}
		sb.WriteString(fmt.Sprintf("%s%d. [%s] %s (%d 轮) — %s\n",
			marker, i+1, idShort, s.State, s.TurnCount, msg))
	}
	sb.WriteString("\n用法: /resume <session_id_前缀> 恢复会话")
	return sb.String()
}

// handleResume 恢复指定 ID 的会话。
func (h *commandHandler) handleResume(args string) string {
	if h.sessLst == nil {
		return "会话持久化未启用"
	}
	if args == "" {
		return "用法: /resume <session_id>\n使用 /sessions 查看可恢复的会话"
	}

	// 支持前缀匹配。
	sessions := h.sessLst.ListSessions()
	var match SessionInfo
	var matchCount int
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, args) {
			match = s
			matchCount++
		}
	}
	if matchCount == 0 {
		return fmt.Sprintf("未找到 ID 以 %q 开头的会话", args)
	}
	if matchCount > 1 {
		return fmt.Sprintf("ID 前缀 %q 匹配了 %d 个会话，请提供更长的前缀", args, matchCount)
	}

	if h.sessLst.ResumeSession(match.ID) {
		msg := match.FirstMessage
		if msg == "" {
			msg = "（空）"
		}
		return fmt.Sprintf("[已恢复会话 %s] %s", match.ID[:8], msg)
	}
	return fmt.Sprintf("恢复会话 %s 失败", match.ID[:8])
}

// SlashCommands 返回所有可用的斜杠命令列表（用于自动补全）。
func SlashCommands() []SlashCommandInfo {
	return []SlashCommandInfo{
		{Name: "help", Desc: "显示帮助"},
		{Name: "clear", Desc: "清空会话历史"},
		{Name: "compact", Desc: "手动触发上下文压缩"},
		{Name: "model", Desc: "查看/切换模型"},
		{Name: "tools", Desc: "列出已注册工具"},
		{Name: "cost", Desc: "显示 token 用量"},
		{Name: "tasks", Desc: "列出任务"},
		{Name: "bg", Desc: "列出后台任务"},
		{Name: "sessions", Desc: "列出历史会话"},
		{Name: "resume", Desc: "恢复历史会话"},
		{Name: "config", Desc: "管理配置"},
		{Name: "status", Desc: "显示状态信息"},
		{Name: "exit", Desc: "退出"},
	}
}

// SlashCommandInfo 是斜杠命令的元信息。
type SlashCommandInfo struct {
	Name string
	Desc string
}
