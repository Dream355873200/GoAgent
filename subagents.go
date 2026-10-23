// subagents.go 子代理治理：句柄、运行中指示、按子代理中断、查询。
//
// 每个子代理运行（同步或后台）启动时登记到会话运行时的子代理表，以父运行中
// Agent_* 调用的 tool_use_id 为 ID——前端已经用它关联调用卡片，同步子代理也
// 由此第一次变得可寻址。每个子代理运行有自己的插话车道，在它自己的工具批
// 边界消费：用户纠正一个跑了十分钟的子代理，指示在它的下一个工具批边界生效，
// 而不是等它整个结束、父循环回到边界时才生效。
//
//	app.SteerAgent(sessionID, agentID, "只看 src/ 目录")   // 运行中指示
//	app.InterruptAgent(sessionID, agentID, "方向错了")     // 只中断这个子代理，父运行继续
//	app.Subagents(sessionID)                              // 列出（运行中与最近结束的）
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/reminder"
)

// ErrAgentNotFound 表示会话中没有该 ID 的运行中子代理。
var ErrAgentNotFound = errors.New("goagent: 子代理不存在或已结束")

// 子代理状态。
const (
	AgentRunning   = "running"
	AgentDone      = "done"
	AgentFailed    = "failed"
	AgentCancelled = "cancelled"
)

// 子代理事件模式（WithSubagentStream）。
const (
	// SubagentStreamSummary 只发 subagent_progress（工具数、token、当前活动），默认。
	SubagentStreamSummary = "summary"
	// SubagentStreamFull 额外把子代理的正文与思考增量、工具调用与结果、插话转发进
	// 父运行的事件流，事件带 AgentPath；不进入父运行的对话记录。
	SubagentStreamFull = "full"
)

// DefaultMaxSubagents 是每个会话同时运行的子代理数上限的默认值。
const DefaultMaxSubagents = 8

// finishedAgentRetention 是结束的子代理在表中保留的时长（供查询结果与记录）。
const finishedAgentRetention = 5 * time.Minute

// SubagentInfo 是一个子代理运行的快照（App.Subagents、GET /agents、agent_status）。
type SubagentInfo struct {
	// ID 是父运行中 Agent_* 调用的 tool_use_id。
	ID string `json:"id"`
	// Path 是归属路径：父子代理的路径 + "." + ID；顶层派生的子代理就是 ID。
	Path   string `json:"path"`
	Name   string `json:"name"`
	Status string `json:"status"` // running | done | failed | cancelled
	// Activity 是当前活动（运行中）或错误原因（失败、取消）。
	Activity   string     `json:"activity,omitempty"`
	ToolUses   int        `json:"tool_uses"`
	Tokens     int        `json:"tokens"`
	Background bool       `json:"background"`
	TaskID     string     `json:"task_id,omitempty"` // 后台运行的 bgtask ID
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	// Result 是结束时的最终文本（截断到 2000 字节）。
	Result string `json:"result,omitempty"`
}

// WithSubagentStream 设置子代理事件并入父流的模式：SubagentStreamSummary（默认）或
// SubagentStreamFull。full 模式下客户端必须按 agent_path 区分子代理的正文与主
// agent 的正文。
func WithSubagentStream(mode string) Option {
	return optionFunc(func(c *appConfig) {
		c.subagentStream = mode
	})
}

// WithMaxSubagents 设置每个会话同时运行的子代理数上限（同步与后台合计，默认
// DefaultMaxSubagents）。超出时 Agent_* 调用返回错误，模型据此串行化。
func WithMaxSubagents(n int) Option {
	return optionFunc(func(c *appConfig) {
		c.maxSubagents = n
	})
}

// agentLane 是一个子代理运行自己的插话车道。
type agentLane struct {
	mu     sync.Mutex
	guides []string
	closed bool
}

// DrainGuides 实现 loop.SteerSource：子循环在自己的工具批边界取走指示。
func (l *agentLane) DrainGuides(string) []string {
	l.mu.Lock()
	raw := l.guides
	l.guides = nil
	l.mu.Unlock()
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, len(raw))
	for i, g := range raw {
		out[i] = reminder.Wrap(reminder.SourceSteer, g)
	}
	return out
}

