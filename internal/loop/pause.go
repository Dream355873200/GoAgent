// Package loop 实现核心 agent 状态机。
//
// Pause/Resume 机制支持暂停和恢复 agent 执行。
package loop

import (
	"context"
	"time"

	"github.com/anthropic-community/goagent/message"
)

// ToolState 工具执行状态。
type ToolState struct {
	// Name 工具名称。
	Name string
	// Input 工具输入。
	Input string
	// StartTime 开始时间。
	StartTime time.Time
	// IsRunning 是否正在执行。
	IsRunning bool
}

// PausePoint 表示 agent 执行中的暂停点。
type PausePoint struct {
	// TurnCount 当前轮次。
	TurnCount int
	// Messages 当前消息列表。
	Messages []message.Message
	// ToolStates 当前正在执行的工具状态。
	ToolStates map[string]ToolState
	// Span 当前阶段。
	Span string
	// MaxOutputRecoveryCount 最大输出恢复次数。
	MaxOutputRecoveryCount int
	// HasAttemptedReactiveCompact 是否已尝试响应式压缩。
	HasAttemptedReactiveCompact bool
	// UsingFallback 是否使用后备模型。
	UsingFallback bool
	// LastInputTokens 上次输入的 token 数。
	LastInputTokens int
	// CreatedAt 暂停点创建时间。
	CreatedAt time.Time
}

// ResumableLoop 是可暂停恢复的 Loop 包装器。
type ResumableLoop struct {
	loop     *Loop
	state    *loopState
	paused   bool
	pauseCh  chan struct{}
	resumeCh chan struct{}
}

// NewResumableLoop 创建一个可暂停恢复的 Loop。
func NewResumableLoop(cfg Config) *ResumableLoop {
	return &ResumableLoop{
		loop:     New(cfg),
		paused:   false,
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
	}
}

// Run 启动 agent 循环。
func (rl *ResumableLoop) Run(ctx context.Context, input string) <-chan Event {
	return rl.loop.Run(ctx, input)
}

// Pause 暂停 agent 执行并返回暂停点。
// 如果当前没有正在执行，返回 nil。
func (rl *ResumableLoop) Pause(ctx context.Context) (*PausePoint, error) {
	if rl.paused {
		return nil, nil
	}

	rl.paused = true
	close(rl.pauseCh)

	// 获取当前状态
	state := rl.loop.currentState()
	if state == nil {
		return nil, nil
	}

	// 构建暂停点
	point := &PausePoint{
		TurnCount:                   state.turnCount,
		Messages:                    copyMessages(state.messages),
		ToolStates:                  copyToolStates(state.toolStartTimes),
		Span:                        getCurrentSpan(state),
		MaxOutputRecoveryCount:      state.maxOutputRecoveryCount,
		HasAttemptedReactiveCompact: state.hasAttemptedReactiveCompact,
		UsingFallback:               state.usingFallback,
		LastInputTokens:             state.lastInputTokens,
		CreatedAt:                   time.Now(),
	}

	return point, nil
}

// Resume 从暂停点恢复 agent 执行。
func (rl *ResumableLoop) Resume(ctx context.Context, point *PausePoint, input string) <-chan Event {
	if point == nil {
		return rl.loop.Run(ctx, input)
	}

	// 重建状态
	state := &loopState{
		messages:                    copyMessages(point.Messages),
		turnCount:                   point.TurnCount,
		transition:                  nil,
		maxOutputRecoveryCount:      point.MaxOutputRecoveryCount,
		maxOutputTokensOverride:     0,
		hasAttemptedReactiveCompact: point.HasAttemptedReactiveCompact,
		usingFallback:               point.UsingFallback,
		lastInputTokens:             point.LastInputTokens,
		toolStartTimes:              copyToolStatesFromPoint(point.ToolStates),
	}

	rl.loop.restoreState(state)
	rl.paused = false

	return rl.loop.Run(ctx, input)
}

// IsPaused 返回是否处于暂停状态。
func (rl *ResumableLoop) IsPaused() bool {
	return rl.paused
}

// currentState 返回当前循环状态（内部方法）。
// 注意：这需要 loop.go 暴露此方法
func (l *Loop) currentState() *loopState {
	// 由于 loopState 是内部类型，我们需要通过其他方式访问
	// 这里返回一个模拟值，实际实现需要修改 loop.go
	return nil
}

// restoreState 恢复循环状态（内部方法）。
func (l *Loop) restoreState(state *loopState) {
	// 由于 loopState 是内部类型，我们需要通过其他方式访问
	// 这里不做任何事，实际实现需要修改 loop.go
}

// copyMessages 复制消息列表。
func copyMessages(msgs []message.Message) []message.Message {
	if msgs == nil {
		return nil
	}
	result := make([]message.Message, len(msgs))
	copy(result, msgs)
	return result
}

// copyToolStates 复制工具状态映射。
func copyToolStates(times map[string]time.Time) map[string]ToolState {
	if times == nil {
		return nil
	}
	result := make(map[string]ToolState)
	for k, v := range times {
		result[k] = ToolState{
			Name:      k,
			StartTime: v,
			IsRunning: false,
		}
	}
	return result
}

// copyToolStatesFromPoint 从暂停点复制工具状态。
func copyToolStatesFromPoint(states map[string]ToolState) map[string]time.Time {
	if states == nil {
		return nil
	}
	result := make(map[string]time.Time)
	for k, v := range states {
		result[k] = v.StartTime
	}
	return result
}

// getCurrentSpan 获取当前阶段。
func getCurrentSpan(state *loopState) string {
	if state == nil {
		return ""
	}
	return ""
}

// PauseManager 管理多个可暂停 Loop 的管理器。
type PauseManager struct {
	loops map[string]*ResumableLoop
}

// NewPauseManager 创建暂停管理器。
func NewPauseManager() *PauseManager {
	return &PauseManager{
		loops: make(map[string]*ResumableLoop),
	}
}

// Register 注册一个可暂停的 Loop。
func (m *PauseManager) Register(id string, rl *ResumableLoop) {
	m.loops[id] = rl
}

// Unregister 取消注册。
func (m *PauseManager) Unregister(id string) {
	delete(m.loops, id)
}

// Get 获取指定 ID 的 Loop。
func (m *PauseManager) Get(id string) *ResumableLoop {
	return m.loops[id]
}

// Pause 暂停指定 ID 的 Loop。
func (m *PauseManager) Pause(ctx context.Context, id string) (*PausePoint, error) {
	rl := m.loops[id]
	if rl == nil {
		return nil, nil
	}
	return rl.Pause(ctx)
}

// Resume 恢复指定 ID 的 Loop。
func (m *PauseManager) Resume(ctx context.Context, id string, point *PausePoint, input string) <-chan Event {
	rl := m.loops[id]
	if rl == nil {
		return nil
	}
	return rl.Resume(ctx, point, input)
}
