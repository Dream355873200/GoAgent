// Package tui 提供基于 Bubble Tea 的终端用户界面。
//
// 对齐 Claude Code 的 Ink/React TUI 层：
//   - 流式文本渲染
//   - Thinking 折叠显示
//   - 工具调用高亮
//   - 权限审批对话框
//   - AskUser 交互
//   - 状态栏（token 用量、工具计数、耗时）
//   - 斜杠命令 + Skill + @mention
package tui

import "github.com/charmbracelet/lipgloss"

// ── 样式定义 ──────────────────────────────────────────────────────
// 对齐 Claude Code 的视觉层次。

var (
	// HeaderStyle 是顶部标题样式（品牌紫色加粗）。
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7C3AED"))

	// ThinkingStyle 是 thinking 内容样式（灰色斜体）。
	ThinkingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Italic(true)

	// ToolNameStyle 是工具名称样式（蓝色加粗）。
	ToolNameStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("33")).
			Bold(true)

	// ToolResultStyle 是工具结果样式（暗灰色）。
	ToolResultStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))

	// ErrorStyle 是错误信息样式（红色加粗）。
	ErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	// StatusBarStyle 是底部状态栏样式（深色背景）。
	StatusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(lipgloss.Color("252")).
			Padding(0, 1)

	// InputPromptStyle 是输入提示符样式（绿色加粗 ">"）。
	InputPromptStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("76")).
				Bold(true)

	// DimStyle 是辅助信息样式（暗灰色）。
	DimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	// UserMsgStyle 是用户消息样式（亮白色加粗）。
	UserMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Bold(true)

	// ApprovalBorder 是权限审批框样式（圆角黄色边框）。
	ApprovalBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("214")).
			Padding(1, 2)

	// ApprovalWarning 是审批框中警告文字样式（红色加粗）。
	ApprovalWarning = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	// AskUserBorder 是 AskUser 提问框样式（圆角蓝色边框）。
	AskUserBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("33")).
			Padding(1, 2)

	// CommandStyle 是命令输出样式（暗灰色）。
	CommandStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))

	// SuccessStyle 是成功信息样式（绿色）。
	SuccessStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("76"))

	// SeparatorStyle 是分隔线样式。
	SeparatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))

	// ActivityDotStyle 是正在执行的闪烁圆点样式（紫色加粗）。
	ActivityDotStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7C3AED")).
				Bold(true)

	// TextDotStyle 是 assistant 文本段前缀圆点样式（白色）。
	TextDotStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	// SuccessDotStyle 是工具执行成功的圆点样式（绿色加粗）。
	SuccessDotStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("76")).
			Bold(true)

	ActivityTextStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
)
