// Package goagent Pipeline DAG 编排引擎。
//
// Pipeline 提供 DAG 流水线编排，支持：
//   - 多节点依赖调度（拓扑排序）
//   - 并行 worker（MapReduce 模式）
//   - 双队列（message 输入 + result 输出）
//   - supervisor 上帝节点（事件驱动审核）
//   - 自动攒批触发审核
//   - Injects 权限控制
package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sync"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/prompts"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/schema"
)

// PipelineNodeNameKey 是 pipeline 运行时注入到 context 的 context key，
// 值为当前执行的 DAG 节点名（string）。
// Observer 可通过 context.Value(PipelineNodeNameKey) 获取当前节点名。
type pipelineNodeNameKey struct{}

// PipelineNodeNameKey 是 context key，用于从 context 获取 pipeline 节点名。
var PipelineNodeNameKey = pipelineNodeNameKey{}

// ---------------------------------------------------------------------------
// 公共类型
// ---------------------------------------------------------------------------

// PipelineConfig 定义 DAG 流水线。
type PipelineConfig struct {
	// Nodes 是 DAG 节点列表。
	Nodes []PipelineNode

	// Supervisor 是上帝节点（可选，不参与 DAG 调度）。
	// 当存在 Review=true 的节点时，框架自动注入审核 tool。
	Supervisor *PipelineAgentDef

	// SharedData 是 pipeline 全局共享数据，会注入到所有节点的 context 中。
	// tool 通过 GetPipelineData(ctx, key) 获取。
	SharedData map[string]any

	// TransactionFactory 应用层事务工厂（可选）。
	// 如果设置，Review=true 的节点每次 worker 执行前会创建事务，
	// approve 时 Commit，reject 时 Rollback 后重试。
	TransactionFactory TransactionFactory
}

// PipelineNode 定义一个 agent 节点。
type PipelineNode struct {
	// Name 节点名（唯一标识，如 "storyboard"）。
	Name string

	// Agent 定义此节点的 agent。
	Agent *PipelineAgentDef

	// Concurrency worker 并发数（默认 1）。
	// 所有节点都创建 message 队列，Concurrency 控制同时运行几个 worker。
	Concurrency int

	// DependsOn 依赖的节点名列表。全部完成后才启动此节点。
	DependsOn []string

	// Message 节点上下文消息。作为每个 worker 的 user message 前缀。
	// 非空时自动 Push 到队列作为初始任务。
	Message string

	// Injects 声明此 node 的 agent tool 可以往哪些下游 node 的队列推送。
	// 框架在运行此 node 时，将指定队列注入到 context。
	Injects []string

	// CloseQueues 当前节点完成时应关闭的下游队列名列表。
	// 默认等于 Injects（向后兼容）。
	// 多个节点声明 Close 同一队列时，队列需要所有 Close 调用后才真正关闭（引用计数）。
	CloseQueues []string

	// MessageType message 队列元素类型的零值（默认 string）。
	// 设置为零值即可，如 Task{}。框架通过反射推断实际类型。
	MessageType any

	// ResultType result 队列元素类型的零值（默认 string）。
	ResultType any

	// Review 是否需要 supervisor 审核 result（默认 false，自动通过）。
	Review bool

	// ReviewBatch 攒多少个 result 触发审核（默认 1）。
	// 当 message 队列为空时也会立刻触发。
	ReviewBatch int

	// MaxRetries 最大重试次数（默认 3）。
	// 仅 Review=true 时生效。
	MaxRetries int

	// OnTaskResult 通过后的回调。
	// Review=false 时，worker 完成后自动调用。
	// Review=true 时，supervisor 批准后调用。
	// result 参数的实际类型由 ResultType 决定，用户需做类型断言。
	OnTaskResult func(result any)

	// OnNodeComplete 所有 worker 完成 + review 通过后触发一次。
	// 适合执行收尾操作（如批量发布、发送通知）。
	OnNodeComplete func()

	// QueueSize 队列缓冲大小（默认 64）。
	QueueSize int
}

// PipelineAgentDef 定义 pipeline 中的子 agent。
type PipelineAgentDef struct {
	// Name agent 名。
	Name string

	// Instruction system prompt。
	Instruction string

	// Tools 此 agent 可用的工具。
	Tools []NamedTool

	// Provider 可选，nil 则继承父 App 的 provider。
	Provider provider.Provider

	// MaxTurns agent 最大轮次（默认 100）。
	MaxTurns int
}

// ---------------------------------------------------------------------------
// MessagePusher — 推送 message 到节点队列
// ---------------------------------------------------------------------------

// MessagePusher 推送任务到节点 message 队列。
type MessagePusher interface {
	// Push 推送一条任务 message。msg 的类型应匹配节点的 MessageType。
	Push(msg any)
	// Close 关闭队列（通知 worker 退出）。
	Close()
}

// GetMessageQueue 从 context 获取指定节点的 message 队列。
// 只有当前 node 的 Injects 中包含的节点才能获取到，否则返回 nil。
func GetMessageQueue(ctx context.Context, nodeName string) MessagePusher {
	queues, ok := ctx.Value(ctxKeyMessageQueues).(*injectedQueues)
	if !ok || queues == nil {
		return nil
	}
	mp, ok := queues.msgPushers[nodeName]
	if !ok {
		return nil
	}
	return mp
}

// ---------------------------------------------------------------------------
// GetPipelineData — 从 context 获取 SharedData
// ---------------------------------------------------------------------------

// GetPipelineData 从 context 获取 SharedData 中 key 对应的值。
func GetPipelineData(ctx context.Context, key string) (any, bool) {
	data, ok := ctx.Value(ctxKeySharedData).(map[string]any)
	if !ok || data == nil {
		return nil, false
	}
	v, ok := data[key]
	return v, ok
}

