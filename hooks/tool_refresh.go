// Package hooks 实现钩子系统。
package hooks

import (
	"context"
	"encoding/json"
)

// ToolRefreshHook 动态刷新工具定义的钩子。
// 对齐 Claude Code 的 dynamicToolsRefresh 机制。
type ToolRefreshHook interface {
	Hook
	// RefreshTools 返回当前有效的工具定义列表。
	RefreshTools(ctx context.Context) ([]json.RawMessage, error)
}

// ToolRefreshHookFunc 函数类型的 ToolRefreshHook 适配器。
type ToolRefreshHookFunc func(ctx context.Context) ([]json.RawMessage, error)

func (f ToolRefreshHookFunc) Name() string {
	return "toolRefresh"
}

func (f ToolRefreshHookFunc) Execute(ctx context.Context, event string, data map[string]any) error {
	return nil // ToolRefreshHookFunc 不响应事件，只提供工具刷新
}

// ToolRefreshManager 管理动态工具刷新。
type ToolRefreshManager struct {
	hooks     []ToolRefreshHook
	lastTools []json.RawMessage
	onRefresh func([]json.RawMessage) // 刷新回调
}

// NewToolRefreshManager 创建工具刷新管理器。
func NewToolRefreshManager(onRefresh func([]json.RawMessage)) *ToolRefreshManager {
	return &ToolRefreshManager{
		hooks:     []ToolRefreshHook{},
		lastTools: []json.RawMessage{},
		onRefresh: onRefresh,
	}
}

// Register 注册一个工具刷新钩子。
func (m *ToolRefreshManager) Register(h ToolRefreshHook) {
	m.hooks = append(m.hooks, h)
}

// Refresh 触发工具刷新。
// 返回新工具列表，如有变化则调用 onRefresh。
func (m *ToolRefreshManager) Refresh(ctx context.Context) ([]json.RawMessage, bool, error) {
	var allTools []json.RawMessage

	for _, h := range m.hooks {
		tools, err := h.RefreshTools(ctx)
		if err != nil {
			continue
		}
		allTools = append(allTools, tools...)
	}

	// 检查是否有变化
	changed := !toolsEqual(allTools, m.lastTools)
	if changed {
		m.lastTools = allTools
		if m.onRefresh != nil {
			m.onRefresh(allTools)
		}
	}

	return allTools, changed, nil
}

// toolsEqual 比较两个工具列表是否相等。
func toolsEqual(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			return false
		}
	}
	return true
}

// AutoRefreshConfig 配置自动刷新。
type AutoRefreshConfig struct {
	// Interval 刷新间隔（秒）。
	Interval int
	// OnChange 变化时的回调。
	OnChange func([]json.RawMessage)
}

// ToolAutoRefresher 自动定时刷新工具。
type ToolAutoRefresher struct {
	mgr    *ToolRefreshManager
	config AutoRefreshConfig
	stopCh chan struct{}
}

// NewToolAutoRefresher 创建自动刷新器。
func NewToolAutoRefresher(mgr *ToolRefreshManager, cfg AutoRefreshConfig) *ToolAutoRefresher {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 // 默认 60 秒
	}
	return &ToolAutoRefresher{
		mgr:    mgr,
		config: cfg,
		stopCh: make(chan struct{}),
	}
}

// Start 开始自动刷新。
func (r *ToolAutoRefresher) Start(ctx context.Context) {
	go func() {
		ticker := newTicker(r.config.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-ticker.C():
				if _, changed, err := r.mgr.Refresh(ctx); err == nil && changed {
					// 工具已刷新，OnChange 会被调用
				}
			}
		}
	}()
}

// Stop 停止自动刷新。
func (r *ToolAutoRefresher) Stop() {
	close(r.stopCh)
}

// ticker 简单的时间触发器。
type ticker struct {
	c    chan struct{}
	stop chan struct{}
}

func newTicker(seconds int) *ticker {
	t := &ticker{
		c:    make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	go func() {
		for {
			select {
			case <-t.stop:
				return
			default:
				t.c <- struct{}{}
			}
		}
	}()
	return t
}

func (t *ticker) C() <-chan struct{} {
	return t.c
}

func (t *ticker) Stop() {
	close(t.stop)
}
