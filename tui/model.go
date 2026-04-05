package tui

// 本文件实现 Bubble Tea TUI 的核心 Model。
//
// 对齐 Claude Code 的 Ink/React TUI 架构：
//   - 流式文本渲染（TextDelta 增量追加）
//   - Thinking 折叠显示（灰色斜体）
//   - 工具调用高亮（蓝色工具名 + 灰色结果）
//   - 活动指示器（● 正在执行... 风格，对齐 Claude Code）
//   - 运行中保持输入框可用（排队下一条消息）
//   - 权限审批对话框（通道桥接 TUIApprover）
//   - AskUser 交互框（通道桥接 TUIAsker）
//   - 状态栏（工具计数 + 耗时 + token 用量）
//   - 斜杠命令 + @mention（userinput.Processor 集成）
//
// 注意：tui 包不能 import goagent（循环依赖），
// 所以通过 AgentRunner 接口和 Event 值对象解耦。

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dream355873200/GoAgent/userinput"
)

// ── 解耦接口 ────────────────────────────────────────────────────────

// AgentRunner 是 TUI 对 App.Run() 的最小接口。
// 由 goagent/cli.go 中的适配器实现。
type AgentRunner interface {
	// Run 启动一次 agent 运行，返回事件通道。
	Run(ctx context.Context, input string) <-chan Event
}

// Resettable 是可选接口，runner 实现后 /clear 会调用 Reset 清空对话历史。
type Resettable interface {
	Reset()
}

// SessionLister 是可选接口，runner 实现后支持 /sessions 和 /resume 命令。
// 对齐 Claude Code 的 session 恢复能力。
type SessionLister interface {
	// ListSessions 返回可恢复的会话摘要列表。
	ListSessions() []SessionInfo
	// ResumeSession 恢复指定 ID 的会话。
	// 返回 false 表示会话不存在。
	ResumeSession(sessionID string) bool
	// CurrentSessionID 返回当前会话 ID。
	CurrentSessionID() string
}

// SessionInfo 是会话摘要信息（由 TUI 渲染）。
type SessionInfo struct {
	ID           string
	State        string
	TurnCount    int
	CreatedAt    int64 // Unix 毫秒
	UpdatedAt    int64 // Unix 毫秒
	FirstMessage string
}

// Event 是 TUI 本地的事件值对象（避免 import goagent）。
// 由 goagent/cli.go 中的适配器从 goagent.Event 转换而来。
type Event struct {
	Type       EventType
	Text       string
	Thinking   string
	ToolName   string
	ToolInput  string
	ToolResult string
	Error      error
	// Usage 增量。
	InputTokens  int
	OutputTokens int
	// 子 Agent 进度字段。
	AgentID       string
	AgentDesc     string
	AgentStatus   string // "running" / "done"
	AgentActivity string
	AgentToolUses int
	AgentTokens   int
}

// EventType 是事件类型枚举。
type EventType int

const (
	EvTextDelta EventType = iota
	EvThinking
	EvToolStart
	EvToolDone
	EvNeedApproval
	EvUsageUpdate
	EvTurnComplete
	EvDone
	EvError
	EvProgress
	EvCompaction
	EvSubAgentProgress
)

// ── viewMode 控制 TUI 当前状态 ──────────────────────────────────────

type viewMode int

const (
	modeInput    viewMode = iota // 等待用户输入
	modeRunning                  // agent 运行中，流式渲染
	modeApproval                 // 显示权限审批对话框
	modeAskUser                  // 显示 AskUser 提问框
)

// ── Bubble Tea 消息类型 ─────────────────────────────────────────────

// agentEventMsg 包装 agent 事件为 Bubble Tea 消息。
type agentEventMsg struct {
	event Event
	done  bool // 通道已关闭
}

// approvalMsg 包装审批请求为 Bubble Tea 消息。
type approvalMsg ApprovalRequest

// askUserMsg 包装 AskUser 请求为 Bubble Tea 消息。
type askUserMsg AskUserRequest

// ── Model ───────────────────────────────────────────────────────────

