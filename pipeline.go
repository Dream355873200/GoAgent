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
}

// PipelineNode 定义一个 agent 节点。
type PipelineNode struct {
	// Name 节点名（唯一标识，如 "storyboard"）。
	Name string

	// Agent 定义此节点的 agent。
	Agent *PipelineAgentDef

	// Concurrency worker 并发数（默认 1）。
	// Concurrency=1：直接执行，无 message 队列。
	// Concurrency>1：自动创建 message 队列 + N 个 worker。
	Concurrency int

	// DependsOn 依赖的节点名列表。全部完成后才启动此节点。
	DependsOn []string

	// Message 初始 message（Concurrency=1 时使用）。
	Message string

	// Injects 声明此 node 的 agent tool 可以往哪些下游 node 的队列推送。
	// 框架在运行此 node 时，将指定队列注入到 context。
	Injects []string

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

	// OnResult 通过后的回调。
	// Review=false 时，worker 完成后自动调用。
	// Review=true 时，supervisor 批准后调用。
	// result 参数的实际类型由 ResultType 决定，用户需做类型断言。
	OnResult func(result any)

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

	// rawResult 未序列化的原始 result（用于 OnResult 回调）。
	rawResult any
}

// ---------------------------------------------------------------------------
// context key 类型
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyMessageQueues ctxKey = iota
	ctxKeyReviewSignal
	ctxKeyPipeline
)

// injectedQueues 是注入到 context 中的队列集合（受 Injects 限制）。
type injectedQueues struct {
	msgPushers map[string]MessagePusher
}

// ---------------------------------------------------------------------------
// messagePusher — MessagePusher 的 channel 实现
// ---------------------------------------------------------------------------

type messagePusher struct {
	ch       chan any
	closed   bool
	mu       sync.Mutex
	nodeType reflect.Type // 期望的 message 类型（nil 表示 string）
}

func newMessagePusher(size int, msgType reflect.Type) *messagePusher {
	return &messagePusher{
		ch:       make(chan any, size),
		nodeType: msgType,
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
	if !mp.closed {
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
	pendingResults map[string][]ReviewResultItem // 每个节点待审 result
	reviewSignal   chan ReviewEvent              // supervisor 的审核事件 channel
	pendingMu      sync.Mutex                    // 保护 pendingResults

	// DAG 状态
	doneCh     chan struct{} // pipeline 完成信号
	doneOnce   sync.Once
	nodesDone  map[string]bool // 节点完成标记
	nodesMu    sync.Mutex
	reviewDone map[string]bool // Review 节点审核完成标记

	// 父 App（用于创建子 agent）
	parentApp *App

	// hasSupervisor 缓存 Supervisor 是否存在（初始化后不变，避免并发读 cfg.Supervisor）。
	hasSupervisor bool
}

type pipelineNodeState struct {
	PipelineNode
	// 运行时状态
	started bool

	// 由 applyNodeDefaults 从零值推断的 reflect.Type
	msgType    reflect.Type
	resultType reflect.Type
}

// newPipeline 创建 pipeline 实例。
func newPipeline(cfg PipelineConfig, parent *App) *pipeline {
	p := &pipeline{
		cfg:            cfg,
		nodes:          make(map[string]*pipelineNodeState),
		msgQueues:      make(map[string]*messagePusher),
		pendingResults: make(map[string][]ReviewResultItem),
		reviewSignal:   make(chan ReviewEvent, 64),
		doneCh:         make(chan struct{}),
		nodesDone:      make(map[string]bool),
		reviewDone:     make(map[string]bool),
		parentApp:      parent,
		hasSupervisor:  cfg.Supervisor != nil,
	}

	// 初始化节点状态和队列
	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]
		applyNodeDefaults(node)
		state := &pipelineNodeState{PipelineNode: *node}
		applyNodeTypes(node, state)
		p.nodes[node.Name] = state

		// 为 Concurrency>1 的节点创建 message 队列
		if node.Concurrency > 1 {
			p.msgQueues[node.Name] = newMessagePusher(node.QueueSize, state.msgType)
		}
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
// Review=false 时自动通过（直接调用 OnResult 回调）。
// Review=true 时进入待审队列，触发攒批检查。
func (p *pipeline) handleResult(nodeName string, result any, originalMsg any, retryCount int) {
	node, ok := p.nodes[nodeName]
	if !ok {
		return
	}

	if !node.Review {
		// 自动通过，直接调用 OnResult 回调。
		if node.OnResult != nil {
			node.OnResult(result)
		}
		return
	}

	// 进入待审队列。
	item := ReviewResultItem{
		Data:            serializeResult(result),
		OriginalMessage: originalMsg,
		RetryCount:      retryCount,
		rawResult:       result,
	}

	p.pendingMu.Lock()
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
					if node.OnResult != nil {
						node.OnResult(item.rawResult)
					}
				}
			}
		}
	}
}

