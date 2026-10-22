// Package cli 提供 GoAgent 的交互式终端界面（基于 Bubble Tea + Lipgloss）。
//
//	app := goagent.New(goagent.WithProvider(...))
//	cli.Run(app)
//
// 导入本包后 App.RunCLI 同样可用。根包 goagent 不依赖终端界面库，只有导入
// 了本包的程序才会链接它们。
package cli

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/extractmem"
	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/statedir"
	"github.com/Dream355873200/GoAgent/tui"
)

func init() {
	goagent.RegisterCLIRunner(Run)
}

// Run 启动交互式终端界面，阻塞直到用户退出。
//
// 审批与提问走终端界面；App 没有配置会话管理器时，自动在状态目录的
// sessions/ 下（默认 .yume/sessions/）创建基于文件的会话存储，每次启动新建
// 一个会话；配置了记忆目录时，每轮对话结束后异步提取记忆。
func Run(app *goagent.App) {
	approver := tui.NewApprover()
	asker := tui.NewAsker()

	app.SetApprover(&tuiApproverAdapter{inner: approver})
	goagent.SetAskUserCallback(asker.Ask)

	if app.Sessions() == nil {
		app.SetSessionManager(session.NewManager(session.NewFileStore(statedir.Join("", "sessions"))))
	}
	runner := &agentRunnerAdapter{app: app, sessMgr: app.Sessions()}
	if sessID, err := runner.sessMgr.Create(context.Background(), nil); err == nil {
		runner.sessionID = sessID
	}
	if dir := app.MemoryDir(); dir != "" && app.Provider() != nil {
		runner.extractor = extractmem.NewExtractor(app.Provider(), memory.NewAutoMemory(dir))
	}

	m := tui.New(runner, &appInfoAdapter{app: app}, approver, asker)
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
	app       *goagent.App
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
			if ev.Type == goagent.EventDone && ev.Messages != nil && a.extractor != nil {
				msgs := ev.Messages
				go func() {
					_, _ = a.extractor.Extract(context.Background(), msgs)
				}()
			}
			out <- toTUIEvent(ev)
		}
	}()
	return out
}

// Reset 清空当前会话并创建新会话（/clear 时调用）。
func (a *agentRunnerAdapter) Reset() {
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
func toTUIEvent(ev goagent.Event) tui.Event {
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
	case goagent.EventTextDelta:
		te.Type = tui.EvTextDelta
	case goagent.EventThinking:
		te.Type = tui.EvThinking
	case goagent.EventToolStart:
		te.Type = tui.EvToolStart
	case goagent.EventToolDone:
		te.Type = tui.EvToolDone
	case goagent.EventNeedApproval:
		te.Type = tui.EvNeedApproval
	case goagent.EventUsageUpdate:
		te.Type = tui.EvUsageUpdate
	case goagent.EventTurnComplete:
		te.Type = tui.EvTurnComplete
	case goagent.EventDone:
		te.Type = tui.EvDone
	case goagent.EventError:
		te.Type = tui.EvError
	case goagent.EventProgress:
		te.Type = tui.EvProgress
	case goagent.EventCompaction:
		te.Type = tui.EvCompaction
	case goagent.EventSubAgentProgress:
		te.Type = tui.EvSubAgentProgress
	case goagent.EventInterrupted:
		te.Type = tui.EvInterrupted
	}

	return te
}

// ── 适配器：goagent.App → tui.AppInfo ──────────────────────────────

// appInfoAdapter 将 App 适配为 tui.AppInfo 接口。
type appInfoAdapter struct {
	app *goagent.App
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

func (a *tuiApproverAdapter) Approve(toolName string, input string, perm goagent.Permission) (bool, bool) {
	return a.inner.Approve(toolName, input, tui.PermLevel(perm))
}
