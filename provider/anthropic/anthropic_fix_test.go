package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// ── 测试辅助 ──────────────────────────────────────────────────────

// sseBody 把若干 JSON 事件拼成 Anthropic SSE 流，event 名取自 JSON 的 type 字段。
func sseBody(t *testing.T, events ...string) string {
	t.Helper()
	var b strings.Builder
	for _, data := range events {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &head); err != nil {
			t.Fatalf("测试事件不是合法 JSON：%v\n%s", err, data)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", head.Type, data)
	}
	return b.String()
}

// newServer 启动测试服务器，返回指向它的 Provider 与请求计数。
func newServer(t *testing.T, handler http.HandlerFunc) (*Provider, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("请求路径 = %s，期望 /v1/messages", r.URL.Path)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New("test-key", WithBaseURL(srv.URL), WithModel("claude-sonnet-4-6")), &hits
}

func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// errorHandler 返回 API 错误响应；x-should-retry=false 让 SDK 不做自带重试。
func errorHandler(status int, body string, header map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func collect(t *testing.T, ch <-chan provider.StreamEvent) []provider.StreamEvent {
	t.Helper()
	var events []provider.StreamEvent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("等待流结束超时")
		}
	}
}

func eventsOf(events []provider.StreamEvent, typ provider.EventType) []provider.StreamEvent {
	var out []provider.StreamEvent
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

const (
	evMessageStart = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}`
	evPing         = `{"type":"ping"}`
	evTextStart    = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	evTextDelta    = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`
	evBlockStop0   = `{"type":"content_block_stop","index":0}`
	evEndTurn      = `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}`
	evMessageStop  = `{"type":"message_stop"}`
)

// ── thinking 签名与 redacted_thinking ─────────────────────────────

func TestStreamKeepsThinkingSignatureAndRedactedThinking(t *testing.T) {
	body := sseBody(t,
		evMessageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`,
		evPing,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAh"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"IM+sig=="}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3p=="}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Checking "}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"weather."}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":89}}`,
		evMessageStop,
	)
	p, _ := newServer(t, sseHandler(body))

	ch, err := p.Stream(context.Background(), &provider.Request{Messages: []message.Message{message.NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Stream 返回错误：%v", err)
	}
	events := collect(t, ch)

	if errs := eventsOf(events, provider.EventError); len(errs) > 0 {
		t.Fatalf("不应出现错误事件：%v", errs[0].Error)
	}
	var thinking strings.Builder
	for _, ev := range eventsOf(events, provider.EventThinkingDelta) {
		thinking.WriteString(ev.Thinking)
	}
	if thinking.String() != "Let me think." {
		t.Errorf("thinking 增量 = %q", thinking.String())
	}
	tools := eventsOf(events, provider.EventToolUseStart)
	if len(tools) != 1 || tools[0].ToolCall.ID != "toolu_01" || string(tools[0].ToolCall.Input) != `{"city":"Paris"}` {
		t.Fatalf("工具调用事件不符：%+v", tools)
	}

	done := eventsOf(events, provider.EventMessageComplete)
	if len(done) != 1 {
		t.Fatalf("MessageComplete 数量 = %d，期望 1", len(done))
	}
	if done[0].StopReason != provider.StopToolUse {
		t.Errorf("StopReason = %q", done[0].StopReason)
	}
	want := []message.ContentBlock{
		{Type: "thinking", Thinking: "Let me think.", Signature: "EqQBCgIYAhIM+sig=="},
		{Type: "redacted_thinking", Data: "EmwKAhgBEgy3va3p=="},
		{Type: "text", Text: "Checking weather."},
		{Type: "tool_use", ToolUseID: "toolu_01", ToolName: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
	}
	got := done[0].Message.Content
	if len(got) != len(want) {
		t.Fatalf("内容块数量 = %d，期望 %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Type != w.Type || g.Thinking != w.Thinking || g.Signature != w.Signature ||
			g.Data != w.Data || g.Text != w.Text || g.ToolUseID != w.ToolUseID ||
			g.ToolName != w.ToolName || string(g.Input) != string(w.Input) {
			t.Errorf("第 %d 块 = %+v，期望 %+v", i, g, w)
		}
	}
}

func TestConvertMessagesReplaysThinkingBlocks(t *testing.T) {
	msgs := []message.Message{
		message.NewUserMessage("天气如何"),
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "thinking", Thinking: "需要查天气", Signature: "sig-1"},
			{Type: "thinking", Thinking: "无签名的思考"}, // 回传必被拒，应跳过
			{Type: "redacted_thinking", Data: "ENC=="},
			{Type: "redacted_thinking"}, // 无密文，应跳过
			{Type: "text", Text: "查一下"},
			{Type: "tool_use", ToolUseID: "toolu_1", ToolName: "weather", Input: json.RawMessage(`{"city":"Paris"}`)},
		}},
		// 只剩无签名 thinking 的消息整条丢弃。
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "thinking", Thinking: "孤立思考"}}},
		message.NewToolResultMessage("toolu_1", "晴", false),
	}

	data, err := json.Marshal(convertMessages(msgs))
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("消息数 = %d，期望 3：%s", len(got), data)
	}
	content := got[1].Content
	var types []string
	for _, c := range content {
		types = append(types, c["type"].(string))
	}
	if strings.Join(types, ",") != "thinking,redacted_thinking,text,tool_use" {
		t.Fatalf("块顺序 = %v：%s", types, data)
	}
	if content[0]["signature"] != "sig-1" || content[0]["thinking"] != "需要查天气" {
		t.Errorf("thinking 块未带原签名回传：%v", content[0])
	}
	if content[1]["data"] != "ENC==" {
		t.Errorf("redacted_thinking 密文未回传：%v", content[1])
	}
	if strings.Contains(string(data), "无签名的思考") || strings.Contains(string(data), "孤立思考") {
		t.Errorf("无签名 thinking 不应回传：%s", data)
	}
}

