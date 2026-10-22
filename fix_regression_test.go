package goagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

type echoIn struct {
	V string `json:"v"`
}

func echoTool() ToolDef {
	return ToolDef{
		Description: "echo",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Execute:     func(ctx Context, in echoIn) (string, error) { return "echo:" + in.V, nil },
	}
}

func drainEvents(ch <-chan Event) []Event {
	var evs []Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

// RunSession 逐条落盘：两次运行后重新加载，消息完整、无重复、配对合法。
func TestFix_RunSessionPersistsEveryMessageOnce(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(session.NewFileStore(dir))
	p := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("t1", "echo", map[string]any{"v": "a"})}},
		{Text: "done1"},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("t2", "echo", map[string]any{"v": "b"})}},
		{Text: "done2"},
	}}
	app := New(WithProvider(p), WithSessionManager(mgr))
	app.Tool("echo", echoTool())

	ctx := context.Background()
	drainEvents(app.RunSession(ctx, "s1", "first"))
	drainEvents(app.RunSession(ctx, "s1", "second"))

	sess, err := mgr.Get(ctx, "s1")
	if err != nil || sess == nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if len(sess.Messages) != 8 {
		t.Fatalf("应有 8 条消息（两轮各：用户、工具调用、工具结果、回复），实际 %d", len(sess.Messages))
	}
	seen := map[string]bool{}
	for _, m := range sess.Messages {
		if m.UID == "" || seen[m.UID] {
			t.Fatalf("落盘消息缺 UID 或重复: %+v", m)
		}
		seen[m.UID] = true
	}
	if err := message.ValidatePairing(sess.Messages); err != nil {
		t.Fatalf("重新加载的历史配对非法: %v", err)
	}
}

// RunWithHistory 不落盘（持久化由调用方负责），不会写出共享的 ephemeral.jsonl。
func TestFix_RunWithHistoryDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	app := New(WithProvider(provider.NewMockProvider("hi")), WithSessionManager(session.NewManager(session.NewFileStore(dir))))
	drainEvents(app.RunWithHistory(context.Background(), nil, "q"))
	if _, err := os.Stat(filepath.Join(dir, "ephemeral.jsonl")); err == nil {
		t.Fatal("RunWithHistory 不应写入会话存储")
	}
}

// 审批请求按会话路由：A 的请求不会推给 B，B 也不能替 A 作答；请求 ID 不可猜。
func TestFix_ApprovalRoutedBySession(t *testing.T) {
	h := NewPermissionHandler()
	chA, unA := h.Subscribe("A")
	defer unA()
	chB, unB := h.Subscribe("B")
	defer unB()

	ctxA := WithSessionContext(context.Background(), "A", "")
	result := make(chan bool, 1)
	go func() {
		ok, _ := h.ApproveWithContext(ctxA, "Bash", `{"command":"ls"}`, Normal)
		result <- ok
	}()

	var req *PermissionRequest
	select {
	case req = <-chA:
	case <-chB:
		t.Fatal("A 会话的审批被推给了 B")
	case <-time.After(2 * time.Second):
		t.Fatal("审批请求没有送达 A")
	}
	if strings.HasPrefix(req.RequestID, "perm-1") && len(req.RequestID) < 10 {
		t.Fatalf("请求 ID 应为随机值: %s", req.RequestID)
	}
	if h.ResolveForSession("B", req.RequestID, true, false, "") {
		t.Fatal("B 不应能解决 A 的审批")
	}
	if !h.ResolveForSession("A", req.RequestID, true, false, "") || !<-result {
		t.Fatal("A 应能批准自己的请求")
	}
}

// 会话未绑定连接时不回落到其他会话的连接；空会话只在唯一连接时路由。
func TestFix_StreamBusNoCrossSessionFallback(t *testing.T) {
	b := newStreamBus[int]()
	_, unA := b.Subscribe("A")
	defer unA()
	if b.Send("B", 1) {
		t.Fatal("未绑定的会话不应路由到 A")
	}
	if !b.Send("", 2) {
		t.Fatal("只有一条连接时，空会话可以路由过去")
	}
	_, unC := b.Subscribe("C")
	defer unC()
	if b.Send("", 3) {
		t.Fatal("多条连接时不应猜测空会话的归属")
	}
}

