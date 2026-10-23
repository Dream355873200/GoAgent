// http_b1_test.go 会话事件总线（v0.14 B1）的端到端测试：/chat 帧的
// session_seq / run_id、断线续传、审批经会话日志到达续传连接、中断原因
// 上线、会话空闲时的续传端点、进程内会话运行可被游标读取。
package goagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// toolThenText 每个会话第 1 轮调用 tool，之后每轮返回文本 "final-<会话>"。
type toolThenText struct {
	tool  string
	mu    sync.Mutex
	turns map[string]int
}

func (p *toolThenText) Capabilities() provider.Capabilities {
	return provider.Capabilities{SupportsTools: true}
}

func (p *toolThenText) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}

func (p *toolThenText) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	sid := SessionIDFromContext(ctx)
	p.mu.Lock()
	if p.turns == nil {
		p.turns = map[string]int{}
	}
	p.turns[sid]++
	turn := p.turns[sid]
	p.mu.Unlock()
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		if turn == 1 {
			id := "call-" + sid
			tc := &message.ToolCall{ID: id, Name: p.tool, Input: json.RawMessage(`{}`)}
			ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: tc}
			ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopToolUse, Message: &message.Message{
				Role:    message.RoleAssistant,
				Content: []message.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: p.tool, Input: json.RawMessage(`{}`)}},
			}}
			return
		}
		text := "final-" + sid
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: text}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn, Message: &message.Message{
			Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: text}},
		}}
	}()
	return ch, nil
}

// sseStream 逐帧读取一条 SSE 响应（记录 id 行）。
type sseStream struct {
	resp   *http.Response
	sc     *bufio.Scanner
	cancel context.CancelFunc
	ids    []string
}

