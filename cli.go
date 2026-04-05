package goagent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dream355873200/GoAgent/extractmem"
	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/tui"
)

// runCLI 启动交互式 TUI REPL。
// 使用 Bubble Tea + Lipgloss 构建终端用户界面。
func runCLI(app *App) {
	// 创建通道桥接。
	approver := tui.NewApprover()
	asker := tui.NewAsker()

	// 设置审批者（goagent.Approver 适配 tui.TUIApprover）。
	app.config.approver = &tuiApproverAdapter{inner: approver}

	// 注册 AskUser 回调（替换默认的 stdin 直读）。
	if setAskUserCallbackFn != nil {
		setAskUserCallbackFn(asker.Ask)
	}

	// 初始化会话持久化。
	// 如果 App 未配置 SessionManager，自动创建基于文件系统的持久化。
	// 会话存储在 .yume/sessions/ 目录下。
	if app.config.sessionManager == nil {
		sessDir := filepath.Join(".yume", "sessions")
		store := session.NewFileStore(sessDir)
		mgr := session.NewManager(store)
		app.config.sessionManager = mgr
	}

	// 创建适配器。
	runner := &agentRunnerAdapter{
		app:     app,
		sessMgr: app.config.sessionManager,
	}

	// 创建新会话（每次启动 TUI 自动创建新会话）。
	sessID, err := app.config.sessionManager.Create(context.Background(), nil)
	if err == nil {
		runner.sessionID = sessID
	}

	// 如果配置了记忆目录，创建记忆提取器（对话结束后自动提取）。
	if app.config.memoryDir != "" && app.provider != nil {
		autoMem := memory.NewAutoMemory(app.config.memoryDir)
		runner.extractor = extractmem.NewExtractor(app.provider, autoMem)
	}

	appInfo := &appInfoAdapter{app: app}

	// 创建并启动 TUI。
	m := tui.New(runner, appInfo, approver, asker)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI 错误: %v\n", err)
		os.Exit(1)
	}
}

// ── 适配器：goagent.App → tui.AgentRunner ──────────────────────────

// agentRunnerAdapter 将 App.RunSession() 适配为 tui.AgentRunner 接口。
// 内部通过 session.Manager 实现会话持久化和恢复。
// 同时实现 tui.Resettable 和 tui.SessionLister 接口。
// 对话结束后异步提取记忆到 auto memory。
type agentRunnerAdapter struct {
	app       *App
	sessMgr   *session.Manager      // 会话管理器
	sessionID string                // 当前活跃会话 ID
	extractor *extractmem.Extractor // 记忆提取器（可为 nil）
}

func (a *agentRunnerAdapter) Run(ctx context.Context, input string) <-chan tui.Event {
	out := make(chan tui.Event, 64)
	go func() {
		defer close(out)
		// 使用 RunSession 自动加载历史和持久化新消息。
		for ev := range a.app.RunSession(ctx, a.sessionID, input) {
			// EventDone 时异步提取记忆。
			if ev.Type == EventDone && ev.Messages != nil {
				if a.extractor != nil {
					msgs := ev.Messages
					go func() {
						_, _ = a.extractor.Extract(context.Background(), msgs)
					}()
				}
			}
			out <- toTUIEvent(ev)
		}
	}()
	return out
}

// Reset 清空当前会话并创建新会话（/clear 时调用）。
func (a *agentRunnerAdapter) Reset() {
	// 创建新会话替换当前会话。
	sessID, err := a.sessMgr.Create(context.Background(), nil)
	if err == nil {
		a.sessionID = sessID
	}
}

// ListSessions 返回所有可恢复的会话列表。
// 实现 tui.SessionLister 接口。
func (a *agentRunnerAdapter) ListSessions() []tui.SessionInfo {
	summaries, err := a.sessMgr.List(context.Background())
	if err != nil {
		return nil
	}
	result := make([]tui.SessionInfo, len(summaries))
	for i, s := range summaries {
		result[i] = tui.SessionInfo{
			ID:           s.ID,
			State:        s.State,
			TurnCount:    s.TurnCount,
			CreatedAt:    s.CreatedAt,
			UpdatedAt:    s.UpdatedAt,
			FirstMessage: s.FirstMessage,
		}
	}
	return result
}

// ResumeSession 恢复指定 ID 的会话。
// 实现 tui.SessionLister 接口。
func (a *agentRunnerAdapter) ResumeSession(sessionID string) bool {
	sess, err := a.sessMgr.Get(context.Background(), sessionID)
	if err != nil || sess == nil {
		return false
	}
	a.sessionID = sessionID
	return true
}

// CurrentSessionID 返回当前会话 ID。
// 实现 tui.SessionLister 接口。
func (a *agentRunnerAdapter) CurrentSessionID() string {
	return a.sessionID
}

