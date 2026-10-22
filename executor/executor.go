// Package executor 实现工具执行，支持流式执行和并发/串行编排。
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/observer"
)

// Config 控制执行器行为。
type Config struct {
	MaxConcurrency int // 最大并行工具执行数，默认 10

	// PanicHandler 在工具 panic 时调用（可选），用于记录堆栈。panic 本身
	// 总会被恢复并转成错误结果返回给模型，不会带走进程。
	PanicHandler func(toolName string, recovered any, stack []byte)
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
	onPanic        func(toolName string, recovered any, stack []byte)
}

// New 创建一个新的 Executor。
func New(cfg Config) *Executor {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 10
	}
	return &Executor{maxConcurrency: cfg.MaxConcurrency, onPanic: cfg.PanicHandler}
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

// canceledResult 构造一个"未执行即取消"的结果。
func canceledResult(call ToolCall, why string) ToolResult {
	return ToolResult{ToolUseID: call.ID, Name: call.Name, Content: why, IsError: true}
}

// executeOne 运行单个工具调用。
// ctx 已取消时不再启动工具；PreCheck 拒绝时直接返回错误结果；
// 工具 panic 被恢复并转成错误结果。
func (e *Executor) executeOne(ctx context.Context, call ToolCall) (res ToolResult) {
	// 调用 ID 随 ctx 下发：工具据此把自身事件（如子 agent 进度）关联到
	// 所属的 tool_use（observer.ToolCallIDFromContext）。
	if call.ID != "" {
		ctx = observer.WithToolCallID(ctx, call.ID)
	}

	defer func() {
		if r := recover(); r != nil {
			if e.onPanic != nil {
				e.onPanic(call.Name, r, debug.Stack())
			}
			res = ToolResult{
				ToolUseID: call.ID,
				Name:      call.Name,
				Content:   fmt.Sprintf("错误: 工具 %s 内部异常（panic）: %v", call.Name, r),
				IsError:   true,
			}
		}
	}()

	if ctx.Err() != nil {
		return canceledResult(call, "已取消：工具尚未开始执行")
	}

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

	if call.Execute == nil {
		return ToolResult{ToolUseID: call.ID, Name: call.Name, Content: fmt.Sprintf("错误: 工具 %s 没有实现", call.Name), IsError: true}
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
			results[idx] = e.executeOne(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}

// --- StreamingExecutor ---

// CancelGrace 是 ctx 取消后等待在跑工具收尾的时长。工具拿到已取消的 ctx
// 应尽快返回；超时仍未返回的工具，其结果由调用方按"被中断"合成，迟到的
// 真实结果被丢弃。
var CancelGrace = 3 * time.Second

// StreamingExecutor 允许工具在 LLM 仍在流式传输响应时开始执行。
// 这是 Claude Code 架构中的关键延迟优化。
//
// 用法：
//
//	se := NewStreamingExecutor(executor)
//	// 在 API 流式传输期间：
//	se.Add(ctx, toolCall)         // 解析后立即开始执行
//	completed := se.Poll()        // 非阻塞检查结果
//	// 流式传输结束后：
//	remaining := se.Wait(ctx)     // 等待所有剩余工具
//
// 每个工具的结果只会从 Poll 或 Wait 中返回一次，调用方必须把两者的结果
// 合并使用。
type StreamingExecutor struct {
	executor      *Executor
	mu            sync.Mutex
	pending       []ToolCall
	running       int
	allConcurrent bool
	ctx           context.Context // 最近一次 Add/Wait 的 ctx，Poll 启动排队工具时使用

	completed   chan ToolResult
	abandoned   chan struct{} // 关闭后，迟到的结果不再投递
	abandonOnce sync.Once
}

// NewStreamingExecutor 创建一个新的流式执行器。
func NewStreamingExecutor(exec *Executor) *StreamingExecutor {
	if exec == nil {
		exec = New(Config{})
	}
	return &StreamingExecutor{
		executor:      exec,
		completed:     make(chan ToolResult, 64),
		abandoned:     make(chan struct{}),
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

// Wait 阻塞直到所有排队和运行中的工具完成。ctx 取消后不再启动排队中的
// 工具（它们以"已取消"结果返回），并最多再等 CancelGrace 让在跑的工具
// 收尾；仍未结束的工具不出现在返回值里。
func (se *StreamingExecutor) Wait(ctx context.Context) []ToolResult {
	var results []ToolResult
	for {
		se.mu.Lock()
		se.ctx = ctx
		if ctx.Err() != nil {
			results = append(results, se.cancelPendingLocked()...)
		} else {
			se.drainPending(ctx)
		}
		running := se.running
		se.mu.Unlock()

		if running == 0 {
			return results
		}

		select {
		case r := <-se.completed:
			results = append(results, r)
			se.mu.Lock()
			se.running--
			se.mu.Unlock()
		case <-ctx.Done():
			se.mu.Lock()
			results = append(results, se.cancelPendingLocked()...)
			se.mu.Unlock()
			return append(results, se.waitRunning(CancelGrace)...)
		}
	}
}

// waitRunning 在 grace 时长内收集在跑工具的结果，超时后放弃剩余工具。
func (se *StreamingExecutor) waitRunning(grace time.Duration) []ToolResult {
	var results []ToolResult
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		se.mu.Lock()
		running := se.running
		se.mu.Unlock()
		if running == 0 {
			return results
		}
		select {
		case r := <-se.completed:
			results = append(results, r)
			se.mu.Lock()
			se.running--
			se.mu.Unlock()
		case <-timer.C:
			se.abandonOnce.Do(func() { close(se.abandoned) })
			return results
		}
	}
}

// cancelPendingLocked 把尚未启动的工具转成"已取消"结果。调用方须持有 se.mu。
func (se *StreamingExecutor) cancelPendingLocked() []ToolResult {
	if len(se.pending) == 0 {
		return nil
	}
	out := make([]ToolResult, 0, len(se.pending))
	for _, c := range se.pending {
		out = append(out, canceledResult(c, "已取消：工具尚未开始执行"))
	}
	se.pending = nil
	return out
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
	go func() {
		result := se.executor.executeOne(ctx, call)
		select {
		case se.completed <- result:
		case <-se.abandoned:
		}
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
