package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// slowToolProvider 跑两轮：第一轮调慢工具（测试侧可控释放），第二轮
// 纯文本结束。用于验证插话在工具批结束边界注入。
type slowToolProvider struct {
	turn int
}

func (p *slowToolProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "steer-probe", ContextWindow: 100000}
}

func (p *slowToolProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.turn++
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		if p.turn == 1 {
			ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{
				ID: "tc-slow", Name: "slow", Input: []byte(`{}`),
			}}
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "done"}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

func (p *slowToolProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

// 插话注入：工具执行期间 Steer → 工具批结束边界进入上下文 →
// EvtSteer 事件发出 → 消息序列中插话位于 tool_result 之后。
func TestSteering_GuideInjectedAtToolBoundary(t *testing.T) {
	toolStarted := make(chan struct{})
	release := make(chan struct{})
	src := &chanSteerSource{guides: make(chan string, 4)}

	l := New(Config{
		Provider:  &slowToolProvider{},
		MaxTurns:  5,
		SessionID: "sess-steer-1",
		Steering:  src,
		Executor:  executor.New(executor.Config{MaxConcurrency: 10}),
		Tools: []ToolEntry{{
			Name:        "slow",
			Description: "slow tool",
			Permission:  0,
			ExecuteFn: func(_ context.Context, _ json.RawMessage) (string, error) {
				close(toolStarted)
				<-release
				return "ok", nil
			},
		}},
	})

	events := l.Run(context.Background(), "hi")

	var steerSeen bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Type == EvtSteer && ev.Text == "插话：改成圆角" {
				steerSeen = true
			}
		}
	}()

	<-toolStarted       // 工具已开始执行
	src.send("插话：改成圆角") // 执行期间插话
	time.Sleep(20 * time.Millisecond)
	close(release) // 放行工具完成 → 边界注入

	<-done
	if !steerSeen {
		t.Fatal("应收到 EvtSteer 事件（工具批结束边界注入）")
	}

	// 消息序列校验：插话 user 消息应位于 tool_result 之后、下一轮请求之前。
	// （tool_result 消息的角色同为 user，按块类型区分。）
	final := l.FinalMessages()
	foundToolResult, foundSteer := false, false
	for _, m := range final {
		isToolResult, isSteer := false, false
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.ForToolUseID == "tc-slow" {
				isToolResult = true
			}
		}
		if m.Role == message.RoleUser && strings.Contains(messageTextOf(m), "改成圆角") &&
			len(m.Content) == 1 && m.Content[0].Type == "text" {
			isSteer = true
		}
		if isToolResult {
			foundToolResult = true
			continue
		}
		if isSteer && foundToolResult {
			foundSteer = true
			break
		}
	}
	if !foundToolResult || !foundSteer {
		t.Fatalf("消息序列不完整: tool_result=%v steer=%v", foundToolResult, foundSteer)
	}
}

// 插话在 run 结束后到达（工具批边界已过）：guide 源在 run 结束时仍
// 持有消息 → 由宿主层 SteeringHub.endRun 降级进 queue（root 包测试覆盖）。
// 本用例验证 loop 侧：边界已过、guide 在流结束后到达时不阻塞正常退出。
func TestSteering_LateGuideDoesNotBlockExit(t *testing.T) {
	src := &chanSteerSource{guides: make(chan string, 4)}
	l := New(Config{
		Provider:  &probeStreamProvider{model: "steer-probe-2"},
		MaxTurns:  3,
		SessionID: "sess-steer-2",
		Steering:  src,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range l.Run(context.Background(), "hi") {
		}
	}()
	// run 很快结束；结束后才投递 guide —— loop 已退出，不应卡住。
	time.Sleep(50 * time.Millisecond)
	src.send("迟到的插话")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run 应正常退出，不因迟到插话阻塞")
	}
}

// chanSteerSource 测试用插话源：send 投递，DrainGuides 非阻塞取走。
type chanSteerSource struct {
	guides chan string
}

func (s *chanSteerSource) send(text string) { s.guides <- text }

func (s *chanSteerSource) DrainGuides(sessionID string) []string {
	var out []string
	for {
		select {
		case g := <-s.guides:
			out = append(out, g)
		default:
			return out
		}
	}
}

// messageTextOf 提取消息的纯文本内容（拼接 text 块）。
func messageTextOf(m message.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
