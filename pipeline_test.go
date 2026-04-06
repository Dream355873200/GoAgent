package goagent

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/provider"
)

// ---------------------------------------------------------------------------
// Test 1: messagePusher Push/Close + worker 消费
// ---------------------------------------------------------------------------

func TestMessagePusher_PushAndConsume(t *testing.T) {
	mp := newMessagePusher(10, reflect.TypeOf(""))

	// Push 3 条消息。
	mp.Push("a")
	mp.Push("b")
	mp.Push("c")

	if mp.len() != 3 {
		t.Fatalf("expected len 3, got %d", mp.len())
	}

	// 消费消息。
	var results []string
	mp.Close()
	for msg := range mp.ch {
		results = append(results, msg.(string))
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0] != "a" || results[1] != "b" || results[2] != "c" {
		t.Fatalf("unexpected results: %v", results)
	}
}

func TestMessagePusher_CloseIdempotent(t *testing.T) {
	mp := newMessagePusher(10, reflect.TypeOf(""))
	mp.Close()
	mp.Close() // should not panic

	if !mp.isClosed() {
		t.Fatal("expected closed")
	}
}

func TestMessagePusher_PushAfterClose(t *testing.T) {
	mp := newMessagePusher(10, reflect.TypeOf(""))
	mp.Close()
	mp.Push("should be dropped") // should not panic or block
}

// ---------------------------------------------------------------------------
// Test 2: Review=false 自动通过 → OnResult 回调触发
// ---------------------------------------------------------------------------

func TestHandleResult_AutoPass(t *testing.T) {
	var called int32
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      false,
				Concurrency: 1,
				OnResult: func(result any) {
					atomic.AddInt32(&called, 1)
				},
			},
		},
	}

	p := newPipeline(cfg, nil)

	// handleResult 应立刻调用 OnResult（Review=false）。
	p.handleResult("worker", "test-result", "test-msg", 0)

	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("expected OnResult called 1 time, got %d", called)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Injects 权限控制
// ---------------------------------------------------------------------------

func TestInjectsPermission(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "splitter",
				Concurrency: 1,
				Injects:     []string{"worker_a", "worker_b"},
			},
			{
				Name:        "worker_a",
				Concurrency: 3,
			},
			{
				Name:        "worker_b",
				Concurrency: 3,
			},
			{
				Name:        "worker_c",
				Concurrency: 3,
			},
		},
	}

	p := newPipeline(cfg, nil)

	ctx := p.injectQueuesForNode(context.Background(), "splitter")

	// splitter 的 Injects 包含 worker_a 和 worker_b，不包含 worker_c。
	if q := GetMessageQueue(ctx, "worker_a"); q == nil {
		t.Fatal("expected worker_a queue to be injected")
	}
	if q := GetMessageQueue(ctx, "worker_b"); q == nil {
		t.Fatal("expected worker_b queue to be injected")
	}
	if q := GetMessageQueue(ctx, "worker_c"); q != nil {
		t.Fatal("expected worker_c queue to NOT be injected")
	}
	if q := GetMessageQueue(ctx, "nonexistent"); q != nil {
		t.Fatal("expected nonexistent queue to return nil")
	}
}

func TestGetMessageQueue_NilContext(t *testing.T) {
	ctx := context.Background()
	if q := GetMessageQueue(ctx, "any"); q != nil {
		t.Fatal("expected nil for context without injected queues")
	}
}

// ---------------------------------------------------------------------------
// Test 4: DAG 拓扑排序
// ---------------------------------------------------------------------------

func TestTopoSort_Linear(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a"},
			{Name: "b", DependsOn: []string{"a"}},
			{Name: "c", DependsOn: []string{"b"}},
		},
	}
	p := newPipeline(cfg, nil)

	order, err := p.topoSort()
	if err != nil {
		t.Fatal(err)
	}

	// a 必须在 b 之前，b 必须在 c 之前。
	idx := make(map[string]int)
	for i, name := range order {
		idx[name] = i
	}
	if idx["a"] >= idx["b"] || idx["b"] >= idx["c"] {
		t.Fatalf("invalid order: %v", order)
	}
}

