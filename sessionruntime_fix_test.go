package goagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

// 会话运行时（v0.13）的回归测试：会话级常驻状态、跨运行的"始终允许"授权、
// 进行中运行的权限门登记与会话级权限模式。

// alwaysApprover 批准并勾选"始终允许"，记录被询问的次数。
type alwaysApprover struct {
	mu    sync.Mutex
	asked int
}

func (a *alwaysApprover) Approve(toolName, input string, perm Permission) (bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return true, true
}

func (a *alwaysApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

func normalTool() ToolDef {
	return ToolDef{
		Description: "有副作用的操作",
		Input:       echoIn{},
		Permission:  Normal,
		Execute:     func(ctx Context, in echoIn) (string, error) { return "done", nil },
	}
}

func oneToolCall(name string) *provider.MockProvider {
	return &provider.MockProvider{Responses: []provider.MockResponse{
		{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("c1", name, map[string]any{"v": "x"})}},
		{Text: "done"},
	}}
}

// "始终允许"在同一会话的后续运行中继续有效：此前权限门每次运行新建，授权
// 只在当前这条用户消息内有效。授权不跨会话，CloseSession 后撤销。
func TestFix_AlwaysAllowPersistsAcrossRunsInSession(t *testing.T) {
	appr := &alwaysApprover{}
	app := New(WithProvider(oneToolCall("deploy")), WithApprover(appr))
	app.Tool("deploy", normalTool())
	run := func(sessionID string) {
		app.SetProvider(oneToolCall("deploy"))
		drainEvents(app.RunWithSessionHistory(context.Background(), sessionID, nil, "go"))
	}

	run("s1")
	run("s1")
	if appr.count() != 1 {
		t.Fatalf("同一会话勾选始终允许后不应再询问，实际询问 %d 次", appr.count())
	}
	if keys := app.SessionGrants("s1").Keys(); len(keys) != 1 || keys[0] != "deploy" {
		t.Fatalf("会话授权应记录 deploy: %v", keys)
	}
	run("s2")
	if appr.count() != 2 {
		t.Fatalf("授权不应跨会话，实际询问 %d 次", appr.count())
	}
	app.CloseSession("s1")
	run("s1")
	if appr.count() != 3 {
		t.Fatalf("CloseSession 后授权应撤销，实际询问 %d 次", appr.count())
	}
}

// blockingRuns 启动若干个会话的运行，每个都停在一次 Normal 级工具调用里，
// 直到 release 被关闭。
func blockingRuns(t *testing.T, app *App, sessions ...string) (release func(), wait func()) {
	t.Helper()
	gate := make(chan struct{})
	entered := make(chan struct{}, len(sessions))
	app.Tool("hold", ToolDef{
		Description: "阻塞直到测试放行",
		Input:       echoIn{},
		Permission:  ReadOnly,
		Concurrent:  true,
		Execute: func(ctx Context, in echoIn) (string, error) {
			entered <- struct{}{}
			<-gate
			return "ok", nil
		},
	})
	var wg sync.WaitGroup
	for i, sid := range sessions {
		app.SetProvider(&provider.MockProvider{Responses: []provider.MockResponse{
			{ToolCalls: []provider.MockToolCall{provider.NewMockToolCall("h", "hold", map[string]any{"v": ""})}},
			{Text: "done"},
		}})
		events := app.RunWithSessionHistory(context.Background(), sid, nil, "go")
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainEvents(events)
		}()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 个运行没有进入阻塞工具", i+1)
		}
	}
	return func() { close(gate) }, wg.Wait
}

func liveModes(app *App) map[string][]permission.PermissionMode {
	out := map[string][]permission.PermissionMode{}
	app.eachLiveGate(func(g *permission.Gate, lg *liveGate) {
		out[lg.sessionID] = append(out[lg.sessionID], g.Mode())
	})
	return out
}

