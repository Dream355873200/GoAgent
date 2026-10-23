// agui.go AG-UI 1.0 剖面：POST /agui 与 GET /agui/capabilities。
//
// 与 v1 剖面共用同一个内核与会话事件日志，只是出口的方言不同（映射见
// aguiMapper）。要点：
//
//   - threadId 就是 GoAgent 的会话 ID；会话记录是权威历史：请求里的 messages
//     按 id 去重，只有会话里没有的 user 消息作为本次输入，运行结束时发
//     MESSAGES_SNAPSHOT 让客户端对齐（消息 id 与会话记录的 UID 一致）。
//   - 审批与提问用"两次运行"桥接：内核在审批点阻塞等待（与 v1 相同），AG-UI
//     本次运行以 RUN_FINISHED{outcome: interrupt} 结束（停车）；客户端下一次
//     请求带 resume[] 作答（审批可带 editedArgs 改参），新的流从停车处接着
//     输出。停车超时按拒绝处理。
//   - 前端工具（RunAgentInput.tools）注入本次运行：模型调用时内核在该工具处
//     等待，AG-UI 本次运行以 RUN_FINISHED{outcome: success, pendingToolCallIds}
//     结束；客户端下一次请求以 role:"tool" 消息带回结果，同样从停车处接着输出。
//   - 扩展只走两条合法通道：CUSTOM 事件（goagent.* 名称）与 metadata 中带
//     goagent. 前缀的键。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/internal/safename"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/protocol/agui"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// AGUIConfig 是 AG-UI 剖面的配置（WithAGUI）。
type AGUIConfig struct {
	// ParkTimeout 是审批或提问停车的最长等待，超时按拒绝处理（提问得到空回答）。
	// 默认 30 分钟。
	ParkTimeout time.Duration
}

const defaultParkTimeout = 30 * time.Minute

// WithAGUI 设置 AG-UI 剖面（POST /agui）的参数。HTTP 模式下 /agui 总是可用，
// 不调用本选项时取默认值。
func WithAGUI(cfg AGUIConfig) Option {
	return optionFunc(func(c *appConfig) {
		c.agui = cfg
	})
}

// aguiPark 是一个停在中断或前端工具上的内核运行。
type aguiPark struct {
	kernelRunID string
	cursor      int64
	interrupts  map[string]string // interrupt id → "approval" | "ask"
	frontend    []string          // 等待前端返回结果的工具调用（按开始顺序）
	timer       *time.Timer
}

// aguiParks 登记各会话停车中的运行。
type aguiParks struct {
	mu    sync.Mutex
	parks map[string]*aguiPark
}

func (p *aguiParks) put(thread string, park *aguiPark) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.parks == nil {
		p.parks = map[string]*aguiPark{}
	}
	if old := p.parks[thread]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	p.parks[thread] = park
}

// take 取走会话的停车记录（resume 时调用）。
func (p *aguiParks) take(thread string) *aguiPark {
	p.mu.Lock()
	defer p.mu.Unlock()
	park := p.parks[thread]
	delete(p.parks, thread)
	if park != nil && park.timer != nil {
		park.timer.Stop()
	}
	return park
}

func (p *aguiParks) has(thread string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parks[thread] != nil
}

// arm 登记停车并设置超时（超时后拒绝它的全部中断、前端工具得到超时错误）。
func (p *aguiParks) arm(app *App, thread string, park *aguiPark) {
	park.timer = time.AfterFunc(app.aguiParkTimeout(), func() { p.expire(app, thread, park) })
	p.put(thread, park)
}

// expire 在停车超时后拒绝它的全部中断（停车已被 resume 取走时什么也不做）。
func (p *aguiParks) expire(app *App, thread string, park *aguiPark) {
	p.mu.Lock()
	if p.parks[thread] != park {
		p.mu.Unlock()
		return
	}
	delete(p.parks, thread)
	p.mu.Unlock()
	for id, kind := range park.interrupts {
		app.resolveInterrupt(thread, id, kind, aguiAnswer{reason: "等待用户审批超时"})
	}
	for _, id := range park.frontend {
		app.aguiFrontend.deliver(thread, id, aguiToolResult{err: "前端工具等待结果超时"})
	}
}

func (a *App) aguiParkTimeout() time.Duration {
	if t := a.config.agui.ParkTimeout; t > 0 {
		return t
	}
	return defaultParkTimeout
}

// aguiFrontendEvent 是前端工具开始等待结果时发出的内部事件：AG-UI 流据此停车。
const aguiFrontendEvent = "goagent.frontend_tool"

// aguiToolResult 是前端返回的工具结果（err 非空表示失败）。
type aguiToolResult struct {
	content string
	err     string
}

// aguiFrontendCalls 登记等待前端返回结果的工具调用。结果可能先于调用开始
// 等待到达（同一轮并行的前端工具，后一个还没开始执行），先存下来。
type aguiFrontendCalls struct {
	mu      sync.Mutex
	waiting map[string]chan aguiToolResult // thread|toolCallId
	early   map[string]aguiToolResult
}

