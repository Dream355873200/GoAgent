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
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/internal/safename"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/prompts"
	"github.com/Dream355873200/GoAgent/provider"
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

	// OnEvent 节点运行事件的透出回调（可选）。
	// pipeline 内部原本只消费 TextDelta/ToolStart/ToolDone/Error，
	// Progress（含 429 状态行的 StatusKey）/Thinking 等事件被静默丢弃——
	// 嵌入方无法把节点的限流等待/思考过程展示给用户。设置本回调后，
	// 每个节点 worker 的 loop 事件（转换后的 PipelineEvent）都会透出。
	OnEvent func(ev PipelineEvent)

	// TraceDir 节点轨迹目录（可选）。设置后每个节点 worker 的运行轨迹
	// （思考/工具调用/文本产出）以 JSONL 追加写入 <TraceDir>/<node>.jsonl，
	// 供宿主回看节点的完整工作过程（像主 agent 时间线一样）。
	TraceDir string

	// SessionID 会话标识（可选）。设置后 RunPipeline 会像 App.run 一样
	// 注入会话上下文（SessionID + WithSessionWorkDir 解析的工作目录）——
	// 此前 pipeline 节点工具的 Context.WorkDir/SessionID 恒为空，文件/Bash
	// 工具只能落在进程 cwd。业务层按会话隔离 pipeline 工作目录时设置。
	// 未设置时沿用 ctx 中已有的会话上下文。
	SessionID string

	// TrustedTools 为 true 时，节点与 supervisor 的工具调用不经权限门、
	// plan 模式与中间件检查（旧版行为）。默认 false：在某次 agent 运行之中
	// 启动的 pipeline（例如宿主工具内部调用 RunPipeline）继承该运行的权限
	// 门，节点工具按其声明的权限级别检查；宿主代码在任何运行之外直接启动
	// 的 pipeline 本就不经权限门。只在节点工具集由宿主固定、且宿主为其
	// 安全负责时设置。动态 pipeline（create_pipeline）忽略此字段。
	TrustedTools bool

	// FinalizePrompt 非空时，全部节点完成后 supervisor 以它为输入再运行一轮
	// 收尾（只带 Supervisor.Tools），例如通知前端流水线已结束。默认不运行
	// 收尾轮。
	FinalizePrompt string
}

// PipelineEvent 是 OnEvent 回调透出的节点运行事件。
type PipelineEvent struct {
	// Node 事件来源节点名。
	Node string

	// Type 事件类型："progress" / "thinking" / "error"。
	// progress: StatusKey 非空时是状态行原地更新语义（429 倒计时等），Text 空 = 清除。
	// thinking: 思考模型的推理过程增量（delta.reasoning_content）。
	// error:    LLM/工具错误（已在内部处理，此为透出供宿主记录）。
	Type string

	// Text progress/thinking/node_output 的内容。
	Text string
	// StatusKey 仅 progress：状态行键（非空 = 原地更新语义）。
	StatusKey string
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
	//
	// **只有没有上游生产者的节点，才会把 Message 自动 Push 成初始任务。**
	// 若有其他节点的 Injects 指向本节点（即真实任务由上游推入队列），Message 就纯粹是
	// 公共前缀，不会单独成为一条任务 —— 否则 worker 会多消费一条「只有上下文、没有
	// 任务数据」的伪任务并白跑一次 LLM（实测 enricher 因此回「我还没收到具体的
	// 任务数据」，storyboard 则拿着 16k 剧本上下文空转一轮去试探自己该做什么）。
	Message string

	// MessageFunc 惰性求值的 Message。非 nil 时在**节点真正被调度那一刻**
	// （DependsOn 全部满足、worker 启动之前）调用，返回值覆盖 Message。
	//
	// 为什么需要它：Message 是在构造 PipelineConfig 时就固化的字符串，而它的内容
	// 往往依赖上游节点的产出。DependsOn 只推迟节点的**执行**，不推迟 Message 的
	// **求值** —— 典型翻车场景：下游节点的 Message 里注入「当前已有的资产清单」，
	// 可构造 pipeline 时上游还没跑，注入进去的是一份空清单，于是下游只能在 prompt 里
	// 被告知「注入的列表可能是空的，你自己去调 list_* 重查」，每个 worker 白花一轮
	// LLM 调用去拿本该直接给它的数据。
	//
	// MessageFunc 求值发生在依赖满足之后，拿到的就是上游产出后的真实状态。
	// 每个节点只求值一次（不是每条任务一次），因此可以放心做数据库查询。
	MessageFunc func(ctx context.Context) string

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

	// RetryPolicy 审核打回时如何处置本轮已产出的数据（默认 RetryRollback）。
	// 仅 Review=true 且配置了 TransactionFactory 时生效。
	RetryPolicy RetryPolicy

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

	// Sandbox 节点级沙箱策略覆盖（可选）。
	// 非 nil 时本节点 runNode 期间单独 Enter 一个沙箱会话（策略为本字段值），
	// 覆盖 run 级会话；nil = 继承 RunPipeline 创建的 run 级会话。
	// 注意：App 未配置 WithSandbox 时本字段无效（无沙箱工厂可用）。
	Sandbox *Policy
}

// RetryPolicy 决定审核打回（reject_result）时如何处置该轮已产出的数据。
//
// 选哪个取决于**节点产出的性质**，不该全局一刀切：
//   - 产出是「整体结构」、必须自洽的（如分集划分：边界是一个整体决策，补一集没有意义）
//     → RetryRollback，撤销后整体重做。
//   - 产出是「可累积集合」、条目互相独立的（如资产提取、世界观条目）
//     → RetryAmend。审核意见通常是「漏了 X」，用整批销毁去补 X 是负期望重试：
//     LLM 重跑不确定，这次补上 X 可能漏掉 Y，重试次数越多结果越可能更差。
//     前提是节点的 tool 幂等（按业务键查重，已存在则复用/补齐）。
type RetryPolicy int

const (
	// RetryRollback 打回时执行 Transaction.Rollback（补偿撤销本轮全部写操作），
	// 然后用「原始输入 + guidance」从零重跑。默认值，保持历史行为。
	RetryRollback RetryPolicy = iota

	// RetryAmend 打回时执行 Transaction.Commit（保留本轮已产出），
	// 只把 guidance 推回重试，让节点在已有数据上增量补齐。
	// 要求节点的写操作幂等，否则重试会产生重复数据。
	RetryAmend
)

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

	// Done 保留字段（兼容旧版）：框架不再发送 Done 事件，supervisor 在全部
	// 节点完成后自行退出。
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
	// txID 是 tx 在 pipeline 未处置事务表中的编号（0 = 无事务）。
	txID int64
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
// messagePusher — MessagePusher 的队列实现
// ---------------------------------------------------------------------------

// messagePusher 是节点的任务队列：Push 写入无界缓冲区、从不阻塞，由一个
// 泵协程把消息按序送进 ch 供 worker 消费；泵协程是 ch 唯一的关闭者。
// 此前 Push 在持锁状态下向有界 channel 阻塞发送：下游尚未启动（隐式依赖
// 让它等上游结束）而上游推送超过缓冲容量时永久死锁，取消也解不开（Close
// 要抢同一把锁）。
type messagePusher struct {
	ch          chan any
	mu          sync.Mutex
	buf         []any
	inflight    int          // 泵协程已取出、正阻塞等待送入 ch 的消息数（0 或 1）
	accepted    int          // 累计接受的消息数
	closed      bool         // 不再接受新消息；缓冲排空后泵协程关闭 ch
	nodeType    reflect.Type // 期望的 message 类型（nil 表示 string）
	closeCount  int          // 需要多少个 Close 调用才真正关闭
	closedTimes int          // 已被 Close 的次数
	onClose     func()       // 每次 Close 后在锁外调用（审核节点据此判断能否自关闭）
	wake        chan struct{}
	abortCh     chan struct{}
	abortOnce   sync.Once
}

