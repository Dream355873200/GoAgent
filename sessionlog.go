// sessionlog.go 会话事件日志。
//
// 每个会话一条只追加的事件日志：运行开始与结束、运行中的每个事件、会话级
// 交互请求（审批、提问）。条目带会话内单调递增的 seq，只保留最近一段（环形
// 缓冲，按条数与估算字节数双上限），读取方用游标（SessionWatcher）按 seq
// 顺序消费：发布永不阻塞在慢读取方上，断线后从上次读到的 seq 继续读即可。
//
// HTTP 的 POST /chat 与 GET /runs/{session}/stream 都从这里写出帧；进程内宿主
// 经 App.WatchSession 使用同一份日志（例如在自己的 WebSocket 协议上做断线
// 续传）。只有会话运行（RunSession、RunWithSessionHistory）会写入日志，无
// 会话身份的 Run / RunWithHistory 不写。
package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// SessionEventKind 是会话事件日志条目的种类。
type SessionEventKind int

const (
	// SessionRunStarted 一次会话运行开始（RunSession / RunWithSessionHistory
	// 取得会话之后）。RunID 标识这次运行。
	SessionRunStarted SessionEventKind = iota + 1
	// SessionRunEvent 运行事件或会话级交互请求（审批、提问），见 Event。
	SessionRunEvent
	// SessionRunFinished 一次会话运行结束（排队续跑在内的整个事件流关闭）。
	// Elapsed 是运行耗时。
	SessionRunFinished
	// SessionResync 读取位置早于缓冲中最旧的条目，其间的条目已被丢弃：
	// 消费方应从会话记录重建（HTTP 为 /sessions/{id}/messages），再继续读取。
	// Oldest 是缓冲中最旧条目的 seq，下一个读到的条目从它开始。
	SessionResync
)

// SessionEvent 是会话事件日志中的一条。
type SessionEvent struct {
	// Seq 会话内单调递增的序号（SessionResync 为 0）。它是不透明的续传游标：
	// 不从 1 开始，同一会话的日志被释放后重建时继续增大。
	Seq  int64
	Kind SessionEventKind
	// RunID 所属运行（运行期间发布的交互请求同样归属当时进行中的运行）。
	RunID string
	// Event 是 SessionRunEvent 的事件。日志不保存 Event.Messages 与
	// Approve/Deny 回调。
	Event Event
	// Elapsed 是 SessionRunFinished 的运行耗时。
	Elapsed time.Duration
	// Oldest 是 SessionResync 时缓冲中最旧条目的 seq。
	Oldest int64
}

// 游标起点（App.WatchSession 的 after 参数）。after >= 0 时从 seq > after 的
// 条目读起：after 为 0 表示从缓冲中最旧的条目读起；after 早于缓冲时第一个
// 条目是 SessionResync。
const (
	// WatchFromActiveRun 从会话当前进行中运行的开头读起；没有进行中的运行时
	// 只读之后追加的条目。
	WatchFromActiveRun int64 = -1
	// WatchFromNow 只读之后追加的条目。
	WatchFromNow int64 = -2
)

// SessionStreamConfig 会话事件日志配置（WithSessionStream）。零值字段取默认值。
type SessionStreamConfig struct {
	// MaxFrames 每个会话保留的最近条目数，默认 2000。
	MaxFrames int
	// MaxBytes 每个会话保留条目的估算字节上限，默认 8 MiB。
	MaxBytes int
	// IdleTTL 会话没有进行中的运行、也没有读取方超过该时长后释放其日志，
	// 默认 30 分钟。
	IdleTTL time.Duration
}

const (
	defaultStreamFrames  = 2000
	defaultStreamBytes   = 8 << 20
	defaultStreamIdleTTL = 30 * time.Minute
	sweepInterval        = time.Minute
	watchBatch           = 256
)

func (c SessionStreamConfig) withDefaults() SessionStreamConfig {
	if c.MaxFrames <= 0 {
		c.MaxFrames = defaultStreamFrames
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = defaultStreamBytes
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = defaultStreamIdleTTL
	}
	return c
}

// ErrSessionStreamClosed 会话事件日志已关闭（CloseSession），游标随之结束。
var ErrSessionStreamClosed = errors.New("goagent: 会话事件流已关闭")

// WithSessionStream 启用会话事件日志并设置缓冲上限。HTTP 模式（RunHTTP）
// 总是启用（未调用本选项时取默认值）；进程内宿主启用后经 App.WatchSession
// 读取，用于自建传输上的断线续传。
func WithSessionStream(cfg SessionStreamConfig) Option {
	return optionFunc(func(c *appConfig) {
		cfg := cfg.withDefaults()
		c.sessionStream = &cfg
	})
}

