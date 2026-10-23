package goagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func readN(t *testing.T, w *SessionWatcher, n int) []SessionEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := make([]SessionEvent, 0, n)
	for len(out) < n {
		ev, err := w.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v（已读 %d 条）", err, len(out))
		}
		out = append(out, ev)
	}
	return out
}

func textEvent(s string) SessionEvent {
	return SessionEvent{Kind: SessionRunEvent, Event: Event{Type: EventTextDelta, Text: s}}
}

// 条目按追加顺序读出，seq 连续递增；读到最新后 Next 阻塞到新条目到达。
func TestSessionLog_OrderAndBlocking(t *testing.T) {
	hub := newEventHub(SessionStreamConfig{})
	l := hub.log("s1")
	w := &SessionWatcher{log: l, cursor: l.startCursor(WatchFromNow)}
	l.addWatcher(1)
	defer w.Close()

	l.append(SessionEvent{Kind: SessionRunStarted, RunID: "r1"})
	l.append(textEvent("a"))
	l.append(textEvent("b"))
	got := readN(t, w, 3)
	if got[0].Kind != SessionRunStarted || got[1].Event.Text != "a" || got[2].Event.Text != "b" {
		t.Fatalf("顺序错误: %+v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("seq 应连续: %d → %d", got[i-1].Seq, got[i].Seq)
		}
	}

	done := make(chan SessionEvent, 1)
	go func() {
		ev, _ := w.Next(context.Background())
		done <- ev
	}()
	select {
	case ev := <-done:
		t.Fatalf("没有新条目时 Next 应阻塞，却返回了 %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	l.append(textEvent("c"))
	select {
	case ev := <-done:
		if ev.Event.Text != "c" {
			t.Fatalf("应读到新追加的条目, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("新条目到达后 Next 应返回")
	}
}

// 超出条数上限后最旧的条目被丢弃；从已丢弃的位置续读先得到 resync。
func TestSessionLog_FrameCapAndResync(t *testing.T) {
	hub := newEventHub(SessionStreamConfig{MaxFrames: 3})
	l := hub.log("s1")
	var seqs []int64
	for _, s := range []string{"1", "2", "3", "4", "5"} {
		seqs = append(seqs, l.append(textEvent(s)))
	}
	w := &SessionWatcher{log: l, cursor: seqs[0]}
	got := readN(t, w, 4)
	if got[0].Kind != SessionResync || got[0].Oldest != seqs[2] {
		t.Fatalf("首条应为 resync{oldest=%d}, got %+v", seqs[2], got[0])
	}
	if got[1].Event.Text != "3" || got[3].Event.Text != "5" {
		t.Fatalf("resync 之后应从最旧的保留条目读起: %+v", got[1:])
	}
}

// 超出字节上限同样丢弃最旧条目，但至少保留最新一条。
func TestSessionLog_ByteCap(t *testing.T) {
	hub := newEventHub(SessionStreamConfig{MaxBytes: 1000})
	l := hub.log("s1")
	l.append(textEvent(strings.Repeat("x", 600)))
	last := l.append(textEvent(strings.Repeat("y", 600)))
	l.mu.Lock()
	n, first := len(l.entries), l.entries[0].ev.Seq
	l.mu.Unlock()
	if n != 1 || first != last {
		t.Fatalf("超出字节上限应只剩最新一条, got %d 条 first=%d last=%d", n, first, last)
	}
	big := l.append(textEvent(strings.Repeat("z", 5000)))
	l.mu.Lock()
	n, first = len(l.entries), l.entries[0].ev.Seq
	l.mu.Unlock()
	if n != 1 || first != big {
		t.Fatalf("单条超过上限时也应保留它, got %d 条", n)
	}
}

// 游标来自更大的 seq（此前的日志）时同样 resync，而不是永远等待。
func TestSessionLog_FutureCursorResyncs(t *testing.T) {
	hub := newEventHub(SessionStreamConfig{})
	l := hub.log("s1")
	s1 := l.append(textEvent("a"))
	w := &SessionWatcher{log: l, cursor: s1 + 100}
	got := readN(t, w, 2)
	if got[0].Kind != SessionResync || got[1].Event.Text != "a" {
		t.Fatalf("未来游标应 resync 后从头读: %+v", got)
	}
}

// WatchFromActiveRun 从进行中运行的开始条目读起；没有进行中的运行时只读新条目。
func TestSessionLog_WatchFromActiveRun(t *testing.T) {
	app := New(WithSessionStream(SessionStreamConfig{}))
	l := app.eventHub().log("s1")
	l.append(SessionEvent{Kind: SessionRunStarted, RunID: "old"})
	l.append(SessionEvent{Kind: SessionRunFinished, RunID: "old"})
	start := l.append(SessionEvent{Kind: SessionRunStarted, RunID: "r2"})
	l.append(textEvent("in-run"))

	w := app.WatchSession("s1", WatchFromActiveRun)
	defer w.Close()
	got := readN(t, w, 2)
	if got[0].Seq != start || got[0].RunID != "r2" || got[1].Event.Text != "in-run" {
		t.Fatalf("应从进行中运行的开头读起: %+v", got)
	}

	l.append(SessionEvent{Kind: SessionRunFinished, RunID: "r2"})
	idle := app.WatchSession("s1", WatchFromActiveRun)
	defer idle.Close()
	if !idle.Idle() {
		t.Fatal("没有进行中的运行时，新游标应处于空闲（只等新条目）")
	}
}

// CloseSession 关闭日志：游标先读完已追加的条目，再返回 ErrSessionStreamClosed；
// 重建的日志 seq 继续增大。
func TestSessionLog_CloseAndReopen(t *testing.T) {
	app := New(WithSessionStream(SessionStreamConfig{}))
	w := app.WatchSession("s1", WatchFromNow)
	defer w.Close()
	l := app.eventHub().log("s1")
	last := l.append(textEvent("a"))
	app.CloseSession("s1")

	ev, err := w.Next(context.Background())
	if err != nil || ev.Event.Text != "a" {
		t.Fatalf("关闭前追加的条目应先读到: %+v %v", ev, err)
	}
	if _, err := w.Next(context.Background()); !errors.Is(err, ErrSessionStreamClosed) {
		t.Fatalf("读完后应返回 ErrSessionStreamClosed, got %v", err)
	}
	if next := app.eventHub().log("s1").append(textEvent("b")); next <= last {
		t.Fatalf("重建日志的 seq 应继续增大: %d <= %d", next, last)
	}
}

// 闲置超过 IdleTTL 的日志在下一次清扫时释放；有读取方或进行中运行的不释放。
func TestSessionLog_IdleEviction(t *testing.T) {
	hub := newEventHub(SessionStreamConfig{IdleTTL: time.Millisecond})
	hub.sweepEvery = 0
	idle := hub.log("idle")
	idle.append(textEvent("x"))
	running := hub.log("running")
	running.append(SessionEvent{Kind: SessionRunStarted, RunID: "r"})
	watched := hub.log("watched")
	watched.addWatcher(1)

	time.Sleep(5 * time.Millisecond)
	hub.log("other")
	if hub.existing("idle") != nil {
		t.Fatal("闲置日志应被释放")
	}
	if hub.existing("running") == nil || hub.existing("watched") == nil {
		t.Fatal("有进行中运行或读取方的日志不应被释放")
	}
}
