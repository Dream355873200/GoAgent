// steering.go 运行中插话通道（双车道）。
//
// agent 执行一个长任务期间，用户/宿主经常需要补充信息、纠正方向或
// 通知外部事件（编辑器保存、后台任务完成）。插话通道提供两条车道：
//
//	guide：注入活跃 run。消息在工具批结束边界（助手消息与工具结果
//	       成对落定之后、下一次模型请求之前）作为 user 消息追加，
//	       当前任务立即看到——不打断模型流，不丢弃已有上下文。
//	queue：排队持有。run 结束后由 RunSession 自动作为下一条输入
//	       续跑（同一会话、同一事件流），直到队列排空。
//
// 无活跃 run 时 Steer 返回 ErrNotSteerable，宿主可自行决定走普通
// 消息路径（新开一轮）或改投 queue。
//
// 典型接线：
//
//	app := goagent.New(..., goagent.WithSteering())
//	hub := app.Steering()
//	// 任务运行中插话：
//	if err := hub.Steer(sessID, "顺便把按钮改成圆角"); err != nil { ... }
//	// 排队（下次自动续跑）：
//	hub.Enqueue(sessID, "完成后再跑一次测试")
package goagent

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Dream355873200/GoAgent/reminder"
)

// ErrNotSteerable 会话当前没有活跃 run，guide 车道无处注入。
// 宿主可捕获后改走普通消息路径或 Enqueue 排队。
var ErrNotSteerable = errors.New("goagent: 会话当前无活跃任务，无法插话")

// QueueItem 是 queue 车道的一条排队消息（公开只读视图）。
// ID 供宿主做单项管理：删除（撤回输入框）/ 立即发送 / 排序。
type QueueItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// queueSeq 队列项 ID 发号器（进程内单调；会话间也唯一，避免宿主串会话误删）。
var queueSeq atomic.Uint64

// queuedItem 内部队列节点。
type queuedItem struct {
	id   string
	text string
}

// steerLane 单个会话的插话车道。
type steerLane struct {
	guides []string      // guide 车道：活跃 run 的工具批边界注入
	queue  []*queuedItem // queue 车道：run 结束后自动续跑
	active int           // 活跃 run 计数（>0 表示 guide 车道可注入）
}

// SteeringHub 管理各会话的插话车道。并发安全。
type SteeringHub struct {
	mu    sync.Mutex
	lanes map[string]*steerLane
}

// NewSteeringHub 创建插话通道。通常不直接调用，用 WithSteering() 让
// App 自动创建并接线（app.Steering() 取回引用）。
func NewSteeringHub() *SteeringHub {
	return &SteeringHub{lanes: map[string]*steerLane{}}
}

// lane 取车道（不存在则创建）。调用方需持锁。
func (h *SteeringHub) lane(sessionID string) *steerLane {
	l := h.lanes[sessionID]
	if l == nil {
		l = &steerLane{}
		h.lanes[sessionID] = l
	}
	return l
}