func TestCompleteKeepsThinkingSignature(t *testing.T) {
	p, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",
			"content":[{"type":"thinking","thinking":"hmm","signature":"sig-x"},{"type":"redacted_thinking","data":"ENC=="},{"type":"text","text":"done"}],
			"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`)
	})

	// 非流式请求的 max_tokens 超过约 21k 时 SDK 会直接拒绝（建议改用流式），
	// 与仓库内的调用方一致显式给出较小的值。
	resp, err := p.Complete(context.Background(), &provider.Request{MaxTokens: 1024, Messages: []message.Message{message.NewUserMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Message.Content
	if len(c) != 3 ||
		c[0].Type != "thinking" || c[0].Thinking != "hmm" || c[0].Signature != "sig-x" ||
		c[1].Type != "redacted_thinking" || c[1].Data != "ENC==" ||
		c[2].Type != "text" || c[2].Text != "done" {
		t.Fatalf("内容块不符：%+v", c)
	}
	if want := (provider.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheCreateTokens: 4}); resp.Usage != want {
		t.Errorf("Usage = %+v，期望 %+v", resp.Usage, want)
	}
}

// ── 用量只上报一次 ────────────────────────────────────────────────

func TestStreamReportsUsageOnce(t *testing.T) {
	cases := []struct {
		name  string
		delta string
		want  provider.Usage
	}{
		{
			// 旧版 API：message_delta 只带 output，input 为 0 不能覆盖 message_start 的值。
			name:  "delta 不含 input",
			delta: `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":89}}`,
			want:  provider.Usage{InputTokens: 120, OutputTokens: 89, CacheReadTokens: 30, CacheCreateTokens: 10},
		},
		{
			name:  "delta 带完整累计值",
			delta: `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":120,"output_tokens":95,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`,
			want:  provider.Usage{InputTokens: 120, OutputTokens: 95, CacheReadTokens: 30, CacheCreateTokens: 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newServer(t, sseHandler(sseBody(t, evMessageStart, evTextStart, evTextDelta, evBlockStop0, tc.delta, evMessageStop)))
			ch, err := p.Stream(context.Background(), &provider.Request{Messages: []message.Message{message.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			events := collect(t, ch)
			usages := eventsOf(events, provider.EventUsage)
			if len(usages) != 1 {
				t.Fatalf("EventUsage 数量 = %d，期望 1", len(usages))
			}
			if *usages[0].Usage != tc.want {
				t.Errorf("Usage = %+v，期望 %+v", *usages[0].Usage, tc.want)
			}
			if len(eventsOf(events, provider.EventMessageComplete)) != 1 {
				t.Error("应有且仅有一个 MessageComplete")
			}
		})
	}
}

// ── 错误分类 ──────────────────────────────────────────────────────

// newAPIError 构造 SDK 的 API 错误，与 SDK 内部的构造方式一致。
func newAPIError(t *testing.T, status int, body string, header http.Header) *anthropic.Error {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	e := &anthropic.Error{
		StatusCode: status,
		Request:    httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil),
		Response:   &http.Response{StatusCode: status, Header: header},
	}
	if err := e.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestClassifyAPIError(t *testing.T) {
	rateLimited := newAPIError(t, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`,
		http.Header{"Retry-After": []string{"7"}})
	if rl, ok := classifyError(rateLimited).(*provider.RateLimitError); !ok || rl.RetryAfter != 7*time.Second {
		t.Fatalf("429 应为 RateLimitError 且 RetryAfter=7s，得到 %#v", classifyError(rateLimited))
	}
	// 包装后的错误同样能识别。
	if _, ok := classifyError(fmt.Errorf("调用失败: %w", rateLimited)).(*provider.RateLimitError); !ok {
		t.Error("包装后的 429 应为 RateLimitError")
	}

	for _, status := range []int{529, 503, 500} {
		e := newAPIError(t, status, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil)
		if _, ok := classifyError(e).(*provider.OverloadError); !ok {
			t.Errorf("%d 应为 OverloadError，得到 %#v", status, classifyError(e))
		}
	}

	tooLong := newAPIError(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Prompt is too long: 210000 tokens > 200000 maximum"}}`, nil)
	if _, ok := classifyError(tooLong).(*provider.PromptTooLongError); !ok {
		t.Errorf("400 prompt is too long 应为 PromptTooLongError，得到 %#v", classifyError(tooLong))
	}
	if _, ok := classifyError(newAPIError(t, 413, `{}`, nil)).(*provider.PromptTooLongError); !ok {
		t.Error("413 应为 PromptTooLongError")
	}

	// 其他 4xx 原样返回，调用方仍能取到 SDK 错误。
	badReq := newAPIError(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be positive"}}`, nil)
	var apiErr *anthropic.Error
	if err := classifyError(badReq); !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Errorf("普通 400 应原样返回，得到 %#v", err)
	}
}

func TestClassifyAPIErrorWithoutRequestDoesNotPanic(t *testing.T) {
	// SDK 的 Error() 不判空 Request/Response，分类时不能直接调用。
	e := &anthropic.Error{StatusCode: 429}
	rl, ok := classifyError(e).(*provider.RateLimitError)
	if !ok || !strings.Contains(rl.Message, "429") || rl.RetryAfter != 0 {
		t.Fatalf("得到 %#v", classifyError(e))
	}
}

func TestClassifyErrorTextFallback(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{`received error while streaming: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "overload"},
		{`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "ratelimit"},
		{`POST "https://api.anthropic.com/v1/messages": 429 Too Many Requests {}`, "ratelimit"},
		{`POST "https://api.anthropic.com/v1/messages": 529 status code 529 {}`, "overload"},
		{`POST "https://api.anthropic.com/v1/messages": 413 Request Entity Too Large {}`, "ptl"},
		{`dial tcp: connection refused`, "other"},
	}
	for _, tc := range cases {
		var got string
		switch classifyError(errors.New(tc.msg)).(type) {
		case *provider.OverloadError:
			got = "overload"
		case *provider.RateLimitError:
			got = "ratelimit"
		case *provider.PromptTooLongError:
			got = "ptl"
		default:
			got = "other"
		}
		if got != tc.want {
			t.Errorf("%q → %s，期望 %s", tc.msg, got, tc.want)
		}
	}
}

func TestStreamHTTPErrorsAreTyped(t *testing.T) {
	req := &provider.Request{MaxTokens: 1024, Messages: []message.Message{message.NewUserMessage("hi")}}

	t.Run("429 RateLimitError", func(t *testing.T) {
		p, _ := newServer(t, errorHandler(429, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`,
			map[string]string{"Retry-After": "3"}))
		_, err := p.Stream(context.Background(), req)
		rl, ok := err.(*provider.RateLimitError)
		if !ok || rl.RetryAfter != 3*time.Second {
			t.Fatalf("Stream 应同步返回 RateLimitError 且 RetryAfter=3s，得到 %#v", err)
		}
		if _, err := p.Complete(context.Background(), req); !errors.As(err, &rl) || rl.RetryAfter != 3*time.Second {
			t.Fatalf("Complete 应返回 RateLimitError，得到 %#v", err)
		}
	})

	t.Run("529 OverloadError", func(t *testing.T) {
		p, _ := newServer(t, errorHandler(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil))
		if _, err := p.Stream(context.Background(), req); !isType[*provider.OverloadError](err) {
			t.Fatalf("得到 %#v", err)
		}
	})

	t.Run("400 PromptTooLongError", func(t *testing.T) {
		p, _ := newServer(t, errorHandler(400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, nil))
		_, err := p.Stream(context.Background(), req)
		// loop 的 withholding 用类型断言而非 errors.As，必须直接返回该类型。
		if _, ok := err.(*provider.PromptTooLongError); !ok {
			t.Fatalf("得到 %#v", err)
		}
	})
}

func isType[T error](err error) bool {
	_, ok := err.(T)
	return ok
}

func TestStreamMidStreamErrorEvent(t *testing.T) {
	cases := map[string]func(error) bool{
		"overloaded_error": isType[*provider.OverloadError],
		"rate_limit_error": isType[*provider.RateLimitError],
	}
	for errType, check := range cases {
		t.Run(errType, func(t *testing.T) {
			body := sseBody(t, evMessageStart, evTextStart, evTextDelta) +
				fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"x\"}}\n\n", errType)
			p, _ := newServer(t, sseHandler(body))
			ch, err := p.Stream(context.Background(), &provider.Request{Messages: []message.Message{message.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			events := collect(t, ch)
			errs := eventsOf(events, provider.EventError)
			if len(errs) != 1 || !check(errs[0].Error) {
				t.Fatalf("应有一个 %s 对应的类型化错误事件，得到 %+v", errType, errs)
			}
			if len(eventsOf(events, provider.EventMessageComplete)) != 0 || len(eventsOf(events, provider.EventUsage)) != 0 {
				t.Error("流中途失败不应再发 Usage / MessageComplete")
			}
		})
	}
}

func TestSDKRetryStillApplies(t *testing.T) {
	var n atomic.Int32
	ok := sseHandler(sseBody(t, evMessageStart, evTextStart, evTextDelta, evBlockStop0, evEndTurn, evMessageStop))
	p, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		ok(w, r)
	})
	ch, err := p.Stream(context.Background(), &provider.Request{Messages: []message.Message{message.NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("SDK 默认重试 2 次后应成功，得到 %v", err)
	}
	if done := eventsOf(collect(t, ch), provider.EventMessageComplete); len(done) != 1 {
		t.Fatal("应收到 MessageComplete")
	}
	if hits.Load() != 3 {
		t.Errorf("请求次数 = %d，期望 3（首次 + SDK 重试 2 次）", hits.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	cases := []struct {
		value    string
		min, max time.Duration
	}{
		{"7", 7 * time.Second, 7 * time.Second},
		{"1.5", 1500 * time.Millisecond, 1500 * time.Millisecond},
		{"", 0, 0},
		{"0", 0, 0},
		{"-3", 0, 0},
		{"NaN", 0, 0},
		{"soon", 0, 0},
		{"1e12", maxRetryAfter, maxRetryAfter},
		{future, 80 * time.Second, 90 * time.Second},
		{past, 0, 0},
	}
	for _, tc := range cases {
		got := parseRetryAfter(http.Header{"Retry-After": []string{tc.value}})
		if got < tc.min || got > tc.max {
			t.Errorf("Retry-After %q → %v，期望 [%v, %v]", tc.value, got, tc.min, tc.max)
		}
	}
	if parseRetryAfter(nil) != 0 {
		t.Error("nil header 应返回 0")
	}
}

// ── 并发与能力表 ──────────────────────────────────────────────────

func TestSetModelConcurrentWithReaders(t *testing.T) {
	p := New("k")
	models := []string{"claude-opus-4-6", "claude-sonnet-4-6-v1", "claude-haiku-4-5-20251001"}
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				switch g % 3 {
				case 0:
					p.SetModel(models[i%len(models)])
				case 1:
					_ = p.Capabilities()
				case 2:
					if _, err := p.buildParams(&provider.Request{}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestCapabilitiesTolerantLookup(t *testing.T) {
	cases := []struct {
		model    string
		thinking bool
	}{
		{"claude-sonnet-4-6", true},
		{"claude-sonnet-4-6-v1", true},
		{"claude-opus-4-6-v1:0", true},
		{"us.anthropic.claude-sonnet-4-6-20250929-v1:0", true},
		{"claude-sonnet-4-6@20250929", true},
		{"Claude-Opus-4-6", true},
		{"claude-haiku-4-5", false},
		{"claude-haiku-4-5-20251001", false},
		{"claude-opus-4-1-20250805", true}, // 未登记，按家族推断
		{"anthropic/claude-sonnet-4.5", true},
		{"glm-4.6", false}, // 非 Claude 模型走默认值
	}
	for _, tc := range cases {
		caps := capabilitiesFor(tc.model)
		if caps.SupportsThinking != tc.thinking {
			t.Errorf("%s SupportsThinking = %v，期望 %v", tc.model, caps.SupportsThinking, tc.thinking)
		}
		if caps.ModelID != tc.model {
			t.Errorf("%s ModelID = %q，应为实际配置的 ID", tc.model, caps.ModelID)
		}
		if caps.ContextWindow != 200_000 || caps.MaxOutputTokens != 64_000 || !caps.SupportsTools {
			t.Errorf("%s 基础能力不符：%+v", tc.model, caps)
		}
	}

	p := New("k")
	if caps := p.Capabilities(); caps.ModelID != "claude-sonnet-4-6-v1" || !caps.SupportsThinking {
		t.Errorf("默认模型能力不符：%+v", caps)
	}
	p.SetModel("claude-sonnet-4-6")
	if caps := p.Capabilities(); caps.ModelID != "claude-sonnet-4-6" || !caps.SupportsThinking {
		t.Errorf("无后缀模型能力不符：%+v", caps)
	}
}

// ── thinking 预算与 max_tokens ────────────────────────────────────

func TestThinkingBudgetRaisesMaxTokens(t *testing.T) {
	cases := []struct {
		name       string
		req        provider.Request
		wantMax    int64
		wantBudget int64
	}{
		{"显式预算超过 max_tokens", provider.Request{MaxTokens: 8000, Thinking: &provider.ThinkingConfig{Type: "enabled", BudgetTokens: 10000}}, 14096, 10000},
		{"预算等于 max_tokens", provider.Request{MaxTokens: 32768, ReasoningEffort: "high"}, 36864, 32768},
		{"max_tokens 足够", provider.Request{MaxTokens: 40000, ReasoningEffort: "high"}, 40000, 32768},
		{"默认 max_tokens", provider.Request{ReasoningEffort: "low"}, 64000, 8192},
		{"不启用思考", provider.Request{MaxTokens: 1000, ReasoningEffort: "off"}, 1000, 0},
	}
	p := New("k")
	for _, tc := range cases {
		params, err := p.buildParams(&tc.req)
		if err != nil {
			t.Fatal(err)
		}
		if params.MaxTokens != tc.wantMax {
			t.Errorf("%s：max_tokens = %d，期望 %d", tc.name, params.MaxTokens, tc.wantMax)
		}
		var budget int64
		if params.Thinking.OfEnabled != nil {
			budget = params.Thinking.OfEnabled.BudgetTokens
		}
		if budget != tc.wantBudget {
			t.Errorf("%s：budget_tokens = %d，期望 %d", tc.name, budget, tc.wantBudget)
		}
	}
}