func newMessagePusher(size int, msgType reflect.Type, expectedCloseCount int) *messagePusher {
	if expectedCloseCount <= 0 {
		expectedCloseCount = 1
	}
	mp := &messagePusher{
		ch:         make(chan any, size),
		nodeType:   msgType,
		closeCount: expectedCloseCount,
		wake:       make(chan struct{}, 1),
		abortCh:    make(chan struct{}),
	}
	go mp.pump()
	return mp
}

func (mp *messagePusher) Push(msg any) {
	mp.mu.Lock()
	if mp.closed {
		mp.mu.Unlock()
		return
	}
	mp.buf = append(mp.buf, msg)
	mp.accepted++
	mp.mu.Unlock()
	mp.signal()
}

func (mp *messagePusher) Close() {
	mp.mu.Lock()
	mp.closedTimes++
	if mp.closedTimes >= mp.closeCount {
		mp.closed = true
	}
	hook := mp.onClose
	mp.mu.Unlock()
	mp.signal()
	if hook != nil {
		hook()
	}
}

// abort 立即关闭队列并丢弃未投递的消息（pipeline 取消或结束时清理）。
func (mp *messagePusher) abort() {
	mp.mu.Lock()
	mp.closed = true
	mp.buf = nil
	mp.mu.Unlock()
	mp.abortOnce.Do(func() { close(mp.abortCh) })
}

func (mp *messagePusher) signal() {
	select {
	case mp.wake <- struct{}{}:
	default:
	}
}

func (mp *messagePusher) pump() {
	defer close(mp.ch)
	for {
		mp.mu.Lock()
		if len(mp.buf) > 0 {
			item := mp.buf[0]
			mp.buf[0] = nil
			mp.buf = mp.buf[1:]
			// ch 有空位时在锁内送入，len() 的计数始终准确。
			select {
			case mp.ch <- item:
				mp.mu.Unlock()
				continue
			default:
			}
			mp.inflight = 1
			mp.mu.Unlock()
			select {
			case mp.ch <- item:
			case <-mp.abortCh:
				return
			}
			mp.mu.Lock()
			mp.inflight = 0
			mp.mu.Unlock()
			continue
		}
		closed := mp.closed
		mp.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-mp.wake:
		case <-mp.abortCh:
			return
		}
	}
}

func (mp *messagePusher) isClosed() bool {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return mp.closed
}

// len 返回尚未被 worker 取走的消息数。
func (mp *messagePusher) len() int {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return len(mp.buf) + mp.inflight + len(mp.ch)
}

// acceptedCount 返回累计接受的消息数。
func (mp *messagePusher) acceptedCount() int {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return mp.accepted
}

// othersClosed 报告除节点自身之外的关闭方（上游生产者）是否都已 Close。
func (mp *messagePusher) othersClosed() bool {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return mp.closedTimes >= mp.closeCount-1
}

// ---------------------------------------------------------------------------
// pipeline 内部状态
// ---------------------------------------------------------------------------

type pipeline struct {
	cfg   PipelineConfig
	nodes map[string]*pipelineNodeState

	// 拓扑排序后的节点执行顺序
	topoOrder []string

	// 队列
	msgQueues map[string]*messagePusher // 每个节点的 message 队列

	// 审核状态，均受 pendingMu 保护：待送审的结果、正在审核的批次、尚未
	// 处置的事务，以及审核节点的未完成计数（pipelineNodeState.processed/inReview）。
	pendingMu      sync.Mutex
	pendingResults map[string][]ReviewResultItem
	rounds         map[string]*reviewRound
	openTx         map[int64]Transaction
	nextTxID       int64
	reviewSignal   chan ReviewEvent // supervisor 的审核事件 channel

	// DAG 状态
	doneCh     chan struct{} // 全部节点完成
	doneOnce   sync.Once
	nodesMu    sync.Mutex
	nodesDone  map[string]bool
	nodeDoneCh chan string // 节点结束通知，调度器据此启动下游

	traceMu sync.Mutex // 节点轨迹文件追加串行化（多 worker 并发）
	cbMu    sync.Mutex // 串行化宿主回调（OnTaskResult / OnNodeComplete / OnEvent / MessageFunc）

	// 第一个致命错误被记录，并取消整个 pipeline。
	firstError error
	errMu      sync.Mutex
	cancelFunc context.CancelFunc

	// 父 App（provider、日志、hooks、观测等的来源；可为 nil）
	parentApp *App

	// 库内日志：来自 parentApp（WithLogger 注入），无 parent 时 slog.Default()。
	logger *slog.Logger

	// hasSupervisor 缓存 Supervisor 是否存在（初始化后不变）。
	hasSupervisor bool

	// scope 是节点与 supervisor 循环继承的作用域（检查链与观测出口，见
	// loop.Scope）；nil 表示宿主信任，工具调用不做检查。
	scope *loop.Scope

	// runCtx 是本次运行的 ctx（Run 中设置）：投递审核事件时据此放弃发送，
	// 避免 supervisor 退出后发送方永久阻塞。
	runCtx context.Context
	// finished 在 Run 返回前置位：之后的宿主回调一律丢弃。
	finished atomic.Bool
	// ran 防止同一个实例被运行两次（运行状态不可复用）。
	ran atomic.Bool
}

type pipelineNodeState struct {
	PipelineNode
	// started 只由调度器（Run 所在的 goroutine）读写。
	started bool

	// nodeCtx 节点执行时的 context（包含注入的 queues、SharedData 等）。
	nodeCtx context.Context

	// 审核节点的未完成工作量（受 pipeline.pendingMu 保护）：队列累计接受的
	// 任务数 − processed + inReview。降到 0 且上游都已关闭队列时，节点关闭
	// 自己的队列让 worker 退出——在此之前被打回的结果还要重新入队。
	processed int // worker 已处理完的任务数（结果已入审，或执行失败）
	inReview  int // 已入审、尚未处置（批准/打回）的结果数

	armed      atomic.Bool // 节点已启动且初始任务已入队，可以判断是否自关闭
	selfClosed atomic.Bool

	// 由 applyNodeTypes 从零值推断的 reflect.Type
	msgType    reflect.Type
	resultType reflect.Type
}

// reviewRound 是 supervisor 正在审核的一个批次及其中每条结果的处置状态。
type reviewRound struct {
	items    []ReviewResultItem
	disposed []bool
}

