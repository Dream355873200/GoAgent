// Package loop — 挂起/恢复（suspend/resume）。
//
// 三语义彻底分开（TODO 设计定稿）：
//   - 终止（已有）：POST /interrupt → ctx cancel → 循环退出，
//     被中断工具写回合成 tool_result，消息序列完整可续聊；
//   - 挂起（本文件）：goroutine 挂起而非取消——状态活在内存，
//     等待外部事件（审批到达/用户确认）唤醒续跑；
//   - 恢复（本文件）：从挂起点继续未完成那步，不是从头重跑。
//
// 挂起的实现复用 429 等待的机制地基：select 等待一个 channel。
// 区别在于 429 等的是定时器，挂起等的是外部信号（Resume channel）。
package loop

import (
	"context"
	"errors"
	"sync"
)

// ErrSuspendTerminated 挂起被 Terminate 终止（区别于 Resume 续跑）。
var ErrSuspendTerminated = errors.New("suspend: 挂起被终止")

// SuspendReason 描述挂起的原因（透出到事件与 checkpoint）。
type SuspendReason string

const (
	// SuspendApproval 宽射程工具的异步审批等待。
	SuspendApproval SuspendReason = "approval"
	// SuspendUser 用户显式挂起（如「暂停，等我确认」）。
	SuspendUser SuspendReason = "user"
)

// SuspendGate 是挂起/恢复的门闩：loop 在挂起点调用 Wait 阻塞，
// 外部（审批系统/HTTP handler）调用 Resume 唤醒或 Terminate 终止。
//
// 一个 Gate 服务一次挂起；重复使用时每轮 Wait 前先 Reset。
type SuspendGate struct {
	mu      sync.Mutex
	resume  chan struct{}
	killed  chan struct{}
	waiting bool
}

// NewSuspendGate 创建挂起门闩。
func NewSuspendGate() *SuspendGate {
	return &SuspendGate{
		resume: make(chan struct{}),
		killed: make(chan struct{}),
	}
}

// Wait 阻塞等待外部信号。返回：
//   - resumed=true：Resume 被调用，循环应继续；
//   - resumed=false, err=nil：ctx 被取消（用户终止），循环应走终止路径；
//   - resumed=false, err!=nil：Terminate 被调用（携带终止原因）。
//
// 若从未挂起就先 Resume（信号早到），Wait 立即返回 true（信号缓存）。
func (g *SuspendGate) Wait(ctx context.Context) (resumed bool, err error) {
	g.mu.Lock()
	if !g.waiting {
		// 复位信号（上一轮挂起的残留）或预热信号：重建 channel。
		select {
		case <-g.resume:
		default:
		}
		g.resume = make(chan struct{})
	}
	g.waiting = true
	resumeCh, killCh := g.resume, g.killed
	g.mu.Unlock()

	select {
	case <-resumeCh:
		g.mu.Lock()
		g.waiting = false
		g.mu.Unlock()
		return true, nil
	case <-killCh:
		g.mu.Lock()
		g.waiting = false
		g.mu.Unlock()
		return false, ErrSuspendTerminated
	case <-ctx.Done():
		g.mu.Lock()
		g.waiting = false
		g.mu.Unlock()
		return false, nil
	}
}

// Resume 唤醒挂起中的循环。返回 false = 当前没有挂起（或已唤醒）。
func (g *SuspendGate) Resume() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.waiting {
		return false
	}
	select {
	case <-g.resume:
		return false // 已有未消费的唤醒信号
	default:
		close(g.resume)
		return true
	}
}

// Terminate 终止挂起（区别于 Resume：循环走终止路径，携带原因）。
func (g *SuspendGate) Terminate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.killed:
		// 已终止（幂等）
	default:
		close(g.killed)
	}
}

// IsWaiting 报告当前是否处于挂起等待中。
func (g *SuspendGate) IsWaiting() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting
}
