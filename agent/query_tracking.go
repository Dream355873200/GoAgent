// Package agent 实现 agent 核心机制。
package agent

import (
	"sync"
	"time"
)

// QueryID 是查询的唯一标识符。
type QueryID string

// QueryEvent 是查询事件的接口。
type QueryEvent interface {
	GetQueryID() QueryID
	GetTimestamp() time.Time
}

// BaseQueryEvent 提供基础实现。
type BaseQueryEvent struct {
	queryID   QueryID
	timestamp time.Time
}

func (e *BaseQueryEvent) GetQueryID() QueryID     { return e.queryID }
func (e *BaseQueryEvent) GetTimestamp() time.Time { return e.timestamp }

// TextDeltaEvent 文本增量事件。
type TextDeltaEvent struct {
	*BaseQueryEvent
	Text string
}

// ToolUseEvent 工具调用事件。
type ToolUseEvent struct {
	*BaseQueryEvent
	ToolName  string
	ToolInput string
}

// ToolResultEvent 工具结果事件。
type ToolResultEvent struct {
	*BaseQueryEvent
	ToolName string
	Result   string
	Error    error
}

// UsageEvent token 使用量事件。
type UsageEvent struct {
	*BaseQueryEvent
	InputTokens  int
	OutputTokens int
}

// QueryTracker 查询追踪器。
// 对齐 Claude Code 的 query tracking / chain ID 机制。
type QueryTracker struct {
	queries   map[QueryID]*Query
	mu        sync.RWMutex
	currentID QueryID
}

// NewQueryTracker 创建查询追踪器。
func NewQueryTracker() *QueryTracker {
	return &QueryTracker{
		queries: make(map[QueryID]*Query),
	}
}

// Query 单个查询的追踪信息。
type Query struct {
	ID          QueryID
	ParentID    QueryID      // 父查询 ID（用于嵌套追踪）
	RootID      QueryID      // 根查询 ID
	Span        string       // 当前阶段
	Events      []QueryEvent // 事件历史
	Status      string       // running, completed, failed
	CreatedAt   time.Time
	CompletedAt time.Time
}

// StartQuery 开始追踪一个新查询。
func (t *QueryTracker) StartQuery(parentID QueryID) QueryID {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := generateQueryID()
	now := time.Now()

	var rootID QueryID
	if parentID != "" {
		rootID = t.queries[parentID].RootID
	} else {
		rootID = id
	}

	t.queries[id] = &Query{
		ID:        id,
		ParentID:  parentID,
		RootID:    rootID,
		Span:      "start",
		Events:    []QueryEvent{},
		Status:    "running",
		CreatedAt: now,
	}

	t.currentID = id
	return id
}

// AddEvent 添加查询事件。
func (t *QueryTracker) AddEvent(queryID QueryID, event QueryEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if query, ok := t.queries[queryID]; ok {
		query.Events = append(query.Events, event)
	}
}

// SetSpan 设置当前阶段。
func (t *QueryTracker) SetSpan(queryID QueryID, span string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if query, ok := t.queries[queryID]; ok {
		query.Span = span
	}
}

// CompleteQuery 标记查询完成。
func (t *QueryTracker) CompleteQuery(queryID QueryID, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if query, ok := t.queries[queryID]; ok {
		query.Status = status
		query.CompletedAt = time.Now()
	}
}

// GetQuery 获取查询信息。
func (t *QueryTracker) GetQuery(queryID QueryID) *Query {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.queries[queryID]
}

// GetCurrentQuery 获取当前查询。
func (t *QueryTracker) GetCurrentQuery() *Query {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.queries[t.currentID]
}

// GetChain 获取查询链（从根到当前）。
func (t *QueryTracker) GetChain(queryID QueryID) []QueryID {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var chain []QueryID
	current := queryID

	for {
		if query, ok := t.queries[current]; ok {
			chain = append([]QueryID{current}, chain...)
			if query.ParentID != "" {
				current = query.ParentID
			} else {
				break
			}
		} else {
			break
		}
	}

	return chain
}

// generateQueryID 生成唯一查询 ID。
func generateQueryID() QueryID {
	return QueryID(time.Now().Format("20060102150405.000000"))
}

// QueryContext 查询上下文（传递给工具）。
type QueryContext struct {
	// QueryID 当前查询 ID。
	QueryID QueryID
	// Chain 查询链。
	Chain []QueryID
	// Span 当前阶段。
	Span string
}

// NewQueryContext 创建查询上下文。
func NewQueryContext(tracker *QueryTracker, queryID QueryID) *QueryContext {
	return &QueryContext{
		QueryID: queryID,
		Chain:   tracker.GetChain(queryID),
		Span:    tracker.GetQuery(queryID).Span,
	}
}