func TestTopoSort_Parallel(t *testing.T) {
	// A → B + C（B 和 C 可并行）
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a"},
			{Name: "b", DependsOn: []string{"a"}},
			{Name: "c", DependsOn: []string{"a"}},
		},
	}
	p := newPipeline(cfg, nil)

	order, err := p.topoSort()
	if err != nil {
		t.Fatal(err)
	}

	idx := make(map[string]int)
	for i, name := range order {
		idx[name] = i
	}
	if idx["a"] >= idx["b"] || idx["a"] >= idx["c"] {
		t.Fatalf("a should come before b and c: %v", order)
	}
}

func TestTopoSort_CyclicDependency(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a", DependsOn: []string{"b"}},
			{Name: "b", DependsOn: []string{"a"}},
		},
	}
	p := newPipeline(cfg, nil)

	_, err := p.topoSort()
	if err == nil {
		t.Fatal("expected error for cyclic dependency")
	}
}

func TestTopoSort_MissingDependency(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a", DependsOn: []string{"nonexistent"}},
		},
	}
	p := newPipeline(cfg, nil)

	_, err := p.topoSort()
	if err == nil {
		t.Fatal("expected error for missing dependency")
	}
}

// ---------------------------------------------------------------------------
// Test 5: applyNodeDefaults + applyNodeTypes
// ---------------------------------------------------------------------------

func TestApplyNodeDefaults(t *testing.T) {
	node := &PipelineNode{Name: "test"}
	applyNodeDefaults(node)

	if node.Concurrency != 1 {
		t.Errorf("expected Concurrency=1, got %d", node.Concurrency)
	}
	if node.QueueSize != 64 {
		t.Errorf("expected QueueSize=64, got %d", node.QueueSize)
	}
	if node.ReviewBatch != 1 {
		t.Errorf("expected ReviewBatch=1, got %d", node.ReviewBatch)
	}
	if node.MaxRetries != 3 {
		t.Errorf("expected MaxRetries=3, got %d", node.MaxRetries)
	}
}

func TestApplyNodeTypes_Default(t *testing.T) {
	node := &PipelineNode{Name: "test"}
	state := &pipelineNodeState{PipelineNode: *node}
	applyNodeTypes(node, state)

	if state.msgType != reflect.TypeOf("") {
		t.Errorf("expected msgType=string, got %v", state.msgType)
	}
	if state.resultType != reflect.TypeOf("") {
		t.Errorf("expected resultType=string, got %v", state.resultType)
	}
}

func TestApplyNodeTypes_Custom(t *testing.T) {
	type Task struct{ ID string }
	node := &PipelineNode{Name: "test", MessageType: Task{}, ResultType: Task{}}
	state := &pipelineNodeState{PipelineNode: *node}
	applyNodeTypes(node, state)

	if state.msgType != reflect.TypeOf(Task{}) {
		t.Errorf("expected msgType=Task, got %v", state.msgType)
	}
	if state.resultType != reflect.TypeOf(Task{}) {
		t.Errorf("expected resultType=Task, got %v", state.resultType)
	}
}

// ---------------------------------------------------------------------------
// Test 6: 自动攒批触发审核
// ---------------------------------------------------------------------------

func TestCheckReviewTrigger_BatchFull(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      true,
				ReviewBatch: 3,
				Concurrency: 3,
			},
		},
		Supervisor: &PipelineAgentDef{Name: "sup", Instruction: "test"},
	}
	p := newPipeline(cfg, nil)

	// 填充 3 个 pending result。
	for i := 0; i < 3; i++ {
		p.pendingMu.Lock()
		p.pendingResults["worker"] = append(p.pendingResults["worker"], ReviewResultItem{
			Index: i,
			Data:  "data",
		})
		p.pendingMu.Unlock()
	}

	// 非阻塞检查 reviewSignal。
	go p.checkReviewTrigger("worker", 3)

	select {
	case event := <-p.reviewSignal:
		if event.NodeName != "worker" {
			t.Fatalf("expected node_name=worker, got %s", event.NodeName)
		}
		if len(event.Results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(event.Results))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for review signal")
	}
}