func (f *aguiFrontendCalls) wait(ctx context.Context, thread, id string) (aguiToolResult, error) {
	key := thread + "|" + id
	f.mu.Lock()
	if r, ok := f.early[key]; ok {
		delete(f.early, key)
		f.mu.Unlock()
		return r, nil
	}
	if f.waiting == nil {
		f.waiting = map[string]chan aguiToolResult{}
	}
	ch := make(chan aguiToolResult, 1)
	f.waiting[key] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		if f.waiting[key] == ch {
			delete(f.waiting, key)
		}
		f.mu.Unlock()
	}()
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return aguiToolResult{}, ctx.Err()
	}
}

// deliver 交付一个前端工具结果（调用还没开始等待时先存下）。
func (f *aguiFrontendCalls) deliver(thread, id string, r aguiToolResult) {
	key := thread + "|" + id
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.waiting[key]; ok {
		delete(f.waiting, key)
		ch <- r
		return
	}
	if f.early == nil {
		f.early = map[string]aguiToolResult{}
	}
	f.early[key] = r
}

// forget 丢弃会话里没有被取走的结果（内核运行结束时）。
func (f *aguiFrontendCalls) forget(thread string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.early {
		if strings.HasPrefix(key, thread+"|") {
			delete(f.early, key)
		}
	}
}

// aguiToolNamePattern 是 provider 接受的工具名。
var aguiToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// aguiFrontendTools 把 RunAgentInput.tools 转为本次运行的工具：模型调用时向流
// 发出停车信号，等待前端在下一次请求中以 tool 消息带回结果。
func (a *App) aguiFrontendTools(thread string, tools []agui.Tool) ([]loop.ToolEntry, error) {
	var out []loop.ToolEntry
	for _, t := range tools {
		if !aguiToolNamePattern.MatchString(t.Name) {
			return nil, fmt.Errorf("前端工具名 %q 不合法（只能含字母、数字、_ 与 -，最长 64）", t.Name)
		}
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
			params = nil
			if err := json.Unmarshal(t.Parameters, &params); err != nil || params == nil {
				return nil, fmt.Errorf("前端工具 %s 的 parameters 不是 JSON Schema 对象", t.Name)
			}
		}
		name := t.Name
		out = append(out, loop.ToolEntry{
			Name:        name,
			Description: t.Description,
			InputSchema: params,
			Permission:  permission.LevelReadOnly,
			Class:       executor.ClassReadOnly,
			ExecuteFn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				id := observer.ToolCallIDFromContext(ctx)
				_ = emitInRun(ctx, aguiFrontendEvent, map[string]string{"toolCallId": id, "name": name})
				r, err := a.aguiFrontend.wait(ctx, thread, id)
				if err != nil {
					return "", fmt.Errorf("前端工具 %s 没有返回结果: %w", name, err)
				}
				if r.err != "" {
					return "", errors.New(r.err)
				}
				return r.content, nil
			},
		})
	}
	return out, nil
}

type runToolsKey struct{}

// withRunTools 让本次运行额外带上 tools（与已注册工具重名的不加入）。
func withRunTools(ctx context.Context, tools []loop.ToolEntry) context.Context {
	if len(tools) == 0 {
		return ctx
	}
	return context.WithValue(ctx, runToolsKey{}, tools)
}

func runToolsFrom(ctx context.Context) []loop.ToolEntry {
	tools, _ := ctx.Value(runToolsKey{}).([]loop.ToolEntry)
	return tools
}

// aguiToolResults 取出请求中回答 ids 的 tool 消息。
func aguiToolResults(in agui.RunAgentInput, ids []string) map[string]aguiToolResult {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := map[string]aguiToolResult{}
	for _, m := range in.Messages {
		if m.Role != "tool" || !want[m.ToolCallID] {
			continue
		}
		r := aguiToolResult{content: aguiContentText(m.Content)}
		if m.Error != "" {
			r.err = m.Error
			if r.content != "" {
				r.err += "\n" + r.content
			}
		}
		out[m.ToolCallID] = r
	}
	return out
}

// aguiOut 写出 AG-UI 事件：data: <json>\n\n。
type aguiOut struct {
	sw *sseWriter
}

func (o aguiOut) write(events ...agui.Event) {
	now := time.Now().UnixMilli()
	for _, ev := range events {
		if ev.Timestamp == 0 {
			ev.Timestamp = now
		}
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		o.sw.writeEvent([]byte("data: " + string(data) + "\n\n"))
	}
}