func (l *agentLane) push(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.guides = append(l.guides, text)
	return true
}

// close 关闭车道并取走未送达的指示。
func (l *agentLane) close() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	left := l.guides
	l.guides = nil
	return left
}

// subagentRun 是子代理表中的一项。
type subagentRun struct {
	info     SubagentInfo
	parent   string // 父子代理的 ID（顶层派生为空）
	lane     *agentLane
	cancel   func(reason string)
	messages []message.Message
}

// subagentRegistry 是一个会话的子代理表。
type subagentRegistry struct {
	sessionID string

	mu    sync.Mutex
	runs  map[string]*subagentRun
	limit int
	full  bool
	hub   *SteeringHub
}

func newSubagentRegistry(sessionID string) *subagentRegistry {
	return &subagentRegistry{sessionID: sessionID, runs: map[string]*subagentRun{}, limit: DefaultMaxSubagents}
}

// configure 写入 App 级配置（每次运行开始时调用，运行时切换随之生效）。
func (r *subagentRegistry) configure(limit int, mode string, hub *SteeringHub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit > 0 {
		r.limit = limit
	} else {
		r.limit = DefaultMaxSubagents
	}
	r.full = mode == SubagentStreamFull
	r.hub = hub
}

func (r *subagentRegistry) fullStream() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full
}

// purgeLocked 清掉结束超过保留期的子代理。
func (r *subagentRegistry) purgeLocked(now time.Time) {
	for id, run := range r.runs {
		if run.info.EndedAt != nil && now.Sub(*run.info.EndedAt) > finishedAgentRetention {
			delete(r.runs, id)
		}
	}
}

// start 登记一个子代理运行。会话中运行中的子代理已达上限时返回错误。
func (r *subagentRegistry) start(id, parent, name string, background bool, cancel func(string)) (*subagentRun, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked(now)
	running := 0
	for _, run := range r.runs {
		if run.info.Status == AgentRunning {
			running++
		}
	}
	if running >= r.limit {
		return nil, fmt.Errorf("同时运行的子代理已达上限（%d 个），请等已有子代理结束后再派发，或把任务合并", r.limit)
	}
	path := id
	if parent != "" {
		if p := r.runs[parent]; p != nil {
			path = p.info.Path + "." + id
		}
	}
	run := &subagentRun{
		info: SubagentInfo{
			ID: id, Path: path, Name: name, Status: AgentRunning,
			Background: background, StartedAt: now,
		},
		parent: parent,
		lane:   &agentLane{},
		cancel: cancel,
	}
	r.runs[id] = run
	return run, nil
}

// progress 更新运行中子代理的进度。
func (r *subagentRegistry) progress(id, activity string, toolUses, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run := r.runs[id]; run != nil && run.info.Status == AgentRunning {
		run.info.Activity, run.info.ToolUses, run.info.Tokens = activity, toolUses, tokens
	}
}

// setTask 记录后台子代理的 bgtask ID。
func (r *subagentRegistry) setTask(id, taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run := r.runs[id]; run != nil {
		run.info.TaskID = taskID
	}
}

