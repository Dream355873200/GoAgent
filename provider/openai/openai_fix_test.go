package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// ── 测试辅助 ──────────────────────────────────────────────────────

// sseStream 把若干 chunk JSON 拼成以 [DONE] 结尾的 SSE 流。
func sseStream(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// recorder 记录测试服务器收到的请求体。
type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
	hits   atomic.Int32
}

func (r *recorder) last(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("服务器未收到请求")
	}
	return r.bodies[len(r.bodies)-1]
}

// newServer 启动 OpenAI 兼容的测试服务器；handler 拿到原始请求体决定如何响应。
func newServer(t *testing.T, cfg Config, handler func(w http.ResponseWriter, raw []byte)) (*Provider, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径 = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("请求体不是合法 JSON：%v", err)
		}
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		handler(w, raw)
	}))
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL + "/v1"
	if cfg.Model == "" {
		cfg.Model = "cfg-model"
	}
	return New(cfg), rec
}

func sseReply(body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func jsonReply(status int, body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const (
	completionOK = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	chunkStop    = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
)

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

func streamAll(t *testing.T, p *Provider, req *provider.Request) []provider.StreamEvent {
	t.Helper()
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream 返回错误：%v", err)
	}
	return collect(t, ch)
}

func hiRequest() *provider.Request {
	return &provider.Request{Messages: []message.Message{message.NewUserMessage("hi")}}
}

// imageMessages 构造带内联图片标记的工具结果，触发图片编码逻辑。
func imageMessages() []message.Message {
	b64 := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	return []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "tool_use", ToolUseID: "t1", ToolName: "look", Input: []byte(`{}`)}}},
		{Role: message.RoleUser, Content: []message.ContentBlock{{Type: "tool_result", ForToolUseID: "t1", Text: "[IMAGE png " + b64 + "]\n截图"}}},
	}
}

// ── 工具调用顺序 ──────────────────────────────────────────────────

func TestStreamToolCallsInIndexOrder(t *testing.T) {
	// 三个工具调用的首个分片按 index 2、0、1 乱序到达，参数分片也交错。
	body := sseStream(
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"tool_c","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":"{\"n\":"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"tool_b","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"0}"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"n\":2}"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	p, _ := newServer(t, Config{}, sseReply(body))
	wantIDs := []string{"call_a", "call_b", "call_c"}
	wantArgs := []string{`{"n":0}`, `{}`, `{"n":2}`}

	// map 遍历顺序随机，多跑几轮确保顺序是确定的而非碰巧。
	for round := 0; round < 20; round++ {
		events := streamAll(t, p, hiRequest())
		starts := eventsOf(events, provider.EventToolUseStart)
		if len(starts) != 3 {
			t.Fatalf("ToolUseStart 数量 = %d", len(starts))
		}
		for i, ev := range starts {
			if ev.ToolCall.ID != wantIDs[i] || string(ev.ToolCall.Input) != wantArgs[i] {
				t.Fatalf("第 %d 轮第 %d 个工具 = %s %s，期望 %s %s", round, i, ev.ToolCall.ID, ev.ToolCall.Input, wantIDs[i], wantArgs[i])
			}
		}
		done := eventsOf(events, provider.EventMessageComplete)
		if len(done) != 1 {
			t.Fatalf("MessageComplete 数量 = %d", len(done))
		}
		blocks := done[0].Message.Content
		if len(blocks) != 3 {
			t.Fatalf("完整消息块数 = %d", len(blocks))
		}
		for i, b := range blocks {
			if b.ToolUseID != wantIDs[i] {
				t.Fatalf("第 %d 轮完整消息第 %d 块 = %s，期望 %s", round, i, b.ToolUseID, wantIDs[i])
			}
		}
	}
}

func TestStreamDuplicateFinishReasonIgnored(t *testing.T) {
	body := sseStream(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
	)
	p, _ := newServer(t, Config{}, sseReply(body))
	events := streamAll(t, p, hiRequest())
	if n := len(eventsOf(events, provider.EventToolUseStart)); n != 1 {
		t.Errorf("工具调用被派发 %d 次，期望 1", n)
	}
	if n := len(eventsOf(events, provider.EventMessageComplete)); n != 1 {
		t.Errorf("MessageComplete 数量 = %d，期望 1", n)
	}
	usages := eventsOf(events, provider.EventUsage)
	if len(usages) != 1 || *usages[0].Usage != (provider.Usage{InputTokens: 10, OutputTokens: 2}) {
		t.Errorf("Usage 事件不符：%+v", usages)
	}
}

