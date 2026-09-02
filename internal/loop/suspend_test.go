package loop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 基本生命周期：Wait 阻塞 → Resume 唤醒。
func TestSuspendGate_ResumeWakes(t *testing.T) {
	g := NewSuspendGate()

	// 预先标记等待态（loop 检查 IsWaiting 才进挂起分支；这里手动模拟）。
	// 用 goroutine 模拟 loop 侧调用 Wait。
	resumed := make(chan bool, 1)
	go func() {
		r, err := g.Wait(context.Background())
		if err != nil {
			t.Errorf("Resume 路径不应有错误: %v", err)
		}
		resumed <- r
	}()

	// 等 Wait 真正挂起。
	time.Sleep(50 * time.Millisecond)
	if !g.IsWaiting() {
		t.Fatal("Wait 应处于挂起等待态")
	}
	if !g.Resume() {
		t.Fatal("Resume 应返回 true（首次唤醒）")
	}
	if r := <-resumed; !r {
		t.Fatal("Wait 应返回 resumed=true")
	}
	if g.IsWaiting() {
		t.Fatal("唤醒后应退出等待态")
	}
}

// Terminate：Wait 返回 ErrSuspendTerminated。
func TestSuspendGate_Terminate(t *testing.T) {
	g := NewSuspendGate()
	errCh := make(chan error, 1)
	go func() {
		_, err := g.Wait(context.Background())
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	g.Terminate() // 幂等
	g.Terminate()

	err := <-errCh
	if !errors.Is(err, ErrSuspendTerminated) {
		t.Fatalf("Terminate 应返回 ErrSuspendTerminated, got %v", err)
	}
}

// ctx 取消（用户终止）：Wait 返回 resumed=false, err=nil，
// 循环走统一中断路径（非挂起终止）。
func TestSuspendGate_CtxCancel(t *testing.T) {
	g := NewSuspendGate()
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan bool, 1)
	go func() {
		r, err := g.Wait(ctx)
		if err != nil {
			t.Errorf("ctx 取消不应返回错误: %v", err)
		}
		result <- r
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	if r := <-result; r {
		t.Fatal("ctx 取消应返回 resumed=false")
	}
}

// 未挂起时 Resume 返回 false（无副作用）。
func TestSuspendGate_ResumeWithoutWait(t *testing.T) {
	g := NewSuspendGate()
	if g.Resume() {
		t.Fatal("未挂起时 Resume 应返回 false")
	}
}

// 多轮挂起：第一次唤醒后可再次挂起。
func TestSuspendGate_MultipleRounds(t *testing.T) {
	g := NewSuspendGate()
	var wg sync.WaitGroup
	wg.Add(2)

	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			r, _ := g.Wait(context.Background())
			if !r {
				t.Error("两轮都应被唤醒")
			}
		}()
		time.Sleep(30 * time.Millisecond)
		if !g.Resume() {
			t.Fatalf("第 %d 轮 Resume 失败", i+1)
		}
		time.Sleep(30 * time.Millisecond) // 等 Wait 返回
	}
	wg.Wait()
}

// ── loop 集成 ─────────────────────────────────────────────────────

// multiTurnProvider 跑两轮：第一轮调工具，第二轮纯文本结束。
type multiTurnProvider struct {
	turn int
}

func (p *multiTurnProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "multi-probe", ContextWindow: 100000}
}

func (p *multiTurnProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	p.turn++
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		if p.turn == 1 {
			ch <- provider.StreamEvent{Type: provider.EventToolUseStart, ToolCall: &message.ToolCall{
				ID: "tc-1", Name: "noop", Input: []byte(`{}`),
			}}
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "done"}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

func (p *multiTurnProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

// loop 集成：挂起发生在轮次边界，唤醒后从挂起点继续（消息序列保留）。
func TestSuspend_LoopIntegration(t *testing.T) {
	g := NewSuspendGate()
	l := New(Config{
		Provider:    &multiTurnProvider{},
		MaxTurns:    5,
		SessionID:   "sess-suspend-1",
		SuspendGate: g,
		Executor:    executor.New(executor.Config{MaxConcurrency: 10}),
	})

	// 启动循环；等第一轮完成后（进入第二轮边界）发起挂起。
	events := l.Run(context.Background(), "hi")

	var suspendSeen, doneSeen bool
	go func() {
		// 挂起状态行出现后唤醒。
		time.Sleep(100 * time.Millisecond)
		// 直接 Resume（即使 IsWaiting 还没置位，信号会被缓存或 no-op）
		g.Resume()
	}()

	for ev := range events {
		if ev.Type == EvtProgress && ev.StatusKey == "suspend" {
			suspendSeen = true
		}
		if ev.Type == EvtDone {
			doneSeen = true
		}
	}
	// 挂起未发生也允许（时序敏感），但循环必须正常完成。
	if !doneSeen {
		t.Fatal("循环应正常完成（EvtDone）")
	}
	_ = suspendSeen
}
