package httprr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/httprr"
	"github.com/Dream355873200/GoAgent/provider/openai"
)

// fakeOpenAI 模拟 OpenAI 兼容服务：第一次请求让模型调用 echo，之后回复文本
// （带上工具结果，证明工具结果进了请求）。要求鉴权头。
func fakeOpenAI(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "session=abc")
		chunk := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if n == 1 {
			chunk(map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": "echo", "arguments": `{"s":"录制"}`}}}}}}})
			chunk(map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		} else {
			last := fmt.Sprint(req.Messages[len(req.Messages)-1].Content)
			chunk(map[string]any{"id": "c2", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "工具说：" + last}}}})
			chunk(map[string]any{"id": "c2", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func runAgent(t *testing.T, client *http.Client, baseURL, key string) (string, int) {
	t.Helper()
	prov := openai.New(openai.Config{APIKey: key, BaseURL: baseURL, Model: "gpt-test", HTTPClient: client})
	app := goagent.New(goagent.WithProvider(prov))
	var echoed int
	app.Tool("echo", goagent.ToolDef{Description: "echo", Input: struct {
		S string `json:"s"`
	}{}, Permission: goagent.ReadOnly,
		Execute: func(_ goagent.Context, in struct {
			S string `json:"s"`
		}) (string, error) {
			echoed++
			return "回声:" + in.S, nil
		}})
	var text strings.Builder
	for ev := range app.Run(context.Background(), "试试 echo") {
		switch ev.Type {
		case goagent.EventTextDelta:
			text.WriteString(ev.Text)
		case goagent.EventError:
			t.Fatalf("运行出错: %v", ev.Error)
		}
	}
	return text.String(), echoed
}

// 录制一次两轮的 agent 运行（真实 OpenAI provider → 假服务），文件里没有密钥与
// Cookie；之后关掉服务、换一个密钥回放，得到同样的结果，工具照常执行。
func TestRecordThenReplayAgentRun(t *testing.T) {
	var calls atomic.Int32
	srv := fakeOpenAI(t, &calls)
	path := filepath.Join(t.TempDir(), "agent.httprr")

	rr, err := httprr.OpenMode(path, httprr.Record, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorded, echoed := runAgent(t, rr.Client(), srv.URL, "sk-secret")
	if err := rr.Close(); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if recorded != "工具说：回声:录制" || echoed != 1 || calls.Load() != 2 {
		t.Fatalf("录制运行: text=%q echoed=%d calls=%d", recorded, echoed, calls.Load())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-secret", "session=abc", "User-Agent"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("录制文件不应包含 %q:\n%s", secret, data)
		}
	}
	if !bytes.HasPrefix(data, []byte("httprr trace v1\n")) || !bytes.Contains(data, []byte(`"tool_calls"`)) {
		t.Fatalf("录制文件格式:\n%s", data)
	}

	rr, err = httprr.OpenMode(path, httprr.Replay, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, echoed := runAgent(t, rr.Client(), srv.URL, "another-key")
	if replayed != recorded || echoed != 1 {
		t.Fatalf("回放结果应与录制一致: %q echoed=%d", replayed, echoed)
	}
}

// 同一请求录到多次时按顺序回放（先失败后成功的重试）；多发一次、请求不同都报错。
func TestReplayQueueAndMismatch(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "ok:%s", body)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "queue.httprr")

	post := func(c *http.Client, body string) (int, string, error) {
		resp, err := c.Post(srv.URL+"/v1/x?key=SECRET&v=2", "text/plain", strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	rr, err := httprr.OpenMode(path, httprr.Record, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{429, 200} {
		if code, _, err := post(rr.Client(), "same"); err != nil || code != want {
			t.Fatalf("录制: code=%d err=%v want %d", code, err, want)
		}
	}
	rr.Close()
	if data, _ := os.ReadFile(path); bytes.Contains(data, []byte("SECRET")) {
		t.Fatal("URL 里的 key 参数应被清洗")
	}

	rr, err = httprr.OpenMode(path, httprr.Replay, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := rr.Client()
	if code, _, _ := post(c, "same"); code != 429 {
		t.Fatalf("第一次回放应是录制的 429, got %d", code)
	}
	if code, body, _ := post(c, "same"); code != 200 || body != "ok:same" {
		t.Fatalf("第二次回放应是录制的 200: %d %q", code, body)
	}
	if _, _, err := post(c, "same"); err == nil || !strings.Contains(err.Error(), "已用完") {
		t.Fatalf("多发一次应报错, got %v", err)
	}
	if _, _, err := post(c, "different"); err == nil || !strings.Contains(err.Error(), "没有这个请求") || !strings.Contains(err.Error(), "1 个录制请求的方法与 URL 相同") {
		t.Fatalf("请求体不同应报错并提示, got %v", err)
	}
}

// 回放时文件不存在：错误包装 fs.ErrNotExist（测试据此跳过）；模式按环境变量选择。
func TestOpenModes(t *testing.T) {
	_, err := httprr.Open(filepath.Join(t.TempDir(), "missing.httprr"), nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("缺文件应包装 fs.ErrNotExist, got %v", err)
	}
	t.Setenv(httprr.EnvVar, "")
	if httprr.ModeFor("testdata/a.httprr") != httprr.Replay {
		t.Fatal("未设置环境变量应回放")
	}
	t.Setenv(httprr.EnvVar, "record")
	if httprr.ModeFor("testdata/a.httprr") != httprr.Record {
		t.Fatal("record 应录制")
	}
	t.Setenv(httprr.EnvVar, `weather`)
	if httprr.ModeFor("testdata/weather.httprr") != httprr.Record || httprr.ModeFor("testdata/a.httprr") != httprr.Replay {
		t.Fatal("正则只录制匹配的文件")
	}
}

// 自定义清洗同时作用于录制文件与回放的匹配键：请求体里随时间变化的部分替换掉
// 之后，回放时时间戳不同也能匹配。
func TestScrubReqAppliesToMatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "scrub.httprr")
	ts := regexp.MustCompile(`"ts":[0-9]+`)
	scrubTime := func(r *http.Request) error {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		r.Body = io.NopCloser(bytes.NewReader(ts.ReplaceAll(b, []byte(`"ts":0`))))
		return nil
	}
	post := func(c *http.Client, body string) error {
		resp, err := c.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	rr, err := httprr.OpenMode(path, httprr.Record, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr.ScrubReq(scrubTime)
	if err := post(rr.Client(), `{"ts":111,"q":"x"}`); err != nil {
		t.Fatal(err)
	}
	rr.Close()
	if data, _ := os.ReadFile(path); !bytes.Contains(data, []byte(`{"ts":0,"q":"x"}`)) {
		t.Fatalf("录制文件应是清洗后的请求体:\n%s", data)
	}

	rr, err = httprr.OpenMode(path, httprr.Replay, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr.ScrubReq(scrubTime)
	if err := post(rr.Client(), `{"ts":222,"q":"x"}`); err != nil {
		t.Fatalf("清洗后应能匹配: %v", err)
	}
}
