package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/message"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkServer 起一个官方 SDK 的 MCP 服务器（Streamable HTTP），供 Dial 端到端测试。
func sdkServer(t *testing.T, setup func(*sdk.Server)) (*sdk.Server, string) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "sdk-test", Version: "9"}, nil)
	setup(server)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return server, srv.URL
}

var objectSchema = map[string]any{"type": "object"}

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func dialSDKServer(t *testing.T, url string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, ServerConfig{Name: "sdk", URL: url})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect() })
	return c
}

// 服务端的进度通知经 WithProgressReporter 交给调用方。
func TestSDKProgress(t *testing.T) {
	_, url := sdkServer(t, func(s *sdk.Server) {
		s.AddTool(&sdk.Tool{Name: "slow", InputSchema: objectSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			token := req.Params.GetProgressToken()
			for i := 1; i <= 2; i++ {
				_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: token, Progress: float64(i), Total: 2, Message: "step"})
			}
			// 进度通知与响应在客户端分别分发，调用返回后到达的进度按设计丢弃：
			// 稍等再返回，让测试不依赖这一竞争。
			time.Sleep(100 * time.Millisecond)
			return textResult("finished"), nil
		})
	})
	c := dialSDKServer(t, url)
	if v := c.NegotiatedVersion(); v != DefaultProtocolVersion {
		t.Fatalf("应按默认协议版本握手, got %q", v)
	}
	tools, err := DiscoverAndConvert(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	ctx := WithProgressReporter(context.Background(), func(msg string) {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	})
	out, err := tools[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil || out != "finished" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[1] != "[2/2] step" {
		t.Fatalf("应收到两条进度, got %v", got)
	}
}

// 调用方取消 ctx：调用立即返回，服务端的处理函数也被取消（notifications/cancelled）。
func TestSDKCancellation(t *testing.T) {
	started := make(chan struct{})
	serverCancelled := make(chan struct{})
	_, url := sdkServer(t, func(s *sdk.Server) {
		s.AddTool(&sdk.Tool{Name: "hang", InputSchema: objectSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			close(started)
			<-ctx.Done()
			close(serverCancelled)
			return nil, ctx.Err()
		})
	})
	c := dialSDKServer(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "hang", nil)
		errc <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "已取消") {
			t.Fatalf("取消后应返回取消错误, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后调用应立即返回")
	}
	select {
	case <-serverCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("服务端应收到取消通知并取消处理函数")
	}
}

// 图片结果以内联图片标记交给模型；只有结构化结果时以其 JSON 作为文本。
func TestSDKImageAndStructuredResults(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	_, url := sdkServer(t, func(s *sdk.Server) {
		s.AddTool(&sdk.Tool{Name: "shot", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "截图如下"},
				&sdk.ImageContent{Data: png, MIMEType: "image/png"},
				&sdk.ImageContent{Data: png, MIMEType: "image/jpeg"},
			}}, nil
		})
		s.AddTool(&sdk.Tool{Name: "data", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{}, StructuredContent: map[string]any{"n": 3}}, nil
		})
	})
	c := dialSDKServer(t, url)
	tools, err := DiscoverAndConvert(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]FrameworkTool{}
	for _, tool := range tools {
		byName[tool.RemoteName] = tool
	}
	out, err := byName["shot"].Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	img, ok := message.SplitImageMarker(out)
	if !ok || !img.Valid || img.MIME() != "image/png" || img.Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("第一张图片应成为内联图片标记: ok=%v %+v", ok, img)
	}
	if !strings.Contains(img.Body, "截图如下") || !strings.Contains(img.Body, "[image image/jpeg") {
		t.Fatalf("其余内容应保留在正文（多余图片为占位说明）: %q", img.Body)
	}
	out, err = byName["data"].Execute(context.Background(), nil)
	if err != nil || out != `{"n":3}` {
		t.Fatalf("结构化结果应转为 JSON 文本: %q %v", out, err)
	}
}

// 服务端增删工具后发 tools/list_changed：客户端重新列出并通知订阅者。
func TestSDKToolsListChanged(t *testing.T) {
	server, url := sdkServer(t, func(s *sdk.Server) {
		s.AddTool(&sdk.Tool{Name: "a", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult("a"), nil
		})
	})
	c := dialSDKServer(t, url)
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := make(chan []ToolInfo, 4)
	c.OnToolsChanged(func(tools []ToolInfo) { changed <- tools })
	server.AddTool(&sdk.Tool{Name: "b", InputSchema: objectSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("b"), nil
	})
	deadline := time.After(10 * time.Second)
	for {
		select {
		case tools := <-changed:
			if len(tools) == 2 {
				return
			}
		case <-deadline:
			t.Fatal("服务端新增工具后应收到变更后的工具列表")
		}
	}
}