// serveAGUI 是 POST /agui：请求体为 RunAgentInput，响应为 AG-UI 事件流。
func serveAGUI(app *App, w http.ResponseWriter, r *http.Request) {
	var in agui.RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "无效的 RunAgentInput", http.StatusBadRequest)
		return
	}
	if in.ThreadID == "" || in.RunID == "" {
		http.Error(w, "threadId 与 runId 必填", http.StatusBadRequest)
		return
	}
	if err := safename.Check("threadId", in.ThreadID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sw, stop, ok := openSSE(w)
	if !ok {
		return
	}
	defer stop()
	out := aguiOut{sw: sw}
	out.write(agui.Event{Type: agui.TypeRunStarted, ThreadID: in.ThreadID, RunID: in.RunID, ProtocolVersion: agui.ProtocolVersion})
	runError := func(code, msg string) {
		out.write(agui.Event{Type: agui.TypeRunError, Code: code, Message: msg})
	}

	mgr := app.config.sessionManager
	if len(in.Resume) > 0 || app.aguiParks.has(in.ThreadID) {
		app.continueAGUI(r.Context(), out, in, mgr, runError)
		return
	}
	if mgr != nil && mgr.IsBusy(in.ThreadID) {
		runError("thread_busy", fmt.Sprintf("会话 %s 正在运行中", in.ThreadID))
		return
	}
	input, inputUID, err := aguiInput(r.Context(), mgr, in)
	if err != nil {
		runError("no_input", err.Error())
		return
	}
	frontTools, err := app.aguiFrontendTools(in.ThreadID, in.Tools)
	if err != nil {
		runError("invalid_tools", err.Error())
		return
	}

	kernelRunID := newRequestID("run")
	watcher := app.WatchSession(in.ThreadID, WatchFromNow)
	defer watcher.Close()
	runCtx, cancelRun := context.WithCancelCause(context.WithoutCancel(r.Context()))
	unregister := func() {}
	if app.interruptHandler != nil {
		unregister = app.interruptHandler.RegisterCause(in.ThreadID, cancelRun)
	}
	runCtx = withRunTools(withInputUID(withRunID(runCtx, kernelRunID), inputUID), frontTools)
	var events <-chan Event
	if mgr != nil {
		events = app.RunSession(runCtx, in.ThreadID, input)
	} else {
		events = app.RunWithSessionHistory(runCtx, in.ThreadID, nil, input)
	}
	first, ok := <-events
	if !watcher.log.hasRun(kernelRunID) {
		defer cancelRun(nil)
		defer unregister()
		msg := "运行没有开始"
		if ok && first.Type == EventError && first.Error != nil {
			msg = first.Error.Error()
		}
		for range events {
		}
		runError("run_not_started", msg)
		return
	}
	go func() {
		defer cancelRun(nil)
		defer unregister()
		for range events {
		}
	}()
	app.streamAGUIRun(r.Context(), out, watcher, in, kernelRunID, false, nil)
}

// continueAGUI 处理接着停车运行的请求：resume 回答中断、tool 消息带回前端工具
// 结果。作答后仍有没回答的中断或前端工具时立即再次停车；否则从停车处接着输出。
func (a *App) continueAGUI(ctx context.Context, out aguiOut, in agui.RunAgentInput, mgr *session.Manager, runError func(code, msg string)) {
	thread := in.ThreadID
	park := a.aguiParks.take(thread)
	if park == nil {
		runError("no_pending_interrupt", "该会话没有等待作答的中断（可能已超时）")
		return
	}
	reject := func(code, msg string) {
		a.aguiParks.arm(a, thread, park)
		runError(code, msg)
	}
	results := aguiToolResults(in, park.frontend)
	if len(in.Resume) == 0 && len(results) == 0 {
		if len(park.interrupts) > 0 {
			reject("pending_interrupts", "该会话有等待作答的中断：请在 resume 中回答它们")
		} else {
			reject("pending_tool_calls", "该会话在等待前端工具的结果："+strings.Join(park.frontend, ", ")+"（以 role:\"tool\" 消息带回）")
		}
		return
	}
	// 会话记录逐条落盘时才能认出哪些 user 消息是新的。
	if mgr != nil && (a.config.autoPersist == nil || *a.config.autoPersist) {
		if parts, _ := aguiNewUserMessages(ctx, mgr, in); len(parts) > 0 {
			reject("thread_busy", "运行停在中断或前端工具上：先作答（resume / tool 消息），再发新消息")
			return
		}
	}
	for _, entry := range in.Resume {
		if _, known := park.interrupts[entry.InterruptID]; !known {
			reject("unknown_interrupt", fmt.Sprintf("未知的中断 %s", entry.InterruptID))
			return
		}
	}
	for _, entry := range in.Resume {
		kind := park.interrupts[entry.InterruptID]
		a.resolveInterrupt(thread, entry.InterruptID, kind, parseResume(kind, entry))
		delete(park.interrupts, entry.InterruptID)
	}
	var remaining []string
	for _, id := range park.frontend {
		if r, ok := results[id]; ok {
			a.aguiFrontend.deliver(thread, id, r)
		} else {
			remaining = append(remaining, id)
		}
	}
	m := newAGUIMapper(a, thread)
	if len(remaining) > 0 || len(a.pendingInterrupts(thread)) > 0 {
		a.parkAGUI(out, m, in, park.kernelRunID, park.cursor, remaining)
		return
	}
	watcher := a.WatchSession(thread, park.cursor)
	defer watcher.Close()
	answered := map[string]bool{}
	for id := range results {
		answered[id] = true
	}
	a.streamAGUIRun(ctx, out, watcher, in, park.kernelRunID, true, answered)
}