func openSSEStream(t *testing.T, method, url, body string, header map[string]string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("%s %s: status %d", method, url, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &sseStream{resp: resp, sc: sc, cancel: cancel}
}

// next 返回下一个 data 帧；流结束时 ok 为 false。
func (s *sseStream) next(t *testing.T) (protocol.Envelope, bool) {
	t.Helper()
	for s.sc.Scan() {
		line := s.sc.Text()
		if strings.HasPrefix(line, "id: ") {
			s.ids = append(s.ids, strings.TrimPrefix(line, "id: "))
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
			t.Fatalf("帧解析失败: %v (%s)", err, line)
		}
		return env, true
	}
	return protocol.Envelope{}, false
}

// until 读到满足 pred 的帧为止（含该帧）。
func (s *sseStream) until(t *testing.T, pred func(protocol.Envelope) bool) []protocol.Envelope {
	t.Helper()
	var out []protocol.Envelope
	for {
		env, ok := s.next(t)
		if !ok {
			t.Fatalf("流在满足条件之前结束，已读: %s", frameTypes(out))
		}
		out = append(out, env)
		if pred(env) {
			return out
		}
	}
}

// rest 读完剩余的帧。
func (s *sseStream) rest(t *testing.T) []protocol.Envelope {
	t.Helper()
	var out []protocol.Envelope
	for {
		env, ok := s.next(t)
		if !ok {
			return out
		}
		out = append(out, env)
	}
}

func (s *sseStream) close() {
	s.cancel()
	s.resp.Body.Close()
}

func frameTypes(frames []protocol.Envelope) string {
	var b strings.Builder
	for i, f := range frames {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(f.Type)
	}
	return b.String()
}

func isType(typ string) func(protocol.Envelope) bool {
	return func(e protocol.Envelope) bool { return e.Type == typ }
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitIdle(t *testing.T, mgr *session.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for mgr.IsBusy(id) {
		if time.Now().After(deadline) {
			t.Fatalf("会话 %s 一直未结束", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// /chat 流里来自会话日志的帧都带连续递增的 session_seq；run_start 与 metadata
// 带同一个 run_id；连接内 seq 仍从 1 起。
func TestB1_ChatFramesCarrySessionSeq(t *testing.T) {
	app := New(
		WithProvider(&toolThenText{tool: "noop"}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
	)
	app.Tool("noop", ToolDef{Description: "noop", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) { return "ok", nil }})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-a"}`, nil)
	defer st.close()
	frames := st.rest(t)
	if len(frames) < 4 || frames[0].Type != protocol.TypeRunStart || frames[len(frames)-1].Type != protocol.TypeMetadata {
		t.Fatalf("帧序列应为 run_start … metadata, got %s", frameTypes(frames))
	}
	runID := frames[0].RunID
	if runID == "" || frames[len(frames)-1].RunID != runID {
		t.Fatalf("run_start 与 metadata 应带同一个 run_id: %q / %q", runID, frames[len(frames)-1].RunID)
	}
	for i, f := range frames {
		if f.Seq != int64(i+1) {
			t.Fatalf("连接内 seq 应从 1 连续递增: frame[%d].seq=%d", i, f.Seq)
		}
		if f.SessionSeq <= 0 {
			t.Fatalf("frame[%d] %s 缺少 session_seq", i, f.Type)
		}
		if i > 0 && f.SessionSeq != frames[i-1].SessionSeq+1 {
			t.Fatalf("session_seq 应连续: %d → %d", frames[i-1].SessionSeq, f.SessionSeq)
		}
	}
}

// 客户端中途断开后运行继续；以最后收到的 session_seq 续传，补齐断线期间的
// 帧直到运行结束。
func TestB1_ResumeAfterDisconnect(t *testing.T) {
	release := make(chan struct{})
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(WithProvider(&toolThenText{tool: "wait"}), WithSessionManager(mgr))
	app.Tool("wait", ToolDef{Description: "wait", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			<-release
			return "waited", nil
		}})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-r"}`, nil)
	seen := st.until(t, isType(protocol.TypeToolStart))
	last := seen[len(seen)-1].SessionSeq
	st.close() // 断线

	close(release)
	waitIdle(t, mgr, "b1-r")

	rs := openSSEStream(t, "GET", fmt.Sprintf("%s/runs/b1-r/stream?after=%d", srv.URL, last), "", nil)
	defer rs.close()
	frames := rs.rest(t)
	if len(frames) == 0 || frames[0].SessionSeq != last+1 {
		t.Fatalf("续传应从 session_seq=%d 开始, got %s", last+1, frameTypes(frames))
	}
	var text string
	for _, f := range frames {
		if f.Type == protocol.TypeTextDelta {
			text += f.Text
		}
	}
	if text != "final-b1-r" {
		t.Fatalf("续传应补齐断线后的正文, got %q（帧 %s）", text, frameTypes(frames))
	}
	if end := frames[len(frames)-1]; end.Type != protocol.TypeMetadata || end.RunID == "" {
		t.Fatalf("续传应以本次运行的 metadata 结束, got %+v", end)
	}
	if len(rs.ids) == 0 || rs.ids[0] != fmt.Sprint(last+1) {
		t.Fatalf("续传端点应为日志帧写 SSE id 行, got %v", rs.ids)
	}

	sess, err := mgr.Get(context.Background(), "b1-r")
	if err != nil || sess == nil {
		t.Fatalf("会话应已落盘: %v", err)
	}
	if got := message.ExtractText(sess.Messages[len(sess.Messages)-1]); got != "final-b1-r" {
		t.Fatalf("断线后运行应继续到结束并落盘, 最后一条消息 %q", got)
	}
}

// 审批请求进入会话日志：原连接断开后，续传连接能收到它并据此作答，运行继续。
func TestB1_ApprovalReachesResumedStream(t *testing.T) {
	ph := NewPermissionHandler()
	mgr := session.NewManager(session.NewMemoryStore())
	var ran sync.WaitGroup
	ran.Add(1)
	app := New(
		WithProvider(&toolThenText{tool: "danger"}),
		WithSessionManager(mgr),
		WithApprover(ph),
		WithYoloClassifier(false),
	)
	app.Tool("danger", ToolDef{Description: "needs approval", Input: struct{}{}, Permission: Normal,
		Execute: func(ctx Context, in struct{}) (string, error) {
			ran.Done()
			return "did it", nil
		}})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-p"}`, nil)
	req := st.until(t, isType(protocol.TypeNeedApproval))
	perm := req[len(req)-1]
	if perm.RequestID == "" || perm.SessionSeq <= 0 || perm.ToolName != "danger" {
		t.Fatalf("permission_request 应带 request_id、session_seq 与工具名: %+v", perm)
	}
	st.close()

	rs := openSSEStream(t, "GET", srv.URL+"/runs/b1-p/stream", "", nil)
	defer rs.close()
	replayed := rs.until(t, isType(protocol.TypeNeedApproval))
	if replayed[0].Type != protocol.TypeRunStart || replayed[len(replayed)-1].RequestID != perm.RequestID {
		t.Fatalf("续传应从运行开头补发到同一个审批请求, got %s", frameTypes(replayed))
	}
	resp := postJSON(t, srv.URL+"/approve", fmt.Sprintf(`{"request_id":%q,"session_id":"b1-p","allow":true}`, perm.RequestID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/approve status %d", resp.StatusCode)
	}
	rest := rs.rest(t)
	ran.Wait()
	if rest[len(rest)-1].Type != protocol.TypeMetadata {
		t.Fatalf("续传流应在运行结束后以 metadata 收尾, got %s", frameTypes(rest))
	}
}

// POST /interrupt 的 reason 出现在 interrupted 帧上。
func TestB1_InterruptReasonOnWire(t *testing.T) {
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(WithProvider(&toolThenText{tool: "block"}), WithSessionManager(mgr))
	app.Tool("block", ToolDef{Description: "block", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}})
	app.SetInterruptHandler(NewInterruptHandler())
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-i"}`, nil)
	defer st.close()
	st.until(t, isType(protocol.TypeToolStart))
	resp := postJSON(t, srv.URL+"/interrupt", `{"session_id":"b1-i","reason":"用户点了停止"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/interrupt status %d", resp.StatusCode)
	}
	frames := st.until(t, isType(protocol.TypeInterrupted))
	if got := frames[len(frames)-1].Reason; got != "用户点了停止" {
		t.Fatalf("interrupted 帧应带中断原因, got %q", got)
	}
	if rest := st.rest(t); len(rest) == 0 || rest[len(rest)-1].Type != protocol.TypeMetadata {
		t.Fatalf("中断后流应以 metadata 结束, got %s", frameTypes(rest))
	}
}

// 会话空闲时：不带 after 只返回 metadata；after=0 补发缓冲中的整次运行；
// Last-Event-ID 等价于 after；游标早于缓冲时先发 resync。
func TestB1_StreamEndpointOnIdleSession(t *testing.T) {
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(
		WithProvider(&toolThenText{tool: "noop"}),
		WithSessionManager(mgr),
		WithSessionStream(SessionStreamConfig{MaxFrames: 6}),
	)
	app.Tool("noop", ToolDef{Description: "noop", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) { return "ok", nil }})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-s"}`, nil)
	full := st.rest(t)
	st.close()
	if len(full) <= 6 {
		t.Fatalf("测试前提：一次运行的帧数应超过缓冲上限 6, got %d", len(full))
	}

	live := openSSEStream(t, "GET", srv.URL+"/runs/b1-s/stream", "", nil)
	if frames := live.rest(t); len(frames) != 1 || frames[0].Type != protocol.TypeMetadata || frames[0].SessionSeq != 0 {
		t.Fatalf("空闲会话不带 after 应只返回本连接的 metadata, got %s", frameTypes(frames))
	}
	live.close()

	all := openSSEStream(t, "GET", srv.URL+"/runs/b1-s/stream?after=0", "", nil)
	frames := all.rest(t)
	all.close()
	if len(frames) != 6 || frames[len(frames)-1].Type != protocol.TypeMetadata || frames[0].SessionSeq != full[len(full)-6].SessionSeq {
		t.Fatalf("after=0 应补发缓冲中保留的最近 6 帧, got %s", frameTypes(frames))
	}

	cut := full[len(full)-3].SessionSeq
	byHeader := openSSEStream(t, "GET", srv.URL+"/runs/b1-s/stream", "", map[string]string{"Last-Event-ID": fmt.Sprint(cut)})
	frames = byHeader.rest(t)
	byHeader.close()
	if len(frames) != 2 || frames[0].SessionSeq != cut+1 {
		t.Fatalf("Last-Event-ID 应等价于 after, got %s", frameTypes(frames))
	}

	stale := openSSEStream(t, "GET", fmt.Sprintf("%s/runs/b1-s/stream?after=%d", srv.URL, full[0].SessionSeq-1), "", nil)
	frames = stale.rest(t)
	stale.close()
	if frames[0].Type != protocol.TypeResync || frames[0].Oldest != full[len(full)-6].SessionSeq {
		t.Fatalf("游标早于缓冲时首帧应为 resync{oldest}, got %+v", frames[0])
	}
}

// 唤醒时队列为空：运行没有开始，结果只写给本连接，不进会话日志。
func TestB1_PreStartResultStaysOnConnection(t *testing.T) {
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(WithProvider(&toolThenText{tool: "noop"}), WithSessionManager(mgr), WithSteering())
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"session_id":"b1-q","resume_queue":true}`, nil)
	frames := st.rest(t)
	st.close()
	if len(frames) != 1 || frames[0].Type != protocol.TypeMetadata || frames[0].SessionSeq != 0 {
		t.Fatalf("没有开始的运行只应返回本连接的 metadata, got %s", frameTypes(frames))
	}
	w := app.WatchSession("b1-q", 0)
	defer w.Close()
	if !w.Idle() {
		t.Fatal("没有开始的运行不应写入会话日志")
	}
}

// failingStore 读取指定会话时报错（模拟会话存储故障）。
type failingStore struct {
	session.SessionStore
	bad string
}

func (s failingStore) Get(ctx context.Context, id string) (*session.Session, error) {
	if id == s.bad {
		return nil, errors.New("存储故障")
	}
	return s.SessionStore.Get(ctx, id)
}

// 取得会话之后、运行开始之前失败（存储故障）：错误只写给本连接。
func TestB1_PreStartErrorStaysOnConnection(t *testing.T) {
	mgr := session.NewManager(failingStore{SessionStore: session.NewMemoryStore(), bad: "b1-e"})
	app := New(WithProvider(&toolThenText{tool: "noop"}), WithSessionManager(mgr))
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	st := openSSEStream(t, "POST", srv.URL+"/chat", `{"message":"hi","session_id":"b1-e"}`, nil)
	frames := st.rest(t)
	st.close()
	if len(frames) != 2 || frames[0].Type != protocol.TypeError || frames[1].Type != protocol.TypeMetadata {
		t.Fatalf("应为 error、metadata, got %s", frameTypes(frames))
	}
	if frames[0].SessionSeq != 0 {
		t.Fatal("没有开始的运行的错误不应带 session_seq")
	}
}

// 宿主进程内直接调用 RunSession 的运行同样写入会话日志；读到运行结束条目时
// 会话已释放（客户端此时发下一条消息不会撞上「正在运行」）。
func TestB1_InProcessRunSessionIsWatchable(t *testing.T) {
	mgr := session.NewManager(session.NewMemoryStore())
	app := New(
		WithProvider(&toolThenText{tool: "noop"}),
		WithSessionManager(mgr),
		WithSessionStream(SessionStreamConfig{}),
	)
	app.Tool("noop", ToolDef{Description: "noop", Input: struct{}{}, Permission: ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "ok", nil
		}})

	w := app.WatchSession("b1-h", WatchFromNow)
	defer w.Close()
	go func() {
		for range app.RunSession(context.Background(), "b1-h", "hi") {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var kinds []SessionEventKind
	for {
		ev, err := w.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		kinds = append(kinds, ev.Kind)
		if ev.Kind == SessionRunFinished {
			if mgr.IsBusy("b1-h") {
				t.Fatal("读到运行结束条目时会话应已释放")
			}
			if ev.Elapsed < 20*time.Millisecond {
				t.Fatalf("运行结束条目应带运行耗时（工具本身耗时 20ms）, got %v", ev.Elapsed)
			}
			break
		}
	}
	if kinds[0] != SessionRunStarted || len(kinds) < 4 {
		t.Fatalf("应读到 RunStarted … RunFinished, got %v", kinds)
	}
}