// newPipeline 创建 pipeline 实例。
func newPipeline(cfg PipelineConfig, parent *App) *pipeline {
	// 节点切片复制一份：下面会填默认值，不能改到调用方（以及 UsePipeline
	// 保存的配置）手里的数组。
	cfg.Nodes = append([]PipelineNode(nil), cfg.Nodes...)
	p := &pipeline{
		cfg:            cfg,
		nodes:          make(map[string]*pipelineNodeState),
		msgQueues:      make(map[string]*messagePusher),
		pendingResults: make(map[string][]ReviewResultItem),
		rounds:         make(map[string]*reviewRound),
		openTx:         make(map[int64]Transaction),
		reviewSignal:   make(chan ReviewEvent, 64),
		doneCh:         make(chan struct{}),
		nodesDone:      make(map[string]bool),
		nodeDoneCh:     make(chan string, len(cfg.Nodes)),
		parentApp:      parent,
		hasSupervisor:  cfg.Supervisor != nil,
		logger:         parentLogger(parent),
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
		p.nodes[node.Name] = state

		q := newMessagePusher(node.QueueSize, state.msgType, closeCounts[node.Name])
		if node.Review {
			q.onClose = func() { p.maybeSelfClose(state) }
		}
		p.msgQueues[node.Name] = q
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
// 配置校验与拓扑排序
// ---------------------------------------------------------------------------

// validate 校验节点配置并返回拓扑序。
func (p *pipeline) validate() ([]string, error) {
	if len(p.cfg.Nodes) == 0 {
		return nil, fmt.Errorf("pipeline: 没有任何节点")
	}
	seen := make(map[string]bool, len(p.cfg.Nodes))
	for _, n := range p.cfg.Nodes {
		if n.Name == "" {
			return nil, fmt.Errorf("pipeline: 存在名称为空的节点")
		}
		if seen[n.Name] {
			return nil, fmt.Errorf("pipeline: 节点名 %q 重复", n.Name)
		}
		seen[n.Name] = true
	}
	return p.topoSort()
}

// topoSort 对节点进行拓扑排序。返回排序后的节点名列表。
// 如果存在循环依赖（含依赖与队列关系组成的环），返回错误。
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
	sort.Strings(queue)

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

	if err := p.checkQueueCycles(); err != nil {
		return nil, err
	}
	return order, nil
}

// checkQueueCycles 检查"依赖边 + 队列边"组成的图是否有环。队列边是
// 生产者 → 消费者（Injects / CloseQueues）：消费者的队列要等生产者结束才
// 关闭，生产者若又（直接或间接）等消费者结束，两者都永远无法结束——包括
// 节点向自己的队列推送（自注入）。
func (p *pipeline) checkQueueCycles() error {
	edges := make(map[string][]string)
	for name, node := range p.nodes {
		for _, dep := range node.DependsOn {
			edges[dep] = append(edges[dep], name)
		}
		targets := append(append([]string(nil), node.Injects...), node.CloseQueues...)
		for _, t := range targets {
			if t == name {
				return fmt.Errorf("pipeline: 节点 %q 向自己的队列推送任务，队列永远无法关闭", name)
			}
			if _, ok := p.nodes[t]; !ok {
				return fmt.Errorf("pipeline: 节点 %q 的 Injects/CloseQueues 指向不存在的节点 %q", name, t)
			}
			edges[name] = append(edges[name], t)
		}
	}
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(p.nodes))
	var visit func(n string, path []string) error
	visit = func(n string, path []string) error {
		color[n] = gray
		path = append(path, n)
		for _, m := range edges[n] {
			switch color[m] {
			case gray:
				return fmt.Errorf("pipeline: 依赖与队列关系成环（%s → %s），流水线会死锁", strings.Join(path, " → "), m)
			case white:
				if err := visit(m, path); err != nil {
					return err
				}
			}
		}
		color[n] = black
		return nil
	}
	names := make([]string, 0, len(p.nodes))
	for name := range p.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if err := visit(n, nil); err != nil {
				return err
			}
		}
	}
	return nil
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

// hasUpstreamProducer 判断是否有别的节点声明往 nodeName 的 message 队列推任务
// （即 nodeName 是队列驱动的 worker 节点，而非自带唯一任务的单任务节点）。
//
// 用途见 runNode 里的 Message Push 判断：队列驱动节点的 Message 只是每条任务的
// 公共前缀，不该自己成为一条任务。
//
// 注意这里判断的是**声明**而非实际推送：上游声明了 Injects 但一条都没推时（如剧本
// 没拆出分集），本节点队列为空、worker 空转退出，这是正确行为 —— 没有任务就是没活干，
// 不该拿 Message 硬凑一条。
func (p *pipeline) hasUpstreamProducer(nodeName string) bool {
	for name, node := range p.nodes {
		if name == nodeName {
			continue // 自己 Injects 自己不算上游
		}
		for _, target := range node.Injects {
			if target == nodeName {
				return true
			}
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
		slog.Warn(fmt.Sprintf("serializeResult failed: %v", err))
		return "{}"
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// context 注入 — injectQueuesForNode
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
// 宿主回调
// ---------------------------------------------------------------------------

// callback 串行调用宿主回调：多个 worker 与 supervisor 并发产生结果，回调
// 内部无需自行加锁。回调 panic 记为节点错误（终止 pipeline），而不是让整个
// 进程崩溃。
func (p *pipeline) callback(nodeName string, fn func()) {
	if p.finished.Load() {
		return
	}
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			p.recordNodeError(nodeName, fmt.Errorf("回调 panic: %v", r))
		}
	}()
	fn()
}

// emitEvent 透出节点运行事件到 PipelineConfig.OnEvent（未设置时 no-op）。
// 事件是旁路观测：回调 panic 只记日志，不影响流水线。
func (p *pipeline) emitEvent(node, typ, text, statusKey string) {
	if p.cfg.OnEvent == nil || p.finished.Load() {
		return
	}
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			p.logger.Warn("pipeline: OnEvent 回调 panic", "node", node, "panic", r)
		}
	}()
	p.cfg.OnEvent(PipelineEvent{Node: node, Type: typ, Text: text, StatusKey: statusKey})
}

// ---------------------------------------------------------------------------
// 结果处理 + 自动攒批触发审核
// ---------------------------------------------------------------------------

// handleResult 处理 worker 产出的 result。
// Review=false 时自动通过（直接调用 OnTaskResult 回调）。
// Review=true 时进入待审队列，触发攒批检查。
func (p *pipeline) handleResult(nodeName string, result any, originalMsg any, retryCount int, tx Transaction) {
	node, ok := p.nodes[nodeName]
	if !ok {
		return
	}
	p.logger.Info(fmt.Sprintf("handleResult: node=%s review=%v retry=%d has_tx=%v", nodeName, node.Review, retryCount, tx != nil))

	if !node.Review {
		if node.OnTaskResult != nil {
			p.callback(nodeName, func() { node.OnTaskResult(result) })
		}
		p.pendingMu.Lock()
		node.processed++
		p.pendingMu.Unlock()
		return
	}

	item := ReviewResultItem{
		Data:            serializeResult(result),
		OriginalMessage: originalMsg,
		RetryCount:      retryCount,
		rawResult:       result,
		tx:              tx,
	}
	p.pendingMu.Lock()
	if tx != nil {
		p.nextTxID++
		item.txID = p.nextTxID
		p.openTx[item.txID] = tx
	}
	pending := append(p.pendingResults[nodeName], item)
	for i := range pending {
		pending[i].Index = i
	}
	p.pendingResults[nodeName] = pending
	node.inReview++
	node.processed++
	count := len(pending)
	p.pendingMu.Unlock()

	p.checkReviewTrigger(nodeName, count)
}

// checkReviewTrigger 检查是否应触发审核。
// 条件：攒够 ReviewBatch，或 message 队列已空（不再等凑满一批）。
func (p *pipeline) checkReviewTrigger(nodeName string, pendingCount int) {
	node := p.nodes[nodeName]

	queueEmpty := true
	if q, ok := p.msgQueues[nodeName]; ok {
		queueEmpty = q.isClosed() || q.len() == 0
	}
	if pendingCount < node.ReviewBatch && !(pendingCount > 0 && queueEmpty) {
		return
	}

	p.pendingMu.Lock()
	items := p.pendingResults[nodeName]
	p.pendingResults[nodeName] = nil
	p.pendingMu.Unlock()
	if len(items) == 0 {
		return
	}

	if p.hasSupervisor {
		p.logger.Info(fmt.Sprintf("checkReviewTrigger: node=%s 送审 %d 条", nodeName, len(items)))
		p.sendReview(ReviewEvent{NodeName: nodeName, Results: items})
		return
	}
	// 无 supervisor：自动通过。
	for _, item := range items {
		p.approveItem(node, item)
	}
}