// ── stream_options 与用量 ─────────────────────────────────────────

func TestStreamOptionsIncludeUsage(t *testing.T) {
	reply := func(w http.ResponseWriter, raw []byte) {
		if strings.Contains(string(raw), `"stream":true`) {
			sseReply(sseStream(chunkStop))(w, raw)
			return
		}
		jsonReply(200, completionOK)(w, raw)
	}

	p, rec := newServer(t, Config{}, reply)
	streamAll(t, p, hiRequest())
	opts, _ := rec.last(t)["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Errorf("流式请求应携带 stream_options.include_usage=true，实际 %v", rec.last(t)["stream_options"])
	}
	// 非流式请求带 stream_options 会被官方 API 拒绝。
	if _, err := p.Complete(context.Background(), hiRequest()); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.last(t)["stream_options"]; ok {
		t.Error("非流式请求不应携带 stream_options")
	}

	p2, rec2 := newServer(t, Config{DisableStreamUsage: true}, reply)
	streamAll(t, p2, hiRequest())
	if _, ok := rec2.last(t)["stream_options"]; ok {
		t.Error("DisableStreamUsage 时不应携带 stream_options")
	}
}

func TestStreamUsageReportedOnceWithCacheAccounting(t *testing.T) {
	want := provider.Usage{InputTokens: 70, OutputTokens: 7, CacheReadTokens: 30}
	cases := map[string]string{
		// finish 块与随后的纯 usage 块都带用量，后者是终值。
		"finish 与 usage 块都带用量": sseStream(
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":30}}}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30}}}`,
		),
		// 官方 OpenAI：usage 块在 finish_reason 之后、[DONE] 之前单独到达。
		"usage 块晚于 finish": sseStream(
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}],"usage":null}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":30}}}`,
		),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := newServer(t, Config{}, sseReply(body))
			events := streamAll(t, p, hiRequest())
			usages := eventsOf(events, provider.EventUsage)
			if len(usages) != 1 {
				t.Fatalf("EventUsage 数量 = %d，期望 1", len(usages))
			}
			if *usages[0].Usage != want {
				t.Errorf("Usage = %+v，期望 %+v", *usages[0].Usage, want)
			}
			if n := len(eventsOf(events, provider.EventMessageComplete)); n != 1 {
				t.Errorf("MessageComplete 数量 = %d", n)
			}
		})
	}
}

func TestCompleteCacheAccounting(t *testing.T) {
	p, _ := newServer(t, Config{}, jsonReply(200, `{"id":"x","object":"chat.completion",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":30}}}`))
	resp, err := p.Complete(context.Background(), hiRequest())
	if err != nil {
		t.Fatal(err)
	}
	if want := (provider.Usage{InputTokens: 70, OutputTokens: 5, CacheReadTokens: 30}); resp.Usage != want {
		t.Errorf("Usage = %+v，期望 %+v", resp.Usage, want)
	}

	// 异常后端报告的缓存数大于 prompt_tokens 时 InputTokens 不为负。
	u := (&chatUsage{PromptTokens: 10, PromptTokensDetails: &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 12}}).toUsage()
	if u.InputTokens != 0 || u.CacheReadTokens != 12 {
		t.Errorf("toUsage = %+v", u)
	}
}

// ── 错误分类 ──────────────────────────────────────────────────────