// Model 是 Bubble Tea 主模型。
type Model struct {
	runner   AgentRunner
	appInfo  AppInfo
	approver *TUIApprover
	asker    *TUIAsker
	proc     *userinput.Processor
	cmdH     *commandHandler
	mode     viewMode

	// UI 组件。
	input textinput.Model
	vp    viewport.Model
	spin  spinner.Model

	// 聊天内容（指针避免值拷贝 panic）。
	content *strings.Builder
	lines   int

	// 当前轮状态。
	eventCh       <-chan Event
	cancelFn      context.CancelFunc
	curReq        *ApprovalRequest
	curAsk        *AskUserRequest
	lastToolName  string // 上一个 EvToolStart 的工具名（供 EvToolDone 使用）
	lastToolInput string // 上一个 EvToolStart 的输入 JSON（供 EvToolDone 使用）
	activityText  string // 当前活动描述（"● 正在读取文件..."）
	pendingInput  string // 运行中用户按 Enter 排队的下一条输入
	inTextBlock   bool   // 当前是否在 assistant 文本块中（用于 ● 前缀判断）
	inThinkBlock  bool   // 当前是否在 <think> 标签内（过滤思考内容）
	thinkBuf      string // 跨 chunk 的部分标签缓冲

	// 子 Agent 并发状态追踪（Claude Code 风格树形显示）。
	runningAgents *subAgentTracker

	// 斜杠命令补全。
	completions     []SlashCommandInfo // 当前匹配的补全列表
	completionIdx   int                // 当前选中的补全项索引（-1 表示无选中）
	showCompletions bool               // 是否显示补全菜单

	// 统计。
	toolsUsed    int
	startTime    time.Time
	turnCount    int
	sessionStart time.Time
	totalUsage   *UsageInfo // 指针，与 commandHandler 共享

	// 终端尺寸。
	width, height int
	ready         bool
}

// New 创建一个新的 TUI Model。
func New(runner AgentRunner, appInfo AppInfo, approver *TUIApprover, asker *TUIAsker) Model {
	// 输入框。
	ti := textinput.New()
	ti.Placeholder = "输入消息，/help 查看帮助..."
	ti.Prompt = "" // 我们在 View 中手动渲染 ">" 前缀。
	ti.Focus()
	ti.CharLimit = 4096

	// Spinner（用于活动指示器动画）。
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#7C3AED"))

	// 输入预处理器。
	proc := userinput.NewProcessor(nil)

	// TUI 层命令处理器（usage 指针与 Model 共享）。
	usage := &UsageInfo{}
	// 如果 runner 实现了 SessionLister，传给命令处理器。
	var sessLst SessionLister
	if sl, ok := runner.(SessionLister); ok {
		sessLst = sl
	}
	cmdH := newCommandHandler(appInfo, usage, sessLst)
	proc.AddBuiltinHandler(cmdH.Handle)

	return Model{
		runner:        runner,
		appInfo:       appInfo,
		approver:      approver,
		asker:         asker,
		proc:          proc,
		cmdH:          cmdH,
		mode:          modeInput,
		input:         ti,
		spin:          sp,
		content:       &strings.Builder{},
		sessionStart:  time.Now(),
		totalUsage:    usage,
		runningAgents: newSubAgentTracker(),
	}
}

// Init 实现 tea.Model 接口。
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.spin.Tick,
		waitForApproval(m.approver.RequestCh()),
		waitForAskUser(m.asker.RequestCh()),
	)
}