func TestCheckReviewTrigger_QueueEmpty(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      true,
				ReviewBatch: 10, // 大于实际任务量
				Concurrency: 3,
			},
		},
		Supervisor: &PipelineAgentDef{Name: "sup", Instruction: "test"},
	}
	p := newPipeline(cfg, nil)
	// 队列已关闭（空）。
	p.msgQueues["worker"].Close()

	// 只有 2 个 pending result（小于 ReviewBatch=10）。
	p.pendingMu.Lock()
	p.pendingResults["worker"] = []ReviewResultItem{
		{Index: 0, Data: "data0"},
		{Index: 1, Data: "data1"},
	}
	p.pendingMu.Unlock()

	go p.checkReviewTrigger("worker", 2)

	select {
	case event := <-p.reviewSignal:
		if len(event.Results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(event.Results))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: expected review trigger when queue is empty")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Review tools — approve_result → OnResult 回调
// ---------------------------------------------------------------------------

func TestReviewTools_Approve(t *testing.T) {
	var approvedResult any
	var mu sync.Mutex

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      true,
				ReviewBatch: 1,
				Concurrency: 3,
				OnResult: func(result any) {
					mu.Lock()
					approvedResult = result
					mu.Unlock()
				},
			},
		},
	}
	p := newPipeline(cfg, nil)

	tools := p.newReviewTools()

	// 模拟向 reviewSignal 发送审核事件。
	go func() {
		p.reviewSignal <- ReviewEvent{
			NodeName: "worker",
			Results: []ReviewResultItem{
				{Index: 0, Data: `"hello"`, rawResult: "hello"},
			},
		}
	}()

	// 调用 wait_for_review。
	ctx := newContextFromStd(context.Background())
	waitTool := tools[0].Def // wait_for_review
	result, err := waitTool.Execute.(func(Context, waitForReviewInput) (string, error))(ctx, waitForReviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected non-empty result from wait_for_review")
	}

	// 调用 approve_result。
	// 需要先标记 nodesDone，模拟 worker 完成。
	p.nodesMu.Lock()
	p.nodesDone["worker"] = true
	p.nodesMu.Unlock()

	approveTool := tools[1].Def // approve_result
	approveResult, err := approveTool.Execute.(func(Context, approveResultInput) (string, error))(ctx, approveResultInput{
		NodeName: "worker",
		Index:    0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if approveResult == "" {
		t.Fatal("expected non-empty result from approve_result")
	}

	mu.Lock()
	if approvedResult != "hello" {
		t.Fatalf("expected OnResult called with 'hello', got %v", approvedResult)
	}
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// Test 8: Review tools — reject_result → 重试消息入队
// ---------------------------------------------------------------------------

func TestReviewTools_Reject(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      true,
				ReviewBatch: 1,
				MaxRetries:  3,
				Concurrency: 3,
			},
		},
	}
	p := newPipeline(cfg, nil)

	tools := p.newReviewTools()

	// 模拟审核事件。
	go func() {
		p.reviewSignal <- ReviewEvent{
			NodeName: "worker",
			Results: []ReviewResultItem{
				{Index: 0, Data: `"bad"`, OriginalMessage: "original task", RetryCount: 0, rawResult: "bad"},
			},
		}
	}()

	ctx := newContextFromStd(context.Background())

	// wait_for_review。
	waitTool := tools[0].Def
	_, err := waitTool.Execute.(func(Context, waitForReviewInput) (string, error))(ctx, waitForReviewInput{})
	if err != nil {
		t.Fatal(err)
	}

	// reject_result。
	rejectTool := tools[2].Def
	result, err := rejectTool.Execute.(func(Context, rejectResultInput) (string, error))(ctx, rejectResultInput{
		NodeName: "worker",
		Index:    0,
		Guidance: "描述不够详细",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected non-empty result from reject_result")
	}

	// 验证消息已入队。
	q := p.msgQueues["worker"]
	if q.len() != 1 {
		t.Fatalf("expected 1 message in queue after reject, got %d", q.len())
	}

	// 读取重试消息。
	msg := <-q.ch
	msgStr, ok := msg.(string)
	if !ok {
		t.Fatalf("expected string message, got %T", msg)
	}
	if msgStr == "" {
		t.Fatal("expected non-empty retry message")
	}
}

