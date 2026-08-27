package goagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// mockProvider 按会话记录调用轮次的假 LLM：
// 每个会话第 1 轮返回 record 工具调用，第 2 轮返回文本收尾。
// 每次调用固定延迟，用于测量并行度。
type mockProvider struct {
	mu      sync.Mutex
	calls   map[string]int // sessionKey → 已调用轮数
	latency time.Duration
}

func (m *mockProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{}
}

func (m *mockProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{Message: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "ok"}}}}, nil
}

func (m *mockProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	time.Sleep(m.latency)

	sessKey := SessionIDFromContext(ctx)
	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[sessKey]++
	turn := m.calls[sessKey]
	m.mu.Unlock()

	ch := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(ch)
		if turn == 1 {
			ch <- provider.StreamEvent{
				Type: provider.EventToolUseStart,
				ToolCall: &message.ToolCall{
					ID:    fmt.Sprintf("call-%s-1", sessKey),
					Name:  "record",
					Input: json.RawMessage(`{}`),
				},
			}
			ch <- provider.StreamEvent{
				Type:       provider.EventMessageComplete,
				StopReason: provider.StopToolUse,
				Message: &message.Message{
					Role: message.RoleAssistant,
					Content: []message.ContentBlock{{
						Type: "tool_use", ToolUseID: fmt.Sprintf("call-%s-1", sessKey),
						ToolName: "record", Input: json.RawMessage(`{}`),
					}},
				},
			}
			return
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "done-" + sessKey}
		ch <- provider.StreamEvent{
			Type:       provider.EventMessageComplete,
			StopReason: provider.StopEndTurn,
			Message: &message.Message{
				Role:    message.RoleAssistant,
				Content: []message.ContentBlock{{Type: "text", Text: "done-" + sessKey}},
			},
		}
	}()
	return ch, nil
}

// TestHTTPParallelSessions 验证 HTTP 层多会话并行（G2）：
// 两个不同 session 的 /chat 同时发出，各自都要经过两轮 LLM 调用
// （每轮 300ms 延迟）。若互不阻塞，总耗时 ≈ 600ms；若被串行化则 ≥1200ms。
// 同时验证 G1 全链路：工具拿到的 Context.SessionID/WorkDir 按会话正确注入。
func TestHTTPParallelSessions(t *testing.T) {
	var (
		toolMu   sync.Mutex
		toolSeen = map[string]string{} // sessionID → workDir
	)
	app := New(
		WithProvider(&mockProvider{latency: 300 * time.Millisecond}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithSessionWorkDir(func(sessionID string) string {
			return "/work/" + sessionID
		}),
	)
	app.Tool("record", ToolDef{
		Description: "记录会话上下文",
		Input:       struct{}{},
		Permission:  ReadOnly,
		Concurrent:  true,
		Execute: func(ctx Context, in struct{}) (string, error) {
			toolMu.Lock()
			toolSeen[ctx.SessionID] = ctx.WorkDir
			toolMu.Unlock()
			return "recorded", nil
		},
	})

	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	postChat := func(sessionID string) (string, error) {
		body := fmt.Sprintf(`{"message":"hi","session_id":%q}`, sessionID)
		resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(body))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("status %d", resp.StatusCode)
		}
		// 读尽 SSE 流直到 done/metadata。
		var tail string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"type":"text_delta"`) || strings.Contains(line, `"type":"done"`) || strings.Contains(line, `"type":"error"`) {
				tail += line + "\n"
			}
		}
		return tail, sc.Err()
	}

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i, sid := range []string{"sess-a", "sess-b"} {
		wg.Add(1)
		go func(i int, sid string) {
			defer wg.Done()
			results[i], errs[i] = postChat(sid)
		}(i, sid)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("会话 %d 请求失败: %v", i, err)
		}
	}
	for i, sid := range []string{"sess-a", "sess-b"} {
		if !strings.Contains(results[i], "done-"+sid) {
			t.Errorf("会话 %s 未收到最终文本，收到: %q", sid, results[i])
		}
	}

	// 并行断言：串行下限 = 4 轮 × 300ms = 1200ms；并行 ≈ 600ms。
	// 阈值 1000ms 留出调度余量，同时仍低于串行下限。
	if elapsed >= 1000*time.Millisecond {
		t.Errorf("两会话总耗时 %v，疑似被串行化（并行应 ≈600ms，串行 ≥1200ms）", elapsed)
	}
	t.Logf("两会话并行耗时: %v", elapsed)

	// G1 全链路断言：工具按会话拿到正确的 SessionID + WorkDir。
	toolMu.Lock()
	defer toolMu.Unlock()
	for _, sid := range []string{"sess-a", "sess-b"} {
		if toolSeen[sid] != "/work/"+sid {
			t.Errorf("会话 %s 的工具看到 WorkDir=%q，want %q（G1 会话上下文注入未生效）", sid, toolSeen[sid], "/work/"+sid)
		}
	}
}