// Update 实现 tea.Model 接口。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	// ── 终端尺寸变化 ──
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// 布局：标题(1) + \n + viewport + \n + 状态栏(1) + \n + 输入(1)
		vpHeight := m.height - 5
		if vpHeight < 3 {
			vpHeight = 3
		}
		if !m.ready {
			m.vp = viewport.New(m.width, vpHeight)
			m.vp.SetContent(m.content.String())
			m.ready = true
		} else {
			m.vp.Width = m.width
			m.vp.Height = vpHeight
		}
		m.input.Width = m.width - 4
		return m, nil

	// ── 键盘输入 ──
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	// ── Agent 事件 ──
	case agentEventMsg:
		return m.handleAgentEvent(msg)

	// ── 审批请求 ──
	case approvalMsg:
		req := ApprovalRequest(msg)
		m.curReq = &req
		m.mode = modeApproval
		m.activityText = ""
		return m, nil

	// ── AskUser 请求 ──
	case askUserMsg:
		req := AskUserRequest(msg)
		m.curAsk = &req
		m.mode = modeAskUser
		m.activityText = ""
		m.input.SetValue("")
		m.input.Placeholder = "输入回答..."
		m.input.Focus()
		return m, textinput.Blink

	// ── Spinner 更新 ──
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	}

	// 传递给 textinput 处理光标闪烁。
	{
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// 传递给 viewport 处理滚动。
	if m.ready {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

// handleKeyMsg 处理键盘输入。
func (m Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {

	case modeInput:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			// 如果有补全选中项，先应用补全。
			if m.showCompletions && m.completionIdx >= 0 && m.completionIdx < len(m.completions) {
				selected := m.completions[m.completionIdx]
				m.input.SetValue("/" + selected.Name + " ")
				m.input.CursorEnd()
				m.showCompletions = false
				m.completionIdx = -1
				return m, nil
			}
			m.showCompletions = false
			return m.handleSubmit()
		case tea.KeyTab:
			// Tab 补全：如果有匹配项，循环选中。
			if m.showCompletions && len(m.completions) > 0 {
				m.completionIdx = (m.completionIdx + 1) % len(m.completions)
				return m, nil
			}
			return m, nil
		case tea.KeyShiftTab:
			if m.showCompletions && len(m.completions) > 0 {
				m.completionIdx--
				if m.completionIdx < 0 {
					m.completionIdx = len(m.completions) - 1
				}
				return m, nil
			}
			return m, nil
		case tea.KeyEscape:
			if m.showCompletions {
				m.showCompletions = false
				m.completionIdx = -1
				return m, nil
			}
			return m, nil
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			// 更新补全提示。
			m.updateCompletions()
			return m, cmd
		}

	case modeRunning:
		// 运行中仍然可以打字。
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.cancelFn != nil {
				m.cancelFn()
			}
			m.appendContent(ErrorStyle.Render("\n[已取消]") + "\n")
			m.mode = modeInput
			m.activityText = ""
			m.input.Focus()
			return m, tea.Batch(
				textinput.Blink,
				waitForApproval(m.approver.RequestCh()),
				waitForAskUser(m.asker.RequestCh()),
			)
		case tea.KeyEnter:
			// 排队下一条消息（运行结束后自动发送）。
			text := strings.TrimSpace(m.input.Value())
			if text != "" {
				m.pendingInput = text
				m.input.SetValue("")
				m.appendContent(DimStyle.Render(fmt.Sprintf("  [排队] %s", text)) + "\n")
			}
			return m, nil
		default:
			// 运行中也可以输入。
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

	case modeApproval:
		switch msg.String() {
		case "y", "Y":
			m.approver.Respond(true, false)
			m.curReq = nil
			m.mode = modeRunning
			return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
		case "n", "N":
			m.approver.Respond(false, false)
			m.curReq = nil
			m.mode = modeRunning
			return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
		case "a", "A":
			m.approver.Respond(true, true)
			m.curReq = nil
			m.mode = modeRunning
			return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
		}
		return m, nil

	case modeAskUser:
		switch msg.Type {
		case tea.KeyEnter:
			answer := m.input.Value()
			m.asker.Respond(answer)
			m.curAsk = nil
			m.mode = modeRunning
			m.input.SetValue("")
			m.input.Placeholder = "输入消息，/help 查看帮助..."
			return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
		case tea.KeyCtrlC:
			m.asker.Respond("")
			m.curAsk = nil
			m.mode = modeRunning
			m.input.SetValue("")
			m.input.Placeholder = "输入消息，/help 查看帮助..."
			return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

// handleSubmit 处理用户提交输入。
func (m Model) handleSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	m.input.SetValue("")
	if text == "" {
		return m, nil
	}

	// 显示用户输入。
	m.appendContent(UserMsgStyle.Render("> "+text) + "\n\n")

	// 通过 userinput.Processor 预处理。
	result := m.proc.Process(context.Background(), text)

	switch result.Action {
	case userinput.ActionBuiltinCommand:
		if result.CommandOutput == "[退出]" {
			return m, tea.Quit
		}
		if result.CommandOutput == "[会话已清空]" {
			m.content.Reset()
			m.lines = 0
			if m.ready {
				m.vp.SetContent("")
			}
			// 重置对话历史（如果 runner 支持）。
			if r, ok := m.runner.(Resettable); ok {
				r.Reset()
			}
			return m, nil
		}
		if result.CommandOutput == "[触发上下文压缩]" {
			// /compact 需要发送给 agent 运行，触发真正的 compaction。
			m.appendContent(DimStyle.Render("正在压缩上下文...") + "\n")
			return m.startAgentRun("[系统指令] 请输出一句简短确认：上下文已压缩。")
		}
		m.appendContent(CommandStyle.Render(result.CommandOutput) + "\n\n")
		return m, nil

	case userinput.ActionSkill:
		m.appendContent(DimStyle.Render(fmt.Sprintf("[技能: %s]", result.SkillName)) + "\n")
		return m.startAgentRun(result.Text)

	case userinput.ActionSendToLLM:
		return m.startAgentRun(result.Text)
	}

	return m, nil
}

// startAgentRun 启动一次 agent 运行。
func (m Model) startAgentRun(input string) (tea.Model, tea.Cmd) {
	ctx, cancel := signal.NotifyContext(context.Background(), interruptSignals()...)
	m.cancelFn = cancel
	m.eventCh = m.runner.Run(ctx, input)
	m.mode = modeRunning
	m.toolsUsed = 0
	m.startTime = time.Now()
	m.activityText = "思考中..."
	m.pendingInput = ""
	m.inTextBlock = false
	m.inThinkBlock = false
	m.thinkBuf = ""
	// 不再 Blur 输入框 — 运行中仍可输入。
	m.input.Placeholder = "输入下一条消息（运行中可排队）..."

	return m, tea.Batch(
		waitForAgentEvent(m.eventCh),
		waitForApproval(m.approver.RequestCh()),
		waitForAskUser(m.asker.RequestCh()),
		m.spin.Tick,
	)
}

// handleAgentEvent 处理来自 agent 的事件。
func (m Model) handleAgentEvent(msg agentEventMsg) (tea.Model, tea.Cmd) {
	if msg.done {
		m.mode = modeInput
		m.eventCh = nil
		m.activityText = ""
		if m.cancelFn != nil {
			m.cancelFn()
			m.cancelFn = nil
		}
		m.input.Focus()
		m.input.Placeholder = "输入消息，/help 查看帮助..."

		// 如果有排队的输入，自动发送。
		if m.pendingInput != "" {
			pending := m.pendingInput
			m.pendingInput = ""
			m.appendContent(UserMsgStyle.Render("> "+pending) + "\n\n")
			return m.startAgentRun(pending)
		}

		return m, tea.Batch(
			textinput.Blink,
			waitForApproval(m.approver.RequestCh()),
			waitForAskUser(m.asker.RequestCh()),
		)
	}

	ev := msg.event
	switch ev.Type {
	case EvTextDelta:
		m.activityText = "生成中..."
		text := ev.Text

		// 过滤 <think>...</think> 标签内容（某些模型把思考过程放在文本中）。
		text = m.filterThinkTags(text)
		if text == "" {
			break
		}

		// Claude Code 风格：每段 assistant 文本以白色 ● 前缀开始。
		if !m.inTextBlock {
			m.appendContent("\n" + TextDotStyle.Render("●") + " ")
			m.inTextBlock = true
		}
		m.appendContent(text)

	case EvThinking:
		// Thinking 内容不显示在聊天区，只更新状态栏（对齐 Claude Code）。
		m.activityText = "思考中..."

	case EvToolStart:
		m.toolsUsed++
		m.lastToolName = ev.ToolName
		m.lastToolInput = ev.ToolInput
		// 结束当前文本块（如果有）。
		if m.inTextBlock {
			m.appendContent("\n\n")
			m.inTextBlock = false
		}
		// 子 Agent 工具：注册到追踪器，不立即渲染。
		if strings.HasPrefix(ev.ToolName, "Agent_") {
			agentDesc := extractAgentDesc(ev.ToolName, ev.ToolInput)
			m.runningAgents.Update(ev.ToolName+ev.ToolInput, agentDesc, "running", "启动中…", 0, 0)
			m.activityText = fmt.Sprintf("Running %d agents…", m.runningAgents.Count())
			break
		}
		// 普通工具：只更新状态栏活动指示器（闪烁），不在内容区渲染。
		// 工具完成后由 EvToolDone 一次性渲染绿色 ● 结果。
		m.activityText = toolActivityText(ev.ToolName, ev.ToolInput)

	case EvToolDone:
		// 子 Agent 工具完成：更新追踪器状态，渲染树。
		if strings.HasPrefix(m.lastToolName, "Agent_") {
			agentID := m.lastToolName + m.lastToolInput
			m.runningAgents.Update(agentID, "", "done", "", 0, 0)
			// 如果所有 agent 都已完成，渲染最终树形并清空追踪器。
			if m.runningAgents.Count() == 0 && m.runningAgents.HasAny() {
				m.appendContent(m.runningAgents.RenderTree() + "\n")
				m.runningAgents.Clear()
			}
			break
		}
		m.appendContent(formatToolDone(m.lastToolName, m.lastToolInput, ev.ToolResult))

	case EvUsageUpdate:
		m.totalUsage.InputTokens += ev.InputTokens
		m.totalUsage.OutputTokens += ev.OutputTokens

	case EvError:
		// 过滤掉用户主动取消导致的错误。
		if ev.Error != nil && strings.Contains(ev.Error.Error(), "context canceled") {
			break
		}
		m.activityText = ""
		m.appendContent(ErrorStyle.Render(fmt.Sprintf("\n[错误] %v", ev.Error)) + "\n")

	case EvCompaction:
		m.activityText = "压缩上下文..."
		m.appendContent(DimStyle.Render(fmt.Sprintf("[压缩] %s", ev.Text)) + "\n")

	case EvProgress:
		m.appendContent(DimStyle.Render(fmt.Sprintf("[信息] %s", ev.Text)) + "\n")

	case EvSubAgentProgress:
		m.runningAgents.Update(ev.AgentID, ev.AgentDesc, ev.AgentStatus, ev.AgentActivity, ev.AgentToolUses, ev.AgentTokens)
		running := m.runningAgents.Count()
		if running > 0 {
			m.activityText = fmt.Sprintf("Running %d agents…", running)
		}

	case EvDone:
		// 结束当前文本块（如果有）。
		if m.inTextBlock {
			m.appendContent("\n")
			m.inTextBlock = false
		}
		m.turnCount++
		elapsed := time.Since(m.startTime)
		summary := fmt.Sprintf("\n─── %.1fs", elapsed.Seconds())
		if m.toolsUsed > 0 {
			summary += fmt.Sprintf(", %d 个工具调用", m.toolsUsed)
		}
		summary += " ───\n\n"
		m.appendContent(DimStyle.Render(summary))
	}

	return m, tea.Batch(waitForAgentEvent(m.eventCh), m.spin.Tick)
}

// appendContent 追加内容到 viewport。
func (m *Model) appendContent(s string) {
	m.content.WriteString(s)
	m.lines += strings.Count(s, "\n")
	if m.ready {
		m.vp.SetContent(m.content.String())
		m.vp.GotoBottom()
	}
}

// updateCompletions 根据当前输入更新斜杠命令补全列表。
func (m *Model) updateCompletions() {
	text := m.input.Value()
	if !strings.HasPrefix(text, "/") || strings.Contains(text, " ") {
		m.showCompletions = false
		m.completionIdx = -1
		m.completions = nil
		return
	}

	prefix := strings.ToLower(strings.TrimPrefix(text, "/"))
	all := SlashCommands()

	if prefix == "" {
		m.completions = all
		m.showCompletions = true
		m.completionIdx = -1
		return
	}

	var matches []SlashCommandInfo
	for _, cmd := range all {
		if strings.HasPrefix(cmd.Name, prefix) {
			matches = append(matches, cmd)
		}
	}

	if len(matches) == 0 {
		m.showCompletions = false
		m.completionIdx = -1
		m.completions = nil
		return
	}

	m.completions = matches
	m.showCompletions = true
	m.completionIdx = -1
}

// ── View ────────────────────────────────────────────────────────────

// View 实现 tea.Model 接口。
func (m Model) View() string {
	if !m.ready {
		return "初始化中..."
	}

	var b strings.Builder

	// 标题（固定 1 行）。
	b.WriteString(HeaderStyle.Render("GoAgent TUI"))
	b.WriteString("\n")

	// 聊天区域（固定高度 viewport）。
	b.WriteString(m.vp.View())
	b.WriteString("\n")

	// 子 Agent 树形显示（运行中时实时渲染）。
	if m.mode == modeRunning && m.runningAgents.HasAny() {
		b.WriteString(m.runningAgents.RenderTree())
	}

	// 状态栏（固定 1 行）— 包含活动指示器。
	b.WriteString(m.renderStatusBar())
	b.WriteString("\n")

	// 底部输入区 / 审批框 / AskUser 框。
	switch m.mode {
	case modeApproval:
		b.WriteString(m.renderApprovalBox())
	case modeAskUser:
		b.WriteString(m.renderAskUserBox())
	default:
		// modeInput 和 modeRunning 都显示输入框。
		// 斜杠命令补全菜单（显示在输入框上方）。
		if m.showCompletions && len(m.completions) > 0 && m.mode == modeInput {
			b.WriteString(m.renderCompletions())
		}
		b.WriteString(InputPromptStyle.Render("> "))
		b.WriteString(m.input.View())
	}

	return b.String()
}

// renderCompletions 渲染斜杠命令补全菜单。
func (m Model) renderCompletions() string {
	var b strings.Builder
	for i, cmd := range m.completions {
		prefix := "  "
		if i == m.completionIdx {
			prefix = "> "
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("76")).Render(
				fmt.Sprintf("%s/%s", prefix, cmd.Name)))
		} else {
			b.WriteString(DimStyle.Render(
				fmt.Sprintf("%s/%s", prefix, cmd.Name)))
		}
		b.WriteString(DimStyle.Render("  " + cmd.Desc))
		b.WriteString("\n")
	}
	return b.String()
}

// renderStatusBar 渲染底部状态栏（严格 1 行）。
// 运行时：✶ 活动描述 (耗时 · ↓ token数 tokens)
// 空闲时：空白（不显示无意义的信息）
func (m Model) renderStatusBar() string {
	if m.mode != modeRunning && m.mode != modeApproval {
		// 空闲时不显示状态栏内容。
		return StatusBarStyle.Width(m.width).Render("")
	}

	// 运行时：✶ 活动描述 (耗时 · ↓ token数 tokens)
	activity := m.activityText
	if activity == "" {
		activity = "运行中..."
	}

	// 闪烁的 spinner + 活动描述。
	bar := m.spin.View() + " " + activity

	// 括号内的统计信息。
	var stats []string
	elapsed := time.Since(m.startTime)
	stats = append(stats, formatDuration(elapsed))

	total := m.totalUsage.InputTokens + m.totalUsage.OutputTokens
	if total > 0 {
		stats = append(stats, fmt.Sprintf("↓ %s tokens", formatTokenCount(total)))
	}

	bar += " (" + strings.Join(stats, " · ") + ")"

	// 截断避免超过终端宽度导致换行。
	if m.width > 0 && lipgloss.Width(bar) > m.width-2 {
		bar = bar[:m.width-5] + "..."
	}
	return StatusBarStyle.Width(m.width).Render(bar)
}

// renderApprovalBox 渲染权限审批框。
func (m Model) renderApprovalBox() string {
	if m.curReq == nil {
		return ""
	}

	var b strings.Builder
	title := "需要权限"
	if m.curReq.Perm == PermDangerous {
		title = ApprovalWarning.Render("⚠  需要权限（危险操作）")
	}
	b.WriteString(title + "\n\n")
	b.WriteString(fmt.Sprintf("工具: %s\n", ToolNameStyle.Render(m.curReq.ToolName)))

	input := m.curReq.Input
	if len(input) > 200 {
		input = input[:200] + "..."
	}
	b.WriteString(fmt.Sprintf("输入: %s\n\n", input))

	options := "[y]确认  [n]拒绝"
	if m.curReq.Perm != PermDangerous {
		options += "  [a]始终允许"
	}
	b.WriteString(DimStyle.Render(options))

	return ApprovalBorder.Width(m.width - 4).Render(b.String())
}

// renderAskUserBox 渲染 AskUser 提问框。
func (m Model) renderAskUserBox() string {
	if m.curAsk == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("Agent 提问\n\n")
	b.WriteString(m.curAsk.Question + "\n\n")
	b.WriteString(InputPromptStyle.Render("> "))
	b.WriteString(m.input.View())

	return AskUserBorder.Width(m.width - 4).Render(b.String())
}

// ── 事件桥接 Cmd ────────────────────────────────────────────────────

// waitForAgentEvent 返回一个 Cmd，读取下一个 agent 事件。
func waitForAgentEvent(ch <-chan Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		return agentEventMsg{event: ev, done: !ok}
	}
}

