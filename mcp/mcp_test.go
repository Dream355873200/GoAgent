package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 假 MCP 服务器：同一套应答逻辑，分别以 stdio 子进程（测试二进制自身
// 以 GOAGENT_MCP_FAKE=1 重入）和 httptest 服务器两种形态跑。

func TestMain(m *testing.M) {
	if os.Getenv("GOAGENT_MCP_FAKE") == "1" {
		runFakeStdioServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHandle 对一条请求给出结果（nil 结果 = 通知，不回复）。
func fakeHandle(msg wireMessage) (result any, rpcErr *ResponseError) {
	switch msg.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": "fake server!", "version": "1"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Cursor == "" {
			return map[string]any{
				"tools":      []any{map[string]any{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}}}},
				"nextCursor": "page2",
			}, nil
		}
		return map[string]any{"tools": []any{map[string]any{"name": "fail.tool"}}}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Name == "echo" {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(p.Arguments["msg"])}}}, nil
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}}, nil
	}
	return nil, &ResponseError{Code: -32601, Message: "method not found"}
}

func runFakeStdioServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var msg wireMessage
		if json.Unmarshal(in.Bytes(), &msg) != nil || len(msg.ID) == 0 || msg.Method == "" {
			continue // 通知 / 对 ping 的回复
		}
		if msg.Method == "tools/call" {
			// 响应前穿插：非 JSON 日志行、通知、服务端发起的 ping
			fmt.Fprintln(os.Stdout, "log: calling tool")
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{}})
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
		}
		result, rpcErr := fakeHandle(msg)
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			reply["error"] = rpcErr
		} else {
			reply["result"] = result
		}
		_ = out.Encode(reply)
	}
}

func fakeHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg wireMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if msg.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
		} else if r.Header.Get("Mcp-Session-Id") != "sess-1" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		result, rpcErr := fakeHandle(msg)
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			reply["error"] = rpcErr
		} else {
			reply["result"] = result
		}
		body, _ := json.Marshal(reply)
		if msg.Method == "tools/call" {
			// SSE 形态：先一条通知，再响应
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func exerciseServer(t *testing.T, cfg ServerConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, tools, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Disconnect()

	if len(tools) != 2 {
		t.Fatalf("翻页后应发现 2 个工具，得到 %d", len(tools))
	}
	if tools[0].Name != "mcp__fake__echo" || tools[1].Name != "mcp__fake__fail_tool" {
		t.Fatalf("工具注册名应取配置名并净化: %q %q", tools[0].Name, tools[1].Name)
	}
	if tools[1].InputSchema["type"] != "object" {
		t.Fatalf("缺省 schema 应补为 object: %v", tools[1].InputSchema)
	}
	if got := client.ServerInfo().Name; got != "fake server!" {
		t.Fatalf("服务端自报名: %q", got)
	}

	out, err := tools[0].Execute(ctx, json.RawMessage(`{"msg":"hi"}`))
	if err != nil || out != "hi" {
		t.Fatalf("echo: out=%q err=%v", out, err)
	}
	// 连续调用：id 配对不串（旧实现所有调用共用一个 id）
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("n%d", i)
		if out, err := tools[0].Execute(ctx, json.RawMessage(`{"msg":"`+msg+`"}`)); err != nil || out != msg {
			t.Fatalf("第 %d 次 echo: out=%q err=%v", i, out, err)
		}
	}
	if _, err := tools[1].Execute(ctx, nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("isError 结果应转为带正文的错误: %v", err)
	}
}

func TestStdioServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exerciseServer(t, ServerConfig{
		Name:    "fake",
		Command: exe,
		Args:    []string{"-test.run=^$"},
		Env:     map[string]string{"GOAGENT_MCP_FAKE": "1"},
	})
}

func TestHTTPServer(t *testing.T) {
	srv := fakeHTTPServer(t)
	defer srv.Close()
	exerciseServer(t, ServerConfig{Name: "fake", URL: srv.URL, Headers: map[string]string{"X-Token": "secret"}})
}

func TestStdioServerExitReportsStderr(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// 未设 GOAGENT_MCP_FAKE：子进程作为普通测试二进制跑一个不存在的测试后退出
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = Dial(ctx, ServerConfig{Name: "dead", Command: exe, Args: []string{"-test.run=^$"}})
	if err == nil {
		t.Fatal("服务器提前退出应报错（不能永久阻塞）")
	}
}

func TestSendRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // 读完请求体，服务端才能感知客户端断开
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, ServerConfig{Name: "slow", URL: srv.URL}); err == nil {
		t.Fatal("超时应返回错误")
	}
}

func TestValidateAndToolName(t *testing.T) {
	if (ServerConfig{Name: "a"}).Validate() == nil {
		t.Fatal("command/url 都缺应报错")
	}
	if (ServerConfig{Name: "a", Command: "x", URL: "y"}).Validate() == nil {
		t.Fatal("command/url 同时给应报错")
	}
	long := ToolName("srv", strings.Repeat("x", 100))
	if len(long) != 64 || !strings.HasPrefix(long, "mcp__srv__") {
		t.Fatalf("超长名应截断到 64: %q", long)
	}
}