// parkAGUI 让本次 AG-UI 运行停车结束：有待作答的审批或提问时以 interrupt 结束，
// 否则以 success + pendingToolCallIds（等待结果的前端工具）结束。内核运行在原处
// 继续等待，下一次请求从 cursor 接着输出。
func (a *App) parkAGUI(out aguiOut, m *aguiMapper, in agui.RunAgentInput, kernelRunID string, cursor int64, frontend []string) {
	interrupts := a.pendingInterrupts(in.ThreadID)
	out.write(m.closeAll()...)
	park := &aguiPark{kernelRunID: kernelRunID, cursor: cursor, interrupts: map[string]string{}, frontend: append([]string(nil), frontend...)}
	expires := time.Now().Add(a.aguiParkTimeout()).UTC().Format(time.RFC3339)
	for i := range interrupts {
		interrupts[i].ExpiresAt = expires
		park.interrupts[interrupts[i].ID] = interruptKind(interrupts[i].Reason)
	}
	a.aguiParks.arm(a, in.ThreadID, park)
	finished := agui.Event{Type: agui.TypeRunFinished, ThreadID: in.ThreadID, RunID: in.RunID, Usage: m.usage()}
	if len(interrupts) > 0 {
		finished.Outcome = &agui.RunOutcome{Type: "interrupt", Interrupts: interrupts}
	} else {
		finished.Outcome = &agui.RunOutcome{Type: "success", PendingToolCallIDs: park.frontend}
	}
	out.write(finished)
}

// streamAGUIRun 把内核运行 kernelRunID 的会话日志帧映射为 AG-UI 事件写出，
// 直到运行结束、遇到需要外部作答的中断或前端工具（停车）或连接断开。
// fromCursor 为 true 时游标已经在运行之中（resume），不需要先找运行开始条目。
// answered 是本次请求带回结果的前端工具调用：客户端已有结果，不再发 TOOL_CALL_RESULT。
func (a *App) streamAGUIRun(ctx context.Context, out aguiOut, w *SessionWatcher, in agui.RunAgentInput, kernelRunID string, fromCursor bool, answered map[string]bool) {
	m := newAGUIMapper(a, in.ThreadID)
	started := fromCursor
	frontendNames := map[string]bool{}
	for _, t := range in.Tools {
		frontendNames[t.Name] = true
	}
	var open []string // 本次流里开始、还没有结果的前端工具调用
	for {
		ev, err := w.Next(ctx)
		if err != nil {
			if errors.Is(err, ErrSessionStreamClosed) {
				out.write(m.closeAll()...)
				out.write(agui.Event{Type: agui.TypeRunError, Code: "session_closed", Message: "会话已关闭"})
			}
			return
		}
		if !started {
			if (ev.Kind == SessionRunStarted && ev.RunID == kernelRunID) || ev.Kind == SessionResync {
				started = true
			}
			continue
		}
		if ev.Kind == SessionRunEvent && ev.Event.AgentPath == "" {
			switch e := ev.Event; {
			case e.Type == EventToolStart && frontendNames[e.ToolName]:
				open = append(open, e.ToolUseID)
			case e.Type == EventToolDone:
				open = slices.DeleteFunc(open, func(id string) bool { return id == e.ToolUseID })
				if answered[e.ToolUseID] {
					continue
				}
			}
		}
		switch {
		case ev.Kind == SessionRunFinished && ev.RunID == kernelRunID:
			a.aguiFrontend.forget(in.ThreadID)
			out.write(m.closeAll()...)
			if snap := a.aguiSnapshot(ctx, in.ThreadID); snap != nil {
				out.write(*snap)
			}
			finished := agui.Event{Type: agui.TypeRunFinished, ThreadID: in.ThreadID, RunID: in.RunID, Usage: m.usage()}
			if m.cancelled {
				finished.Outcome = &agui.RunOutcome{Type: "cancelled"}
			}
			out.write(finished)
			return
		case ev.Kind == SessionRunEvent && ev.Event.Type == EventError && ev.Event.AgentPath == "":
			out.write(m.closeAll()...)
			out.write(agui.Event{Type: agui.TypeRunError, Code: "agent_error", Message: errString(ev.Event.Error), Usage: m.usage()})
			return
		case ev.Kind == SessionRunEvent && (ev.Event.Type == EventNeedApproval || ev.Event.Type == EventAskUser):
			if len(a.pendingInterrupts(in.ThreadID)) == 0 {
				continue // 已经作答（resume 之后重放到它）
			}
			a.parkAGUI(out, m, in, kernelRunID, w.Cursor(), open)
			return
		case ev.Kind == SessionRunEvent && ev.Event.Type == EventCustom && ev.Event.CustomName == aguiFrontendEvent:
			if len(open) == 0 && len(a.pendingInterrupts(in.ThreadID)) == 0 {
				continue // 结果已经带回（停车之后重放到它）
			}
			a.parkAGUI(out, m, in, kernelRunID, w.Cursor(), open)
			return
		default:
			out.write(m.mapEntry(ev)...)
		}
		if out.sw.isClosed() {
			return
		}
	}
}

func interruptKind(reason string) string {
	if reason == "ask_user" {
		return "ask"
	}
	return "approval"
}