// waitForApproval 返回一个 Cmd，读取下一个审批请求。
func waitForApproval(ch <-chan ApprovalRequest) tea.Cmd {
	return func() tea.Msg {
		req := <-ch
		return approvalMsg(req)
	}
}

// waitForAskUser 返回一个 Cmd，读取下一个 AskUser 请求。
func waitForAskUser(ch <-chan AskUserRequest) tea.Cmd {
	return func() tea.Msg {
		req := <-ch
		return askUserMsg(req)
	}
}

// ── 辅助函数 ────────────────────────────────────────────────────────

// toolActivityText 根据工具名和输入生成活动指示器文本。
// 对齐 Claude Code 的 "● Reading file..." 风格。
func toolActivityText(toolName, toolInput string) string {
	detail := extractToolDetail(toolName, toolInput)
	switch toolName {
	case "Read":
		if detail != "" {
			return "读取 " + detail
		}
		return "读取文件..."
	case "Write":
		if detail != "" {
			return "写入 " + detail
		}
		return "写入文件..."
	case "Edit":
		if detail != "" {
			return "编辑 " + detail
		}
		return "编辑文件..."
	case "Bash":
		if detail != "" {
			if len(detail) > 40 {
				detail = detail[:40] + "..."
			}
			return "执行 " + detail
		}
		return "执行命令..."
	case "Glob":
		return "搜索文件..."
	case "Grep":
		return "搜索内容..."
	case "AskUser":
		return "等待用户回答..."
	default:
		return fmt.Sprintf("执行 %s...", toolName)
	}
}