// GetPipelineDataString 获取 SharedData 中 key 对应的字符串值。
func GetPipelineDataString(ctx context.Context, key string) (string, bool) {
	v, ok := GetPipelineData(ctx, key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// GetPipelineDataInt64 获取 SharedData 中 key 对应的 int64 值。
func GetPipelineDataInt64(ctx context.Context, key string) (int64, bool) {
	v, ok := GetPipelineData(ctx, key)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// ReviewEvent — supervisor 审核事件
// ---------------------------------------------------------------------------

// ReviewEvent 是 supervisor 收到的审核事件。
type ReviewEvent struct {
	// NodeName 产生 result 的节点名。
	NodeName string `json:"node_name"`

	// Results 待审核的 result 列表（JSON 序列化后的数据）。
	Results []ReviewResultItem `json:"results"`

	// Done 为 true 表示所有 Review 节点完成，supervisor 可以退出。
	Done bool `json:"done"`
}

// ReviewResultItem 待审核的单个 result。
type ReviewResultItem struct {
	// Index 在本批次中的索引。
	Index int `json:"index"`

	// Data JSON 序列化的 result 数据。
	Data string `json:"data"`

	// OriginalMessage 产生此 result 的原始 message（用于 reject 时重试）。
	OriginalMessage any `json:"-"`

	// RetryCount 此 message 已重试的次数。
	RetryCount int `json:"-"`

	// rawResult 未序列化的原始 result（用于 OnTaskResult 回调）。
	rawResult any

	// tx 此 worker 执行期间的应用层事务（可选）。
	// approve 时 Commit，reject 时 Rollback。
	tx Transaction
}

// ---------------------------------------------------------------------------
// context key 类型
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyMessageQueues ctxKey = iota
	ctxKeyReviewSignal
	ctxKeyPipeline
	ctxKeySharedData
)

// injectedQueues 是注入到 context 中的队列集合（受 Injects 限制）。
type injectedQueues struct {
	msgPushers map[string]MessagePusher
}

// ---------------------------------------------------------------------------
// messagePusher — MessagePusher 的 channel 实现
// ---------------------------------------------------------------------------

type messagePusher struct {
	ch          chan any
	closed      bool
	mu          sync.Mutex
	nodeType    reflect.Type // 期望的 message 类型（nil 表示 string）
	closeCount  int          // 需要多少个 Close 调用才真正关闭
	closedTimes int          // 已被 Close 的次数
}

func newMessagePusher(size int, msgType reflect.Type, expectedCloseCount int) *messagePusher {
	if expectedCloseCount <= 0 {
		expectedCloseCount = 1
	}
	return &messagePusher{
		ch:         make(chan any, size),
		nodeType:   msgType,
		closeCount: expectedCloseCount,
	}
}

func (mp *messagePusher) Push(msg any) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.closed {
		return
	}
	mp.ch <- msg
}

func (mp *messagePusher) Close() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.closedTimes++
	if mp.closedTimes >= mp.closeCount && !mp.closed {
		mp.closed = true
		close(mp.ch)
	}
}

func (mp *messagePusher) isClosed() bool {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return mp.closed
}

func (mp *messagePusher) len() int {
	return len(mp.ch)
}

// ---------------------------------------------------------------------------
// pipeline — 内部调度器
// ---------------------------------------------------------------------------

type pipeline struct {
	cfg   PipelineConfig
	nodes map[string]*pipelineNodeState

	// 拓扑排序后的节点执行顺序
	topoOrder []string

	// 队列
	msgQueues map[string]*messagePusher // 每个节点的 message 队列

	// 审核
	pendingResults     map[string][]ReviewResultItem // 每个节点待审 result
	reviewSignal       chan ReviewEvent              // supervisor 的审核事件 channel
	pendingMu          sync.Mutex                    // 保护 pendingResults
	currentReviewItems map[string][]ReviewResultItem // 当前正在审核的 items（供 reject_result 读取）

	// DAG 状态
	doneCh       chan struct{} // pipeline 完成信号
	doneOnce     sync.Once
	nodesDone    map[string]bool // 节点完成标记
	nodesMu      sync.Mutex
	reviewDone   map[string]bool // Review 节点审核完成标记
	reviewDoneCh chan string     // 审核完成信号，通知调度器重新检查下游依赖

	// 节点错误状态：当某个节点的 LLM 返回空响应或致命错误时记录。
	// recordNodeError 会 cancel pipeline context，中断所有正在运行的节点。
	firstError error
	errMu      sync.Mutex

	// cancelFunc 用于中断 pipeline（由 recordNodeError 调用）。
	cancelFunc context.CancelFunc

	// 父 App（用于创建子 agent）
	parentApp *App

	// hasSupervisor 缓存 Supervisor 是否存在（初始化后不变，避免并发读 cfg.Supervisor）。
	hasSupervisor bool
}

type pipelineNodeState struct {
	PipelineNode
	// 运行时状态
	started bool

	// nodeCtx 节点执行时的 context（包含注入的 queues、SharedData 等）。
	// 在 runNode 中设置，供 reject 重试时复用。
	nodeCtx context.Context

	// reviewApproved 当 Review=true 时，review 全部通过后关闭此 channel，
	// 通知 runNode 可以 self-close 队列并退出 worker。
	reviewApproved chan struct{}

	// 由 applyNodeDefaults 从零值推断的 reflect.Type
	msgType    reflect.Type
	resultType reflect.Type
}

// newPipeline 创建 pipeline 实例。
func newPipeline(cfg PipelineConfig, parent *App) *pipeline {
	p := &pipeline{
		cfg:                cfg,
		nodes:              make(map[string]*pipelineNodeState),
		msgQueues:          make(map[string]*messagePusher),
		pendingResults:     make(map[string][]ReviewResultItem),
		currentReviewItems: make(map[string][]ReviewResultItem),
		reviewSignal:       make(chan ReviewEvent, 64),
		doneCh:             make(chan struct{}),
		nodesDone:          make(map[string]bool),
		reviewDone:         make(map[string]bool),
		reviewDoneCh:       make(chan string, len(cfg.Nodes)),
		parentApp:          parent,
		hasSupervisor:      cfg.Supervisor != nil,
	}

	// 统计每个队列被多少个节点声明 Close（引用计数）。
	// 每个节点的自身队列额外 +1（self-close：节点执行完自己关闭自己的队列）。
	closeCounts := make(map[string]int)
	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]
		if len(node.CloseQueues) == 0 {
			node.CloseQueues = node.Injects
		}
		for _, target := range node.CloseQueues {
			closeCounts[target]++
		}
		closeCounts[node.Name]++ // self-close
	}

	// 初始化节点状态和队列
	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]
		applyNodeDefaults(node)
		state := &pipelineNodeState{PipelineNode: *node}
		applyNodeTypes(node, state)
		if node.Review {
			state.reviewApproved = make(chan struct{})
		}
		p.nodes[node.Name] = state

		// 所有节点都创建 message 队列
		p.msgQueues[node.Name] = newMessagePusher(node.QueueSize, state.msgType, closeCounts[node.Name])
	}

	return p
}

