package goagent

import "sync"

// streamSlot 是一条连接（SSE / WebSocket）的下行通道：请求开始时注册、
// 请求结束时注销，slot 内部用互斥锁协调「发送 / 关闭」竞态——关闭后
// send 返回 false 而不是 panic，消费方用 range 消费、关闭后自然退出。
type streamSlot[T any] struct {
	ch     chan T
	mu     sync.Mutex
	closed bool
}

func (s *streamSlot[T]) send(item T) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- item:
		return true
	default:
		return false // 消费方阻塞/拥塞：按失败处理，由调用方兜底
	}
}

func (s *streamSlot[T]) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch) // range 消费方收尾退出；缓冲中未取走的条目仍可收到
}

// streamBus 是按会话绑定的下发通道注册表，HTTP/SSE 宿主用它把「等待用户
// 参与的请求」（提问/审批/计划确认）精确路由到发起该次运行的活跃连接，
// 替代「每条连接各起一个 goroutine 消费同一份共享 channel」的转发模式。
// 旧模式有两个结构性缺陷：
//
//  1. 泄漏：请求结束后转发 goroutine 仍阻塞在共享 channel 的接收上，
//     永远不会退出；
//  2. 抢帧：channel 唤醒按接收队列 FIFO，最先阻塞的消费者总是最老的
//     stale 转发器——新运行的请求被它抢走、写进早已断开的旧连接，
//     当前流收不到帧，工具在引擎侧无限期阻塞。
//
// 绑定生命周期与 HTTP 请求严格一致（handler 里 defer 注销），stale
// 消费者不再存在。未命中任何绑定连接时返回 false，调用方落到共享
// channel（SDK 直连消费模式，行为向后兼容）。
type streamBus[T any] struct {
	mu      sync.Mutex
	streams map[string]*streamSlot[T] // sessionID → 活跃连接
	latest  *streamSlot[T]            // 最近绑定的连接（顺序执行宿主 = 当前 run）
}

func newStreamBus[T any]() *streamBus[T] {
	return &streamBus[T]{streams: make(map[string]*streamSlot[T])}
}

// Subscribe 绑定一条连接，返回接收 channel 与注销函数。
// 同会话重复绑定（重连）时旧 slot 被关闭，旧转发 goroutine 随 range 退出。
func (b *streamBus[T]) Subscribe(sessionID string) (<-chan T, func()) {
	slot := &streamSlot[T]{ch: make(chan T, 16)}
	b.mu.Lock()
	if old, ok := b.streams[sessionID]; ok {
		old.close() // 同会话换连接（重连）：旧通道关闭，旧转发器退出
	}
	b.streams[sessionID] = slot
	b.latest = slot
	b.mu.Unlock()
	return slot.ch, func() { b.unsubscribe(sessionID, slot) }
}

// unsubscribe 注销一条绑定。slot 已被新连接替换时不动它（新连接的
// 注销函数负责清理），只保证本次的 slot 被关闭。
func (b *streamBus[T]) unsubscribe(sessionID string, slot *streamSlot[T]) {
	b.mu.Lock()
	if b.streams[sessionID] == slot {
		delete(b.streams, sessionID)
		slot.close()
	}
	b.mu.Unlock()
}

// Send 路由：sessionID 精确命中 → 该连接；否则（含空会话）→ 最近绑定的
// 连接；一条绑定都没有 → false。发送走 slot 的非阻塞 select，缓冲满视为
// 拥塞失败。
func (b *streamBus[T]) Send(sessionID string, item T) bool {
	b.mu.Lock()
	slot := b.streams[sessionID]
	if slot == nil {
		slot = b.latest
	}
	b.mu.Unlock()
	return slot != nil && slot.send(item)
}

// CloseAll 关闭所有绑定连接（宿主整体关闭时兜底）。
func (b *streamBus[T]) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, slot := range b.streams {
		slot.close()
		delete(b.streams, k)
	}
}
