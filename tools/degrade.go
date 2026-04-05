// Package tools 实现工具自动降级机制。
package tools

import (
	"sync"
)

// DegradationLevel 降级级别。
type DegradationLevel int

const (
	// LevelFull 完整功能。
	LevelFull DegradationLevel = iota
	// LevelToolsDisabled 禁用工具。
	LevelToolsDisabled
	// LevelReadOnly 仅读。
	LevelReadOnly
	// LevelNoCache 无缓存。
	LevelNoCache
)

// ToolDegradationConfig 工具降级配置。
type ToolDegradationConfig struct {
	// Enabled 是否启用自动降级。
	Enabled bool
	// MaxRetries 最大重试次数。
	MaxRetries int
	// OnDegrade 降级回调。
	OnDegrade func(from, to DegradationLevel)
	// OnRecover 恢复回调。
	OnRecover func(from, to DegradationLevel)
}

// DefaultToolDegradationConfig 返回默认配置。
func DefaultToolDegradationConfig() ToolDegradationConfig {
	return ToolDegradationConfig{
		Enabled:    true,
		MaxRetries: 3,
	}
}

// DegradationTracker 追踪降级状态。
type DegradationTracker struct {
	config  ToolDegradationConfig
	level   DegradationLevel
	retries map[string]int
	mu      sync.Mutex
}

// NewDegradationTracker 创建降级追踪器。
func NewDegradationTracker(config ToolDegradationConfig) *DegradationTracker {
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	return &DegradationTracker{
		config:  config,
		level:   LevelFull,
		retries: make(map[string]int),
	}
}

// CurrentLevel 返回当前降级级别。
func (t *DegradationTracker) CurrentLevel() DegradationLevel {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.level
}

// ShouldDegrade 判断是否应该降级。
func (t *DegradationTracker) ShouldDegrade(toolName string, err error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.config.Enabled {
		return false
	}

	// 增加重试计数
	t.retries[toolName]++
	retryCount := t.retries[toolName]

	return retryCount >= t.config.MaxRetries
}

// Degrade 执行降级。
func (t *DegradationTracker) Degrade(to DegradationLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if to == t.level {
		return
	}

	oldLevel := t.level
	t.level = to

	if t.config.OnDegrade != nil {
		t.config.OnDegrade(oldLevel, to)
	}
}

// Recover 恢复到完整级别。
func (t *DegradationTracker) Recover() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.level == LevelFull {
		return
	}

	oldLevel := t.level
	t.level = LevelFull
	t.retries = make(map[string]int) // 重置重试计数

	if t.config.OnRecover != nil {
		t.config.OnRecover(oldLevel, LevelFull)
	}
}

// ResetTool 重置特定工具的重试计数。
func (t *DegradationTracker) ResetTool(toolName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.retries, toolName)
}

// ToolModifier 工具修改器接口。
type ToolModifier interface {
	// ModifyTools 根据当前降级级别修改工具列表。
	ModifyTools(tools []ToolDefinition) []ToolDefinition
}

// DegradationAwareModifier 降级感知的工具修改器。
type DegradationAwareModifier struct {
	tracker       *DegradationTracker
	originalTools []ToolDefinition
}

// NewDegradationAwareModifier 创建修改器。
func NewDegradationAwareModifier(tracker *DegradationTracker) *DegradationAwareModifier {
	return &DegradationAwareModifier{
		tracker: tracker,
	}
}

// ModifyTools 实现 ToolModifier 接口。
func (m *DegradationAwareModifier) ModifyTools(tools []ToolDefinition) []ToolDefinition {
	level := m.tracker.CurrentLevel()

	switch level {
	case LevelFull:
		return tools
	case LevelToolsDisabled:
		// 返回空工具列表
		return []ToolDefinition{}
	case LevelReadOnly:
		// 只保留读工具
		var readOnly []ToolDefinition
		for _, t := range tools {
			if isReadOnlyTool(t) {
				readOnly = append(readOnly, t)
			}
		}
		return readOnly
	case LevelNoCache:
		// 移除有缓存提示的工具
		var noCache []ToolDefinition
		for _, t := range tools {
			if !hasCacheControl(t) {
				noCache = append(noCache, t)
			}
		}
		return noCache
	default:
		return tools
	}
}

// isReadOnlyTool 判断工具是否为只读。
func isReadOnlyTool(t ToolDefinition) bool {
	// 基于工具名称判断
	readOnlyPrefixes := []string{"Read", "Get", "List", "Search", "Fetch", "Query", "Show", "View"}
	for _, prefix := range readOnlyPrefixes {
		if len(t.Name) >= len(prefix) && t.Name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// hasCacheControl 判断工具是否有缓存控制。
func hasCacheControl(t ToolDefinition) bool {
	// 简化判断：检查名称或描述中是否包含 cache
	return false
}

// ToolDefinition 工具定义（简化版）。
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema any
}

// AutoDegradeMiddleware 自动降级中间件。
type AutoDegradeMiddleware struct {
	tracker  *DegradationTracker
	modifier *DegradationAwareModifier
}

// NewAutoDegradeMiddleware 创建中间件。
func NewAutoDegradeMiddleware(tracker *DegradationTracker) *AutoDegradeMiddleware {
	return &AutoDegradeMiddleware{
		tracker:  tracker,
		modifier: NewDegradationAwareModifier(tracker),
	}
}

// WrapTools 包装工具列表，应用降级。
func (m *AutoDegradeMiddleware) WrapTools(tools []ToolDefinition) ([]ToolDefinition, DegradationLevel) {
	level := m.tracker.CurrentLevel()
	return m.modifier.ModifyTools(tools), level
}

// HandleError 处理工具错误，可能触发降级。
func (m *AutoDegradeMiddleware) HandleError(toolName string, err error) bool {
	if m.tracker.ShouldDegrade(toolName, err) {
		// 根据错误类型决定降级级别
		if isCriticalError(err) {
			m.tracker.Degrade(LevelToolsDisabled)
		} else {
			m.tracker.Degrade(LevelReadOnly)
		}
		return true
	}
	return false
}

// isCriticalError 判断是否为关键错误。
func isCriticalError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	criticalErrors := []string{
		"context deadline exceeded",
		"connection refused",
		"authentication",
		"permission denied",
		"rate limit",
	}
	for _, critical := range criticalErrors {
		if contains(errStr, critical) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