// ---------------------------------------------------------------------------
// Test 9: reject_result → MaxRetries 限制（自动通过）
// ---------------------------------------------------------------------------

func TestReviewTools_RejectMaxRetries(t *testing.T) {
	var autoApproved int32

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Review:      true,
				ReviewBatch: 1,
				MaxRetries:  2,
				Concurrency: 3,
				OnResult: func(result any) {
					atomic.AddInt32(&autoApproved, 1)
				},
			},
		},
	}
	p := newPipeline(cfg, nil)

	tools := p.newReviewTools()

	// 发送 retryCount=2 的 result（已达 MaxRetries）。
	go func() {
		p.reviewSignal <- ReviewEvent{
			NodeName: "worker",
			Results: []ReviewResultItem{
				{Index: 0, Data: `"bad"`, OriginalMessage: "task", RetryCount: 2, rawResult: "bad"},
			},
		}
	}()

	ctx := newContextFromStd(context.Background())

	// wait_for_review。
	waitTool := tools[0].Def
	_, _ = waitTool.Execute.(func(Context, waitForReviewInput) (string, error))(ctx, waitForReviewInput{})

	// reject_result — 应该自动通过因为达到 MaxRetries。
	rejectTool := tools[2].Def
	result, err := rejectTool.Execute.(func(Context, rejectResultInput) (string, error))(ctx, rejectResultInput{
		NodeName: "worker",
		Index:    0,
		Guidance: "还是不行",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 应该自动通过，OnResult 被调用。
	if atomic.LoadInt32(&autoApproved) != 1 {
		t.Fatalf("expected auto-approve when MaxRetries reached, got OnResult called %d times", autoApproved)
	}

	// 消息不应该入队。
	q := p.msgQueues["worker"]
	if q.len() != 0 {
		t.Fatalf("expected 0 messages in queue after max retries, got %d", q.len())
	}

	_ = result
}

// ---------------------------------------------------------------------------
// Test 10: hasReviewNodes
// ---------------------------------------------------------------------------

func TestHasReviewNodes(t *testing.T) {
	cfg1 := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a", Review: false},
			{Name: "b", Review: true},
		},
	}
	p1 := newPipeline(cfg1, nil)
	if !p1.hasReviewNodes() {
		t.Fatal("expected hasReviewNodes=true")
	}

	cfg2 := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a", Review: false},
			{Name: "b", Review: false},
		},
	}
	p2 := newPipeline(cfg2, nil)
	if p2.hasReviewNodes() {
		t.Fatal("expected hasReviewNodes=false")
	}
}

// ---------------------------------------------------------------------------
// Test 11: serializeResult
// ---------------------------------------------------------------------------

func TestSerializeResult(t *testing.T) {
	// string
	s := serializeResult("hello")
	if s != `"hello"` {
		t.Fatalf("expected '\"hello\"', got %s", s)
	}

	// struct
	type Foo struct {
		Name string `json:"name"`
	}
	s2 := serializeResult(Foo{Name: "bar"})
	if s2 != `{"name":"bar"}` {
		t.Fatalf("expected '{\"name\":\"bar\"}', got %s", s2)
	}
}

// ---------------------------------------------------------------------------
// Test 12: newPipeline 初始化
// ---------------------------------------------------------------------------