// pendingInterrupts 把会话当前等待作答的审批与提问转为 AG-UI 中断。
func (a *App) pendingInterrupts(sessionID string) []agui.Interrupt {
	var out []agui.Interrupt
	if ph, ok := a.config.approver.(*PermissionHandler); ok {
		for _, req := range ph.PendingBySession(sessionID) {
			out = append(out, agui.Interrupt{
				ID:         req.RequestID,
				Reason:     "tool_call",
				Message:    fmt.Sprintf("是否允许调用工具 %s？", req.ToolName),
				ToolCallID: req.ToolUseID,
				ResponseSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"approved":   map[string]any{"type": "boolean"},
						"reason":     map[string]any{"type": "string"},
						"editedArgs": map[string]any{"type": "object", "description": "批准时替换工具参数（完整替换）"},
					},
					"required": []any{"approved"},
				},
				SubagentRunID: req.AgentPath,
				Metadata:      agui.Metadata{"goagent.toolName": req.ToolName, "goagent.toolInput": json.RawMessage(req.ToolInput), "goagent.permission": req.Permission},
			})
		}
	}
	if h := a.askUserHandler; h != nil {
		for _, req := range h.pendingAllBySession(sessionID) {
			in := agui.Interrupt{
				ID:         req.RequestID,
				Reason:     "ask_user",
				Message:    req.Question,
				ToolCallID: req.ToolUseID,
				ResponseSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"answer": map[string]any{"type": "string"}},
					"required":   []any{"answer"},
				},
				SubagentRunID: req.AgentPath,
			}
			if len(req.Payload) > 0 {
				in.Metadata = agui.Metadata{"goagent.payload": req.Payload}
			}
			out = append(out, in)
		}
	}
	return out
}

// aguiAnswer 是对一个中断的作答。
type aguiAnswer struct {
	allow  bool
	answer string
	reason string
	edited json.RawMessage // 批准时替换工具参数（editedArgs）
}

// parseResume 解析一条 resume：审批取 payload.approved（或布尔值）与可选的
// payload.editedArgs，提问取 payload.answer（或字符串）。status=cancelled 视为
// 拒绝 / 取消回答。
func parseResume(kind string, entry agui.ResumeEntry) aguiAnswer {
	if entry.Status == "cancelled" {
		return aguiAnswer{answer: "（用户取消了回答）", reason: "用户取消"}
	}
	var obj struct {
		Approved   *bool           `json:"approved"`
		Answer     string          `json:"answer"`
		Reason     string          `json:"reason"`
		EditedArgs json.RawMessage `json:"editedArgs"`
	}
	if kind == "ask" {
		var s string
		if json.Unmarshal(entry.Payload, &s) == nil {
			return aguiAnswer{answer: s}
		}
		_ = json.Unmarshal(entry.Payload, &obj)
		return aguiAnswer{answer: obj.Answer}
	}
	var b bool
	if json.Unmarshal(entry.Payload, &b) == nil {
		return aguiAnswer{allow: b, reason: "用户拒绝"}
	}
	if json.Unmarshal(entry.Payload, &obj) == nil && obj.Approved != nil {
		ans := aguiAnswer{allow: *obj.Approved, reason: obj.Reason}
		if ans.reason == "" && !ans.allow {
			ans.reason = "用户拒绝"
		}
		if ans.allow && len(obj.EditedArgs) > 0 && string(obj.EditedArgs) != "null" {
			ans.edited = obj.EditedArgs
		}
		return ans
	}
	return aguiAnswer{reason: "用户拒绝"}
}

// resolveInterrupt 把作答交给阻塞中的审批点或提问。
func (a *App) resolveInterrupt(sessionID, id, kind string, ans aguiAnswer) {
	if kind == "ask" {
		if h := a.askUserHandler; h != nil {
			for _, req := range h.pendingAllBySession(sessionID) {
				if req.RequestID == id {
					h.Resolve(id, ans.answer)
				}
			}
		}
		return
	}
	if ph, ok := a.config.approver.(*PermissionHandler); ok {
		if ans.allow && len(ans.edited) > 0 {
			ph.ResolveWithInput(sessionID, id, ans.edited)
			return
		}
		ph.ResolveForSession(sessionID, id, ans.allow, false, ans.reason)
	}
}

// aguiInput 从请求的 messages 中取出本次输入：会话记录里还没有的 user 消息
// （按 id 去重）。多条时按顺序合并，沿用最后一条的 id。context 作为宿主提醒附在后面。
func aguiInput(ctx context.Context, mgr *session.Manager, in agui.RunAgentInput) (input, uid string, err error) {
	parts, uid := aguiNewUserMessages(ctx, mgr, in)
	if len(parts) == 0 {
		return "", "", errors.New("messages 中没有会话里尚不存在的 user 消息")
	}
	input = strings.Join(parts, "\n\n")
	if len(in.Context) > 0 {
		var b strings.Builder
		b.WriteString("\n\n<context>")
		for _, c := range in.Context {
			fmt.Fprintf(&b, "\n%s: %s", c.Description, c.Value)
		}
		b.WriteString("\n</context>")
		input += b.String()
	}
	return input, uid, nil
}

// aguiNewUserMessages 返回请求中会话记录里还没有的 user 消息文本与最后一条的 id。
func aguiNewUserMessages(ctx context.Context, mgr *session.Manager, in agui.RunAgentInput) (parts []string, uid string) {
	known := map[string]bool{}
	if mgr != nil {
		if sess, _ := mgr.Get(ctx, in.ThreadID); sess != nil {
			for _, m := range sess.Messages {
				if m.UID != "" {
					known[m.UID] = true
				}
			}
		}
	}
	for _, m := range in.Messages {
		if m.Role != "user" || known[m.ID] {
			continue
		}
		if text := aguiContentText(m.Content); text != "" {
			parts = append(parts, text)
			uid = m.ID
		}
	}
	return parts, uid
}

