package goagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// 对话记录与模型视图（v0.13）的端到端回归测试。

// firstMessageProvider 记录每次请求的全部消息文本。
type firstMessageProvider struct {
	*provider.MockProvider
	mu   sync.Mutex
	reqs [][]string
}

func (p *firstMessageProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	var texts []string
	for _, m := range req.Messages {
		texts = append(texts, message.ExtractText(m))
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, texts)
	p.mu.Unlock()
	return p.MockProvider.Stream(ctx, req)
}

// 会话存储中的摘要记录在重新加载时还原为模型视图：被摘要覆盖的早期消息不再
// 发给模型（也不会因为记录超长而再请求一次摘要），回放端点仍返回完整的原始
// 对话、不显示摘要记录；EventDone 携带完整对话记录。
func TestFix_ReloadProjectsSummaryRecord(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManager(session.NewFileStore(t.TempDir()))
	if _, err := mgr.GetOrCreate(ctx, "s1", nil); err != nil {
		t.Fatal(err)
	}
	u1 := withTestUID(message.NewUserMessage("很早的问题"))
	a1 := withTestUID(message.NewAssistantMessage("很早的回答"))
	u2 := withTestUID(message.NewUserMessage("最近的问题"))
	a2 := withTestUID(message.NewAssistantMessage("最近的回答"))
	sum := message.NewUserMessage("[对话已压缩]\n\n用户之前问过一个问题。")
	sum.UID, sum.IsMeta, sum.IsCompactSummary, sum.CompactBoundary = message.NewUID(), true, true, u2.UID
	for _, m := range []message.Message{u1, a1, u2, a2, sum} {
		if err := mgr.AppendMessage(ctx, "s1", m); err != nil {
			t.Fatal(err)
		}
	}

	prov := &firstMessageProvider{MockProvider: provider.NewMockProvider("新的回答")}
	app := New(WithProvider(prov), WithSessionManager(mgr), WithHTTPInsecure())
	var done []message.Message
	for ev := range app.RunSession(ctx, "s1", "新的问题") {
		if ev.Type == EventDone {
			done = ev.Messages
		}
	}

	if len(prov.reqs) != 1 {
		t.Fatalf("应只有一次模型请求（不重新摘要），实际 %d 次", len(prov.reqs))
	}
	got := strings.Join(prov.reqs[0], " | ")
	if strings.Contains(got, "很早的") || !strings.HasPrefix(got, "[对话已压缩]") || !strings.Contains(got, "最近的问题") {
		t.Fatalf("请求应为摘要 + 边界之后的消息: %s", got)
	}
	if len(done) != 7 {
		t.Fatalf("EventDone 应携带完整对话记录（5 条已有 + 本轮 2 条），实际 %d 条", len(done))
	}

	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var replay []message.Message
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range replay {
		if m.IsCompactSummary {
			t.Fatal("回放不应包含摘要记录")
		}
		texts = append(texts, message.ExtractText(m))
	}
	if all := strings.Join(texts, " | "); !strings.Contains(all, "很早的问题") || !strings.Contains(all, "新的回答") {
		t.Fatalf("回放应是完整的原始对话: %s", all)
	}
}

func withTestUID(m message.Message) message.Message {
	m.UID = message.NewUID()
	return m
}
