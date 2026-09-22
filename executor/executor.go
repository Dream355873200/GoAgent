// Package executor 实现工具执行，支持流式执行和并发/串行编排。
package executor

import (
	"context"
	"encoding/json"
	"sync"
)

// Config 控制执行器行为。
type Config struct {
	MaxConcurrency int // 最大并行工具执行数，默认 10
}

// Class 工具副作用分级——决定并行编排策略。三级替代旧的 bool：
//
//	ClassReadOnly   只读：无副作用（Read/Glob/Grep/WebSearch），任何
//	                场景可并行，包括独占工具运行期间。
//	ClassParallel   并行安全：有副作用但并发调用互不冲突
//	                （TaskCreate、独立请求），可与并行/只读工具并行，
//	                不能与独占工具重叠。
//	ClassExclusive  独占：有全局副作用（写文件、git、构建、Bash），
//	                必须独占执行，不与任何其他工具并行。
type Class int

const (
	// ClassUnset 未显式分级——按兼容字段 Concurrent 推导
	// （true→Parallel，false→Exclusive）。
	ClassUnset Class = iota
	ClassExclusive
	ClassParallel
	ClassReadOnly
)

// ExecuteFn 是工具执行的函数签名。
type ExecuteFn func(ctx context.Context, input json.RawMessage) (string, error)

// PreCheckFn 是工具执行前的预检查函数（权限检查、输入验证等）。
// 返回 (拒绝原因, 是否被拒绝)。被拒绝时 reason 会作为 tool_result 返回给模型。
type PreCheckFn func(ctx context.Context) (reason string, denied bool)

// ToolCall 是一个待执行的工具调用。
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
	// Class 副作用分级。非 Unset 时优先生效；Unset 时按 Concurrent 推导。
	Class Class
	// Concurrent 兼容字段：旧代码用 bool 标记「可并行」。
	// Class 显式分级存在时被忽略。新代码请用 Class。
	// Deprecated: 使用 Class。
	Concurrent bool
	Execute    ExecuteFn
	// PreCheck 在执行前调用（权限检查、中间件、hooks 等）。
	// 为 nil 时跳过预检查直接执行。
	// 对齐 Claude Code 的 StreamingToolExecutor 在 executeTool 内做权限检查。
	PreCheck PreCheckFn
}

// effect 返回调用的有效分级（显式 Class 优先，兼容字段兜底）。
func (c ToolCall) effect() Class {
	if c.Class != ClassUnset {
		return c.Class
	}
	if c.Concurrent {
		return ClassParallel
	}
	return ClassExclusive
}

// ToolResult 是工具执行的结果。
type ToolResult struct {
	ToolUseID string
	Name      string
	Content   string
	IsError   bool
}

// Executor 管理工具执行的并发控制。
type Executor struct {
	maxConcurrency int
}

// New 创建一个新的 Executor。
func New(cfg Config) *Executor {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 10
	}
	return &Executor{maxConcurrency: cfg.MaxConcurrency}
}

// Execute 使用适当的串行/并发编排运行一批工具调用。
// 只读/并行安全的工具并行运行；独占工具串行运行。
// 对齐 Claude Code 的分区策略。
func (e *Executor) Execute(ctx context.Context, calls []ToolCall) []ToolResult {
	batches := e.partition(calls)
	var allResults []ToolResult

	for _, batch := range batches {
		if len(batch) == 1 || batch[0].effect() == ClassExclusive {
			// 串行执行。
			for _, call := range batch {
				result := e.executeOne(ctx, call)
				allResults = append(allResults, result)
				// 在串行调用之间检查中止。
				if ctx.Err() != nil {
					return allResults
				}
			}
		} else {
			// 并行执行。
			results := e.executeConcurrent(ctx, batch)
			allResults = append(allResults, results...)
		}
	}

	return allResults
}

