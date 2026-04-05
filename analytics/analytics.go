// Package analytics 实现使用分析和遥测追踪。
//
// 追踪 agent 循环的关键指标：
//   - 每轮耗时
//   - 工具调用频率和延迟
//   - Token 使用趋势
//   - 压缩触发频率
//   - 错误率
//
// 所有数据仅保留在本地内存中，不发送到任何外部服务。
package analytics

import (
	"sync"
	"time"
)

// EventKind 标识分析事件类型。
type EventKind string

const (
	EventTurnStart    EventKind = "turn_start"
	EventTurnEnd      EventKind = "turn_end"
	EventToolCall     EventKind = "tool_call"
	EventToolResult   EventKind = "tool_result"
	EventCompaction   EventKind = "compaction"
	EventAPICall      EventKind = "api_call"
	EventError        EventKind = "error"
	EventTokenWarning EventKind = "token_warning"
)

// AnalyticsEvent 是单个分析事件。
type AnalyticsEvent struct {
	Kind      EventKind      `json:"kind"`
	Timestamp time.Time      `json:"timestamp"`
	Duration  time.Duration  `json:"duration,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Tracker 追踪分析事件。
type Tracker struct {
	mu     sync.Mutex
	events []AnalyticsEvent
	// 聚合统计。
	turnCount       int
	toolCallCount   int
	compactionCount int
	errorCount      int
	totalDuration   time.Duration
	toolDurations   map[string]time.Duration // toolName -> 总耗时
	toolCounts      map[string]int           // toolName -> 调用次数
}

// NewTracker 创建一个新的分析追踪器。
func NewTracker() *Tracker {
	return &Tracker{
		toolDurations: make(map[string]time.Duration),
		toolCounts:    make(map[string]int),
	}
}

// Record 记录一个分析事件。
func (t *Tracker) Record(event AnalyticsEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()

	event.Timestamp = time.Now()
	t.events = append(t.events, event)

	// 更新聚合统计。
	switch event.Kind {
	case EventTurnEnd:
		t.turnCount++
		t.totalDuration += event.Duration
	case EventToolCall:
		t.toolCallCount++
		if name, ok := event.Metadata["tool_name"].(string); ok {
			t.toolCounts[name]++
		}
	case EventToolResult:
		if name, ok := event.Metadata["tool_name"].(string); ok {
			t.toolDurations[name] += event.Duration
		}
	case EventCompaction:
		t.compactionCount++
	case EventError:
		t.errorCount++
	}
}

// RecordTurnStart 记录轮次开始。
func (t *Tracker) RecordTurnStart() {
	t.Record(AnalyticsEvent{Kind: EventTurnStart})
}

// RecordTurnEnd 记录轮次结束及耗时。
func (t *Tracker) RecordTurnEnd(duration time.Duration) {
	t.Record(AnalyticsEvent{
		Kind:     EventTurnEnd,
		Duration: duration,
	})
}

// RecordToolCall 记录工具调用。
func (t *Tracker) RecordToolCall(toolName string) {
	t.Record(AnalyticsEvent{
		Kind:     EventToolCall,
		Metadata: map[string]any{"tool_name": toolName},
	})
}

// RecordToolResult 记录工具结果及耗时。
func (t *Tracker) RecordToolResult(toolName string, duration time.Duration, isError bool) {
	t.Record(AnalyticsEvent{
		Kind:     EventToolResult,
		Duration: duration,
		Metadata: map[string]any{
			"tool_name": toolName,
			"is_error":  isError,
		},
	})
}

// RecordCompaction 记录压缩事件。
func (t *Tracker) RecordCompaction(tokensFreed int) {
	t.Record(AnalyticsEvent{
		Kind:     EventCompaction,
		Metadata: map[string]any{"tokens_freed": tokensFreed},
	})
}

// RecordError 记录错误事件。
func (t *Tracker) RecordError(err error) {
	t.Record(AnalyticsEvent{
		Kind:     EventError,
		Metadata: map[string]any{"error": err.Error()},
	})
}

// Summary 返回分析摘要。
type Summary struct {
	TurnCount       int                 `json:"turn_count"`
	ToolCallCount   int                 `json:"tool_call_count"`
	CompactionCount int                 `json:"compaction_count"`
	ErrorCount      int                 `json:"error_count"`
	TotalDuration   time.Duration       `json:"total_duration"`
	AvgTurnDuration time.Duration       `json:"avg_turn_duration"`
	ToolStats       map[string]ToolStat `json:"tool_stats"`
}

// ToolStat 是单个工具的统计信息。
type ToolStat struct {
	Count     int           `json:"count"`
	TotalTime time.Duration `json:"total_time"`
	AvgTime   time.Duration `json:"avg_time"`
}

// GetSummary 返回当前的分析摘要。
func (t *Tracker) GetSummary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := Summary{
		TurnCount:       t.turnCount,
		ToolCallCount:   t.toolCallCount,
		CompactionCount: t.compactionCount,
		ErrorCount:      t.errorCount,
		TotalDuration:   t.totalDuration,
		ToolStats:       make(map[string]ToolStat),
	}

	if t.turnCount > 0 {
		s.AvgTurnDuration = t.totalDuration / time.Duration(t.turnCount)
	}

	for name, count := range t.toolCounts {
		stat := ToolStat{Count: count}
		if dur, ok := t.toolDurations[name]; ok {
			stat.TotalTime = dur
			if count > 0 {
				stat.AvgTime = dur / time.Duration(count)
			}
		}
		s.ToolStats[name] = stat
	}

	return s
}

// Reset 清除所有追踪数据。
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = nil
	t.turnCount = 0
	t.toolCallCount = 0
	t.compactionCount = 0
	t.errorCount = 0
	t.totalDuration = 0
	t.toolDurations = make(map[string]time.Duration)
	t.toolCounts = make(map[string]int)
}