func TestNewPipeline_QueueCreation(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "single", Concurrency: 1},
			{Name: "multi", Concurrency: 3},
		},
	}
	p := newPipeline(cfg, nil)

	// Concurrency=1 不创建队列。
	if _, ok := p.msgQueues["single"]; ok {
		t.Fatal("Concurrency=1 should not have a message queue")
	}
	// Concurrency>1 创建队列。
	if _, ok := p.msgQueues["multi"]; !ok {
		t.Fatal("Concurrency>1 should have a message queue")
	}
}

// ---------------------------------------------------------------------------
// Test 13: markNodeDone + checkAllReviewDone
// ---------------------------------------------------------------------------

func TestMarkNodeDone_AllComplete(t *testing.T) {
	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{Name: "a", Review: false, Concurrency: 1},
			{Name: "b", Review: false, Concurrency: 1},
		},
	}
	p := newPipeline(cfg, nil)

	p.markNodeDone("a")

	// doneCh 不应该被关闭（b 还没完成）。
	select {
	case <-p.doneCh:
		t.Fatal("doneCh closed too early")
	default:
		// OK
	}

	p.markNodeDone("b")

	// 现在 doneCh 应该被关闭。
	select {
	case <-p.doneCh:
		// OK
	case <-time.After(time.Second):
		t.Fatal("timeout: doneCh should be closed when all nodes done")
	}
}

// ---------------------------------------------------------------------------
// Test 14: 端到端 — 线性 Pipeline（Concurrency=1）
// ---------------------------------------------------------------------------

func TestPipeline_E2E_Linear(t *testing.T) {
	mockProv := provider.NewMockProvider("处理完成")

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "step1",
				Concurrency: 1,
				Message:     "做第一步",
				Agent: &PipelineAgentDef{
					Name:        "step1",
					Instruction: "直接输出：处理完成",
					MaxTurns:    1,
				},
			},
			{
				Name:        "step2",
				Concurrency: 1,
				DependsOn:   []string{"step1"},
				Message:     "做第二步",
				Agent: &PipelineAgentDef{
					Name:        "step2",
					Instruction: "直接输出：处理完成",
					MaxTurns:    1,
					Provider:    mockProv,
				},
			},
		},
	}

	var step2Result string
	var mu sync.Mutex
	cfg.Nodes[1].OnResult = func(result any) {
		mu.Lock()
		step2Result = fmt.Sprintf("%v", result)
		mu.Unlock()
	}

	p := newPipeline(cfg, &App{provider: mockProv})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("pipeline Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if step2Result != "处理完成" {
		t.Fatalf("expected step2 result '处理完成', got %q", step2Result)
	}
}

// ---------------------------------------------------------------------------
// Test 15: 端到端 — producer 通过 tool 注入队列 → 多 Worker 并行消费
// ---------------------------------------------------------------------------

func TestPipeline_E2E_MultiWorker(t *testing.T) {
	var results []string
	var mu sync.Mutex

	type sendTaskInput struct {
		Task string `json:"task" desc:"任务内容" required:"true"`
	}
	sendTaskTool := NamedTool{
		Name: "send_task",
		Def: ToolDef{
			Description: "向 worker 队列推送任务",
			Input:       sendTaskInput{},
			Permission:  ReadOnly,
			Execute: func(ctx Context, in sendTaskInput) (string, error) {
				q := GetMessageQueue(ctx.Context, "worker")
				if q == nil {
					return "", fmt.Errorf("worker queue not found")
				}
				q.Push(in.Task)
				return "已推送", nil
			},
		},
	}

	// Mock: 第1轮调用 send_task 3次(tool_use)，第2轮纯文本结束。
	mockProv := &provider.MockProvider{
		Responses: []provider.MockResponse{
			{
				ToolCalls: []provider.MockToolCall{
					provider.NewMockToolCall("call-1", "send_task", sendTaskInput{Task: "task-1"}),
					provider.NewMockToolCall("call-2", "send_task", sendTaskInput{Task: "task-2"}),
					provider.NewMockToolCall("call-3", "send_task", sendTaskInput{Task: "task-3"}),
				},
			},
			{Text: "done"},
		},
	}

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "producer",
				Concurrency: 1,
				Message:     "拆分任务为 3 个子任务",
				Injects:     []string{"worker"},
				Agent: &PipelineAgentDef{
					Name:        "producer",
					Instruction: "调用 send_task 工具发送 3 个任务",
					MaxTurns:    5,
					Provider:    mockProv,
					Tools:       []NamedTool{sendTaskTool},
				},
			},
			{
				Name:        "worker",
				Concurrency: 2,
				DependsOn:   []string{"producer"},
				Agent: &PipelineAgentDef{
					Name:        "worker",
					Instruction: "直接输出：done",
					MaxTurns:    1,
					Provider:    mockProv,
				},
				OnResult: func(result any) {
					mu.Lock()
					results = append(results, fmt.Sprintf("%v", result))
					mu.Unlock()
				},
			},
		},
	}

	p := newPipeline(cfg, &App{provider: mockProv})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("pipeline Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d: %v", len(results), results)
	}
}