// markNodeDone 标记节点完成。
// 如果节点有 Review=true，还需要刷新剩余的待审 result。
// 当所有节点完成后，发送 done 信号给 supervisor。
func (p *pipeline) markNodeDone(nodeName string) {
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
			// 无 supervisor：自动通过，直接调用 OnResult。
			for _, item := range items {
				if node.OnResult != nil {
					node.OnResult(item.rawResult)
				}
			}
		}
	} else {
		// Review=false 的节点，完成即审核完成。
		p.nodesMu.Lock()
		p.reviewDone[nodeName] = true
		p.nodesMu.Unlock()
	}

	// 检查是否所有 Review 节点都审核完成。
	p.checkAllReviewDone()
}

// markReviewDone 标记某个 Review 节点的审核全部完成。
func (p *pipeline) markReviewDone(nodeName string) {
	p.nodesMu.Lock()
	p.reviewDone[nodeName] = true
	p.nodesMu.Unlock()
	p.checkAllReviewDone()
}

// checkAllReviewDone 检查是否所有节点（含审核）都已完成。
func (p *pipeline) checkAllReviewDone() {
	p.nodesMu.Lock()
	defer p.nodesMu.Unlock()

	allDone := true
	for name := range p.nodes {
		if !p.nodesDone[name] || !p.reviewDone[name] {
			allDone = false
			break
		}
	}

	if allDone {
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

// newReviewTools 创建审核 tool 列表。
// pipeline 指针通过闭包捕获。
func (p *pipeline) newReviewTools() []NamedTool {
	// 当前待审批次（由 wait_for_review 获取，供 approve/reject 引用）。
	var currentBatch *ReviewEvent
	var batchMu sync.Mutex

	return []NamedTool{
		{
			Name: "wait_for_review",
			Def: ToolDef{
				Description: "阻塞等待审核事件。返回待审 result 列表。当所有 Review 节点完成时返回 {\"done\": true}。",
				Input:       waitForReviewInput{},
				Permission:  ReadOnly,
				Concurrent:  false,
				Execute: func(ctx Context, in waitForReviewInput) (string, error) {
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case event := <-p.reviewSignal:
						batchMu.Lock()
						currentBatch = &event
						batchMu.Unlock()

						data, _ := json.Marshal(event)
						return string(data), nil
					}
				},
			},
		},
		{
			Name: "approve_result",
			Def: ToolDef{
				Description: "批准指定 result。调用该节点的 OnResult 回调。",
				Input:       approveResultInput{},
				Permission:  Normal,
				Concurrent:  false,
				Execute: func(ctx Context, in approveResultInput) (string, error) {
					batchMu.Lock()
					batch := currentBatch
					batchMu.Unlock()

					if batch == nil {
						return "", fmt.Errorf("没有待审批次，请先调用 wait_for_review")
					}
					if in.NodeName != batch.NodeName {
						return "", fmt.Errorf("节点名不匹配：期望 %q，得到 %q", batch.NodeName, in.NodeName)
					}
					if in.Index < 0 || in.Index >= len(batch.Results) {
						return "", fmt.Errorf("索引 %d 超出范围 [0, %d)", in.Index, len(batch.Results))
					}

					item := batch.Results[in.Index]
					node := p.nodes[in.NodeName]
					if node != nil && node.OnResult != nil {
						node.OnResult(item.rawResult)
					}

					// 检查该节点所有 worker 是否已完成且无待审 result。
					p.nodesMu.Lock()
					nodeDone := p.nodesDone[in.NodeName]
					p.nodesMu.Unlock()

					p.pendingMu.Lock()
					noPending := len(p.pendingResults[in.NodeName]) == 0
					p.pendingMu.Unlock()

					if nodeDone && noPending {
						// 检查当前批次是否全部处理完（简化：approve 一个就检查一次）。
						allProcessed := true
						for _, r := range batch.Results {
							_ = r // 此处无法精确追踪每个 item 是否被 approve/reject
						}
						if allProcessed && noPending {
							p.markReviewDone(in.NodeName)
						}
					}

					return fmt.Sprintf("已批准 %s[%d]", in.NodeName, in.Index), nil
				},
			},
		},
		{
			Name: "reject_result",
			Def: ToolDef{
				Description: "拒绝指定 result 并提供修改指导。原始 message + guidance 会重新入队让 worker 重试。达到最大重试次数后自动通过。",
				Input:       rejectResultInput{},
				Permission:  Normal,
				Concurrent:  false,
				Execute: func(ctx Context, in rejectResultInput) (string, error) {
					batchMu.Lock()
					batch := currentBatch
					batchMu.Unlock()

					if batch == nil {
						return "", fmt.Errorf("没有待审批次，请先调用 wait_for_review")
					}
					if in.NodeName != batch.NodeName {
						return "", fmt.Errorf("节点名不匹配：期望 %q，得到 %q", batch.NodeName, in.NodeName)
					}
					if in.Index < 0 || in.Index >= len(batch.Results) {
						return "", fmt.Errorf("索引 %d 超出范围 [0, %d)", in.Index, len(batch.Results))
					}

					item := batch.Results[in.Index]
					node := p.nodes[in.NodeName]

					// 检查重试次数。
					if item.RetryCount >= node.MaxRetries {
						// 达到最大重试次数，自动通过。
						if node.OnResult != nil {
							node.OnResult(item.rawResult)
						}
						return fmt.Sprintf("已达最大重试次数 %d，自动通过 %s[%d]", node.MaxRetries, in.NodeName, in.Index), nil
					}

					// 将原始 message + guidance 重新入队。
					q, ok := p.msgQueues[in.NodeName]
					if !ok || q.isClosed() {
						return "", fmt.Errorf("节点 %q 的 message 队列不存在或已关闭", in.NodeName)
					}

					// 构造重试消息：原始 message + guidance。
					retryMsg := fmt.Sprintf("%v\n\n[审核反馈] %s", item.OriginalMessage, in.Guidance)
					q.Push(retryMsg)

					return fmt.Sprintf("已拒绝 %s[%d]，重试消息已入队（第 %d/%d 次重试）", in.NodeName, in.Index, item.RetryCount+1, node.MaxRetries), nil
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
func (p *pipeline) Run(ctx context.Context) error {
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
			p.runSupervisor(ctx)
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
			p.runNode(ctx, name, doneCh)
		}()
	}

	// 调度器 goroutine。
	go func() {
		started := make(map[string]bool)
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.doneCh:
				return
			case name := <-readyCh:
				if !started[name] {
					started[name] = true
					startNode(name)
				}
			case name := <-doneCh:
				// 节点完成，检查下游是否就绪。
				for _, candidate := range order {
					if started[candidate] {
						continue
					}
					cNode := p.nodes[candidate]
					allDepsDone := true
					for _, dep := range cNode.DependsOn {
						p.nodesMu.Lock()
						done := p.nodesDone[dep]
						p.nodesMu.Unlock()
						if !done {
							allDepsDone = false
							break
						}
					}
					if allDepsDone {
						started[candidate] = true
						startNode(candidate)
					}
				}
				_ = name
			}
		}
	}()

	// 等待 pipeline 完成或 context 取消。
	select {
	case <-ctx.Done():
		// 关闭所有队列，通知 worker 退出。
		for _, q := range p.msgQueues {
			q.Close()
		}
		wg.Wait()
		return ctx.Err()
	case <-p.doneCh:
		wg.Wait()
		return nil
	}
}

// runNode 运行单个 DAG 节点。
func (p *pipeline) runNode(ctx context.Context, nodeName string, doneCh chan<- string) {
	node := p.nodes[nodeName]

	// 注入下游队列到 context。
	nodeCtx := p.injectQueuesForNode(ctx, nodeName)

	if node.Concurrency == 1 {
		// 单 worker 直接执行。
		result := p.runSingleWorker(nodeCtx, node)
		p.handleResult(nodeName, result, node.Message, 0)
	} else {
		// 多 worker 并行消费 message 队列。
		p.runMultiWorkers(nodeCtx, node)
	}

	// 关闭此节点 Injects 的下游队列（上游完成 → 通知下游不再有新任务）。
	for _, target := range node.Injects {
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

	// 可组合压缩：只启用 L0(Budget) + L2(Micro) + L4(Auto)。
	compCfg := compaction.Config{
		MaxResultSize:        50_000,
		AutoCompactThreshold: 0.8,
		Layers:               []compaction.Layer{compaction.LayerBudget, compaction.LayerMicro, compaction.LayerAuto},
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
func (p *pipeline) runLightweight(ctx context.Context, agentDef *PipelineAgentDef, input string) string {
	l := p.buildLightweightLoop(agentDef, ctx)

	var finalText string
	for ev := range l.Run(ctx, input) {
		if ev.Type == loop.EvtTextDelta {
			finalText += ev.Text
		}
	}
	return finalText
}

// runSingleWorker 运行 Concurrency=1 的节点。
func (p *pipeline) runSingleWorker(ctx context.Context, node *pipelineNodeState) string {
	if node.Agent == nil {
		return ""
	}
	return p.runLightweight(ctx, node.Agent, node.Message)
}

// runMultiWorkers 运行 Concurrency>1 的并行 worker。
func (p *pipeline) runMultiWorkers(ctx context.Context, node *pipelineNodeState) {
	q := p.msgQueues[node.Name]
	if q == nil {
		return
	}

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
					result := p.runWorkerOnce(ctx, node, msg)
					p.handleResult(node.Name, result, msg, 0)
				}
			}
		}()
	}
	workerWg.Wait()
}

// runWorkerOnce 为单条 message 创建精简 agent 并执行。
func (p *pipeline) runWorkerOnce(ctx context.Context, node *pipelineNodeState, msg any) string {
	if node.Agent == nil {
		return ""
	}
	input := fmt.Sprintf("%v", msg)
	return p.runLightweight(ctx, node.Agent, input)
}

// runSupervisor 运行 supervisor 上帝节点。
func (p *pipeline) runSupervisor(ctx context.Context) {
	sup := p.cfg.Supervisor
	if sup == nil {
		return
	}

	prov := sup.Provider
	if prov == nil {
		prov = p.parentApp.provider
	}

	maxTurns := sup.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 1000
	}

	subApp := New(
		WithProvider(prov),
		WithSystemPrompt(sup.Instruction),
		WithMaxTurns(maxTurns),
		WithApprover(AutoApprover()),
	)

	// 注册用户配置的 tools。
	subApp.UseTools(sup.Tools...)

	// 注入框架审核 tools。
	subApp.UseTools(p.newReviewTools()...)

	// supervisor 启动后立刻进入审核循环。
	for ev := range subApp.Run(ctx, "Pipeline 已启动。请调用 wait_for_review 等待审核事件。") {
		_ = ev // supervisor 事件目前不对外暴露。
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