// finish 记下子代理的终态，关闭它的插话车道；未送达的指示转交给父运行。
func (r *subagentRegistry) finish(id, status, detail, result string, msgs []message.Message) {
	r.mu.Lock()
	run := r.runs[id]
	if run == nil || run.info.Status != AgentRunning {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	run.info.Status = status
	run.info.EndedAt = &now
	run.info.Activity = detail
	run.info.Result = truncateBytes(result, 2000)
	run.messages = msgs
	run.cancel = nil
	name, parent := run.info.Name, run.parent
	parentLane := (*agentLane)(nil)
	if p := r.runs[parent]; parent != "" && p != nil && p.info.Status == AgentRunning {
		parentLane = p.lane
	}
	hub := r.hub
	r.mu.Unlock()

	for _, g := range run.lane.close() {
		text := fmt.Sprintf("（发给子代理「%s」的指示在它结束前未送达）%s", name, g)
		if parentLane != nil && parentLane.push(text) {
			continue
		}
		if hub != nil && !ephemeralSession(r.sessionID) {
			if err := hub.Steer(r.sessionID, text); err != nil {
				hub.Enqueue(r.sessionID, reminder.Wrap(reminder.SourceSteer, text))
			}
		}
	}
}

// steer 向运行中的子代理投递指示。
func (r *subagentRegistry) steer(id, text string) error {
	r.mu.Lock()
	run := r.runs[id]
	r.mu.Unlock()
	if run == nil || !run.lane.push(text) {
		return ErrAgentNotFound
	}
	return nil
}

// interrupt 只中断该子代理（同步子代理的父运行收到错误结果后继续）。
func (r *subagentRegistry) interrupt(id, reason string) error {
	r.mu.Lock()
	run := r.runs[id]
	var cancel func(string)
	if run != nil && run.info.Status == AgentRunning {
		cancel = run.cancel
	}
	r.mu.Unlock()
	if cancel == nil {
		return ErrAgentNotFound
	}
	cancel(reason)
	return nil
}

// cancelAll 中断会话中全部运行中的子代理（CloseSession）。
func (r *subagentRegistry) cancelAll(reason string) {
	r.mu.Lock()
	var cancels []func(string)
	for _, run := range r.runs {
		if run.info.Status == AgentRunning && run.cancel != nil {
			cancels = append(cancels, run.cancel)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c(reason)
	}
}

func (r *subagentRegistry) list() []SubagentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked(time.Now())
	out := make([]SubagentInfo, 0, len(r.runs))
	for _, run := range r.runs {
		out = append(out, run.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (r *subagentRegistry) get(id string) (SubagentInfo, []message.Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[id]
	if run == nil {
		return SubagentInfo{}, nil, false
	}
	return run.info, run.messages, true
}

// agentIDKey 记录 ctx 所在的子代理（嵌套子代理据此算出归属路径）。
type agentIDKey struct{}

func withAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, id)
}

func agentIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(agentIDKey{}).(string)
	return id
}

// AgentPathFromContext 返回 ctx 所在子代理的归属路径（不在子代理里时为空）。
// 审批请求与提问据此带上 AgentPath。
func AgentPathFromContext(ctx context.Context) string {
	id := agentIDFrom(ctx)
	if id == "" {
		return ""
	}
	if rt := sessionRuntimeFrom(ctx); rt != nil {
		if info, _, ok := rt.agents.get(id); ok {
			return info.Path
		}
	}
	return id
}

// agentRegistryFor 返回会话已有运行时的子代理表（会话没有运行过时为 nil）。
func (a *App) agentRegistryFor(sessionID string) *subagentRegistry {
	if rt := a.existingRuntime(sessionID); rt != nil {
		return rt.agents
	}
	return nil
}

// Subagents 返回会话中的子代理：运行中的与最近 5 分钟内结束的，按开始时间排序。
func (a *App) Subagents(sessionID string) []SubagentInfo {
	if r := a.agentRegistryFor(sessionID); r != nil {
		return r.list()
	}
	return nil
}

// Subagent 返回一个子代理的快照与结束后保留的对话记录（运行中为 nil）。
func (a *App) Subagent(sessionID, agentID string) (SubagentInfo, []message.Message, bool) {
	if r := a.agentRegistryFor(sessionID); r != nil {
		return r.get(agentID)
	}
	return SubagentInfo{}, nil, false
}

// SteerAgent 向运行中的子代理投递指示：它在自己的下一个工具批边界看到这条
// 消息（作为插话提醒）。子代理在指示送达前结束时，指示转交给它的父运行。
// 找不到运行中的子代理时返回 ErrAgentNotFound（不会静默投给顶层运行）。
func (a *App) SteerAgent(sessionID, agentID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("goagent: SteerAgent 需要非空 text")
	}
	if r := a.agentRegistryFor(sessionID); r != nil {
		return r.steer(agentID, text)
	}
	return ErrAgentNotFound
}