// eventHub 是全部会话事件日志的登记处。
type eventHub struct {
	cfg SessionStreamConfig

	mu         sync.Mutex
	logs       map[string]*sessionLog
	lastSweep  time.Time
	sweepEvery time.Duration
	// released 记录已释放日志的下一个 seq：同一进程内重建时从这里续号，
	// 墙钟回拨也不会让 seq 倒退。
	released map[string]int64
}

func newEventHub(cfg SessionStreamConfig) *eventHub {
	return &eventHub{cfg: cfg.withDefaults(), logs: map[string]*sessionLog{}, released: map[string]int64{}, sweepEvery: sweepInterval}
}

// log 返回会话的日志（不存在则创建），顺带释放闲置过久的日志。
func (h *eventHub) log(sessionID string) *sessionLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.Sub(h.lastSweep) >= h.sweepEvery {
		h.lastSweep = now
		for id, l := range h.logs {
			if id != sessionID && l.idleSince(now) >= h.cfg.IdleTTL {
				h.releaseLocked(id, l)
			}
		}
	}
	if l := h.logs[sessionID]; l != nil {
		// 刚取到的日志不能在调用方用它之前被并发的清扫释放。
		l.addWatcher(0)
		return l
	}
	// seq 起点取微秒时间戳：进程重启或日志释放后重建，新 seq 总大于此前
	// 发出的任何 seq，客户端带着旧游标来续传时能识别出缺口。
	base := now.UnixMicro()
	if prev := h.released[sessionID]; prev > base {
		base = prev
	}
	delete(h.released, sessionID)
	l := newSessionLog(sessionID, h.cfg, base)
	h.logs[sessionID] = l
	return l
}

// existing 返回会话已有的日志（没有时为 nil，不创建）。
func (h *eventHub) existing(sessionID string) *sessionLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs[sessionID]
}

// close 释放会话的日志；正在读取的游标读完缓冲后返回 ErrSessionStreamClosed。
func (h *eventHub) close(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.logs[sessionID]; l != nil {
		h.releaseLocked(sessionID, l)
	}
}

func (h *eventHub) releaseLocked(id string, l *sessionLog) {
	delete(h.logs, id)
	h.released[id] = l.close()
}

// sessionLog 是一个会话的事件日志。
type sessionLog struct {
	id  string
	cfg SessionStreamConfig

	mu sync.Mutex
	// entries 是保留窗口，最旧在前：entries[i].ev.Seq == entries[0].ev.Seq + i。
	entries []logEntry
	bytes   int
	next    int64
	// notify 在每次追加或关闭时关闭并换新，等待中的游标据此醒来。
	notify chan struct{}
	closed bool
	// active 是进行中的运行：runID → SessionRunStarted 的 seq。
	active map[string]int64
	// watchers 是未关闭的游标数；有游标时日志不会因闲置被释放。
	watchers int
	touched  time.Time
}

type logEntry struct {
	ev   SessionEvent
	size int
}

func newSessionLog(id string, cfg SessionStreamConfig, base int64) *sessionLog {
	return &sessionLog{
		id:      id,
		cfg:     cfg,
		next:    base,
		notify:  make(chan struct{}),
		active:  map[string]int64{},
		touched: time.Now(),
	}
}

// append 追加一条并返回分配的 seq（日志已关闭时返回 0）。
func (l *sessionLog) append(ev SessionEvent) int64 {
	size := sessionEventSize(ev)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0
	}
	ev.Seq = l.next
	l.next++
	l.entries = append(l.entries, logEntry{ev: ev, size: size})
	l.bytes += size
	switch ev.Kind {
	case SessionRunStarted:
		l.active[ev.RunID] = ev.Seq
	case SessionRunFinished:
		delete(l.active, ev.RunID)
	}
	// 至少保留最新一条，即使它单独超过字节上限。
	for len(l.entries) > 1 && (len(l.entries) > l.cfg.MaxFrames || l.bytes > l.cfg.MaxBytes) {
		l.bytes -= l.entries[0].size
		l.entries[0] = logEntry{}
		l.entries = l.entries[1:]
	}
	l.touched = time.Now()
	close(l.notify)
	l.notify = make(chan struct{})
	return ev.Seq
}

// close 关闭日志并返回下一个 seq（重建时续号用）。
func (l *sessionLog) close() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.notify)
	}
	return l.next
}