// applyNodeDefaults 填充节点默认值。
func applyNodeDefaults(node *PipelineNode) {
	if node.Concurrency <= 0 {
		node.Concurrency = 1
	}
	if node.QueueSize <= 0 {
		node.QueueSize = 64
	}
	if node.ReviewBatch <= 0 {
		node.ReviewBatch = 1
	}
	if node.MaxRetries <= 0 {
		node.MaxRetries = 3
	}
}

// applyNodeTypes 从零值推断 reflect.Type 并写入 pipelineNodeState。
func applyNodeTypes(node *PipelineNode, state *pipelineNodeState) {
	if node.MessageType != nil {
		state.msgType = reflect.TypeOf(node.MessageType)
	} else {
		state.msgType = reflect.TypeOf("")
	}
	if node.ResultType != nil {
		state.resultType = reflect.TypeOf(node.ResultType)
	} else {
		state.resultType = reflect.TypeOf("")
	}
}

// ---------------------------------------------------------------------------
// 拓扑排序
// ---------------------------------------------------------------------------

// topoSort 对节点进行拓扑排序。返回排序后的节点名列表。
// 如果存在循环依赖，返回错误。
func (p *pipeline) topoSort() ([]string, error) {
	inDegree := make(map[string]int)
	children := make(map[string][]string) // parent -> children

	for name := range p.nodes {
		inDegree[name] = 0
	}
	for name, node := range p.nodes {
		for _, dep := range node.DependsOn {
			if _, ok := p.nodes[dep]; !ok {
				return nil, fmt.Errorf("pipeline: 节点 %q 依赖不存在的节点 %q", name, dep)
			}
			children[dep] = append(children[dep], name)
			inDegree[name]++
		}
	}

	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}

	var order []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		for _, child := range children[cur] {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if len(order) != len(p.nodes) {
		return nil, fmt.Errorf("pipeline: 检测到循环依赖")
	}

	return order, nil
}

// ---------------------------------------------------------------------------
// hasReviewNodes 检查是否有 Review=true 的节点
// ---------------------------------------------------------------------------

