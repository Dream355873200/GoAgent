// protocol_test.go 统一信封协议测试。
//
// 覆盖：/chat 全流信封字段（run_start 首帧、seq 连接内单调、v 版本盖章、
// metadata 末帧）、GET /protocol 自描述、ask_user 结构化载荷透传。
package goagent

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/session"
)

// parseSSE 读完 SSE 流并解析全部 data 帧为信封列表。
func parseSSE(t *testing.T, body io.Reader) []protocol.Envelope {
	t.Helper()
	var frames []protocol.Envelope
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
			t.Fatalf("帧解析失败: %v (%s)", err, line)
		}
		frames = append(frames, env)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	return frames
}

// TestChatEnvelopeProtocol /chat 全流应符合统一信封契约：首帧 run_start、
// seq 从 1 起连接内严格递增、v 盖章协议版本、末帧 metadata。
func TestChatEnvelopeProtocol(t *testing.T) {
	app := New(
		WithProvider(&mockProvider{}),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
	)
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(`{"message":"hi","session_id":"p-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := parseSSE(t, resp.Body)
	if len(frames) < 3 {
		t.Fatalf("应至少有 run_start/事件/metadata 三帧, got %d", len(frames))
	}
	if frames[0].Type != protocol.TypeRunStart || frames[0].SessionID != "p-1" {
		t.Fatalf("首帧应为 run_start 且绑定会话, got %+v", frames[0])
	}
	if frames[len(frames)-1].Type != protocol.TypeMetadata {
		t.Fatalf("末帧应为 metadata, got %s", frames[len(frames)-1].Type)
	}
	for i, f := range frames {
		if f.Seq != int64(i+1) {
			t.Fatalf("seq 应连接内从 1 严格递增, frame[%d].seq=%d", i, f.Seq)
		}
		if f.Version != protocol.Version {
			t.Fatalf("帧应盖章协议版本 %d, got %d", protocol.Version, f.Version)
		}
		if f.Type == "" || f.Type == "unknown" {
			t.Fatalf("帧类型不应为空/unknown: %+v", f)
		}
	}
}

// TestProtocolSelfDescribe GET /protocol 返回协议自描述（版本/帧/端点）。
func TestProtocolSelfDescribe(t *testing.T) {
	app := New(WithProvider(&mockProvider{}))
	srv := httptest.NewServer(newHTTPMux(app))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/protocol")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var desc protocol.Description
	if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
		t.Fatal(err)
	}
	if desc.Version != protocol.Version {
		t.Fatalf("自描述版本应=%d, got %d", protocol.Version, desc.Version)
	}
	if len(desc.Events) == 0 || len(desc.Endpoints) == 0 {
		t.Fatal("自描述应含帧类型表与端点表")
	}
	found := false
	for _, ep := range desc.Endpoints {
		if ep.Path == "/chat" && ep.Method == "POST" {
			found = true
		}
	}
	if !found {
		t.Fatal("端点表应含 POST /chat")
	}
}

// TestAskUserPayloadForwarded AskStructured 的结构化载荷应随请求透传
// （http.go 的转发 goroutine 原样写入 ask_user 帧；通用库不感知语义，
// 只做透明搬运，客户端按 payload.kind 分发渲染）。
func TestAskUserPayloadForwarded(t *testing.T) {
	h := NewAskUserHandler()
	done := make(chan string, 1)
	go func() {
		answer, err := h.AskStructured("选一个", map[string]any{
			"kind": "confirm", "mode": "single", "choices": []string{"A", "B"},
		})
		if err != nil {
			t.Errorf("Ask 失败: %v", err)
		}
		done <- answer
	}()
	req := <-h.Requests()
	if req.Question != "选一个" {
		t.Fatalf("问题应透传, got %q", req.Question)
	}
	if req.Payload == nil || req.Payload["kind"] != "confirm" {
		t.Fatalf("载荷应随请求透传, got %+v", req.Payload)
	}
	req.Respond("A")
	if answer := <-done; answer != "A" {
		t.Fatalf("回答应回传, got %q", answer)
	}
}