// formatDuration 格式化时间段。
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f秒", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%d分%d秒", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	return fmt.Sprintf("%d时%d分", h, m)
}

// formatTokenCount 格式化 token 数量（带 k/M 后缀）。
func formatTokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

// filterThinkTags 过滤流式文本中的 <think>...</think> 标签内容。
// 某些模型（如 deepseek、glm 等）会把思考过程放在 <think> 标签中输出。
// 支持标签跨 TextDelta chunk 分割的情况（通过 thinkBuf 缓冲部分匹配）。
func (m *Model) filterThinkTags(text string) string {
	// 将上次缓冲的部分标签与新文本拼接。
	if m.thinkBuf != "" {
		text = m.thinkBuf + text
		m.thinkBuf = ""
	}

	var result strings.Builder
	i := 0
	for i < len(text) {
		if m.inThinkBlock {
			// 在 think 块内，寻找 </think>。
			idx := strings.Index(text[i:], "</think>")
			if idx >= 0 {
				m.inThinkBlock = false
				i += idx + 8 // 跳过 </think>
				continue
			}
			// 可能 </think> 跨 chunk：检查末尾是否有 "<" 开头的部分匹配。
			tag := "</think>"
			for tail := 1; tail < len(tag) && tail <= len(text)-i; tail++ {
				if text[len(text)-tail:] == tag[:tail] {
					m.thinkBuf = text[len(text)-tail:]
					return result.String()
				}
			}
			// 整段都在 think 块内，全部丢弃。
			return result.String()
		}

		// 不在 think 块内，寻找 <think>。
		idx := strings.Index(text[i:], "<think>")
		if idx >= 0 {
			// 输出 <think> 之前的内容。
			result.WriteString(text[i : i+idx])
			m.inThinkBlock = true
			i += idx + 7 // 跳过 <think>
			continue
		}

		// 检查末尾是否有 "<" 开头的部分标签匹配（可能跨 chunk）。
		tag := "<think>"
		for tail := 1; tail < len(tag) && tail <= len(text)-i; tail++ {
			if text[len(text)-tail:] == tag[:tail] {
				// 将确定的部分输出，缓冲不确定的部分。
				result.WriteString(text[i : len(text)-tail])
				m.thinkBuf = text[len(text)-tail:]
				return result.String()
			}
		}

		// 无匹配，全部输出。
		result.WriteString(text[i:])
		break
	}
	return result.String()
}