// InterruptAgent 只中断指定的子代理：同步子代理的父运行收到"被用户中断"的
// 错误结果后继续；后台子代理按 TaskStop 处理。reason 会写进给父模型的结果。
func (a *App) InterruptAgent(sessionID, agentID, reason string) error {
	if r := a.agentRegistryFor(sessionID); r != nil {
		return r.interrupt(agentID, reason)
	}
	return ErrAgentNotFound
}

// agentInterrupt 是 InterruptAgent 取消子代理时的原因。
type agentInterrupt struct{ reason string }

func (e *agentInterrupt) Error() string           { return "子代理被用户中断" }
func (e *agentInterrupt) InterruptReason() string { return e.reason }

// interruptedAgentText 是被用户中断的子代理交给父模型的结果：明确说明，避免
// 父模型自动重试同一任务。
func interruptedAgentText(name, reason string) string {
	text := fmt.Sprintf("agent「%s」已被用户中断。不要自动重试同一任务；如需继续，先向用户确认。", name)
	if reason != "" {
		text += "中断原因：" + reason
	}
	return text
}

// AgentControlTools 返回父模型管理子代理的工具：agent_send（向运行中的后台
// 子代理发指示）与 agent_status（查看子代理状态）。WithSubAgents 自动注册；
// 用 App.AgentTool 手工装配子代理工具的宿主可以自行注册。
func (a *App) AgentControlTools() []NamedTool {
	return []NamedTool{
		{Name: "agent_send", Def: ToolDef{
			Description: "向运行中的子代理发送指示：它在下一个工具批边界看到这条消息并据此调整。" +
				"agent_id 是派发该子代理时 Agent_* 调用的 ID（agent_status 可查）。只对后台运行（run_in_background）的子代理有用——" +
				"同步子代理运行期间你在等待它的结果，无法调用本工具。",
			Input:      agentSendInput{},
			Permission: ReadOnly,
			Execute: func(ctx Context, in agentSendInput) (string, error) {
				if err := a.SteerAgent(ctx.SessionID, in.AgentID, in.Message); err != nil {
					return "", fmt.Errorf("子代理 %s 不在运行，指示未送达", in.AgentID)
				}
				return fmt.Sprintf("指示已送达子代理 %s，它会在下一个工具批边界看到。", in.AgentID), nil
			},
		}},
		{Name: "agent_status", Def: ToolDef{
			Description: "查看本会话子代理的状态：ID、名称、状态（running/done/failed/cancelled）、当前活动、工具调用数、token。" +
				"不带 agent_id 时列出全部（运行中与最近结束的）。",
			Input:      agentStatusInput{},
			Permission: ReadOnly,
			Concurrent: true,
			Execute: func(ctx Context, in agentStatusInput) (string, error) {
				var infos []SubagentInfo
				if in.AgentID != "" {
					info, _, ok := a.Subagent(ctx.SessionID, in.AgentID)
					if !ok {
						return "", fmt.Errorf("没有 ID 为 %s 的子代理", in.AgentID)
					}
					infos = []SubagentInfo{info}
				} else {
					infos = a.Subagents(ctx.SessionID)
				}
				if len(infos) == 0 {
					return "本会话没有运行中或最近结束的子代理。", nil
				}
				b, _ := json.MarshalIndent(infos, "", "  ")
				return string(b), nil
			},
		}},
	}
}

type agentSendInput struct {
	AgentID string `json:"agent_id" desc:"子代理 ID（派发它的 Agent_* 调用 ID）" required:"true"`
	Message string `json:"message" desc:"给子代理的指示" required:"true"`
}

type agentStatusInput struct {
	AgentID string `json:"agent_id,omitempty" desc:"只看这个子代理；省略时列出全部"`
}

// truncateBytes 截断到 n 字节以内的 UTF-8 边界。
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

var _ loop.SteerSource = (*agentLane)(nil)