func TestHandleHTTPErrorClassification(t *testing.T) {
	p := New(Config{BaseURL: "http://x/v1"})
	classify := func(status int, body string, header http.Header) error {
		if header == nil {
			header = http.Header{}
		}
		return p.handleHTTPError(&http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))})
	}

	overflow := []struct {
		status int
		body   string
	}{
		{400, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`},
		{400, `{"error":{"message":"Prompt is too long: 210000 tokens > 200000 maximum"}}`},
		{400, `{"error":"Too many tokens in request"}`},
		{400, `{"error":{"message":"input exceeds the context window of this model"}}`},
		{400, `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`},
		{422, `{"detail":"This model's maximum context length is 32768 tokens"}`},
		{413, `Request Entity Too Large`},
	}
	for _, c := range overflow {
		if _, ok := classify(c.status, c.body, nil).(*provider.PromptTooLongError); !ok {
			t.Errorf("HTTP %d %s 应为 PromptTooLongError", c.status, c.body)
		}
	}
	if err := classify(400, `{"error":{"message":"Invalid value for temperature"}}`, nil); errors.As(err, new(*provider.PromptTooLongError)) {
		t.Errorf("普通 400 不应识别为超窗：%v", err)
	}

	rl, ok := classify(429, `rate limited`, http.Header{"Retry-After": {"12"}}).(*provider.RateLimitError)
	if !ok || rl.RetryAfter != 12*time.Second {
		t.Errorf("429 应为 RateLimitError 且 RetryAfter=12s，得到 %#v", rl)
	}
	date := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	rl, ok = classify(429, `rate limited`, http.Header{"Retry-After": {date}}).(*provider.RateLimitError)
	if !ok || rl.RetryAfter < 50*time.Second || rl.RetryAfter > time.Minute {
		t.Errorf("HTTP 日期形式的 Retry-After 解析错误：%#v", rl)
	}
	rl, ok = classify(429, `rate limited`, nil).(*provider.RateLimitError)
	if !ok || rl.RetryAfter != 0 {
		t.Errorf("无 Retry-After 时应为 0：%#v", rl)
	}

	for _, status := range []int{529, 503} {
		if _, ok := classify(status, `busy`, nil).(*provider.OverloadError); !ok {
			t.Errorf("HTTP %d 应为 OverloadError", status)
		}
	}
}

func TestContextOverflowReturnedFromStreamAndComplete(t *testing.T) {
	body := `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`
	p, _ := newServer(t, Config{}, jsonReply(400, body))
	// loop 的 withholding 与 yolo 分类器用类型断言识别，必须直接返回该类型。
	if _, err := p.Stream(context.Background(), hiRequest()); !isPTL(err) {
		t.Errorf("Stream 应返回 PromptTooLongError，得到 %#v", err)
	}
	if _, err := p.Complete(context.Background(), hiRequest()); !isPTL(err) {
		t.Errorf("Complete 应返回 PromptTooLongError，得到 %#v", err)
	}
}

func isPTL(err error) bool {
	_, ok := err.(*provider.PromptTooLongError)
	return ok
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"5":    5 * time.Second,
		"0.25": 250 * time.Millisecond,
		"":     0,
		"-1":   0,
		"NaN":  0,
		"x":    0,
		"1e15": maxRetryAfter,
		time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat): 0,
	}
	for v, want := range cases {
		if got := parseRetryAfter(http.Header{"Retry-After": {v}}); got != want {
			t.Errorf("Retry-After %q → %v，期望 %v", v, got, want)
		}
	}
}

// ── Request.Model 覆盖 ────────────────────────────────────────────

func TestRequestModelOverridesConfig(t *testing.T) {
	p, rec := newServer(t, Config{Model: "cfg-model"}, sseReply(sseStream(chunkStop)))

	req := hiRequest()
	req.Model = "req-model"
	streamAll(t, p, req)
	if got := rec.last(t)["model"]; got != "req-model" {
		t.Errorf("请求模型 = %v，期望 req-model", got)
	}
	streamAll(t, p, hiRequest())
	if got := rec.last(t)["model"]; got != "cfg-model" {
		t.Errorf("未指定 Request.Model 时应使用配置模型，实际 %v", got)
	}
}

// ── 图片编码自适应 ────────────────────────────────────────────────

func TestImageFormatSwitchIsIdempotentUnderConcurrentFailures(t *testing.T) {
	t.Setenv("FLAI_IMAGE_URL_FORMAT", "")
	p := New(Config{BaseURL: "http://x/v1", Model: "m"})
	imgErr := errors.New(`API 错误 (HTTP 400): {"error":"invalid image url"}`)

	used := p.imageFormat("m")
	// 两个用同一编码组装、同时失败的请求：只切换一次，不能来回翻转。
	for i := 0; i < 2; i++ {
		if !p.maybeSwitchImageFormat("m", used, imgErr) {
			t.Fatal("图片相关错误应触发重试")
		}
	}
	if got := p.imageFormat("m"); got != "base64" {
		t.Fatalf("编码 = %q，期望 base64", got)
	}
	if !p.maybeSwitchImageFormat("m", "base64", imgErr) || p.imageFormat("m") != "datauri" {
		t.Errorf("base64 失败后应切回 datauri，实际 %q", p.imageFormat("m"))
	}
	if p.imageFormat("other") != "" {
		t.Error("编码记忆应按模型隔离")
	}

	// 已识别的错误即使错误体含 image 字样也不切换编码。
	ptl := &provider.PromptTooLongError{Message: "maximum context length exceeded (image tokens)"}
	if p.maybeSwitchImageFormat("m", p.imageFormat("m"), ptl) {
		t.Error("超窗错误不应触发图片编码切换")
	}
}

func TestImageFormatRetryUsesRequestModel(t *testing.T) {
	t.Setenv("FLAI_IMAGE_URL_FORMAT", "")
	p, rec := newServer(t, Config{Model: "cfg-model"}, func(w http.ResponseWriter, raw []byte) {
		if strings.Contains(string(raw), "data:image/png;base64,") {
			jsonReply(400, `{"error":{"message":"invalid image url format"}}`)(w, raw)
			return
		}
		jsonReply(200, completionOK)(w, raw)
	})

	req := &provider.Request{Model: "vision-x", Messages: imageMessages()}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("切换编码重试后应成功：%v", err)
	}
	if rec.hits.Load() != 2 {
		t.Errorf("请求次数 = %d，期望 2", rec.hits.Load())
	}
	if p.imageFormat("vision-x") != "base64" || p.imageFormat("cfg-model") != "" {
		t.Errorf("编码应记在实际请求的模型下：vision-x=%q cfg-model=%q", p.imageFormat("vision-x"), p.imageFormat("cfg-model"))
	}
}

func TestConcurrentModelSwitchAndImageFormat(t *testing.T) {
	p := New(Config{BaseURL: "http://x/v1", Model: "m"})
	req := &provider.Request{Messages: imageMessages()}
	imgErr := errors.New("API 错误 (HTTP 400): invalid image")
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch g % 3 {
				case 0:
					p.SetModel(fmt.Sprintf("m%d", i%3))
				case 1:
					body := p.buildChatRequest(req, true)
					p.maybeSwitchImageFormat(body.Model, body.imgFormat, imgErr)
				case 2:
					_ = p.Capabilities()
				}
			}
		}(g)
	}
	wg.Wait()
}

// ── 失败请求转储 ──────────────────────────────────────────────────

func TestDumpFailedRequestIsPrivateAndSilent(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
	if got := os.TempDir(); got != dir {
		t.Skipf("无法重定向临时目录（%s）", got)
	}

	t.Setenv("FLAI_DEBUG_REQUEST", "")
	dumpFailedRequest([]byte(`{"skip":true}`))

	t.Setenv("FLAI_DEBUG_REQUEST", "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	dumpFailedRequest([]byte(`{"n":1}`))
	dumpFailedRequest([]byte(`{"n":2}`))
	os.Stdout = stdout
	_ = w.Close()
	if out, _ := io.ReadAll(r); len(out) > 0 {
		t.Errorf("转储不应写 stdout：%q", out)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "ycode-failed-request-*.json"))
	if len(files) != 2 {
		t.Fatalf("转储文件数 = %d，期望 2（关闭时不写，两次失败各一个文件）：%v", len(files), files)
	}
	name := regexp.MustCompile(`^ycode-failed-request-\d{8}-\d{6}-\d+\.json$`)
	got := map[string]bool{}
	for _, f := range files {
		if !name.MatchString(filepath.Base(f)) {
			t.Errorf("文件名应带时间戳：%s", filepath.Base(f))
		}
		data, _ := os.ReadFile(f)
		got[string(data)] = true
		if runtime.GOOS != "windows" { // Windows 不支持 Unix 权限位
			info, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s 权限 = %v，期望 0600", f, perm)
			}
		}
	}
	if !got[`{"n":1}`] || !got[`{"n":2}`] {
		t.Errorf("转储内容不符：%v", got)
	}
}