// toTUIEvent 将 goagent.Event 转换为 tui.Event。
func toTUIEvent(ev Event) tui.Event {
	te := tui.Event{
		Text:          ev.Text,
		Thinking:      ev.Thinking,
		ToolName:      ev.ToolName,
		ToolResult:    ev.ToolResult,
		Error:         ev.Error,
		AgentID:       ev.AgentID,
		AgentDesc:     ev.AgentDesc,
		AgentStatus:   ev.AgentStatus,
		AgentActivity: ev.AgentActivity,
		AgentToolUses: ev.AgentToolUses,
		AgentTokens:   ev.AgentTokens,
	}

	// ToolInput → string。
	if len(ev.ToolInput) > 0 {
		te.ToolInput = string(ev.ToolInput)
	}

	// Usage。
	if ev.Usage != nil {
		te.InputTokens = ev.Usage.InputTokens
		te.OutputTokens = ev.Usage.OutputTokens
	}

	// EventType 映射。
	switch ev.Type {
	case EventTextDelta:
		te.Type = tui.EvTextDelta
	case EventThinking:
		te.Type = tui.EvThinking
	case EventToolStart:
		te.Type = tui.EvToolStart
	case EventToolDone:
		te.Type = tui.EvToolDone
	case EventNeedApproval:
		te.Type = tui.EvNeedApproval
	case EventUsageUpdate:
		te.Type = tui.EvUsageUpdate
	case EventTurnComplete:
		te.Type = tui.EvTurnComplete
	case EventDone:
		te.Type = tui.EvDone
	case EventError:
		te.Type = tui.EvError
	case EventProgress:
		te.Type = tui.EvProgress
	case EventCompaction:
		te.Type = tui.EvCompaction
	case EventSubAgentProgress:
		te.Type = tui.EvSubAgentProgress
	}

	return te
}

// ── 适配器：goagent.App → tui.AppInfo ──────────────────────────────

// appInfoAdapter 将 App 适配为 tui.AppInfo 接口。
type appInfoAdapter struct {
	app *App
}

func (a *appInfoAdapter) ToolNames() []string {
	return a.app.ToolNames()
}

func (a *appInfoAdapter) ToolDescription(name string) string {
	return a.app.ToolDescription(name)
}

func (a *appInfoAdapter) TaskSummaryLines() []string {
	summaries := a.app.TaskSummaries()
	lines := make([]string, 0, len(summaries))
	for _, s := range summaries {
		lines = append(lines, fmt.Sprintf("  #%s [%s] %s", s.ID, s.Status, s.Subject))
	}
	return lines
}

func (a *appInfoAdapter) BgTaskLines() []string {
	mgr := a.app.BgTasks()
	if mgr == nil {
		return nil
	}
	tasks := mgr.List()
	lines := make([]string, 0, len(tasks))
	for _, t := range tasks {
		desc := t.Description
		if t.Command != "" {
			desc = t.Command
		}
		lines = append(lines, fmt.Sprintf("  %s [%s] %s", t.ID, t.Status, desc))
	}
	return lines
}

func (a *appInfoAdapter) ModelID() string {
	return a.app.ModelID()
}

func (a *appInfoAdapter) SetModel(modelID string) bool {
	return a.app.SetModel(modelID)
}

// ── 适配器：tui.TUIApprover → goagent.Approver ─────────────────────

// tuiApproverAdapter 将 tui.TUIApprover 适配为 goagent.Approver 接口。
type tuiApproverAdapter struct {
	inner *tui.TUIApprover
}

func (a *tuiApproverAdapter) Approve(toolName string, input string, perm Permission) (bool, bool) {
	return a.inner.Approve(toolName, input, tui.PermLevel(perm))
}

// ── 延迟注入 ────────────────────────────────────────────────────────

// setAskUserCallbackFn 是由 builtin 包注入的 AskUser 回调设置函数。
// 通过延迟注入避免 goagent→builtin→goagent 的循环依赖。
var setAskUserCallbackFn func(fn func(string) (string, error))

// RegisterSetAskUserCallback 由 builtin 包调用以注册 AskUser 回调设置函数。
func RegisterSetAskUserCallback(fn func(func(string) (string, error))) {
	setAskUserCallbackFn = fn
}

// ── SDK/Headless Fallback ───────────────────────────────────────────

// stdinApprover 通过 stdin 提示用户（SDK/headless fallback）。
type stdinApprover struct{}

func (a *stdinApprover) Approve(toolName string, input string, perm Permission) (bool, bool) {
	display := input
	if len(display) > 100 {
		display = display[:100] + "..."
	}

	warning := ""
	if perm == Dangerous {
		warning = " ⚠️  危险操作"
	}

	fmt.Printf("\n─── 需要权限%s ───\n", warning)
	fmt.Printf("工具:  %s\n", toolName)
	fmt.Printf("输入: %s\n", display)

	options := "[y]确认 / [n]拒绝"
	if perm == Normal {
		options += " / [a]始终允许"
	}
	fmt.Printf("允许? (%s): ", options)

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(strings.ToLower(line))

	switch answer {
	case "y", "yes":
		return true, false
	case "a", "always":
		return true, true
	default:
		return false, false
	}
}

// formatDuration 格式化时间段为人类可读的字符串。
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f秒", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.0f分%.0f秒", d.Minutes(), d.Seconds()-d.Minutes()*60)
	}
	return fmt.Sprintf("%.0f时%.0f分", d.Hours(), d.Minutes()-d.Hours()*60)
}
