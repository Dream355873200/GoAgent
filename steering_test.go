// steering_test.go 插话通道测试。
//
// 覆盖：双车道基本语义（guide 注入活跃 run / queue 排队续跑）、
// 无活跃 run 拒绝插话、run 结束后未消费插话降级进队列、
// loop 工具批边界注入（端到端）。
package goagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/session"
)

// steerTestProvider 按脚本返回预设响应的假 provider。
// 脚本元素：带 [tool:名字] 前缀 = 发起一次工具调用；否则为纯文本回复。
type steerTestProvider struct {
	mu      sync.Mutex
	script  []string
	calls   int
	reqs    []provider.Request
	toolNow func(name string) string // 工具执行体（nil = 返回固定文本）
}

func (p *steerTestProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "steer-test", SupportsTools: true}
}

func (p *steerTestProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, fmt.Errorf("steerTestProvider: 不支持同步调用（测试只走 Stream 路径）")
}

func (p *steerTestProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, *req)
	idx := p.calls
	p.calls++
	out := make(chan provider.StreamEvent, 8)
	go func() {
		defer close(out)
		if idx >= len(p.script) {
			out <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
			return
		}
		line := p.script[idx]
		if name, ok := strings.CutPrefix(line, "[tool:"); ok {
			name = strings.TrimSuffix(name, "]")
			out <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{
				ID: "tc-" + name, Name: name, Input: []byte(`{}`),
			}}
			out <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopToolUse}
			return
		}
		out <- provider.StreamEvent{Type: provider.EventTextDelta, Text: line}
		out <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
	}()
	return out, nil
}

// calls 模型请求次数快照（并发安全）。
func (p *steerTestProvider) calls2() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newSteerTestApp(t *testing.T, script []string) (*App, *SteeringHub, *steerTestProvider) {
	t.Helper()
	p := &steerTestProvider{script: script}
	app := New(
		WithProvider(p),
		WithMaxTurns(10),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithSteering(),
	)
	return app, app.Steering(), p
}

// installGatedTool 注册一个由门闩控制的空转工具：启动时发 toolStarted，
// 等 release 关闭后才返回。测试用它制造稳定的「工具批执行中」窗口，
// 确保插话能赶上工具批结束边界（工具瞬间完成会因插话迟到而降级进
// queue 走续跑路径，改变断言目标）。
func installGatedTool(app *App, toolStarted chan<- struct{}, release <-chan struct{}) {
	app.Tool("noop", ToolDef{
		Description: "门控空转工具",
		Input:       struct{}{},
		Execute: func(ctx Context, in struct{}) (string, error) {
			toolStarted <- struct{}{}
			<-release
			return "done", nil
		},
	})
}

// collectDrain 消费事件流直到结束，返回收到的事件文本列表。
// 分两路收集：EventQueueRun（queue 消费）与 EventSteer（guide 注入）。
func collectDrain(events <-chan Event) (runs, steers []string, errs []error) {
	for ev := range events {
		switch ev.Type {
		case EventQueueRun:
			runs = append(runs, ev.Text)
		case EventSteer:
			steers = append(steers, ev.Text)
		case EventError:
			errs = append(errs, ev.Error)
		}
	}
	return
}

// TestSteerGuideInjectedAtBoundary 端到端：活跃 run 中 Steer 的消息
// 在工具批结束边界进入下一次模型请求（作为 user 消息）。
func TestSteerGuideInjectedAtBoundary(t *testing.T) {
	app, hub, p := newSteerTestApp(t, []string{"[tool:noop]", "收到插话，任务完成"})
	toolStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	installGatedTool(app, toolStarted, release)

	go func() {
		<-toolStarted // 等工具批打开（run 必然活跃且未到边界）
		_ = hub.Steer("s1", "请把标题改成红色")
		close(release)
	}()
	events := app.RunSession(context.Background(), "s1", "开始干活")
	_, steers, errs := collectDrain(events)
	if len(errs) > 0 {
		t.Fatalf("事件流错误: %v", errs)
	}
	// 注入上下文前打 system-reminder 标记（source=steer）；事件携带
	// 与上下文一致的标记文本。
	if len(steers) != 1 || !strings.Contains(steers[0], "请把标题改成红色") ||
		!strings.Contains(steers[0], `<system-reminder source="steer">`) {
		t.Fatalf("应收到 1 条带 reminder 标记的 steer 事件, got %v", steers)
	}
	// 第二次模型请求应携带插话（user 消息）。
	p.mu.Lock()
	last := p.reqs[len(p.reqs)-1]
	p.mu.Unlock()
	var found bool
	for _, m := range last.Messages {
		if m.Role == message.RoleUser && strings.Contains(messageText(m), "请把标题改成红色") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二次请求应包含插话 user 消息")
	}
}

// TestSteerNotSteerable 无活跃 run 时 Steer 应返回 ErrNotSteerable。
func TestSteerNotSteerable(t *testing.T) {
	_, hub, _ := newSteerTestApp(t, nil)
	if err := hub.Steer("no-such-session", "hello"); err != ErrNotSteerable {
		t.Fatalf("期望 ErrNotSteerable, got %v", err)
	}
}

