package loop

import (
	"context"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

// cancelStreamProvider 建流后在流里返回 context.Canceled——模拟用户
// 中断传导到 provider 层（流建立成功、中途被打断）。
type cancelStreamProvider struct{}

func (p *cancelStreamProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "cancel-probe", ContextWindow: 100000}
}

func (p *cancelStreamProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 1)
	go func() {
		defer close(ch)
		ch <- provider.StreamEvent{Type: provider.EventError, Error: context.Canceled}
	}()
	return ch, nil
}

func (p *cancelStreamProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

// 事件类型数值对齐公共 EventType（toPublicEvent 数值直转）。
func TestInterrupted_EventValueMatchesPublic(t *testing.T) {
	if EvtInterrupted != 15 {
		t.Fatalf("EvtInterrupted=%d, 公共 EventInterrupted 应为 15（SubAgentProgress=11/AskUser=12/PlanConfirm=13/Interrupt=14 之后）", EvtInterrupted)
	}
}

// 用户中断（流中 EventError 携带 context.Canceled）应发 EvtInterrupted
// 而非 EvtError——前端靠事件类型区分「已停止」与报错。
func TestInterrupted_StreamCanceledEmitsInterruptedEvent(t *testing.T) {
	l := New(Config{
		Provider:  &cancelStreamProvider{},
		MaxTurns:  3,
		SessionID: "sess-intr-1",
	})

	var gotInterrupted, gotError bool
	for ev := range l.Run(context.Background(), "hi") {
		switch ev.Type {
		case EvtInterrupted:
			gotInterrupted = true
			if ev.Text != "用户中断" {
				t.Errorf("Interrupted 事件 Text=%q, 期望「用户中断」", ev.Text)
			}
		case EvtError:
			gotError = true
		}
	}
	if !gotInterrupted {
		t.Fatal("流中取消应发出 EvtInterrupted")
	}
	if gotError {
		t.Fatal("用户中断不应走 EvtError 通道")
	}
}

// 轮次起点的 ctx 已取消（如 /interrupt 在轮间隙路由到 cancel）。
func TestInterrupted_CtxCanceledBeforeTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := New(Config{
		Provider:  &cancelStreamProvider{},
		MaxTurns:  3,
		SessionID: "sess-intr-2",
	})

	var gotInterrupted bool
	for ev := range l.Run(ctx, "hi") {
		if ev.Type == EvtInterrupted {
			gotInterrupted = true
		}
		if ev.Type == EvtError {
			t.Fatalf("用户中断不应走 EvtError: %v", ev.Error)
		}
	}
	if !gotInterrupted {
		t.Fatal("轮前取消应发出 EvtInterrupted")
	}
}

// 非用户取消的取消（DeadlineExceeded）仍走 EvtError——一等事件只认 Canceled。
func TestInterrupted_DeadlineStillError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	l := New(Config{
		Provider:  &cancelStreamProvider{},
		MaxTurns:  3,
		SessionID: "sess-intr-3",
	})

	for ev := range l.Run(ctx, "hi") {
		if ev.Type == EvtInterrupted {
			t.Fatal("DeadlineExceeded 不是用户中断，不应发 EvtInterrupted")
		}
	}
}