// idleSince 返回日志闲置了多久（有进行中的运行或读取方时为 0）。
func (l *sessionLog) idleSince(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.active) > 0 || l.watchers > 0 {
		return 0
	}
	return now.Sub(l.touched)
}

// readAfter 返回 seq > after 的条目（至多 max 条）。after 不在可续读范围内
// （早于缓冲中最旧的条目，或晚于已分配的最大 seq——来自此前的日志）时
// lost 为 true，oldest 是可读的第一个 seq。没有新条目时返回等待通道。
func (l *sessionLog) readAfter(after int64, max int) (out []SessionEvent, wait <-chan struct{}, oldest int64, lost, closed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	closed = l.closed
	if len(l.entries) == 0 {
		if after != l.next-1 {
			return nil, nil, l.next, true, closed
		}
		return nil, l.notify, 0, false, closed
	}
	first := l.entries[0].ev.Seq
	if after < first-1 || after >= l.next {
		return nil, nil, first, true, closed
	}
	idx := int(after + 1 - first)
	if idx >= len(l.entries) {
		return nil, l.notify, 0, false, closed
	}
	end := min(idx+max, len(l.entries))
	out = make([]SessionEvent, 0, end-idx)
	for _, e := range l.entries[idx:end] {
		out = append(out, e.ev)
	}
	return out, nil, 0, false, closed
}

// startCursor 把游标起点换算成 after 值。
func (l *sessionLog) startCursor(after int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case after == WatchFromNow:
		return l.next - 1
	case after == WatchFromActiveRun:
		start := int64(0)
		for _, s := range l.active {
			if start == 0 || s < start {
				start = s
			}
		}
		if start == 0 {
			return l.next - 1
		}
		return start - 1
	case after == 0:
		if len(l.entries) == 0 {
			return l.next - 1
		}
		return l.entries[0].ev.Seq - 1
	default:
		return after
	}
}

// caughtUp 报告 seq <= after 的条目之后没有新条目，且没有进行中的运行。
func (l *sessionLog) caughtUp(after int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return after >= l.next-1 && len(l.active) == 0
}

// activeRun 返回当前进行中的运行 ID（多个时取最早开始的；没有时为空）。
func (l *sessionLog) activeRun() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, start := "", int64(0)
	for run, s := range l.active {
		if start == 0 || s < start {
			id, start = run, s
		}
	}
	return id
}

// hasRun 报告 runID 的运行已经开始（进行中，或其开始条目仍在缓冲中）。
func (l *sessionLog) hasRun(runID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.active[runID]; ok {
		return true
	}
	for i := len(l.entries) - 1; i >= 0; i-- {
		if e := l.entries[i].ev; e.Kind == SessionRunStarted && e.RunID == runID {
			return true
		}
	}
	return false
}

// addWatcher 调整读取方计数并刷新闲置计时（delta 为 0 时只刷新计时）。
func (l *sessionLog) addWatcher(delta int) {
	l.mu.Lock()
	l.watchers += delta
	l.touched = time.Now()
	l.mu.Unlock()
}

// SessionWatcher 是会话事件日志的读取游标（App.WatchSession 返回）。
// 不是并发安全的：一个游标只由一个 goroutine 读取。用完必须 Close。
type SessionWatcher struct {
	log    *sessionLog
	cursor int64
	buf    []SessionEvent
	closed bool
}

// Next 返回下一条，没有新条目时阻塞到有新条目、ctx 结束或日志关闭（返回
// ErrSessionStreamClosed；关闭前已追加的条目仍会先读完）。
func (w *SessionWatcher) Next(ctx context.Context) (SessionEvent, error) {
	for {
		if len(w.buf) > 0 {
			ev := w.buf[0]
			w.buf = w.buf[1:]
			w.cursor = ev.Seq
			return ev, nil
		}
		out, wait, oldest, lost, closed := w.log.readAfter(w.cursor, watchBatch)
		if lost {
			w.cursor = oldest - 1
			return SessionEvent{Kind: SessionResync, Oldest: oldest}, nil
		}
		if len(out) > 0 {
			w.buf = out
			continue
		}
		if closed {
			return SessionEvent{}, ErrSessionStreamClosed
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return SessionEvent{}, ctx.Err()
		}
	}
}

// Cursor 返回最后读到的 seq（续传时作为 after 传回）。
func (w *SessionWatcher) Cursor() int64 { return w.cursor }