// TestSteerLeftoverPromotedToQueue run 结束时未消费的 guide 消息应
// 降级进 queue 车道（hub 级单测：endRun 把残留 guides 移入 queue）。
// 降级后由下一次 RunSession 自动续跑的行为见 TestSteerQueueAutoContinuation。
func TestSteerLeftoverPromotedToQueue(t *testing.T) {
	hub := NewSteeringHub()
	hub.beginRun("s2")
	if err := hub.Steer("s2", "这条没赶上边界"); err != nil {
		t.Fatalf("Steer 应成功: %v", err)
	}
	hub.drainGuides("s2") // 边界 drain 走过一轮（本条之后才到）
	_ = hub.Steer("s2", "边界已过才到达")
	hub.endRun("s2") // run 结束：残留插话应降级进 queue

	if hub.PendingQueued("s2") != 1 {
		t.Fatalf("run 结束后未消费的插话应进 queue, pending=%d", hub.PendingQueued("s2"))
	}
	// 降级进 queue 时已打 system-reminder 标记（queue 车道混有用户真实
	// 排队消息，降级的插话进上下文前必须可区分）。
	if msg, ok := hub.claimQueued("s2"); !ok || !strings.Contains(msg, "边界已过才到达") ||
		!strings.Contains(msg, `<system-reminder source="steer">`) {
		t.Fatalf("降级的插话应带 reminder 标记且可从 queue 取回, got %q ok=%v", msg, ok)
	}
}

// TestSteerQueueAutoContinuation Enqueue 的消息在当前 run 结束后自动续跑
// （每条作为独立输入开新一轮，发 EventQueueRun）。
func TestSteerQueueAutoContinuation(t *testing.T) {
	app, hub, _ := newSteerTestApp(t, []string{"第一轮回复", "第二轮回复"})
	itemA := hub.Enqueue("s3", "排队消息A")
	hub.Enqueue("s3", "排队消息B")
	if itemA.ID == "" || itemA.Text != "排队消息A" {
		t.Fatalf("Enqueue 应返回带 ID 的排队项, got %+v", itemA)
	}
	events := app.RunSession(context.Background(), "s3", "开始")
	runs, _, errs := collectDrain(events)
	if len(errs) > 0 {
		t.Fatalf("事件流错误: %v", errs)
	}
	if len(runs) != 2 {
		t.Fatalf("应续跑 2 条排队消息, got %v", runs)
	}
	if runs[0] != "排队消息A" || runs[1] != "排队消息B" {
		t.Fatalf("续跑顺序应 FIFO, got %v", runs)
	}
	if hub.PendingQueued("s3") != 0 {
		t.Fatalf("队列应排空, pending=%d", hub.PendingQueued("s3"))
	}
}

// TestRunSessionEmptyInputResumesQueue 空输入 = 空闲唤醒：取队头开跑并
// 续跑余下排队项；队列为空时直接结束、不跑任何一轮。
func TestRunSessionEmptyInputResumesQueue(t *testing.T) {
	app, hub, _ := newSteerTestApp(t, []string{"读到结论", "继续"})
	if runs, _, errs := collectDrain(app.RunSession(context.Background(), "s4", "")); len(runs) != 0 || len(errs) != 0 {
		t.Fatalf("空队列唤醒不应起跑, runs=%v errs=%v", runs, errs)
	}
	hub.Enqueue("s4", "后台任务完成")
	hub.Enqueue("s4", "第二条")
	runs, _, errs := collectDrain(app.RunSession(context.Background(), "s4", ""))
	if len(errs) > 0 {
		t.Fatalf("事件流错误: %v", errs)
	}
	if len(runs) != 2 || runs[0] != "后台任务完成" || runs[1] != "第二条" {
		t.Fatalf("应按 FIFO 唤醒并续跑, got %v", runs)
	}
}

// TestQueueItemListRemove 排队消息的单项管理：List 返回队头在前、
// Remove 按 ID 取走并返回原文、Remove 不存在的 ID 返回 false。
func TestQueueItemListRemove(t *testing.T) {
	hub := NewSteeringHub()
	a := hub.Enqueue("sq", "第一条")
	b := hub.Enqueue("sq", "第二条")

	got := hub.ListQueued("sq")
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("ListQueued 应队头在前返回全部条目, got %v", got)
	}
	if got[0].Text != "第一条" || got[1].Text != "第二条" {
		t.Fatalf("ListQueued 应携带原文, got %v", got)
	}

	text, ok := hub.RemoveQueued("sq", b.ID)
	if !ok || text != "第二条" {
		t.Fatalf("RemoveQueued 应返回原文, got %q ok=%v", text, ok)
	}
	if hub.PendingQueued("sq") != 1 {
		t.Fatalf("删除后应剩 1 条, pending=%d", hub.PendingQueued("sq"))
	}
	if _, ok := hub.RemoveQueued("sq", "q-nope"); ok {
		t.Fatalf("不存在的 ID 应返回 false")
	}
	// 队头仍可正常消费。
	if next, ok := hub.claimQueued("sq"); !ok || next != "第一条" {
		t.Fatalf("删除中间项后队头应可消费, got %q ok=%v", next, ok)
	}
}

// messageText 拼接消息文本块（测试断言用）。
func messageText(m message.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// 编译期确认 loop.SteerSource 接口由适配器实现。
var _ loop.SteerSource = steerSourceAdapter{}