// sendReview 向 supervisor 投递审核事件；运行结束（ctx 取消）后放弃。
func (p *pipeline) sendReview(ev ReviewEvent) {
	ctx := p.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case p.reviewSignal <- ev:
	case <-ctx.Done():
	}
}

// markNodeDone 标记节点完成（全部 worker 已退出；审核节点此时所有结果都已
// 处置）。OnNodeComplete 先于完成标记执行，下游节点的 MessageFunc 能看到
// 回调产生的状态。
func (p *pipeline) markNodeDone(nodeName string) {
	p.logger.Info(fmt.Sprintf("markNodeDone: node=%s", nodeName))
	if node := p.nodes[nodeName]; node != nil && node.OnNodeComplete != nil {
		p.callback(nodeName, node.OnNodeComplete)
	}
	p.nodesMu.Lock()
	p.nodesDone[nodeName] = true
	p.nodesMu.Unlock()
	p.checkAllDone()
}

// checkAllDone 全部节点完成时关闭 doneCh（只关一次）。
func (p *pipeline) checkAllDone() {
	p.nodesMu.Lock()
	allDone := true
	for name := range p.nodes {
		if !p.nodesDone[name] {
			allDone = false
			break
		}
	}
	p.nodesMu.Unlock()
	if allDone {
		p.doneOnce.Do(func() {
			p.logger.Info("pipeline: 全部节点完成")
			close(p.doneCh)
		})
	}
}