// Idle 报告游标已读到最新，且会话没有进行中的运行。
func (w *SessionWatcher) Idle() bool {
	return len(w.buf) == 0 && w.log.caughtUp(w.cursor)
}

// Close 关闭游标。
func (w *SessionWatcher) Close() {
	if w.closed {
		return
	}
	w.closed = true
	w.log.addWatcher(-1)
}

// eventHub 返回会话事件日志（未启用时为 nil）。
func (a *App) eventHub() *eventHub {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	return a.hub
}

// enableEventHub 启用会话事件日志（已启用时直接返回）。
func (a *App) enableEventHub() *eventHub {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	if a.hub == nil {
		cfg := SessionStreamConfig{}
		if a.config.sessionStream != nil {
			cfg = *a.config.sessionStream
		}
		a.hub = newEventHub(cfg)
	}
	return a.hub
}

// WatchSession 打开会话事件日志的读取游标。after 见 WatchFromActiveRun、
// WatchFromNow；after >= 0 时从 seq > after 的条目读起（续传：传回上次读到的
// SessionEvent.Seq）。
//
// 日志未启用（既没有 WithSessionStream 也没有进入 HTTP 模式）时本调用会启用
// 它：此后开始的会话运行才会写入。
func (a *App) WatchSession(sessionID string, after int64) *SessionWatcher {
	l := a.enableEventHub().log(sessionID)
	l.addWatcher(1)
	return &SessionWatcher{log: l, cursor: l.startCursor(after)}
}

// publishSessionEvent 向会话日志发布一个会话级事件（交互请求等），归属当前
// 进行中的运行。日志未启用或没有会话身份时返回 false。
func (a *App) publishSessionEvent(sessionID string, ev Event) bool {
	hub := a.eventHub()
	if hub == nil || ephemeralSession(sessionID) {
		return false
	}
	l := hub.log(sessionID)
	return l.append(SessionEvent{Kind: SessionRunEvent, RunID: l.activeRun(), Event: logEvent(ev)}) != 0
}

// runIDKey 运行 ID 的 context 键：调用方（HTTP 层）预先指定会话运行的 ID，
// 以便在日志里认出自己发起的那次运行。
type runIDKey struct{}

func withRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

func runIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
}

// teeSessionRun 让一次会话运行的事件在送达 events 之前写入会话日志（日志
// 未启用时原样返回 events）。调用方此后只能经 out 发送事件，结束时调用
// finish：它等待在途事件写完，再追加运行结束条目。
func (a *App) teeSessionRun(ctx context.Context, sessionID string, events chan<- Event) (out chan<- Event, finish func()) {
	hub := a.eventHub()
	if hub == nil || ephemeralSession(sessionID) {
		return events, func() {}
	}
	l := hub.log(sessionID)
	runID := runIDFromContext(ctx)
	if runID == "" {
		runID = newRequestID("run")
	}
	start := time.Now()
	l.append(SessionEvent{Kind: SessionRunStarted, RunID: runID})
	inner := make(chan Event, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range inner {
			l.append(SessionEvent{Kind: SessionRunEvent, RunID: runID, Event: logEvent(ev)})
			events <- ev
		}
	}()
	return inner, func() {
		close(inner)
		<-done
		l.append(SessionEvent{Kind: SessionRunFinished, RunID: runID, Elapsed: time.Since(start)})
	}
}

// logEvent 去掉不进日志的字段：完整对话记录（EventDone 携带，由会话存储
// 负责）与 SDK 审批回调。
func logEvent(ev Event) Event {
	ev.Messages = nil
	ev.Approve = nil
	ev.Deny = nil
	return ev
}

// sessionEventSize 估算条目占用的字节数（缓冲上限按它计算）。
func sessionEventSize(ev SessionEvent) int {
	e := ev.Event
	n := 96 + len(ev.RunID) + len(e.Text) + len(e.Thinking) + len(e.ToolName) + len(e.ToolInput) +
		len(e.ToolUseID) + len(e.ToolResult) + len(e.StatusKey) + len(e.AgentID) + len(e.AgentDesc) +
		len(e.AgentStatus) + len(e.AgentActivity) + len(e.Question) + len(e.PlanContent) +
		len(e.InterruptReason) + len(e.RequestID) + len(e.Permission) + len(e.CustomName) + len(e.CustomValue)
	if e.Error != nil {
		n += len(e.Error.Error())
	}
	if len(e.Payload) > 0 {
		if b, err := json.Marshal(e.Payload); err == nil {
			n += len(b)
		}
	}
	return n
}
