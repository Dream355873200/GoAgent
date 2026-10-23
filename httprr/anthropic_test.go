package httprr_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/httprr"
	"github.com/Dream355873200/GoAgent/provider/anthropic"
)

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好，回放"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func runAnthropic(t *testing.T, client *http.Client, baseURL, key string) string {
	t.Helper()
	prov := anthropic.New(key, anthropic.WithBaseURL(baseURL), anthropic.WithModel("claude-test"), anthropic.WithHTTPClient(client))
	app := goagent.New(goagent.WithProvider(prov))
	var text strings.Builder
	for ev := range app.Run(context.Background(), "你好") {
		switch ev.Type {
		case goagent.EventTextDelta:
			text.WriteString(ev.Text)
		case goagent.EventError:
			t.Fatalf("运行出错: %v", ev.Error)
		}
	}
	return text.String()
}

// Anthropic SDK 的请求带 x-api-key 与随版本、机器变化的 X-Stainless-* 头：录制
// 文件里都没有，换密钥回放照样匹配。
func TestAnthropicRecordReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "sk-ant-secret" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicSSE)
	}))
	path := filepath.Join(t.TempDir(), "anthropic.httprr")

	rr, err := httprr.OpenMode(path, httprr.Record, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runAnthropic(t, rr.Client(), srv.URL, "sk-ant-secret"); got != "你好，回放" {
		t.Fatalf("录制: %q", got)
	}
	rr.Close()
	srv.Close()
	data, _ := os.ReadFile(path)
	for _, s := range []string{"sk-ant-secret", "X-Stainless", "x-stainless"} {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("录制文件不应包含 %q", s)
		}
	}

	rr, err = httprr.OpenMode(path, httprr.Replay, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runAnthropic(t, rr.Client(), srv.URL, "other-key"); got != "你好，回放" {
		t.Fatalf("回放: %q", got)
	}
}