// 全局模式切换作用于所有进行中的运行：此前只记住最后启动的那次运行的权限门，
// 并发的其他会话不受影响。会话级模式只作用于该会话，全局切换不覆盖它。
func TestFix_PermissionModeSwitchReachesAllLiveRuns(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider()))
	release, wait := blockingRuns(t, app, "a", "b")

	app.SetPermissionMode(PermissionPlanOnly)
	for sid, modes := range liveModes(app) {
		if len(modes) != 1 || modes[0] != permission.ModePlan {
			t.Fatalf("会话 %s 的进行中运行应切到 plan: %v", sid, modes)
		}
	}

	app.SetSessionPermissionMode("a", PermissionBypass)
	app.SetPermissionMode(PermissionDefault)
	modes := liveModes(app)
	if modes["a"][0] != permission.ModeBypassPermissions || modes["b"][0] != permission.ModeDefault {
		t.Fatalf("会话级模式只作用于该会话，且不被全局切换覆盖: %v", modes)
	}
	if app.SessionPermissionMode("a") != PermissionBypass || app.SessionPermissionMode("b") != PermissionDefault {
		t.Fatalf("SessionPermissionMode 应反映会话级设置")
	}

	app.ClearSessionPermissionMode("a")
	if m := liveModes(app)["a"][0]; m != permission.ModeDefault {
		t.Fatalf("撤销会话级模式后应回到全局模式: %v", m)
	}

	release()
	wait()
	if n := len(liveModes(app)); n != 0 {
		t.Fatalf("运行结束后权限门应注销，仍有 %d 个", n)
	}
}

// 运行期注册的 Guard 同样作用于所有进行中的运行。
func TestFix_GuardReachesAllLiveRuns(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider()))
	release, wait := blockingRuns(t, app, "a", "b")
	defer func() { release(); wait() }()

	app.AddPermissionGuard(permission.Guard{Name: "red-line", Fn: func(toolName, input string, level permission.Level) *permission.PermissionResult {
		return &permission.PermissionResult{Behavior: permission.BehaviorDeny, Reason: "红线"}
	}})
	n := 0
	app.eachLiveGate(func(g *permission.Gate, _ *liveGate) {
		if r := g.CheckResult("anything", "{}", permission.LevelReadOnly); r.Behavior == permission.BehaviorDeny {
			n++
		}
	})
	if n != 2 {
		t.Fatalf("Guard 应作用于全部 2 个进行中的运行，实际 %d", n)
	}
}

// HTTP /mode 支持按会话切换：只影响该会话，GET 带 session_id 返回会话生效的模式。
func TestFix_HTTPModePerSession(t *testing.T) {
	app := New(WithProvider(provider.NewMockProvider()), WithHTTPInsecure())
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	post := func(body string) int {
		resp, err := http.Post(srv.URL+"/mode", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	get := func(query string) string {
		resp, err := http.Get(srv.URL + "/mode" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Mode string }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Mode
	}

	if code := post(`{"mode":"bypass","session_id":"s1"}`); code != http.StatusOK {
		t.Fatalf("按会话切换应成功: %d", code)
	}
	if got := get("?session_id=s1"); got != "bypass" {
		t.Fatalf("会话 s1 应为 bypass: %q", got)
	}
	if got := get(""); got != "default" {
		t.Fatalf("全局模式不应受会话切换影响: %q", got)
	}
	if got := get("?session_id=s2"); got != "default" {
		t.Fatalf("其他会话沿用全局模式: %q", got)
	}
	if code := post(`{"mode":"","session_id":"s1"}`); code != http.StatusOK {
		t.Fatalf("撤销会话级模式应成功: %d", code)
	}
	if got := get("?session_id=s1"); got != "default" {
		t.Fatalf("撤销后会话回到全局模式: %q", got)
	}
	if code := post(`{"mode":"bypass","session_id":"../x"}`); code != http.StatusBadRequest {
		t.Fatalf("非法会话 ID 应被拒绝: %d", code)
	}
}