func (p *pipeline) hasReviewNodes() bool {
	for _, node := range p.nodes {
		if node.Review {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// serializeResult 将 result 序列化为 JSON 字符串
// ---------------------------------------------------------------------------

func serializeResult(result any) string {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// Step 2: context 注入 — injectQueuesForNode
// ---------------------------------------------------------------------------

// injectQueuesForNode 根据 node 的 Injects 配置，将下游队列注入到 context。
// 只有 Injects 中声明的节点队列才会被注入。
func (p *pipeline) injectQueuesForNode(ctx context.Context, nodeName string) context.Context {
	node, ok := p.nodes[nodeName]
	if !ok {
		return ctx
	}

	pushers := make(map[string]MessagePusher)
	for _, target := range node.Injects {
		if q, ok := p.msgQueues[target]; ok {
			pushers[target] = q
		}
	}

	if len(pushers) == 0 {
		return ctx
	}

	return context.WithValue(ctx, ctxKeyMessageQueues, &injectedQueues{
		msgPushers: pushers,
	})
}

// ---------------------------------------------------------------------------
// Step 3: handleResult + 自动攒批触发审核
// ---------------------------------------------------------------------------

// handleResult 处理 worker 产出的 result。
// Review=false 时自动通过（直接调用 OnTaskResult 回调）。
// Review=true 时进入待审队列，触发攒批检查。
func (p *pipeline) handleResult(nodeName string, result any, originalMsg any, retryCount int, tx Transaction) {
	node, ok := p.nodes[nodeName]
	if !ok {
		return
	}
	log.Printf("[pipeline] handleResult: node=%s review=%v retry=%d result_type=%T has_tx=%v", nodeName, node.Review, retryCount, result, tx != nil)

	if !node.Review {
		// 自动通过，直接调用 OnTaskResult 回调。
		if node.OnTaskResult != nil {
			node.OnTaskResult(result)
		}
		return
	}

	// 进入待审队列。
	log.Printf("[pipeline] handleResult: acquiring pendingMu lock for node=%s", nodeName)
	item := ReviewResultItem{
		Data:            serializeResult(result),
		OriginalMessage: originalMsg,
		RetryCount:      retryCount,
		rawResult:       result,
		tx:              tx,
	}

	p.pendingMu.Lock()
	log.Printf("[pipeline] handleResult: pendingMu lock acquired for node=%s", nodeName)
	p.pendingResults[nodeName] = append(p.pendingResults[nodeName], item)
	pending := p.pendingResults[nodeName]

	// 重新计算索引。
	for i := range pending {
		pending[i].Index = i
	}

	pendingCount := len(pending)
	p.pendingMu.Unlock()

	p.checkReviewTrigger(nodeName, pendingCount)
}

// checkReviewTrigger 检查是否应触发审核。
// 条件：攒够 ReviewBatch 或 message 队列为空。
func (p *pipeline) checkReviewTrigger(nodeName string, pendingCount int) {
	node := p.nodes[nodeName]

	msgQueueEmpty := true
	if q, ok := p.msgQueues[nodeName]; ok {
		msgQueueEmpty = q.isClosed() || q.len() == 0
	}

	log.Printf("[pipeline] checkReviewTrigger: node=%s pending=%d batch=%d queueEmpty=%v", nodeName, pendingCount, node.ReviewBatch, msgQueueEmpty)

	if pendingCount >= node.ReviewBatch || (pendingCount > 0 && msgQueueEmpty) {
		p.pendingMu.Lock()
		items := p.pendingResults[nodeName]
		p.pendingResults[nodeName] = nil
		p.pendingMu.Unlock()

		if len(items) > 0 {
			if p.hasSupervisor {
				p.reviewSignal <- ReviewEvent{
					NodeName: nodeName,
					Results:  items,
				}
			} else {
				// 无 supervisor：自动通过。
				for _, item := range items {
					if item.tx != nil {
						item.tx.Commit()
					}
					if node.OnTaskResult != nil {
						node.OnTaskResult(item.rawResult)
					}
				}
				// 关闭 reviewApproved channel，让 worker 可以退出。
				if node.reviewApproved != nil {
					select {
					case <-node.reviewApproved:
					default:
						close(node.reviewApproved)
					}
				}
				p.markReviewDone(nodeName)
			}
		}
	}
}

// markNodeDone 标记节点完成。
// 如果节点有 Review=true，还需要刷新剩余的待审 result。
// 当所有节点完成后，发送 done 信号给 supervisor。
func (p *pipeline) markNodeDone(nodeName string) {
	log.Printf("[pipeline] markNodeDone: node=%s", nodeName)
	p.nodesMu.Lock()
	p.nodesDone[nodeName] = true
	p.nodesMu.Unlock()

	node := p.nodes[nodeName]

	// 节点完成后，如果有未触发审核的 pending results，立刻触发。
	if node.Review {
		p.pendingMu.Lock()
		items := p.pendingResults[nodeName]
		p.pendingResults[nodeName] = nil
		p.pendingMu.Unlock()

		if p.cfg.Supervisor != nil {
			// 有 supervisor：发送审核事件。
			if len(items) > 0 {
				p.reviewSignal <- ReviewEvent{
					NodeName: nodeName,
					Results:  items,
				}
			}
		} else {
			// 无 supervisor：自动通过，直接调用 OnTaskResult。
			for _, item := range items {
				if item.tx != nil {
					item.tx.Commit()
				}
				if node.OnTaskResult != nil {
					node.OnTaskResult(item.rawResult)
				}
			}
			// 关闭 reviewApproved channel，让 worker 可以退出。
			if node.reviewApproved != nil {
				select {
				case <-node.reviewApproved:
				default:
					close(node.reviewApproved)
				}
			}
			p.markReviewDone(nodeName)
		}
	} else {
		// Review=false 的节点，完成即审核完成。
		p.nodesMu.Lock()
		p.reviewDone[nodeName] = true
		p.nodesMu.Unlock()
	}

	// 检查是否所有 Review 节点都审核完成。
	p.checkAllReviewDone()

	// 所有 worker 完成 + review 通过后触发一次。
	if node.OnNodeComplete != nil {
		node.OnNodeComplete()
	}
}

// markReviewDone 标记某个 Review 节点的审核全部完成。
func (p *pipeline) markReviewDone(nodeName string) {
	log.Printf("[pipeline] markReviewDone: node=%s", nodeName)
	p.nodesMu.Lock()
	p.reviewDone[nodeName] = true
	p.nodesMu.Unlock()
	p.reviewDoneCh <- nodeName // 通知调度器重新检查下游依赖
	p.checkAllReviewDone()
}

// checkAllReviewDone 检查是否所有节点（含审核）都已完成。
func (p *pipeline) checkAllReviewDone() {
	p.nodesMu.Lock()
	defer p.nodesMu.Unlock()

	allDone := true
	for name := range p.nodes {
		if !p.nodesDone[name] || !p.reviewDone[name] {
			log.Printf("[pipeline] checkAllReviewDone: node=%s nodesDone=%v reviewDone=%v (blocking)", name, p.nodesDone[name], p.reviewDone[name])
			allDone = false
			break
		}
	}

	if allDone {
		log.Printf("[pipeline] checkAllReviewDone: ALL DONE, closing doneCh")
		// 通知 supervisor 可以退出。
		if p.hasReviewNodes() && p.hasSupervisor {
			p.reviewSignal <- ReviewEvent{Done: true}
		}
		p.doneOnce.Do(func() { close(p.doneCh) })
	}
}

// ---------------------------------------------------------------------------
// Step 4: 框架自动注入的审核 tool
// ---------------------------------------------------------------------------

// waitForReviewInput 是 wait_for_review 工具的空输入。
type waitForReviewInput struct{}

// approveResultInput 是 approve_result 工具的输入。
type approveResultInput struct {
	NodeName string `json:"node_name" desc:"产生 result 的节点名" required:"true"`
	Index    int    `json:"index" desc:"待审 result 在批次中的索引" required:"true"`
}

// rejectResultInput 是 reject_result 工具的输入。
type rejectResultInput struct {
	NodeName string `json:"node_name" desc:"产生 result 的节点名" required:"true"`
	Index    int    `json:"index" desc:"待审 result 在批次中的索引" required:"true"`
	Guidance string `json:"guidance" desc:"拒绝原因和修改指导" required:"true"`
}

// newReviewTools 创建审核 tool 列表（approve_result / reject_result）。
// wait_for_review 逻辑已内化到 runSupervisor 的事件循环中。
func (p *pipeline) newReviewTools() []NamedTool {
	return []NamedTool{
		{
			Name: "approve_result",
			Def: ToolDef{
				Description: "审核通过，允许节点结果生效。",
				Input:       approveResultInput{},
				Permission:  Normal,
				Concurrent:  false,
				Execute: func(ctx Context, in approveResultInput) (string, error) {
					log.Printf("[pipeline] approve_result: node=%s index=%d", in.NodeName, in.Index)
					node := p.nodes[in.NodeName]
					if node == nil {
						return "", fmt.Errorf("未知节点: %s", in.NodeName)
					}

					// Commit 所有审核通过的事务。
					p.pendingMu.Lock()
					items := p.currentReviewItems[in.NodeName]
					p.pendingMu.Unlock()
					for _, item := range items {
						if item.tx != nil {
							log.Printf("[pipeline] approve_result: node=%s committing transaction", in.NodeName)
							item.tx.Commit()
						}
					}

					if node.OnTaskResult != nil {
						node.OnTaskResult(nil)
					}
					// 关闭 reviewApproved channel，让 worker 可以退出。
					if node.reviewApproved != nil {
						select {
						case <-node.reviewApproved:
							// 已关闭，忽略
						default:
							close(node.reviewApproved)
						}
					}
					p.markReviewDone(in.NodeName)
					return fmt.Sprintf("已批准 %s[%d]", in.NodeName, in.Index), nil
				},
			},
		},
		{
			Name: "reject_result",
			Def: ToolDef{
				Description: "审核不通过，将任务退回给节点重新执行。如果超过最大重试次数则自动放行。",
				Input:       rejectResultInput{},
				Permission:  Normal,
				Concurrent:  false,
				Execute: func(ctx Context, in rejectResultInput) (string, error) {
					log.Printf("[pipeline] reject_result: node=%s index=%d guidance=%s", in.NodeName, in.Index, in.Guidance)
					node := p.nodes[in.NodeName]
					if node == nil {
						return "", fmt.Errorf("未知节点: %s", in.NodeName)
					}

					// 从 currentReviewItems 中取出被 reject 的 item。
					p.pendingMu.Lock()
					items := p.currentReviewItems[in.NodeName]
					p.pendingMu.Unlock()

					if in.Index < 0 || in.Index >= len(items) {
						return fmt.Sprintf("无效的索引 %d，当前批次共 %d 条", in.Index, len(items)), nil
					}
					item := items[in.Index]

					// 执行事务回滚（如果有）。
					if item.tx != nil {
						log.Printf("[pipeline] reject_result: node=%s index=%d rolling back transaction", in.NodeName, in.Index)
						if err := item.tx.Rollback(context.Background()); err != nil {
							log.Printf("[pipeline] reject_result: node=%s index=%d rollback error: %v", in.NodeName, in.Index, err)
						}
					}

					// 检查重试次数。
					if item.RetryCount >= node.MaxRetries {
						log.Printf("[pipeline] reject_result: node=%s index=%d max_retries=%d reached, auto-approving", in.NodeName, in.Index, node.MaxRetries)
						if node.OnTaskResult != nil {
							node.OnTaskResult(item.rawResult)
						}
						// 关闭 reviewApproved channel，让 worker 可以退出。
						if node.reviewApproved != nil {
							select {
							case <-node.reviewApproved:
							default:
								close(node.reviewApproved)
							}
						}
						p.markReviewDone(in.NodeName)
						return fmt.Sprintf("已达最大重试次数 (%d)，自动放行 %s[%d]", node.MaxRetries, in.NodeName, in.Index), nil
					}

					// 构造重试消息：原始 input + guidance 拼接。
					var originalInput string
					if s, ok := item.OriginalMessage.(string); ok {
						originalInput = s
					} else {
						data, _ := json.Marshal(item.OriginalMessage)
						originalInput = string(data)
					}
					// 如果节点有 Message 上下文，拼接到前面。
					fullInput := originalInput
					if node.Message != "" && originalInput != node.Message {
						fullInput = node.Message + "\n" + originalInput
					}
					fullInput += "\n\n[审核反馈，请根据以下指导修正] " + in.Guidance

					q := p.msgQueues[in.NodeName]
					if q == nil {
						return fmt.Sprintf("节点 %s 的队列不存在", in.NodeName), nil
					}
					q.Push(&retryMessage{
						msg:        item.OriginalMessage,
						input:      fullInput,
						retryCount: item.RetryCount + 1,
					})
					log.Printf("[pipeline] reject_result: node=%s index=%d retry=%d rolled back and pushed to queue", in.NodeName, in.Index, item.RetryCount+1)

					return fmt.Sprintf("已将 %s[%d] 回滚并退回重试（第 %d/%d 次）", in.NodeName, in.Index, item.RetryCount+1, node.MaxRetries), nil
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Step 5: DAG 拓扑调度 + worker 启动
// ---------------------------------------------------------------------------

// Run 启动 pipeline DAG 调度。
// 阻塞直到所有节点（含审核）完成或 context 被取消。
// 如果某个节点的 LLM 返回空响应（provider 配置错误等），会中断整个 pipeline 并返回错误。
func (p *pipeline) Run(ctx context.Context) error {
	// 创建可取消的子 context，用于 recordNodeError 中断所有节点。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.cancelFunc = cancel

	// 拓扑排序。
	order, err := p.topoSort()
	if err != nil {
		return err
	}
	p.topoOrder = order

	var wg sync.WaitGroup

	// 启动 supervisor（如果有 Review 节点）。
	if p.hasSupervisor && p.hasReviewNodes() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runSupervisor(runCtx)
		}()
	} else {
		// 没有 supervisor，所有节点自动标记审核完成。
		for name := range p.nodes {
			p.nodesMu.Lock()
			p.reviewDone[name] = true
			p.nodesMu.Unlock()
		}
	}

	// 启动无依赖的节点。
	readyCh := make(chan string, len(order))
	for _, name := range order {
		node := p.nodes[name]
		if len(node.DependsOn) == 0 {
			readyCh <- name
		}
	}

	// 调度循环：启动就绪节点，等待节点完成后检查下游。
	doneCh := make(chan string, len(order)) // 节点完成信号

	startNode := func(name string) {
		node := p.nodes[name]
		if node.started {
			return
		}
		node.started = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runNode(runCtx, name, doneCh)
		}()
	}

	// 调度器 goroutine。
	go func() {
		started := make(map[string]bool)
		// 检查下游依赖是否就绪，就绪则启动。
		checkDownstream := func() {
			for _, candidate := range order {
				if started[candidate] {
					continue
				}
				cNode := p.nodes[candidate]
				allDepsDone := true
				for _, dep := range cNode.DependsOn {
					p.nodesMu.Lock()
					done := p.reviewDone[dep]
					p.nodesMu.Unlock()
					if !done {
						allDepsDone = false
						break
					}
				}
				if allDepsDone {
					started[candidate] = true
					log.Printf("[pipeline] starting downstream node: %s", candidate)
					startNode(candidate)
				}
			}
		}
		for {
			select {
			case <-runCtx.Done():
				return
			case <-p.doneCh:
				return
			case name := <-readyCh:
				if !started[name] {
					started[name] = true
					startNode(name)
				}
			case <-doneCh:
				// 节点执行完成，检查下游是否就绪。
				checkDownstream()
			case <-p.reviewDoneCh:
				// 节点审核完成，重新检查下游依赖。
				checkDownstream()
			}
		}
	}()

	// 等待 pipeline 完成或 context 取消。
	select {
	case <-runCtx.Done():
		// 关闭所有队列，通知 worker 退出。
		for _, q := range p.msgQueues {
			q.Close()
		}
		wg.Wait()
		// 如果是 recordNodeError 触发的取消，返回节点错误而非 context.Canceled
		p.errMu.Lock()
		nodeErr := p.firstError
		p.errMu.Unlock()
		if nodeErr != nil {
			return nodeErr
		}
		return runCtx.Err()
	case <-p.doneCh:
		wg.Wait()
		// 正常完成也要检查是否有节点错误（理论上 doneCh 触发前已 cancel，但保险起见）
		p.errMu.Lock()
		nodeErr := p.firstError
		p.errMu.Unlock()
		if nodeErr != nil {
			return nodeErr
		}
		return nil
	}
}

// runNode 运行单个 DAG 节点。
func (p *pipeline) runNode(ctx context.Context, nodeName string, doneCh chan<- string) {
	node := p.nodes[nodeName]

	nodeCtx := ctx

	// 注入 SharedData 到 context。
	if len(p.cfg.SharedData) > 0 {
		nodeCtx = context.WithValue(nodeCtx, ctxKeySharedData, p.cfg.SharedData)
	}

	// 注入下游队列到 context。
	nodeCtx = p.injectQueuesForNode(nodeCtx, nodeName)

	// 注入节点名到 context，供 Observer 使用（如 usage logging 的 step_group）。
	nodeCtx = context.WithValue(nodeCtx, PipelineNodeNameKey, nodeName)

	// 保存 nodeCtx 供 reject 重试时复用。
	node.nodeCtx = nodeCtx

	// 如果有 Message，Push 到队列作为初始任务。
	if node.Message != "" {
		p.msgQueues[nodeName].Push(node.Message)
	}

	if node.Review && node.reviewApproved != nil {
		// Review 节点：不立即 self-close 队列。
		// 先 Push 初始任务，然后启动 workers。
		// workers 在消费完当前任务后会阻塞等待新消息（reject 重推）。
		// review 全部通过后 reviewApproved 被关闭，此时 self-close 队列让 workers 退出。
		go func() {
			select {
			case <-node.reviewApproved:
			case <-ctx.Done():
			}
			p.msgQueues[nodeName].Close()
		}()
	} else {
		// 非 Review 节点：立即 self-close，workers 消费完即退出。
		p.msgQueues[nodeName].Close()
	}

	// 统一走队列消费模式。
	p.runMultiWorkers(nodeCtx, node)

	// 关闭此节点声明的下游队列（引用计数 Close）。
	for _, target := range node.CloseQueues {
		if q, ok := p.msgQueues[target]; ok {
			q.Close()
		}
	}

	p.markNodeDone(nodeName)
	doneCh <- nodeName
}

// resolveAgentDef 提取 agent 定义的公共字段。
func (p *pipeline) resolveAgentDef(agentDef *PipelineAgentDef) (prov provider.Provider, maxTurns int) {
	prov = agentDef.Provider
	if prov == nil {
		prov = p.parentApp.provider
	}
	maxTurns = agentDef.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 100
	}
	return
}

// buildLightweightLoop 为 pipeline worker 构建精简的 loop。
// 跳过 Permission、Memory、Budget、SessionMemory、PlanChecker 等重型子系统。
// worker 需要：Provider + Tools + SystemPrompt + MaxTurns + Executor + Compaction + Hooks + Observer。
func (p *pipeline) buildLightweightLoop(agentDef *PipelineAgentDef, nodeCtx context.Context) *loop.Loop {
	prov, maxTurns := p.resolveAgentDef(agentDef)

	// 构建精简 tool 定义。
	// Permission 统一设为 ReadOnly — pipeline worker 内部调用，无需权限检查。
	tools := make([]loop.ToolEntry, 0, len(agentDef.Tools))
	for _, t := range agentDef.Tools {
		rt := &registeredTool{
			def:         t.Def,
			inputSchema: schema.Generate(t.Def.Input),
		}
		tools = append(tools, loop.ToolEntry{
			Name:        t.Name,
			Description: t.Def.Description,
			InputSchema: rt.inputSchema,
			Permission:  permission.LevelReadOnly,
			Concurrent:  t.Def.Concurrent,
			ExecuteFn: func(ctx context.Context, input json.RawMessage) (string, error) {
				return rt.def.call(ctx, input)
			},
		})
	}

	// 精简 executor：只做 tool 并发调度。
	exec := executor.New(executor.Config{
		MaxConcurrency: 10,
	})

	// 保底压缩：只保留 L4(Auto)，80% 触发。去掉 L0(Budget) 和 L2(Micro) 避免提前截断 tool result。
	compCfg := compaction.Config{
		AutoCompactThreshold: 0.8,
		Layers:               []compaction.Layer{compaction.LayerAuto},
	}
	if p.parentApp.config.promptDir != "" {
		compCfg.PromptFile = p.parentApp.config.promptDir + "/" + prompts.Compact
	}
	compMgr := compaction.NewManager(compCfg)
	compMgr.SetProvider(prov)

	// Hooks: 继承父 App 的 hooksMgr（如果有）。
	var hooksRunner loop.HooksRunner
	if p.parentApp.hooksMgr != nil {
		hooksRunner = &hooksRunnerAdapter{mgr: p.parentApp.hooksMgr}
	}

	// Observer: 继承父 App 的 obsRegistry（如果有）。
	var obs observer.Observer
	if p.parentApp.obsRegistry != nil {
		obs = p.parentApp.obsRegistry.Observer()
	}

	return loop.New(loop.Config{
		Provider:     prov,
		Tools:        tools,
		SystemPrompt: agentDef.Instruction,
		MaxTurns:     maxTurns,
		Executor:     exec,
		Compaction:   compMgr,
		Hooks:        hooksRunner,
		Observer:     obs,
	})
}

// runLightweight 运行精简 loop 并收集最终文本。
// 如果 LLM 返回空响应（无工具调用、无文本、无错误），返回错误而非静默空字符串。
func (p *pipeline) runLightweight(ctx context.Context, agentDef *PipelineAgentDef, input string) (string, error) {
	l := p.buildLightweightLoop(agentDef, ctx)

	log.Printf("[pipeline] runLightweight: agent=%s tools=%d input_len=%d", agentDef.Name, len(agentDef.Tools), len(input))

	var finalText string
	var toolStartCount, toolDoneCount, errorCount int
	var lastError error
	for ev := range l.Run(ctx, input) {
		switch ev.Type {
		case loop.EvtTextDelta:
			finalText += ev.Text
		case loop.EvtToolStart:
			toolStartCount++
			log.Printf("[pipeline] runLightweight: agent=%s tool_start=%s", agentDef.Name, ev.ToolName)
		case loop.EvtToolDone:
			toolDoneCount++
			resultPreview := ev.ToolResult
			if len(resultPreview) > 200 {
				resultPreview = resultPreview[:200] + "..."
			}
			log.Printf("[pipeline] runLightweight: agent=%s tool_done=%s result=%s", agentDef.Name, ev.ToolName, resultPreview)
		case loop.EvtError:
			errorCount++
			lastError = ev.Error
			log.Printf("[pipeline] runLightweight: agent=%s error=%v", agentDef.Name, ev.Error)
		}
	}

	log.Printf("[pipeline] runLightweight: agent=%s completed tools_started=%d tools_done=%d errors=%d text_len=%d",
		agentDef.Name, toolStartCount, toolDoneCount, errorCount, len(finalText))
	if len(finalText) > 0 {
		preview := finalText
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		log.Printf("[pipeline] runLightweight: agent=%s text_preview=%s", agentDef.Name, preview)
	}

	// 空响应检测：LLM 既没有调用工具、也没有输出文本、也没有返回错误。
	// 这通常意味着 provider 配置有误（API Key 无效、模型名错误、BaseURL 不通等），
	// LLM API 静默返回空内容。不应当作正常结果静默通过。
	if toolStartCount == 0 && len(finalText) == 0 && errorCount == 0 {
		log.Printf("[pipeline] runLightweight: agent=%s empty response detected (no tools, no text, no errors), likely provider misconfiguration", agentDef.Name)
		return "", fmt.Errorf("agent %q: LLM returned empty response (0 tool calls, 0 text chars) — provider may be misconfigured (check API key, model name, base URL)", agentDef.Name)
	}

	// 如果有 LLM 错误但同时也无任何产出，也返回错误
	if errorCount > 0 && toolStartCount == 0 && len(finalText) == 0 && lastError != nil {
		return "", fmt.Errorf("agent %q: LLM error with no output: %w", agentDef.Name, lastError)
	}

	return finalText, nil
}

// retryMessage 包装重试的消息，携带重试计数。
type retryMessage struct {
	msg        any    // 原始消息内容（用于传给 runWorkerOnce 的 msg type 判断）
	input      string // 拼接了 guidance 的实际输入
	retryCount int    // 当前重试次数
}

// runMultiWorkers 运行并行 worker 消费 message 队列。
func (p *pipeline) runMultiWorkers(ctx context.Context, node *pipelineNodeState) {
	q := p.msgQueues[node.Name]
	if q == nil {
		return
	}

	// 判断是否需要为此节点创建应用层事务。
	needTx := node.Review && p.cfg.TransactionFactory != nil

	var workerWg sync.WaitGroup
	for i := 0; i < node.Concurrency; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-q.ch:
					if !ok {
						return
					}
					// 检查是否是重试消息。
					retryCount := 0
					originalMsg := msg
					if rm, ok := msg.(*retryMessage); ok {
						retryCount = rm.retryCount
						originalMsg = rm.msg
						msg = rm // runWorkerOnce 会检查 retryMessage 类型
					}

					// 如果需要事务，创建并注入到 context。
					var tx Transaction
					workerCtx := ctx
					if needTx {
						tx = p.cfg.TransactionFactory(ctx)
						workerCtx = WithTransaction(ctx, tx)
					}

					result := p.runWorkerOnce(workerCtx, node, msg)
					p.handleResult(node.Name, result, originalMsg, retryCount, tx)
				}
			}
		}()
	}
	workerWg.Wait()
}

// runWorkerOnce 为单条 message 创建精简 agent 并执行。
// 如果节点有 Message 且不是 Message 本身，拼接到任务前面作为上下文。
func (p *pipeline) runWorkerOnce(ctx context.Context, node *pipelineNodeState, msg any) string {
	if node.Agent == nil {
		return ""
	}

	// 处理重试消息：直接使用已拼接好的 input。
	if rm, ok := msg.(*retryMessage); ok {
		result, err := p.runLightweight(ctx, node.Agent, rm.input)
		if err != nil {
			p.recordNodeError(node.Name, err)
			return ""
		}
		return result
	}

	var taskInput string
	if s, ok := msg.(string); ok {
		taskInput = s
	} else {
		data, err := json.Marshal(msg)
		if err != nil {
			taskInput = fmt.Sprintf("%v", msg)
		} else {
			taskInput = string(data)
		}
	}

	// 如果节点有 Message 且当前任务不是 Message 本身，拼接上下文
	input := taskInput
	if node.Message != "" && taskInput != node.Message {
		input = node.Message + "\n" + taskInput
	}

	result, err := p.runLightweight(ctx, node.Agent, input)
	if err != nil {
		p.recordNodeError(node.Name, err)
		return ""
	}
	return result
}

// recordNodeError 记录节点的致命错误并中断 pipeline。
// 当 LLM 返回空响应（provider 配置错误等）时调用，取消 pipeline context，
// 使所有正在运行的节点退出，最终 RunPipeline 返回该错误。
func (p *pipeline) recordNodeError(nodeName string, err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()

	if p.firstError != nil {
		log.Printf("[pipeline] recordNodeError: node=%s error=%v, but firstError already set, skipping", nodeName, err)
		return // 只记录第一个错误
	}

	p.firstError = fmt.Errorf("pipeline node %q failed: %w", nodeName, err)
	log.Printf("[pipeline] FATAL: recordNodeError: node=%s, cancelling entire pipeline. error=%v", nodeName, err)

	// 打印当前所有节点的状态
	for name, node := range p.nodes {
		log.Printf("[pipeline] recordNodeError: node=%s started=%v review=%v", name, node.started, node.Review)
	}
	p.nodesMu.Lock()
	for name, done := range p.nodesDone {
		log.Printf("[pipeline] recordNodeError: nodesDone[%s]=%v reviewDone[%s]=%v", name, done, name, p.reviewDone[name])
	}
	p.nodesMu.Unlock()

	// 取消 pipeline context，通知所有节点退出
	if p.cancelFunc != nil {
		p.cancelFunc()
	}
}

// runSupervisor 运行 supervisor 上帝节点。
// 采用"事件驱动"模式：每次 wait_for_review 返回后启动一轮 loop 做审核，
// 审核完成后继续等待下一个事件。只有 done=true 时才真正退出。
func (p *pipeline) runSupervisor(ctx context.Context) {
	sup := p.cfg.Supervisor
	if sup == nil {
		return
	}

	// 注入 SharedData 到 supervisor context，让 supervisor 工具可以通过 GetPipelineData 获取数据。
	if len(p.cfg.SharedData) > 0 {
		ctx = context.WithValue(ctx, ctxKeySharedData, p.cfg.SharedData)
	}

	prov := sup.Provider
	if prov == nil {
		prov = p.parentApp.provider
	}

	// 创建审核 tools（包含 wait_for_review、approve_result、reject_result）。
	reviewTools := p.newReviewTools()

	// 等待下一个审核事件。
	waitForEvent := func() ReviewEvent {
		select {
		case <-ctx.Done():
			return ReviewEvent{Done: true}
		case event := <-p.reviewSignal:
			return event
		}
	}

	for {
		event := waitForEvent()
		if event.Done {
			log.Printf("[pipeline] supervisor: all nodes done, running finalization")
			// 收尾：让 supervisor 调用 progress_update(enrichment_complete) 通知前端。
			// publish 已由各节点的 OnNodeComplete 回调处理。
			finalApp := New(
				WithProvider(prov),
				WithSystemPrompt(sup.Instruction),
				WithMaxTurns(5),
				WithApprover(AutoApprover()),
			)
			if p.parentApp.obsRegistry != nil {
				finalApp.obsRegistry = p.parentApp.obsRegistry
			}
			finalApp.UseTools(sup.Tools...)
			for ev := range finalApp.Run(ctx, "所有 DAG 节点已完成审核。请执行收尾操作：调用 progress_update(event_type=\"enrichment_complete\") 通知前端。") {
				if ev.Type == EventToolDone {
					log.Printf("[pipeline] supervisor: finalization tool=%s", ev.ToolName)
				}
			}
			log.Printf("[pipeline] supervisor: finalization complete, exiting")
			return
		}

		log.Printf("[pipeline] supervisor: reviewing node=%s, results=%d", event.NodeName, len(event.Results))

		// 保存当前审核的 items，供 reject_result 读取。
		p.pendingMu.Lock()
		p.currentReviewItems[event.NodeName] = event.Results
		p.pendingMu.Unlock()

		// 把审核事件序列化为用户消息，让 LLM 审核并决定 approve/reject。
		reviewData, _ := json.Marshal(event)
		userMsg := fmt.Sprintf("节点 %s 提交了结果供你审核。请用 get_* 工具抽查验证质量，然后调用 approve_result 或 reject_result。\n\n审核数据：%s", event.NodeName, string(reviewData))

		// 每轮审核用独立的 App.Run()（单轮对话）。
		approved := false
		subApp := New(
			WithProvider(prov),
			WithSystemPrompt(sup.Instruction),
			WithMaxTurns(10), // 单轮审核最多 10 次 LLM 调用（wait_for_review + 读取工具 + approve/reject）
			WithApprover(AutoApprover()),
		)
		if p.parentApp.obsRegistry != nil {
			subApp.obsRegistry = p.parentApp.obsRegistry
		}
		subApp.UseTools(sup.Tools...)
		subApp.UseTools(reviewTools...)

		for ev := range subApp.Run(ctx, userMsg) {
			if ev.Type == EventToolDone && ev.ToolName == "approve_result" {
				log.Printf("[pipeline] supervisor: approved node=%s", event.NodeName)
				approved = true
			}
			if ev.Type == EventToolDone && ev.ToolName == "reject_result" {
				log.Printf("[pipeline] supervisor: rejected node=%s", event.NodeName)
				approved = true // reject 也是有结论的
			}
		}

		if !approved {
			log.Printf("[pipeline] supervisor: no approve/reject for node=%s, auto-approving", event.NodeName)
			// 兜底：LLM 没调 approve/reject，自动 approve
			node := p.nodes[event.NodeName]
			if node != nil {
				// Commit 所有事务。
				for _, item := range event.Results {
					if item.tx != nil {
						log.Printf("[pipeline] supervisor: auto-approve committing transaction for node=%s", event.NodeName)
						item.tx.Commit()
					}
				}
				if node.OnTaskResult != nil {
					for _, item := range event.Results {
						node.OnTaskResult(item.rawResult)
					}
				}
				// 关闭 reviewApproved channel，让 worker 可以退出。
				if node.reviewApproved != nil {
					select {
					case <-node.reviewApproved:
					default:
						close(node.reviewApproved)
					}
				}
			}
			p.markReviewDone(event.NodeName)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 6: UsePipeline — App 集成
// ---------------------------------------------------------------------------

// UsePipeline 配置 DAG 流水线。
// 调用后，使用 RunPipeline() 启动 pipeline。
func (a *App) UsePipeline(cfg PipelineConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pipeline = newPipeline(cfg, a)
}

// RunPipeline 启动 DAG 流水线，阻塞直到所有节点完成。
func (a *App) RunPipeline(ctx context.Context) error {
	a.mu.RLock()
	pl := a.pipeline
	a.mu.RUnlock()

	if pl == nil {
		return fmt.Errorf("goagent: 未配置 pipeline，请先调用 UsePipeline()")
	}
	return pl.Run(ctx)
}