// aguiContentText 取 user 消息内容中的文本：字符串，或内容块数组中的 text 块。
func aguiContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if obj, ok := p.(map[string]any); ok && obj["type"] == "text" {
				if s, ok := obj["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// aguiSnapshot 把会话记录转为 MESSAGES_SNAPSHOT（没有会话存储时为 nil）。
func (a *App) aguiSnapshot(ctx context.Context, thread string) *agui.Event {
	mgr := a.config.sessionManager
	if mgr == nil {
		return nil
	}
	sess, err := mgr.Get(context.WithoutCancel(ctx), thread)
	if err != nil || sess == nil {
		return nil
	}
	msgs := aguiMessages(sess.Messages)
	if len(msgs) == 0 {
		return nil
	}
	return &agui.Event{Type: agui.TypeMessagesSnapshot, Messages: msgs}
}

// aguiMessages 把会话记录转为 AG-UI 消息：思考成为 reasoning 消息，工具结果
// 成为 tool 消息，插话等系统提醒成为 developer 消息；元消息与压缩摘要不展示。
func aguiMessages(transcript []message.Message) []agui.Message {
	var out []agui.Message
	for i, m := range transcript {
		if m.IsMeta || m.IsCompactSummary {
			continue
		}
		id := m.UID
		if id == "" {
			id = fmt.Sprintf("goagent-msg-%d", i)
		}
		switch m.Role {
		case message.RoleAssistant:
			if thinking := message.ExtractThinking(m); thinking != "" {
				out = append(out, agui.Message{ID: id + "-r", Role: "reasoning", Content: thinking})
			}
			am := agui.Message{ID: id, Role: "assistant"}
			if text := message.ExtractText(m); text != "" {
				am.Content = text
			}
			for _, tc := range message.ExtractToolCalls(m) {
				args := string(tc.Input)
				if args == "" {
					args = "{}"
				}
				am.ToolCalls = append(am.ToolCalls, agui.ToolCall{ID: tc.ID, Type: "function", Function: agui.FunctionCall{Name: tc.Name, Arguments: args}})
			}
			out = append(out, am)
		case message.RoleUser:
			if len(m.Content) == 1 && m.Content[0].Type == "tool_result" {
				b := m.Content[0]
				tm := agui.Message{ID: id, Role: "tool", Content: b.Text, ToolCallID: b.ForToolUseID}
				if b.IsError {
					tm.Error = b.Text
				}
				out = append(out, tm)
				continue
			}
			text := message.ExtractText(m)
			if text == "" {
				continue
			}
			role := "user"
			if strings.HasPrefix(strings.TrimSpace(text), "<system-reminder") {
				role = "developer"
			}
			out = append(out, agui.Message{ID: id, Role: role, Content: text})
		}
	}
	return out
}

// aguiMapper 把一次 AG-UI 运行期间的会话日志条目映射为 AG-UI 事件，维护
// 开闭配对：同一归属（主 agent 或某个子代理）的文本消息、推理一次只开一个，
// 工具调用开始前、轮次结束时、运行结束前全部关闭。
type aguiMapper struct {
	app       *App
	thread    string
	text      map[string]string // 归属路径 → 打开的文本消息 id
	reasoning map[string]string // 归属路径 → 打开的推理消息 id
	subagents map[string]bool   // 已发 SUBAGENT_STARTED 且未结束的子代理
	tokens    provider.Usage
	cancelled bool
}

func newAGUIMapper(app *App, thread string) *aguiMapper {
	return &aguiMapper{app: app, thread: thread, text: map[string]string{}, reasoning: map[string]string{}, subagents: map[string]bool{}}
}

func (m *aguiMapper) usage() []agui.TokenUsage {
	u := m.tokens
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return []agui.TokenUsage{{
		Model:                 m.app.ModelID(),
		InputTokens:           u.InputTokens,
		OutputTokens:          u.OutputTokens,
		TotalTokens:           u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreateTokens,
		CachedInputTokens:     u.CacheReadTokens,
		CacheWriteInputTokens: u.CacheCreateTokens,
	}}
}

func strPtr(s string) *string { return &s }

// closePath 关闭某个归属的文本消息与推理。
func (m *aguiMapper) closePath(path string) []agui.Event {
	var out []agui.Event
	if id := m.text[path]; id != "" {
		out = append(out, agui.Event{Type: agui.TypeTextMessageEnd, MessageID: id, SubagentRunID: path})
		delete(m.text, path)
	}
	if id := m.reasoning[path]; id != "" {
		out = append(out,
			agui.Event{Type: agui.TypeReasoningMessageEnd, MessageID: id, SubagentRunID: path},
			agui.Event{Type: agui.TypeReasoningEnd, MessageID: id, SubagentRunID: path})
		delete(m.reasoning, path)
	}
	return out
}

// closeAll 关闭一切打开的消息、推理与子代理（RUN_FINISHED / RUN_ERROR / 停车之前）。
func (m *aguiMapper) closeAll() []agui.Event {
	var out []agui.Event
	paths := map[string]bool{}
	for p := range m.text {
		paths[p] = true
	}
	for p := range m.reasoning {
		paths[p] = true
	}
	for _, p := range sortedKeys(paths) {
		out = append(out, m.closePath(p)...)
	}
	for _, p := range sortedKeys(m.subagents) {
		out = append(out, agui.Event{Type: agui.TypeSubagentFinished, SubagentRunID: p, SubagentOutcome: &agui.SubagentOutcome{Type: "suspended"}})
		delete(m.subagents, p)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ensureSubagent 在子代理的第一个事件之前发 SUBAGENT_STARTED。
func (m *aguiMapper) ensureSubagent(path, name string) []agui.Event {
	if path == "" || m.subagents[path] {
		return nil
	}
	m.subagents[path] = true
	id, parent := path, ""
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		id, parent = path[i+1:], path[:i]
	}
	if name == "" {
		if info, _, ok := m.app.Subagent(m.thread, id); ok {
			name = info.Name
		} else {
			name = id
		}
	}
	return []agui.Event{{Type: agui.TypeSubagentStarted, SubagentRunID: path, Name: name, ParentToolCallID: id, ParentSubagentRunID: parent}}
}

func custom(name string, value any) agui.Event {
	return agui.Event{Type: agui.TypeCustom, Name: name, Value: value}
}

// mapEntry 映射一条会话日志条目（运行开始/结束与中断由调用方处理）。
func (m *aguiMapper) mapEntry(entry SessionEvent) []agui.Event {
	var out []agui.Event
	switch entry.Kind {
	case SessionResync:
		out = append(out, custom("goagent.resync", map[string]any{"oldest": entry.Oldest}))
	case SessionRunEvent:
		out = m.mapEvent(entry.Event)
	}
	for i := range out {
		if entry.Seq > 0 {
			if out[i].Metadata == nil {
				out[i].Metadata = agui.Metadata{}
			}
			out[i].Metadata["goagent.seq"] = entry.Seq
		}
	}
	return out
}

func (m *aguiMapper) mapEvent(ev Event) []agui.Event {
	path := ev.AgentPath
	var out []agui.Event
	if path != "" && ev.Type != EventSubAgentProgress {
		out = append(out, m.ensureSubagent(path, "")...)
	}
	switch ev.Type {
	case EventTextDelta:
		id := ev.MessageID
		if id == "" {
			id = "goagent-text-" + path
		}
		if m.reasoning[path] != "" {
			out = append(out, m.closePath(path)...)
		}
		if m.text[path] != id {
			out = append(out, m.closePath(path)...)
			out = append(out, agui.Event{Type: agui.TypeTextMessageStart, MessageID: id, Role: "assistant", SubagentRunID: path})
			m.text[path] = id
		}
		out = append(out, agui.Event{Type: agui.TypeTextMessageContent, MessageID: id, Delta: strPtr(ev.Text), SubagentRunID: path})
	case EventThinking:
		id := ev.MessageID + "-r"
		if m.text[path] != "" || (m.reasoning[path] != "" && m.reasoning[path] != id) {
			out = append(out, m.closePath(path)...)
		}
		if m.reasoning[path] != id {
			out = append(out,
				agui.Event{Type: agui.TypeReasoningStart, MessageID: id, SubagentRunID: path},
				agui.Event{Type: agui.TypeReasoningMessageStart, MessageID: id, Role: "reasoning", SubagentRunID: path})
			m.reasoning[path] = id
		}
		out = append(out, agui.Event{Type: agui.TypeReasoningMessageContent, MessageID: id, Delta: strPtr(ev.Thinking), SubagentRunID: path})
	case EventToolStart:
		out = append(out, m.closePath(path)...)
		args := string(ev.ToolInput)
		if args == "" {
			args = "{}"
		}
		out = append(out,
			agui.Event{Type: agui.TypeToolCallStart, ToolCallID: ev.ToolUseID, ToolCallName: ev.ToolName, ParentMessageID: ev.MessageID, SubagentRunID: path},
			agui.Event{Type: agui.TypeToolCallArgs, ToolCallID: ev.ToolUseID, Delta: strPtr(args), SubagentRunID: path},
			agui.Event{Type: agui.TypeToolCallEnd, ToolCallID: ev.ToolUseID, SubagentRunID: path})
	case EventToolDone:
		id := ev.MessageID
		if id == "" {
			id = "goagent-result-" + ev.ToolUseID
		}
		res := agui.Event{Type: agui.TypeToolCallResult, MessageID: id, ToolCallID: ev.ToolUseID, Content: ev.ToolResult, Role: "tool", SubagentRunID: path}
		if ev.ToolIsError {
			res.Metadata = agui.Metadata{"goagent.toolIsError": true}
		}
		out = append(out, res)
	case EventTurnComplete:
		out = append(out, m.closePath(path)...)
	case EventSubAgentProgress:
		out = append(out, m.mapSubagentProgress(ev)...)
	case EventUsageUpdate:
		if ev.Usage != nil && path == "" {
			m.tokens.InputTokens += ev.Usage.InputTokens
			m.tokens.OutputTokens += ev.Usage.OutputTokens
			m.tokens.CacheReadTokens += ev.Usage.CacheReadTokens
			m.tokens.CacheCreateTokens += ev.Usage.CacheCreateTokens
			out = append(out, custom("goagent.usage", map[string]any{
				"inputTokens": ev.Usage.InputTokens, "outputTokens": ev.Usage.OutputTokens,
				"cachedInputTokens": ev.Usage.CacheReadTokens, "cacheWriteInputTokens": ev.Usage.CacheCreateTokens,
			}))
		}
	case EventProgress:
		if ev.StatusKey != "" {
			out = append(out, agui.Event{Type: agui.TypeActivitySnapshot, MessageID: "goagent-status-" + ev.StatusKey,
				ActivityType: "goagent.status", Content: map[string]any{"text": ev.Text, "statusKey": ev.StatusKey}, SubagentRunID: path})
		} else {
			out = append(out, custom("goagent.progress", map[string]any{"text": ev.Text}))
		}
	case EventCompaction:
		out = append(out, custom("goagent.compaction", map[string]any{"text": ev.Text}))
	case EventRetrieval:
		out = append(out, custom("goagent.retrieval", map[string]any{"text": ev.Text}))
	case EventSteer:
		out = append(out, custom("goagent.steer", map[string]any{"text": ev.Text}))
	case EventQueueRun:
		out = append(out, m.closePath("")...)
		out = append(out, custom("goagent.queueRun", map[string]any{"text": ev.Text}))
	case EventCustom:
		var v any = map[string]any{}
		if len(ev.CustomValue) > 0 {
			var decoded any
			if json.Unmarshal(ev.CustomValue, &decoded) == nil && decoded != nil {
				v = decoded
			}
		}
		e := custom(ev.CustomName, v)
		e.SubagentRunID = path
		out = append(out, e)
	case EventInterrupted:
		if path == "" {
			m.cancelled = true
		}
	}
	return out
}

// mapSubagentProgress 把 subagent_progress 映射为子代理生命周期：框架子代理
// （带归属路径）发 SUBAGENT_STARTED / FINISHED / ERROR，进度本身走 CUSTOM；
// 宿主自己发的进度（团队成员、流水线节点，没有归属路径）只走 CUSTOM。
func (m *aguiMapper) mapSubagentProgress(ev Event) []agui.Event {
	progress := custom("goagent.subagentProgress", map[string]any{
		"agentId": ev.AgentID, "agentPath": ev.AgentPath, "name": ev.AgentDesc, "status": ev.AgentStatus,
		"activity": ev.AgentActivity, "toolUses": ev.AgentToolUses, "tokens": ev.AgentTokens,
	})
	path := ev.AgentPath
	if path == "" {
		return []agui.Event{progress}
	}
	out := m.ensureSubagent(path, ev.AgentDesc)
	switch ev.AgentStatus {
	case AgentDone:
		out = append(out, m.closePath(path)...)
		out = append(out, progress, agui.Event{Type: agui.TypeSubagentFinished, SubagentRunID: path, SubagentOutcome: &agui.SubagentOutcome{Type: "success"}})
		delete(m.subagents, path)
	case AgentFailed, AgentCancelled:
		out = append(out, m.closePath(path)...)
		msg := ev.AgentActivity
		if msg == "" {
			msg = ev.AgentStatus
		}
		out = append(out, progress, agui.Event{Type: agui.TypeSubagentError, SubagentRunID: path, Message: msg, Code: ev.AgentStatus})
		delete(m.subagents, path)
	default:
		out = append(out, progress)
	}
	return out
}

// serveAGUICapabilities 是 GET /agui/capabilities。
func serveAGUICapabilities(app *App, w http.ResponseWriter) {
	_, hitl := app.config.approver.(*PermissionHandler)
	caps := agui.AgentCapabilities{
		Identity:       &agui.IdentityCapabilities{Name: "goagent", Type: "goagent", Version: clientVersionString},
		Transport:      &agui.TransportCapabilities{Streaming: true, Resumable: false},
		Tools:          &agui.ToolsCapabilities{Supported: true, ParallelCalls: true, ClientProvided: true},
		MultiAgent:     &agui.MultiAgentCapabilities{Supported: true, Delegation: true, Handoffs: false},
		Reasoning:      &agui.ReasoningCapabilities{Supported: true, Streaming: true, Encrypted: false},
		HumanInTheLoop: &agui.HumanInTheLoopCapabilities{Supported: hitl || app.askUserHandler != nil, Approvals: hitl, Interrupts: true, ApproveWithEdits: hitl},
		Custom:         map[string]any{"goagent.customEvents": app.RegisteredEvents(), "goagent.v1Protocol": true},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(caps)
}

// clientVersionString 是自报的框架版本。
const clientVersionString = "0.14.0"

// inputUIDBox 让会话运行的第一条输入沿用指定的 UID（AG-UI 客户端的消息 id）；
// 排队续跑的后续输入不沿用。
type inputUIDBox struct {
	mu   sync.Mutex
	uid  string
	used bool
}

func (b *inputUIDBox) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used {
		return ""
	}
	b.used = true
	return b.uid
}

type inputUIDKey struct{}

func withInputUID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, inputUIDKey{}, &inputUIDBox{uid: uid})
}

func inputUIDFrom(ctx context.Context) string {
	if b, ok := ctx.Value(inputUIDKey{}).(*inputUIDBox); ok {
		return b.take()
	}
	return ""
}
