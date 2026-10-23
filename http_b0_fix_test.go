package goagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/session"
)

// 未启用插话通道时，会话忙的第二个 /chat 被拒绝。此前拒绝发生在订阅之后：
// 第二个请求先把在跑那条连接的审批通道换成自己的，返回时再注销——在跑
// 任务之后的审批请求再也到不了任何连接。
func TestFix_BusyRejectKeepsRunningStreamSubscriptions(t *testing.T) {
	ph := NewPermissionHandler()
	app := New(
		WithProvider(&mockProvider{}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithApprover(ph),
	)
	reachedTool := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTool := func() { releaseOnce.Do(func() { close(release) }) }
	approved := make(chan bool, 1)
	app.Tool("record", ToolDef{
		Description: "等待测试放行后发起审批",
		Input:       struct{}{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			close(reachedTool)
			<-release
			allowed, _ := ph.ApproveWithContext(ctx, "deploy", "{}", Normal)
			approved <- allowed
			return "ok", nil
		},
	})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()
	// 断言失败时也要放行在跑的工具：否则 srv.Close 等它的 handler 结束，测试挂死。
	defer ph.Close()
	defer releaseTool()

	firstDone := make(chan []protocol.Envelope, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(`{"message":"hi","session_id":"busy-1"}`))
		if err != nil {
			t.Error(err)
			firstDone <- nil
			return
		}
		defer resp.Body.Close()
		firstDone <- parseSSE(t, resp.Body)
	}()
	<-reachedTool

	resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(`{"message":"again","session_id":"busy-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	second := parseSSE(t, resp.Body)
	resp.Body.Close()
	if len(second) == 0 || second[0].Type != protocol.TypeError {
		t.Fatalf("忙会话的第二个请求应以 error 帧拒绝, got %+v", second)
	}
	for _, f := range second {
		if f.Type == protocol.TypeRunStart {
			t.Fatalf("被拒绝的请求不应开新 run: %+v", second)
		}
	}

	releaseTool()
	deadline := time.After(3 * time.Second)
	for {
		if pending := ph.PendingBySession("busy-1"); len(pending) > 0 {
			ph.Resolve(pending[0].RequestID, true, false, "")
			break
		}
		select {
		case <-deadline:
			t.Fatal("审批请求未出现")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-approved:
	case <-time.After(5 * time.Second):
		t.Fatal("审批决定未送达在跑任务")
	}

	var frames []protocol.Envelope
	select {
	case frames = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个流未结束")
	}
	found := false
	for _, f := range frames {
		if f.Type == protocol.TypeNeedApproval && f.ToolName == "deploy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("审批帧应下发到在跑任务的连接（订阅未被第二个请求顶掉）, frames=%+v", frames)
	}
}

// orderSink 记录写出顺序；每次写入让出调度，放大并发写的交错窗口。
type orderSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *orderSink) writeEvent(data []byte) {
	runtime.Gosched()
	s.mu.Lock()
	s.frames = append(s.frames, append([]byte(nil), data...))
	s.mu.Unlock()
}

// 事件流与审批、提问转发协程并发写同一条连接：seq 必须与线上顺序一致。
// 此前 seq 在写锁外分配，两个协程拿到 seq 后谁先写出不确定。
func TestFix_FrameWriterSeqMatchesWireOrder(t *testing.T) {
	sink := &orderSink{}
	fw := newFrameWriter(sink)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				fw.write(protocol.Envelope{Type: protocol.TypeProgress})
			}
		}()
	}
	wg.Wait()

	if len(sink.frames) != 16*200 {
		t.Fatalf("帧数不对: %d", len(sink.frames))
	}
	for i, raw := range sink.frames {
		var env protocol.Envelope
		if err := json.Unmarshal(bytes.TrimSuffix(bytes.TrimPrefix(raw, []byte("data: ")), []byte("\n\n")), &env); err != nil {
			t.Fatalf("帧解析失败: %v", err)
		}
		if env.Seq != int64(i+1) {
			t.Fatalf("seq 应与线上顺序一致: 第 %d 帧 seq=%d", i+1, env.Seq)
		}
		if env.Version != protocol.Version {
			t.Fatalf("帧应盖章协议版本")
		}
	}
}

// 端到端：/chat 流中事件帧与并发的提问帧交错时，seq 仍与线上顺序一致。
func TestFix_SeqMatchesWireOrderUnderConcurrentWrites(t *testing.T) {
	ah := NewAskUserHandler()
	app := New(
		WithProvider(&mockProvider{}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
	)
	app.SetAskUserHandler(ah)
	app.Tool("record", ToolDef{
		Description: "并发提问",
		Input:       struct{}{},
		Permission:  ReadOnly,
		Execute: func(ctx Context, in struct{}) (string, error) {
			answerCtx, stop := context.WithCancel(ctx)
			defer stop()
			go func() {
				for answerCtx.Err() == nil {
					if r := ah.PendingBySession(ctx.SessionID); r != nil {
						ah.Resolve(r.RequestID, "a")
					}
					time.Sleep(time.Millisecond)
				}
			}()
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					askCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					_, _ = ah.AskSessionCtx(askCtx, ctx.SessionID, "q", nil)
				}()
			}
			wg.Wait()
			return "ok", nil
		},
	})
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(`{"message":"hi","session_id":"seq-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := parseSSE(t, resp.Body)
	asks := 0
	for i, f := range frames {
		if f.Seq != int64(i+1) {
			t.Fatalf("seq 应与线上顺序一致: frame[%d].seq=%d type=%s", i, f.Seq, f.Type)
		}
		if f.Type == protocol.TypeAskUser {
			asks++
		}
	}
	if asks == 0 {
		t.Fatal("前置条件：应有并发写入的 ask_user 帧")
	}
}

// HTTP 模式未配置提问处理器时自动启用：内置 AskUser 不再回退去读服务器 stdin。
func TestFix_PrepareHTTPInstallsAskUserHandler(t *testing.T) {
	app := New(WithProvider(&mockProvider{}), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	if app.askUserHandler != nil {
		t.Fatal("前置条件：未配置时为 nil")
	}
	prepareHTTP(app)
	if app.askUserHandler == nil {
		t.Fatal("HTTP 模式应自动配置 AskUserHandler")
	}

	custom := NewAskUserHandler()
	app2 := New(WithProvider(&mockProvider{}), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	app2.SetAskUserHandler(custom)
	prepareHTTP(app2)
	if app2.askUserHandler != custom {
		t.Fatal("宿主已配置的处理器不应被覆盖")
	}
}

// tool_done 帧携带 tool_is_error；usage 帧携带缓存读写 token。
func TestFix_EnvelopeCarriesToolErrorAndCacheTokens(t *testing.T) {
	data, _ := json.Marshal(envelopeFromEvent(Event{Type: EventToolDone, ToolUseID: "t1", ToolResult: "boom", ToolIsError: true}, "s"))
	if !strings.Contains(string(data), `"tool_is_error":true`) {
		t.Fatalf("tool_done 应携带 tool_is_error, got %s", data)
	}
	data, _ = json.Marshal(envelopeFromEvent(Event{Type: EventToolDone, ToolUseID: "t2", ToolResult: "fine"}, "s"))
	if strings.Contains(string(data), "tool_is_error") {
		t.Fatalf("成功的工具结果不应带 tool_is_error, got %s", data)
	}
}

// GET /protocol 的端点表必须与 http.go 实际注册的路由一致：此前漏了 10 个路由。
// 新增路由时忘了登记会让本测试失败。
func TestFix_ProtocolDescribesEveryRoute(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		registered[m[1]+" "+m[2]] = true
	}
	described := map[string]bool{}
	for _, ep := range protocol.Describe().Endpoints {
		path := ep.Path
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		described[ep.Method+" "+path] = true
	}
	for r := range registered {
		if !described[r] {
			t.Errorf("路由 %s 未在 protocol.Describe 中登记", r)
		}
	}
	for d := range described {
		if !registered[d] {
			t.Errorf("protocol.Describe 登记了不存在的路由 %s", d)
		}
	}
}