// 审批等待中 ctx 取消立即返回拒绝（中断能打断审批）。
func TestFix_ApprovalCanceledByContext(t *testing.T) {
	h := NewPermissionHandler()
	_, un := h.Subscribe("A")
	defer un()
	ctx, cancel := context.WithCancel(WithSessionContext(context.Background(), "A", ""))
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if ok, _ := h.ApproveWithContext(ctx, "Bash", "{}", Normal); ok {
		t.Fatal("取消后应拒绝")
	}
	if time.Since(start) > time.Second {
		t.Fatal("取消后应立即返回")
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

// HTTP 准入层：鉴权、跨站拦截、允许的源、DNS 重绑定。
func TestFix_HTTPSecurity(t *testing.T) {
	cfg := appConfig{httpAuthToken: "s3cret", httpAllowedOrigins: []string{"http://localhost:5173"}}
	h := newHTTPSecurity(cfg, true).wrap(okHandler())

	do := func(mut func(r *http.Request)) int {
		r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader("{}"))
		r.Host = "127.0.0.1:8080"
		mut(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do(func(r *http.Request) {}); c != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }); c != http.StatusOK {
		t.Fatalf("带令牌应放行，得到 %d", c)
	}
	if c := do(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	}); c != http.StatusForbidden {
		t.Fatalf("跨站请求应 403，得到 %d", c)
	}
	if c := do(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "http://localhost:5173")
	}); c != http.StatusOK {
		t.Fatalf("允许的源应放行，得到 %d", c)
	}
	if c := do(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
		r.Host = "rebind.evil.example:8080"
	}); c != http.StatusForbidden {
		t.Fatalf("回环监听时非回环 Host 应 403，得到 %d", c)
	}
}

// 未配置鉴权时只允许回环监听；":8080" 收紧为 127.0.0.1。
func TestFix_ResolveListenAddr(t *testing.T) {
	if got, lo, err := resolveListenAddr(":8080", appConfig{}); err != nil || got != "127.0.0.1:8080" || !lo {
		t.Fatalf(":8080 应收紧为回环: %q %v %v", got, lo, err)
	}
	if _, _, err := resolveListenAddr("0.0.0.0:8080", appConfig{}); err == nil {
		t.Fatal("无鉴权的非回环监听应报错")
	}
	if got, _, err := resolveListenAddr("0.0.0.0:8080", appConfig{httpAuthToken: "x"}); err != nil || got != "0.0.0.0:8080" {
		t.Fatalf("配置令牌后应允许: %q %v", got, err)
	}
}

// /interrupt 必须指明会话（空会话 ID 曾会中断所有会话）。
func TestFix_InterruptRequiresSession(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider("hi")))
	app.SetInterruptHandler(NewInterruptHandler())
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/interrupt", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 session_id 应 400，得到 %d", resp.StatusCode)
	}
}

// 工具签名错误在注册时就报出；ToolKit 之间的重名在 New 中跳过而不是 panic。
func TestFix_ToolRegistrationValidation(t *testing.T) {
	app := New()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("签名错误的工具应在注册时 panic")
			}
		}()
		app.Tool("bad", ToolDef{Input: echoIn{}, Execute: func(s string) string { return s }})
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Input 与 Execute 参数不一致应在注册时 panic")
			}
		}()
		app.Tool("mismatch", ToolDef{Input: struct{ X int }{}, Execute: func(ctx Context, in echoIn) (string, error) { return "", nil }})
	}()

	kit := ToolKit{Name: "k"}.WithTools(NamedTool{Name: "echo", Def: echoTool()})
	a2 := New(WithToolKits(kit, kit))
	if names := a2.ToolNames(); len(names) != 1 {
		t.Fatalf("重名工具应只注册一次: %v", names)
	}
}

// 工具 Context 的 Store/Load 在同一会话内共享。
func TestFix_ContextStoreLoad(t *testing.T) {
	p := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("w", "put", map[string]any{"v": "42"})}},
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("r", "get", map[string]any{"v": ""})}},
		{Text: "ok"},
	}}
	var got atomic.Value
	app := New(WithProvider(p))
	app.Tool("put", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
		ctx.Store("k", in.V)
		return "stored", nil
	}})
	app.Tool("get", ToolDef{Input: echoIn{}, Permission: ReadOnly, Execute: func(ctx Context, in echoIn) (string, error) {
		v, _ := ctx.Load("k")
		got.Store(v)
		return "loaded", nil
	}})
	drainEvents(app.RunWithSessionHistory(context.Background(), "kv-session", nil, "go"))
	if got.Load() != "42" {
		t.Fatalf("Load 应读到同会话 Store 的值，得到 %v", got.Load())
	}
}

// SetProvider 与进行中的运行并发时无数据竞争（-race）。
func TestFix_SetProviderDuringRunNoRace(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider("hi")))
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer stop.Store(true)
		for i := 0; i < 50; i++ {
			drainEvents(app.Run(context.Background(), "q"))
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			app.SetProvider(provider.NewMockProvider("hi"))
			_ = app.ModelID()
		}
	}()
	wg.Wait()
}

// HTTP 模式未配置审批者时，需要审批的工具被拒绝而不是自动执行。
func TestFix_NoApproverDeniesApprovalTools(t *testing.T) {
	var ran atomic.Bool
	p := &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("d", "danger", map[string]any{})}},
		{Text: "ok"},
	}}
	app := New(WithProvider(p))
	app.Tool("danger", ToolDef{Input: echoIn{}, Permission: Dangerous, Execute: func(ctx Context, in echoIn) (string, error) {
		ran.Store(true)
		return "done", nil
	}})
	drainEvents(app.Run(context.Background(), "go"))
	if ran.Load() {
		t.Fatal("未配置审批者时 Dangerous 工具不应执行")
	}
}