// depsDone 报告节点的 DependsOn 是否全部完成（审核节点完成即意味着其结果
// 都已处置）。
func (p *pipeline) depsDone(node *pipelineNodeState) bool {
	p.nodesMu.Lock()
	defer p.nodesMu.Unlock()
	for _, dep := range node.DependsOn {
		if !p.nodesDone[dep] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 审核处置
// ---------------------------------------------------------------------------

// beginRound 登记 supervisor 正在审核的批次。
func (p *pipeline) beginRound(ev ReviewEvent) {
	p.pendingMu.Lock()
	p.rounds[ev.NodeName] = &reviewRound{items: ev.Results, disposed: make([]bool, len(ev.Results))}
	p.pendingMu.Unlock()
}

// claimItem 取出当前批次中尚未处置的第 idx 条结果，并标记为已处置。
func (p *pipeline) claimItem(nodeName string, idx int) (*pipelineNodeState, ReviewResultItem, error) {
	node := p.nodes[nodeName]
	if node == nil {
		return nil, ReviewResultItem{}, fmt.Errorf("未知节点: %s", nodeName)
	}
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	r := p.rounds[nodeName]
	if r == nil {
		return nil, ReviewResultItem{}, fmt.Errorf("节点 %s 当前没有待审核的结果", nodeName)
	}
	if idx < 0 || idx >= len(r.items) {
		return nil, ReviewResultItem{}, fmt.Errorf("无效的索引 %d：本批次共 %d 条，有效范围 0..%d", idx, len(r.items), len(r.items)-1)
	}
	if r.disposed[idx] {
		return nil, ReviewResultItem{}, fmt.Errorf("%s[%d] 已经处置过", nodeName, idx)
	}
	r.disposed[idx] = true
	return node, r.items[idx], nil
}

// endRound 结束批次，返回其中没有被处置的结果。
func (p *pipeline) endRound(nodeName string) []ReviewResultItem {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	r := p.rounds[nodeName]
	delete(p.rounds, nodeName)
	if r == nil {
		return nil
	}
	var rest []ReviewResultItem
	for i, item := range r.items {
		if !r.disposed[i] {
			rest = append(rest, item)
		}
	}
	return rest
}

// approveItem 批准一条结果：提交事务，把结果交给 OnTaskResult。
func (p *pipeline) approveItem(node *pipelineNodeState, item ReviewResultItem) {
	if item.tx != nil {
		item.tx.Commit()
		p.forgetTx(item.txID)
	}
	if node.OnTaskResult != nil {
		p.callback(node.Name, func() { node.OnTaskResult(item.rawResult) })
	}
	p.itemDisposed(node)
}

// rejectItem 打回一条结果：按 RetryPolicy 处置本轮产出，把原始任务连同审核
// 意见重新推回节点队列。重试次数用尽时改为批准。返回给模型的处置说明。
func (p *pipeline) rejectItem(node *pipelineNodeState, item ReviewResultItem, idx int, guidance string) string {
	// 先判重试次数，再处置事务：重试用尽就是放行，放行必须 Commit（先
	// Rollback 再放行会留下一个数据已经不存在的"通过"结果）。
	if item.RetryCount >= node.MaxRetries {
		p.logger.Info(fmt.Sprintf("reject_result: node=%s index=%d max_retries=%d reached, auto-approving", node.Name, idx, node.MaxRetries))
		p.approveItem(node, item)
		return fmt.Sprintf("已达最大重试次数 (%d)，自动放行 %s[%d]", node.MaxRetries, node.Name, idx)
	}

	amend := node.RetryPolicy == RetryAmend
	if item.tx != nil {
		if amend {
			item.tx.Commit()
		} else if err := item.tx.Rollback(context.Background()); err != nil {
			p.logger.Warn(fmt.Sprintf("reject_result: node=%s index=%d rollback error: %v", node.Name, idx, err))
		}
		p.forgetTx(item.txID)
	}

	// 重试输入：原始任务 + 上一轮产出的去向 + 审核意见。必须告知产出去向，
	// 否则 agent 无法判断该「补齐」还是「重建」。
	var originalInput string
	if s, ok := item.OriginalMessage.(string); ok {
		originalInput = s
	} else {
		data, _ := json.Marshal(item.OriginalMessage)
		originalInput = string(data)
	}
	fullInput := originalInput
	if node.Message != "" && originalInput != node.Message {
		fullInput = node.Message + "\n" + originalInput
	}
	if amend {
		fullInput += "\n\n[重要] 你上一轮已创建的数据**已保留**，请勿重复创建；只需针对下面的反馈补齐缺失或修正问题项。"
	} else {
		fullInput += "\n\n[重要] 你上一轮创建的数据**已全部撤销**，请按下面的反馈**从头完整重做**，不要只处理反馈中提到的项。"
	}
	fullInput += "\n\n[审核反馈，请根据以下指导修正] " + guidance

	// 先重新入队、再计为已处置：未完成计数始终不为 0，队列不会在两步之间被关闭。
	p.msgQueues[node.Name].Push(&retryMessage{
		msg:        item.OriginalMessage,
		input:      fullInput,
		retryCount: item.RetryCount + 1,
	})
	p.itemDisposed(node)

	disposition := "回滚"
	if amend {
		disposition = "保留已产出"
	}
	p.logger.Info(fmt.Sprintf("reject_result: node=%s index=%d retry=%d %s and pushed to queue", node.Name, idx, item.RetryCount+1, disposition))
	return fmt.Sprintf("已将 %s[%d] %s并退回重试（第 %d/%d 次）", node.Name, idx, disposition, item.RetryCount+1, node.MaxRetries)
}

// itemDisposed 一条结果处置完毕：更新未完成计数并检查节点能否收尾。
func (p *pipeline) itemDisposed(node *pipelineNodeState) {
	p.pendingMu.Lock()
	node.inReview--
	p.pendingMu.Unlock()
	p.maybeSelfClose(node)
}

// maybeSelfClose 审核节点在上游都已关闭队列、且所有任务都已处理并处置后，
// 关闭自己的队列让 worker 退出。此前第一次批准就关闭队列：之后被打回的
// 结果重新入队时队列已关，任务静默丢失；而下游在其余结果还在审核时就已启动。
func (p *pipeline) maybeSelfClose(node *pipelineNodeState) {
	if !node.Review || !node.armed.Load() || node.selfClosed.Load() {
		return
	}
	q := p.msgQueues[node.Name]
	if q == nil || !q.othersClosed() {
		return
	}
	p.pendingMu.Lock()
	unfinished := q.acceptedCount() - node.processed + node.inReview
	p.pendingMu.Unlock()
	if unfinished > 0 {
		return
	}
	if node.selfClosed.CompareAndSwap(false, true) {
		q.Close()
	}
}

func (p *pipeline) forgetTx(id int64) {
	if id == 0 {
		return
	}
	p.pendingMu.Lock()
	delete(p.openTx, id)
	p.pendingMu.Unlock()
}

// rollbackOpenTx 回滚运行结束时仍未处置的事务（取消或失败时留在审核中的
// 结果）：只有被批准的结果才生效。
func (p *pipeline) rollbackOpenTx() {
	p.pendingMu.Lock()
	txs := p.openTx
	p.openTx = make(map[int64]Transaction)
	p.pendingMu.Unlock()
	for id, tx := range txs {
		if err := tx.Rollback(context.Background()); err != nil {
			p.logger.Warn("pipeline: 回滚未处置的事务失败", "tx", id, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// 框架自动注入的审核 tool
// ---------------------------------------------------------------------------

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

// newReviewTools 创建审核工具（approve_result / reject_result）。审核是框架
// 内部的流程控制，工具按只读级别登记，不触发用户审批。
func (p *pipeline) newReviewTools() []NamedTool {
	return []NamedTool{
		{
			Name: "approve_result",
			Def: ToolDef{
				Description: "审核通过：批准节点本批次中第 index 条结果，使其生效。" +
					"本批次中你没有处置（批准或打回）的结果会在本轮审核结束时自动批准。",
				Input:      approveResultInput{},
				Permission: ReadOnly,
				Execute: func(ctx Context, in approveResultInput) (string, error) {
					node, item, err := p.claimItem(in.NodeName, in.Index)
					if err != nil {
						return "", err
					}
					p.logger.Info(fmt.Sprintf("approve_result: node=%s index=%d", in.NodeName, in.Index))
					p.approveItem(node, item)
					return fmt.Sprintf("已批准 %s[%d]", in.NodeName, in.Index), nil
				},
			},
		},
		{
			Name: "reject_result",
			Def: ToolDef{
				Description: "审核不通过：把节点本批次中第 index 条结果连同修改意见退回节点重新执行。" +
					"超过最大重试次数时自动放行。",
				Input:      rejectResultInput{},
				Permission: ReadOnly,
				Execute: func(ctx Context, in rejectResultInput) (string, error) {
					node, item, err := p.claimItem(in.NodeName, in.Index)
					if err != nil {
						return "", err
					}
					p.logger.Info(fmt.Sprintf("reject_result: node=%s index=%d guidance=%s", in.NodeName, in.Index, in.Guidance))
					return p.rejectItem(node, item, in.Index, in.Guidance), nil
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// DAG 调度 + worker
// ---------------------------------------------------------------------------

// Run 启动 pipeline DAG 调度，阻塞直到所有节点完成、某个节点失败或 ctx 取消。
// 节点失败时返回该节点的错误；ctx 取消时返回 ctx 的错误。
func (p *pipeline) Run(ctx context.Context) error {
	if !p.ran.CompareAndSwap(false, true) {
		return fmt.Errorf("pipeline: 同一个实例不能重复运行")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.runCtx = runCtx
	p.cancelFunc = cancel

	order, err := p.validate()
	if err != nil {
		p.shutdown()
		return err
	}
	p.topoOrder = order

	var wg sync.WaitGroup
	if p.hasSupervisor && p.hasReviewNodes() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runSupervisor(runCtx)
		}()
	}

	// 调度只在本 goroutine 进行：每当有节点结束，就启动依赖已全部完成的节点。
	// 依赖以"节点完成"为准（审核节点完成即其结果都已处置）。此前没有
	// supervisor 时所有节点一开始就被标记为"审核完成"，任何一个节点结束都会
	// 连带启动所有尚未启动的节点，DependsOn 形同虚设。
	startReady := func() {
		for _, name := range order {
			node := p.nodes[name]
			if node.started || !p.depsDone(node) {
				continue
			}
			node.started = true
			p.logger.Info(fmt.Sprintf("pipeline: 启动节点 %s", name))
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.runNode(runCtx, name)
			}()
		}
	}
	startReady()

	completed := false
	for !completed && runCtx.Err() == nil {
		select {
		case <-runCtx.Done():
		case <-p.doneCh:
			completed = true
		case <-p.nodeDoneCh:
			startReady()
		}
	}
	if !completed {
		// 取消或节点失败：中止所有队列，让 worker 立即退出。
		for _, q := range p.msgQueues {
			q.abort()
		}
	}
	// 正常完成时，这里只剩 supervisor 的收尾轮还在运行。
	wg.Wait()
	p.shutdown()

	p.errMu.Lock()
	nodeErr := p.firstError
	p.errMu.Unlock()
	if nodeErr != nil {
		return nodeErr
	}
	if !completed {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	return nil
}

// shutdown 运行结束时的清理：停止宿主回调，停掉全部队列的泵协程，回滚
// 未处置的事务。
func (p *pipeline) shutdown() {
	p.finished.Store(true)
	for _, q := range p.msgQueues {
		q.abort()
	}
	p.rollbackOpenTx()
}

// runNode 运行单个 DAG 节点。
func (p *pipeline) runNode(ctx context.Context, nodeName string) {
	node := p.nodes[nodeName]
	// 结束通知最后发出：调度器收到时 markNodeDone 已执行（或节点已失败/被取消）。
	defer func() { p.nodeDoneCh <- nodeName }()

	// 节点生命周期事件：started / done。宿主据此维护 DAG 状态图
	//（pending/running/done），与 OnEvent 的细粒度直播（thinking/progress）
	// 职责分离。
	p.emitEvent(nodeName, "node_started", "", "")
	nodeCtx := ctx

	// 注入 SharedData 到 context。
	if len(p.cfg.SharedData) > 0 {
		nodeCtx = context.WithValue(nodeCtx, ctxKeySharedData, p.cfg.SharedData)
	}

	// 注入下游队列到 context。
	nodeCtx = p.injectQueuesForNode(nodeCtx, nodeName)

	// 注入节点名到 context，供 Observer 使用（如 usage logging 的 step_group）。
	nodeCtx = context.WithValue(nodeCtx, PipelineNodeNameKey, nodeName)

	// 节点级沙箱覆盖：node.Sandbox 非 nil 且 App 配了沙箱工厂时，本节点
	// 单独 Enter 一个会话（生命周期 = 整个 runNode，横跨全部 reject 重试）。
	if node.Sandbox != nil && p.parentApp != nil {
		var sb Sandbox
		p.parentApp.mu.RLock()
		sb = p.parentApp.config.sandbox
		p.parentApp.mu.RUnlock()
		if sb != nil {
			sbSess, err := sb.Enter(nodeCtx, nodeName, *node.Sandbox)
			if err != nil {
				p.recordNodeError(nodeName, fmt.Errorf("节点沙箱初始化失败: %w", err))
				return
			}
			defer func() {
				if err := sbSess.Close(); err != nil {
					p.logger.Warn("sandbox: 关闭节点沙箱会话失败", "node", nodeName, "error", err)
				}
			}()
			nodeCtx = WithSandboxSession(nodeCtx, sbSess)
			if r := sbSess.Root(); r != "" {
				nodeCtx = WithSessionContext(nodeCtx, SessionIDFromContext(nodeCtx), r)
			}
		}
	}

	// 保存 nodeCtx 供 reject 重试时复用。
	node.nodeCtx = nodeCtx

	// Message 惰性求值：此刻 DependsOn 已全部满足，上游产出已落地，
	// 拿到的是真实状态而非构造 pipeline 时的空快照。详见 MessageFunc 字段注释。
	if node.MessageFunc != nil {
		p.callback(nodeName, func() { node.Message = node.MessageFunc(nodeCtx) })
	}

	// 如果有 Message，Push 到队列作为初始任务。
	//
	// 仅限**没有上游生产者**的节点：没人往它队列推任务，Message 就是它的唯一任务。
	// 有上游 Injects 指向本节点时，真实任务由上游推入，Message 只在 runWorkerOnce 里
	// 当作每条任务的前缀 —— 若这里也 Push 一次，worker 就会多消费一条「只有上下文、
	// 没有任务数据」的伪任务，白跑一次 LLM。
	q := p.msgQueues[nodeName]
	if node.Message != "" && !p.hasUpstreamProducer(nodeName) {
		q.Push(node.Message)
	}

	if node.Review {
		// 审核节点的队列要留着接收被打回的任务：等上游都关闭队列、所有任务
		// 都处理完并处置后，才由 maybeSelfClose 关闭。
		node.armed.Store(true)
		p.maybeSelfClose(node)
	} else {
		// 非审核节点：立即 self-close，workers 消费完即退出。
		q.Close()
	}

	// 统一走队列消费模式。
	p.runMultiWorkers(nodeCtx, node)
	if ctx.Err() != nil {
		return
	}

	// 关闭此节点声明的下游队列（引用计数 Close）。
	for _, target := range node.CloseQueues {
		if tq, ok := p.msgQueues[target]; ok {
			tq.Close()
		}
	}

	p.markNodeDone(nodeName)
	p.emitEvent(nodeName, "node_done", "", "")
}

// retryMessage 包装重试的消息，携带重试计数。
type retryMessage struct {
	msg        any    // 原始消息内容
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
				var msg any
				select {
				case <-ctx.Done():
					return
				case m, ok := <-q.ch:
					if !ok {
						return
					}
					msg = m
				}
				p.processTask(ctx, node, msg, needTx)
			}
		}()
	}
	workerWg.Wait()
}

// processTask 执行一条任务并交付结果。
func (p *pipeline) processTask(ctx context.Context, node *pipelineNodeState, msg any, needTx bool) {
	retryCount := 0
	originalMsg := msg
	if rm, ok := msg.(*retryMessage); ok {
		retryCount = rm.retryCount
		originalMsg = rm.msg
	}

	var tx Transaction
	workerCtx := ctx
	if needTx {
		tx = p.cfg.TransactionFactory(ctx)
		if tx != nil {
			workerCtx = WithTransaction(ctx, tx)
		}
	}

	result, err := p.runWorkerOnce(workerCtx, node, msg)
	if err != nil {
		// 执行失败或被取消：本轮写操作回滚，结果不进入审核与回调。
		if tx != nil {
			if rbErr := tx.Rollback(context.Background()); rbErr != nil {
				p.logger.Warn(fmt.Sprintf("pipeline: node=%s 失败后回滚事务出错: %v", node.Name, rbErr))
			}
		}
		if ctx.Err() == nil {
			p.recordNodeError(node.Name, err)
		}
		p.pendingMu.Lock()
		node.processed++
		p.pendingMu.Unlock()
		p.maybeSelfClose(node)
		return
	}
	p.handleResult(node.Name, result, originalMsg, retryCount, tx)
	p.maybeSelfClose(node)
}

// runWorkerOnce 为单条 message 创建精简 agent 并执行。
// 如果节点有 Message 且不是 Message 本身，拼接到任务前面作为上下文。
func (p *pipeline) runWorkerOnce(ctx context.Context, node *pipelineNodeState, msg any) (string, error) {
	if node.Agent == nil {
		return "", nil
	}

	// 处理重试消息：直接使用已拼接好的 input。
	if rm, ok := msg.(*retryMessage); ok {
		return p.runLightweight(ctx, node.Agent, rm.input)
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
	return p.runLightweight(ctx, node.Agent, input)
}

// recordNodeError 记录节点的致命错误并中断 pipeline：取消 pipeline context，
// 所有正在运行的节点退出，最终 Run 返回该错误。只记录第一个错误。
func (p *pipeline) recordNodeError(nodeName string, err error) {
	p.errMu.Lock()
	if p.firstError != nil {
		p.errMu.Unlock()
		p.logger.Warn(fmt.Sprintf("recordNodeError: node=%s error=%v（已有更早的错误，忽略）", nodeName, err))
		return
	}
	p.firstError = fmt.Errorf("pipeline node %q failed: %w", nodeName, err)
	p.errMu.Unlock()

	p.logger.Error(fmt.Sprintf("recordNodeError: node=%s, cancelling entire pipeline. error=%v", nodeName, err))
	if p.cancelFunc != nil {
		p.cancelFunc()
	}
}

// ---------------------------------------------------------------------------
// 节点 agent 循环
// ---------------------------------------------------------------------------

// appSnapshot 读取父 App 的配置与当前 provider（无 parent 时为零值）。
func (p *pipeline) appSnapshot() (appConfig, provider.Provider) {
	if p.parentApp == nil {
		return appConfig{}, nil
	}
	p.parentApp.mu.RLock()
	defer p.parentApp.mu.RUnlock()
	return p.parentApp.config, p.parentApp.provider
}

// buildLightweightLoop 为 pipeline worker / supervisor 构建精简的 loop。
// 跳过 Memory、Budget、SessionMemory 等重型子系统；工具调用的检查链
// （权限门、plan、hooks、中间件）来自 p.scope——由 RunPipeline 从所在的
// agent 运行继承，宿主信任的 pipeline 为 nil（不检查）。
func (p *pipeline) buildLightweightLoop(agentDef *PipelineAgentDef, nodeCtx context.Context) *loop.Loop {
	appCfg, appProv := p.appSnapshot()

	prov := agentDef.Provider
	if prov == nil {
		prov = appProv
	}
	maxTurns := agentDef.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 100
	}

	tools := make([]loop.ToolEntry, 0, len(agentDef.Tools))
	for _, t := range agentDef.Tools {
		def := t.Def
		tools = append(tools, loop.ToolEntry{
			Name:        t.Name,
			Description: def.Description,
			InputSchema: inputSchemaOf(def),
			Permission:  permission.Level(def.Permission),
			Concurrent:  def.Concurrent,
			ExecuteFn: func(ctx context.Context, input json.RawMessage) (string, error) {
				return def.call(ctx, input)
			},
		})
	}

	// 节点压缩：L2(Micro) + L4(Auto)。
	// Micro 裁剪过期工具结果（长任务 worker 的质量/成本关键；图片走统一
	// 回收策略不会被截断）；Auto 在 0.8 水位做 LLM 摘要。
	// L0(Budget)/L1(Snip) 不进节点：head+tail 截断会损坏图片数据。
	compCfg := compaction.Config{
		AutoCompactThreshold: 0.8,
		Layers:               []compaction.Layer{compaction.LayerMicro, compaction.LayerAuto},
	}
	if appCfg.promptDir != "" {
		compCfg.PromptFile = appCfg.promptDir + "/" + prompts.Compact
	}
	compMgr := compaction.NewManager(compCfg)
	compMgr.SetProvider(prov)

	// Observer: 继承父 App 的 obsRegistry（如果有）。
	// 与主循环同规则合并 ctx 边界注入链（observer.IntoContext）。
	var obs observer.Observer
	if p.parentApp != nil && p.parentApp.obsRegistry != nil {
		obs = p.parentApp.obsRegistry.Observer()
	}
	obs = observer.ResolveObserver(nodeCtx, obs)

	cfg := loop.Config{
		Provider:     prov,
		Tools:        tools,
		SystemPrompt: agentDef.Instruction,
		MaxTurns:     maxTurns,
		Compaction:   compMgr,
		Observer:     obs,
		// 日志/测试模式继承父 App：WithDebugMode 开启后节点级 loop 同样输出细节轨迹。
		Logger: p.logger,
		Debug:  appCfg.debug,
		Nested: true,

		SkipInputValidation: appCfg.skipInputValidation,
	}
	if p.scope != nil {
		p.scope.ApplyTo(&cfg)
	} else if p.parentApp != nil && p.parentApp.hooksMgr != nil {
		cfg.Hooks = &hooksRunnerAdapter{mgr: p.parentApp.hooksMgr}
	}
	return loop.New(cfg)
}

// traceAppend 追加一条节点轨迹行（TraceDir 未设置时空操作）。多 worker
// 并发写同一节点文件由 traceMu 串行化；JSONL 供宿主按行解析回放。
func (p *pipeline) traceAppend(node string, entry map[string]any) {
	if p.cfg.TraceDir == "" {
		return
	}
	entry["ts"] = time.Now().UnixMilli()
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	p.traceMu.Lock()
	defer p.traceMu.Unlock()
	// 文件名来自节点/agent 名（动态 pipeline 中由模型指定），先净化再拼路径。
	f, err := os.OpenFile(filepath.Join(p.cfg.TraceDir, safename.Sanitize(node)+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// tracePreview 截断长文本（不切坏多字节字符）。
func tracePreview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// runLightweight 运行精简 loop 并收集最终文本。
// 如果 LLM 返回空响应（无工具调用、无文本、无错误），返回错误而非静默空字符串。
func (p *pipeline) runLightweight(ctx context.Context, agentDef *PipelineAgentDef, input string) (string, error) {
	l := p.buildLightweightLoop(agentDef, ctx)

	p.logger.Info(fmt.Sprintf("runLightweight: agent=%s tools=%d input_len=%d", agentDef.Name, len(agentDef.Tools), len(input)))

	var final strings.Builder
	var toolStartCount, toolDoneCount, errorCount int
	var lastError error
	var textBuf, thinkBuf strings.Builder
	flushTextTrace := func() {
		if textBuf.Len() > 0 {
			p.traceAppend(agentDef.Name, map[string]any{"type": "text", "text": textBuf.String()})
			textBuf.Reset()
		}
	}
	// 思考是逐词流式 delta：聚合成块再落盘，否则轨迹里一行一个字。
	flushThinkTrace := func() {
		if thinkBuf.Len() > 0 {
			p.traceAppend(agentDef.Name, map[string]any{"type": "thinking", "text": thinkBuf.String()})
			thinkBuf.Reset()
		}
	}
	for ev := range l.Run(ctx, input) {
		switch ev.Type {
		case loop.EvtTextDelta:
			final.WriteString(ev.Text)
			textBuf.WriteString(ev.Text)
		case loop.EvtThinking:
			// 思考模型推理过程（delta.reasoning_content）。
			textBuf.Reset() // 思考打断了文本段
			thinkBuf.WriteString(ev.Thinking)
			p.emitEvent(agentDef.Name, "thinking", ev.Thinking, "")
		case loop.EvtToolStart:
			toolStartCount++
			flushTextTrace()
			flushThinkTrace()
			p.traceAppend(agentDef.Name, map[string]any{"type": "tool_start", "tool": ev.ToolName, "input": tracePreview(string(ev.ToolInput), 800)})
			p.logger.Info(fmt.Sprintf("runLightweight: agent=%s tool_start=%s", agentDef.Name, ev.ToolName))
		case loop.EvtToolDone:
			toolDoneCount++
			flushTextTrace()
			flushThinkTrace()
			p.traceAppend(agentDef.Name, map[string]any{"type": "tool_done", "tool": ev.ToolName, "result": tracePreview(ev.ToolResult, 2000)})
			p.logger.Info(fmt.Sprintf("runLightweight: agent=%s tool_done=%s result=%s", agentDef.Name, ev.ToolName, tracePreview(ev.ToolResult, 200)))
		case loop.EvtError:
			errorCount++
			lastError = ev.Error
			flushTextTrace()
			flushThinkTrace()
			p.traceAppend(agentDef.Name, map[string]any{"type": "error", "text": ev.Error.Error()})
			p.logger.Warn(fmt.Sprintf("runLightweight: agent=%s error=%v", agentDef.Name, ev.Error))
			p.emitEvent(agentDef.Name, "error", ev.Error.Error(), "")
		case loop.EvtInterrupted:
			// 用户主动终止：区别于错误通道透出，前端渲染「已停止」。
			p.logger.Info(fmt.Sprintf("runLightweight: agent=%s interrupted", agentDef.Name))
			p.emitEvent(agentDef.Name, "interrupted", ev.Text, "")
		case loop.EvtProgress:
			// 429 状态行等：StatusKey 非空 = 原地更新语义，Text 空 = 清除。
			p.emitEvent(agentDef.Name, "progress", ev.Text, ev.StatusKey)
		case loop.EvtCustom:
			// 节点工具发射的自定义事件到达发起流水线的运行的事件流。
			loop.EmitFromTool(ctx, ev)
		}
	}
	flushTextTrace()
	flushThinkTrace()

	finalText := final.String()
	p.logger.Info(fmt.Sprintf("runLightweight: agent=%s completed tools_started=%d tools_done=%d errors=%d text_len=%d",
		agentDef.Name, toolStartCount, toolDoneCount, errorCount, len(finalText)))

	// 被取消：不是节点失败，也不交付半截产出。
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 节点产出透出：宿主（create_pipeline 工具）据此把末端产出带回给
	// 主 agent——此前最终文本只进日志，产物在流水线内部蒸发。
	if len(finalText) > 0 {
		p.emitEvent(agentDef.Name, "node_output", finalText, "")
		p.logger.Info(fmt.Sprintf("runLightweight: agent=%s text_preview=%s", agentDef.Name, tracePreview(finalText, 300)))
	}

	// 空响应检测：LLM 既没有调用工具、也没有输出文本、也没有返回错误。
	// 这通常意味着 provider 配置有误（API Key 无效、模型名错误、BaseURL 不通等），
	// LLM API 静默返回空内容。不应当作正常结果静默通过。
	if toolStartCount == 0 && len(finalText) == 0 && errorCount == 0 {
		p.logger.Warn(fmt.Sprintf("runLightweight: agent=%s empty response detected (no tools, no text, no errors), likely provider misconfiguration", agentDef.Name))
		return "", fmt.Errorf("agent %q: LLM returned empty response (0 tool calls, 0 text chars) — provider may be misconfigured (check API key, model name, base URL)", agentDef.Name)
	}

	// LLM 错误一律传播：中途报错但已有部分产出时，残缺产出不能当成功结果
	// 流向下游（DB/队列）。传播语义：recordNodeError → 取消整个 pipeline →
	// RunPipeline 返回错误。部分产出的上下文带进错误信息，方便定位。
	if errorCount > 0 && lastError != nil {
		return "", fmt.Errorf("agent %q: LLM error after %d tool calls, %d text chars: %w",
			agentDef.Name, toolDoneCount, len(finalText), lastError)
	}

	return finalText, nil
}

// ---------------------------------------------------------------------------
// supervisor
// ---------------------------------------------------------------------------

// runSupervisor 运行 supervisor：逐个处理审核事件，每个批次一轮独立对话；
// 全部节点完成后，若配置了 FinalizePrompt 再运行一轮收尾。
func (p *pipeline) runSupervisor(ctx context.Context) {
	sup := p.cfg.Supervisor
	if sup == nil {
		return
	}

	// 注入 SharedData 到 supervisor context，让 supervisor 工具可以通过 GetPipelineData 获取数据。
	if len(p.cfg.SharedData) > 0 {
		ctx = context.WithValue(ctx, ctxKeySharedData, p.cfg.SharedData)
	}
	reviewTools := p.newReviewTools()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-p.reviewSignal:
			p.reviewBatch(ctx, sup, reviewTools, ev)
		case <-p.doneCh:
			// 审核节点要等结果全部处置才会结束，此时不应再有待审批次；仍先
			// 排空，防御性处理。
		drain:
			for {
				select {
				case ev := <-p.reviewSignal:
					p.reviewBatch(ctx, sup, reviewTools, ev)
				default:
					break drain
				}
			}
			if p.cfg.FinalizePrompt != "" {
				p.logger.Info("supervisor: 全部节点完成，运行收尾轮")
				if err := p.runSupervisorTurns(ctx, p.supervisorDef(sup, nil, 5), p.cfg.FinalizePrompt); err != nil {
					p.logger.Warn(fmt.Sprintf("supervisor: 收尾轮出错: %v", err))
				}
			}
			return
		}
	}
}

// reviewBatch 用一轮独立对话审核一个批次；本轮没有被处置的结果自动批准
// （LLM 没调工具、参数填错、调用本身报错都会走到这里——审核节点不会因此
// 永远挂起）。运行已取消时不放行。
func (p *pipeline) reviewBatch(ctx context.Context, sup *PipelineAgentDef, reviewTools []NamedTool, ev ReviewEvent) {
	node := p.nodes[ev.NodeName]
	if node == nil || len(ev.Results) == 0 {
		return
	}
	p.logger.Info(fmt.Sprintf("supervisor: reviewing node=%s, results=%d", ev.NodeName, len(ev.Results)))
	p.beginRound(ev)

	data, _ := json.Marshal(ev)
	input := fmt.Sprintf("节点 %s 提交了 %d 条结果供你审核。可以先用你的工具抽查验证质量，然后对每条结果调用 approve_result 或 reject_result（index 为结果在本批次中的序号）；没有处置的结果会在本轮结束时自动批准。\n\n审核数据：%s",
		ev.NodeName, len(ev.Results), data)
	if err := p.runSupervisorTurns(ctx, p.supervisorDef(sup, reviewTools, 10), input); err != nil {
		p.logger.Warn(fmt.Sprintf("supervisor: 审核 node=%s 出错: %v", ev.NodeName, err))
	}

	rest := p.endRound(ev.NodeName)
	if ctx.Err() != nil {
		// 运行已取消：未处置的结果不放行，其事务在运行结束时回滚。
		return
	}
	if len(rest) > 0 {
		p.logger.Info(fmt.Sprintf("supervisor: node=%s 有 %d 条结果未处置，自动批准", ev.NodeName, len(rest)))
	}
	for _, item := range rest {
		p.approveItem(node, item)
	}
}

// supervisorDef 组装 supervisor 一轮对话的 agent 定义。
func (p *pipeline) supervisorDef(sup *PipelineAgentDef, extra []NamedTool, defaultTurns int) *PipelineAgentDef {
	name := sup.Name
	if name == "" {
		name = "supervisor"
	}
	maxTurns := sup.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultTurns
	}
	tools := append(append([]NamedTool(nil), sup.Tools...), extra...)
	return &PipelineAgentDef{
		Name:        name,
		Instruction: sup.Instruction,
		Tools:       tools,
		Provider:    sup.Provider,
		MaxTurns:    maxTurns,
	}
}

// runSupervisorTurns 运行 supervisor 的一轮对话，返回其中最后一个模型错误。
func (p *pipeline) runSupervisorTurns(ctx context.Context, def *PipelineAgentDef, input string) error {
	l := p.buildLightweightLoop(def, ctx)
	var lastErr error
	for ev := range l.Run(ctx, input) {
		switch ev.Type {
		case loop.EvtToolDone:
			p.logger.Info(fmt.Sprintf("supervisor: tool=%s done error=%v", ev.ToolName, ev.ToolIsError))
		case loop.EvtError:
			lastErr = ev.Error
		}
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// App 集成
// ---------------------------------------------------------------------------

// UsePipeline 配置 DAG 流水线，之后用 RunPipeline 运行。每次 RunPipeline 都
// 基于这份配置新建一个运行实例，可以重复运行，也可以并发运行。
func (a *App) UsePipeline(cfg PipelineConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pipelineCfg = &cfg
}

// RunPipeline 运行 UsePipeline 配置的流水线，阻塞直到所有节点完成、某个
// 节点失败或 ctx 取消。
//
// 权限：在某次 agent 运行之中调用时（例如在宿主工具内部），节点与
// supervisor 的工具调用继承该运行的权限门、plan 模式、hooks 与中间件，
// 按工具声明的权限级别检查（PipelineConfig.TrustedTools 可关闭）；宿主
// 代码在任何运行之外直接调用时，工具不经权限门，由宿主负责。
func (a *App) RunPipeline(ctx context.Context) error {
	a.mu.RLock()
	cfg := a.pipelineCfg
	a.mu.RUnlock()
	if cfg == nil {
		return fmt.Errorf("goagent: 未配置 pipeline，请先调用 UsePipeline()")
	}
	pl := newPipeline(*cfg, a)
	if !cfg.TrustedTools {
		pl.scope = inheritedScope(ctx)
	}
	return a.runPipeline(ctx, pl)
}

// runPipeline 运行一个 pipeline 实例：嵌套深度检查、会话上下文与沙箱。
func (a *App) runPipeline(ctx context.Context, pl *pipeline) error {
	ctx, err := enterSubRun(ctx, "pipeline")
	if err != nil {
		return err
	}
	a.mu.RLock()
	cfg := a.config
	a.mu.RUnlock()

	// 会话上下文：PipelineConfig.SessionID 指定的会话与 ctx 中的不同时切换
	// 过去（工作目录经 WithSessionWorkDir 解析）；未指定时沿用 ctx 中已有的
	// 会话上下文——在 agent 运行中启动时即该运行的会话与工作目录。此前
	// SessionID 为空时会把 ctx 中的会话与工作目录清空。
	if sid := pl.cfg.SessionID; sid != "" && sid != SessionIDFromContext(ctx) {
		workDir := ""
		if cfg.sessionWorkDirFn != nil {
			workDir = cfg.sessionWorkDirFn(sid)
		}
		ctx = WithSessionContext(ctx, sid, workDir)
	}

	// 沙箱：ctx 已处于某个沙箱会话（由所在的 agent 运行进入）时沿用；否则
	// 为本次运行进入一个，覆盖 supervisor、全部节点与审核工具。沙箱根非空
	// 时覆盖工作目录（优先级规则见 sandbox.go）。
	if cfg.sandbox != nil && SandboxFromContext(ctx) == nil {
		sid := SessionIDFromContext(ctx)
		sbSess, err := cfg.sandbox.Enter(ctx, sid, cfg.sandboxPolicy)
		if err != nil {
			return fmt.Errorf("goagent: pipeline 沙箱初始化失败: %w", err)
		}
		defer func() {
			if err := sbSess.Close(); err != nil {
				a.logger.Warn("sandbox: 关闭 pipeline 沙箱会话失败", "error", err)
			}
		}()
		ctx = WithSandboxSession(ctx, sbSess)
		if r := sbSess.Root(); r != "" {
			ctx = WithSessionContext(ctx, sid, r)
		}
	}

	return pl.Run(ctx)
}

// parentLogger 取 parent App 的 logger；无 parent（独立构造 pipeline）时用 slog.Default()。
func parentLogger(parent *App) *slog.Logger {
	if parent != nil && parent.logger != nil {
		return parent.logger
	}
	return slog.Default()
}