// Steer 向活跃 run 的 guide 车道插入一条消息。消息会在最近的工具批
// 结束边界进入模型上下文（EventSteer 事件随后发出）。会话无活跃 run
// 时返回 ErrNotSteerable——此时消息不会进入任何队列。
func (h *SteeringHub) Steer(sessionID, text string) error {
	if sessionID == "" || text == "" {
		return errors.New("goagent: Steer 需要 sessionID 和非空 text")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lane(sessionID)
	if l.active == 0 {
		return ErrNotSteerable
	}
	l.guides = append(l.guides, text)
	return nil
}

// Enqueue 向 queue 车道排队一条消息。run 结束后自动作为下一条输入
// 续跑。返回排队项（含 ID，供宿主后续单项管理）。
func (h *SteeringHub) Enqueue(sessionID, text string) QueueItem {
	item := QueueItem{ID: fmt.Sprintf("q-%d", queueSeq.Add(1)), Text: text}
	if sessionID == "" || text == "" {
		return item
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lane(sessionID)
	l.queue = append(l.queue, &queuedItem{id: item.ID, text: item.Text})
	return item
}

// ListQueued 返回该会话 queue 车道的排队消息（队头在前）。
func (h *SteeringHub) ListQueued(sessionID string) []QueueItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil {
		return nil
	}
	items := make([]QueueItem, 0, len(l.queue))
	for _, it := range l.queue {
		items = append(items, QueueItem{ID: it.id, Text: it.text})
	}
	return items
}

// RemoveQueued 按 ID 取走一条排队消息（不再续跑）。返回原文——
// 宿主「撤回输入框编辑」与「立即发送」都靠它拿回内容。
func (h *SteeringHub) RemoveQueued(sessionID, itemID string) (string, bool) {
	if sessionID == "" || itemID == "" {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil {
		return "", false
	}
	for i, it := range l.queue {
		if it.id == itemID {
			text := it.text
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return text, true
		}
	}
	return "", false
}

// DrainQueued 取走并清空该会话的排队消息（宿主想自己控制续跑时机时用；
// 默认 RunSession 会自动消费队列，无需调用）。
func (h *SteeringHub) DrainQueued(sessionID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil {
		return nil
	}
	msgs := make([]string, 0, len(l.queue))
	for _, it := range l.queue {
		msgs = append(msgs, it.text)
	}
	l.queue = nil
	return msgs
}

// PendingQueued 返回该会话 queue 车道的积压数。
func (h *SteeringHub) PendingQueued(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil {
		return 0
	}
	return len(l.queue)
}

// PendingGuides 返回该会话 guide 车道的待注入数（已入队、尚未在
// 工具批边界进入上下文的插话）。
func (h *SteeringHub) PendingGuides(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil {
		return 0
	}
	return len(l.guides)
}

// ---------- 内部接线（App.run / loop 使用） ----------

// beginRun 标记会话进入活跃状态（guide 车道自此可注入）。
func (h *SteeringHub) beginRun(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lane(sessionID).active++
}

// endRun 标记 run 结束。活跃计数归零后，未消费的 guide 消息自动
// 降级进 queue 车道——插话永不丢失，只是从「本轮注入」变成「下轮续跑」。
// 降级时打上 system-reminder 标记（queue 车道原是用户排队的真实输入，
// 降级的插话是环境提醒，进入模型上下文前必须可区分）。
func (h *SteeringHub) endRun(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lane(sessionID)
	if l.active > 0 {
		l.active--
	}
	if l.active == 0 && len(l.guides) > 0 {
		for _, g := range l.guides {
			// 降级的插话是环境提醒，进模型上下文前必须与用户真实排队消息可区分。
			l.queue = append(l.queue, &queuedItem{
				id:   fmt.Sprintf("q-%d", queueSeq.Add(1)),
				text: reminder.Wrap(reminder.SourceSteer, g),
			})
		}
		l.guides = nil
	}
}

// drainGuides 取走该会话 guide 车道的全部待注入消息（loop 在工具批
// 结束边界调用）。
func (h *SteeringHub) drainGuides(sessionID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil || len(l.guides) == 0 {
		return nil
	}
	msgs := l.guides
	l.guides = nil
	return msgs
}

// claimQueued 取走 queue 车道队首消息文本（RunSession 续跑时逐条领取）。
func (h *SteeringHub) claimQueued(sessionID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lanes[sessionID]
	if l == nil || len(l.queue) == 0 {
		return "", false
	}
	next := l.queue[0].text
	l.queue = l.queue[1:]
	return next, true
}

// steerSourceAdapter 把 SteeringHub 适配为 loop.SteerSource。
// internal/loop 不能 import 根包（避免循环依赖），经该适配器解耦。
type steerSourceAdapter struct {
	hub       *SteeringHub
	sessionID string
}

func (s steerSourceAdapter) DrainGuides(sessionID string) []string {
	raw := s.hub.drainGuides(s.sessionID)
	if len(raw) == 0 {
		return nil
	}
	// 注入模型上下文前打 system-reminder 标记（source=steer）——
	// 插话是环境提醒而非用户发言。EvtSteer 事件仍带原文（前端展示用）。
	wrapped := make([]string, len(raw))
	for i, g := range raw {
		wrapped[i] = reminder.Wrap(reminder.SourceSteer, g)
	}
	return wrapped
}