// ---------------------------------------------------------------------------
// Test 16: 端到端 — Review=true + 无 Supervisor 自动通过
// ---------------------------------------------------------------------------

func TestPipeline_E2E_ReviewNoSupervisor(t *testing.T) {
	var results []string
	var mu sync.Mutex

	mockProv := provider.NewMockProvider("idea")

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "generator",
				Concurrency: 1,
				Message:     "生成创意",
				Review:      true,
				Agent: &PipelineAgentDef{
					Name:        "generator",
					Instruction: "直接输出：idea",
					MaxTurns:    1,
					Provider:    mockProv,
				},
				OnResult: func(result any) {
					mu.Lock()
					results = append(results, fmt.Sprintf("%v", result))
					mu.Unlock()
				},
			},
		},
		// 无 Supervisor — Review 节点结果自动标记审核完成。
	}

	p := newPipeline(cfg, &App{provider: mockProv})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("pipeline Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0] != "idea" {
		t.Fatalf("expected 'idea', got %q", results[0])
	}
}

// ---------------------------------------------------------------------------
// Test 17: 端到端 — context 取消
// ---------------------------------------------------------------------------

func TestPipeline_E2E_ContextCancel(t *testing.T) {
	mockProv := provider.NewMockProvider("sleeping...")

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "slow",
				Concurrency: 1,
				Message:     "很慢的任务",
				Agent: &PipelineAgentDef{
					Name:        "slow",
					Instruction: "直接输出：sleeping...",
					MaxTurns:    1,
					Provider:    mockProv,
				},
			},
		},
	}

	p := newPipeline(cfg, &App{provider: mockProv})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	err := p.Run(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}

// ---------------------------------------------------------------------------
// Test 18: MessageType/ResultType 零值推断 → 队列创建
// ---------------------------------------------------------------------------

func TestPipeline_E2E_CustomMessageType(t *testing.T) {
	type CustomTask struct {
		ID   string
		Data string
	}

	mockProv := provider.NewMockProvider("ok")

	cfg := PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:        "worker",
				Concurrency: 2,
				MessageType: CustomTask{},
				ResultType:  CustomTask{},
				Agent: &PipelineAgentDef{
					Name:        "worker",
					Instruction: "输出 ok",
					MaxTurns:    1,
					Provider:    mockProv,
				},
				OnResult: func(result any) {
					// result 应该是 string（runWorkerOnce 返回 string）
					_ = result
				},
			},
		},
	}

	p := newPipeline(cfg, &App{provider: mockProv})

	// 验证内部类型推断正确。
	state := p.nodes["worker"]
	if state.msgType != reflect.TypeOf(CustomTask{}) {
		t.Fatalf("expected msgType=CustomTask, got %v", state.msgType)
	}
	if state.resultType != reflect.TypeOf(CustomTask{}) {
		t.Fatalf("expected resultType=CustomTask, got %v", state.resultType)
	}

	// 手动推送任务并关闭队列。
	q := p.msgQueues["worker"]
	q.Push(CustomTask{ID: "1", Data: "test"})
	q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("pipeline Run failed: %v", err)
	}
}