// partition 将工具调用分组为批次：
// 连续的只读/并行安全工具组成一个批次（并行），
// 每个独占工具是自己的批次（串行）。
func (e *Executor) partition(calls []ToolCall) [][]ToolCall {
	var batches [][]ToolCall
	var currentBatch []ToolCall

	for _, call := range calls {
		if call.effect() != ClassExclusive {
			currentBatch = append(currentBatch, call)
		} else {
			// 刷新待处理的并发批次。
			if len(currentBatch) > 0 {
				batches = append(batches, currentBatch)
				currentBatch = nil
			}
			// 独占工具是自己的批次。
			batches = append(batches, []ToolCall{call})
		}
	}

	// 刷新剩余的并发批次。
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

// executeOne 运行单个工具调用。
// 如果 ToolCall.PreCheck 不为 nil，先执行预检查（权限/验证），
// 被拒绝则直接返回错误结果，不执行工具。
// 对齐 Claude Code 的 StreamingToolExecutor.executeTool() 内置权限检查。
func (e *Executor) executeOne(ctx context.Context, call ToolCall) ToolResult {
	// 预检查（权限检查、输入验证、中间件、hooks 等）。
	if call.PreCheck != nil {
		if reason, denied := call.PreCheck(ctx); denied {
			return ToolResult{
				ToolUseID: call.ID,
				Name:      call.Name,
				Content:   reason,
				IsError:   true,
			}
		}
	}

	content, err := call.Execute(ctx, call.Input)
	if err != nil {
		return ToolResult{
			ToolUseID: call.ID,
			Name:      call.Name,
			Content:   "错误: " + err.Error(),
			IsError:   true,
		}
	}
	return ToolResult{
		ToolUseID: call.ID,
		Name:      call.Name,
		Content:   content,
		IsError:   false,
	}
}

// executeConcurrent 并行运行多个并发安全的工具。
func (e *Executor) executeConcurrent(ctx context.Context, calls []ToolCall) []ToolResult {
	results := make([]ToolResult, len(calls))
	sem := make(chan struct{}, e.maxConcurrency)
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c ToolCall) {
			defer wg.Done()
			sem <- struct{}{}        // 获取信号量
			defer func() { <-sem }() // 释放信号量

			if ctx.Err() != nil {
				results[idx] = ToolResult{
					ToolUseID: c.ID,
					Name:      c.Name,
					Content:   "已取消",
					IsError:   true,
				}
				return
			}
			results[idx] = e.executeOne(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}

// --- StreamingExecutor ---

// StreamingExecutor 允许工具在 LLM 仍在流式传输响应时开始执行。
// 这是 Claude Code 架构中的关键延迟优化。
//
// 用法：
//
//	se := NewStreamingExecutor(executor)
//	// 在 API 流式传输期间：
//	se.Add(toolCall)              // 解析后立即开始执行
//	completed := se.Poll()        // 非阻塞检查结果
//	// 流式传输结束后：
//	remaining := se.Wait(ctx)     // 等待所有剩余工具
type StreamingExecutor struct {
	executor      *Executor
	mu            sync.Mutex
	pending       []ToolCall
	completed     chan ToolResult
	wg            sync.WaitGroup
	running       int
	allConcurrent bool
	ctx           context.Context // 保存调用方的 context
}

// NewStreamingExecutor 创建一个新的流式执行器。
func NewStreamingExecutor(exec *Executor) *StreamingExecutor {
	return &StreamingExecutor{
		executor:      exec,
		completed:     make(chan ToolResult, 64),
		allConcurrent: true,
		ctx:           context.Background(),
	}
}

// Add 将工具调用排队执行。分级调度：
// 只读工具在并发上限内随时可启动（含独占工具运行期间）；
// 并行安全工具在无独占工具运行时可启动；独占工具等全部完成后独占。
func (se *StreamingExecutor) Add(ctx context.Context, call ToolCall) {
	se.mu.Lock()
	defer se.mu.Unlock()

	// 保存调用方的 context，供后续 drainPending 使用。
	se.ctx = ctx

	if call.effect() == ClassExclusive {
		se.allConcurrent = false
	}

	if se.canStartNow(call) {
		se.startTool(ctx, call)
	} else {
		se.pending = append(se.pending, call)
	}
}

// Poll 非阻塞地返回已完成的结果。
func (se *StreamingExecutor) Poll() []ToolResult {
	var results []ToolResult
	for {
		select {
		case r := <-se.completed:
			results = append(results, r)
			se.mu.Lock()
			se.running--
			se.drainPending(se.ctx)
			se.mu.Unlock()
		default:
			return results
		}
	}
}

// Wait 阻塞直到所有排队和运行中的工具完成。
func (se *StreamingExecutor) Wait(ctx context.Context) []ToolResult {
	// 循环：每次 drain 启动一批 pending 工具，等待它们完成后再检查剩余 pending。
	// 确保非并发工具一个接一个执行时不会遗漏。
	var results []ToolResult
	for {
		se.mu.Lock()
		se.ctx = ctx
		se.drainPending(ctx)
		running := se.running
		pending := len(se.pending)
		se.mu.Unlock()

		if running == 0 && pending == 0 {
			break
		}

		// 等待当前 running 的工具完成。
		se.wg.Wait()

		// 排空已完成的结果（同时递减 running 并 drainPending）。
		var roundResults []ToolResult
		for {
			select {
			case r := <-se.completed:
				roundResults = append(roundResults, r)
				se.mu.Lock()
				se.running--
				se.mu.Unlock()
			default:
				goto nextRound
			}
		}
	nextRound:
		// 收集本轮结果
		results = append(results, roundResults...)
	}

	// 最终排空已完成的通道。
	for {
		select {
		case r := <-se.completed:
			results = append(results, r)
		default:
			return results
		}
	}
}

// Discard 取消所有待处理的工具（用于模型后备切换）。
func (se *StreamingExecutor) Discard() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.pending = nil
}

func (se *StreamingExecutor) canStartNow(call ToolCall) bool {
	switch call.effect() {
	case ClassReadOnly:
		// 只读：无副作用，只受全局并发上限约束。
		return se.running < se.executor.maxConcurrency
	case ClassParallel:
		if se.allConcurrent && se.running < se.executor.maxConcurrency {
			return true
		}
		return se.running == 0
	default: // ClassExclusive
		return se.running == 0
	}
}

func (se *StreamingExecutor) startTool(ctx context.Context, call ToolCall) {
	se.running++
	se.wg.Add(1)
	go func() {
		defer se.wg.Done()
		result := se.executor.executeOne(ctx, call)
		se.completed <- result
	}()
}

func (se *StreamingExecutor) drainPending(ctx context.Context) {
	for len(se.pending) > 0 {
		call := se.pending[0]
		if se.canStartNow(call) {
			se.pending = se.pending[1:]
			se.startTool(ctx, call)
		} else {
			break
		}
	}
}
