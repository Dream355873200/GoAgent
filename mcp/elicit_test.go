package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type testCtxKey struct{}

// elicitServer 的工具 ask 向客户端发起 elicitation，把回应原样写进结果；
// 工具 link 发起 url 模式的 elicitation；hold 在 release 关闭前不返回。
func elicitServer(t *testing.T, release chan struct{}) string {
	t.Helper()
	_, url := sdkServer(t, func(s *sdk.Server) {
		s.AddTool(&sdk.Tool{Name: "ask", InputSchema: objectSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{
				Message: "请提供姓名与年龄",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{
					"name": map[string]any{"type": "string", "title": "姓名"},
					"age":  map[string]any{"type": "integer"},
				}, "required": []any{"name"}},
			})
			if err != nil {
				return textResult("elicit error: " + err.Error()), nil
			}
			b, _ := json.Marshal(res.Content)
			return textResult(res.Action + " " + string(b)), nil
		})
		s.AddTool(&sdk.Tool{Name: "link", InputSchema: objectSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{Mode: "url", Message: "请授权", URL: "https://auth.example/x", ElicitationID: "e-1"})
			if err != nil {
				return textResult("elicit error: " + err.Error()), nil
			}
			return textResult(res.Action), nil
		})
		s.AddTool(&sdk.Tool{Name: "hold", InputSchema: objectSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return textResult("held"), nil
		})
	})
	return url
}

func dialElicit(t *testing.T, url string, h ElicitHandler) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, ServerConfig{Name: "el", URL: url, Elicit: h})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect() })
	return c
}

func callText(t *testing.T, c *Client, ctx context.Context, name string) string {
	t.Helper()
	res, err := c.CallTool(ctx, name, map[string]any{})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.ModelText()
}

// form 模式：处理函数拿到消息、表单与服务器名，ctx 带着触发它的调用的值；
// 接受的内容、拒绝都回到服务器。url 模式拿到地址与 ID。
func TestElicitFormAndURL(t *testing.T) {
	var mu sync.Mutex
	var got []Elicitation
	action := ElicitAccept
	c := dialElicit(t, elicitServer(t, nil), func(ctx context.Context, e Elicitation) (ElicitResponse, error) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		if ctx.Value(testCtxKey{}) != "call-1" {
			return ElicitResponse{}, fmt.Errorf("ctx 应带调用方的值, got %v", ctx.Value(testCtxKey{}))
		}
		return ElicitResponse{Action: action, Content: map[string]any{"name": "Ada", "age": 36}}, nil
	})
	ctx := context.WithValue(context.Background(), testCtxKey{}, "call-1")

	if out := callText(t, c, ctx, "ask"); out != `accept {"age":36,"name":"Ada"}` {
		t.Fatalf("接受的内容应到达服务器: %q", out)
	}
	action = ElicitDecline
	if out := callText(t, c, ctx, "ask"); out != "decline null" {
		t.Fatalf("拒绝应到达服务器（不带内容）: %q", out)
	}
	action = ElicitAccept
	if out := callText(t, c, ctx, "link"); out != "accept" {
		t.Fatalf("url 模式: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	form, link := got[0], got[2]
	props, _ := form.Schema["properties"].(map[string]any)
	if form.Server != "el" || form.Mode != "form" || form.Message != "请提供姓名与年龄" || len(props) != 2 {
		t.Fatalf("form elicitation: %+v", form)
	}
	if link.Mode != "url" || link.URL != "https://auth.example/x" || link.ID != "e-1" || link.Schema != nil {
		t.Fatalf("url elicitation: %+v", link)
	}
}

// 没有设置处理函数时不声明能力，服务器的 Elicit 直接失败。
func TestElicitNotAdvertisedWithoutHandler(t *testing.T) {
	c := dialElicit(t, elicitServer(t, nil), nil)
	if out := callText(t, c, context.Background(), "ask"); !strings.Contains(out, "elicit error") {
		t.Fatalf("没有处理函数时服务器应拿到错误: %q", out)
	}
}

// 几个调用同时进行：同一范围内交给最新的调用；范围不同或缺失时无法确定，
// ctx 不带调用方的值。
func TestElicitRoutingWithConcurrentCalls(t *testing.T) {
	for _, tc := range []struct {
		name            string
		holdScope       string
		askScope        string
		wantCallerValue bool
	}{
		{"同一范围", "sess-1", "sess-1", true},
		{"不同范围", "sess-1", "sess-2", false},
		{"缺少范围", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var seen any
			c := dialElicit(t, elicitServer(t, release), func(ctx context.Context, e Elicitation) (ElicitResponse, error) {
				seen = ctx.Value(testCtxKey{})
				return ElicitResponse{Action: ElicitDecline}, nil
			})
			holdCtx := WithElicitScope(context.WithValue(context.Background(), testCtxKey{}, "hold"), tc.holdScope)
			holdDone := make(chan struct{})
			go func() {
				defer close(holdDone)
				callText(t, c, holdCtx, "hold")
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				c.calls.mu.Lock()
				n := len(c.calls.calls)
				c.calls.mu.Unlock()
				if n == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("hold 调用没有开始")
				}
				time.Sleep(5 * time.Millisecond)
			}
			askCtx := WithElicitScope(context.WithValue(context.Background(), testCtxKey{}, "ask"), tc.askScope)
			callText(t, c, askCtx, "ask")
			close(release)
			<-holdDone
			if got := seen == "ask"; got != tc.wantCallerValue {
				t.Fatalf("处理函数看到的调用方值 %v，期望带上=%v", seen, tc.wantCallerValue)
			}
		})
	}
}
