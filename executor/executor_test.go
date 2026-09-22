package executor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// effect 兼容推导：旧 bool 字段 → Class。
func TestEffectCompat(t *testing.T) {
	if got := (ToolCall{Concurrent: true}).effect(); got != ClassParallel {
		t.Errorf("Concurrent=true 应推导为 Parallel, got %v", got)
	}
	if got := (ToolCall{}).effect(); got != ClassExclusive {
		t.Errorf("零值应推导为 Exclusive, got %v", got)
	}
	if got := (ToolCall{Concurrent: true, Class: ClassReadOnly}).effect(); got != ClassReadOnly {
		t.Errorf("显式 Class 应优先于兼容字段, got %v", got)
	}
}

// partition：独占工具各自成批，只读/并行工具连续合批。
func TestPartitionClasses(t *testing.T) {
	e := New(Config{})
	calls := []ToolCall{
		{Name: "r1", Class: ClassReadOnly},
		{Name: "p1", Class: ClassParallel},
		{Name: "w1", Class: ClassExclusive},
		{Name: "p2", Class: ClassParallel},
		{Name: "p3", Class: ClassParallel},
	}
	batches := e.partition(calls)
	if len(batches) != 3 {
		t.Fatalf("应为 3 批 [r1,p1] [w1] [p2,p3], got %d", len(batches))
	}
	if batches[0][0].Name != "r1" || batches[0][1].Name != "p1" ||
		batches[1][0].Name != "w1" || batches[2][0].Name != "p2" || batches[2][1].Name != "p3" {
		t.Fatalf("分批内容不符: %v", batches)
	}
}

// 调度：只读工具可在独占工具运行期间启动；并行工具不能。
func TestStreamingReadOnlyOverlapsExclusive(t *testing.T) {
	e := New(Config{MaxConcurrency: 4})
	se := NewStreamingExecutor(e)

	var mu sync.Mutex
	running := 0
	maxSeen := 0

	block := make(chan struct{})
	started := make(chan string, 8)

	slowExclusive := ToolCall{
		ID: "w1", Name: "write", Class: ClassExclusive, Input: raw(t, nil),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			started <- "write"
			<-block // 阻塞直到测试放行
			return "ok", nil
		},
	}
	readOnly := ToolCall{
		ID: "r1", Name: "read", Class: ClassReadOnly, Input: raw(t, nil),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			mu.Lock()
			running++
			if running > maxSeen {
				maxSeen = running
			}
			mu.Unlock()
			started <- "read"
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return "ok", nil
		},
	}

	se.Add(context.Background(), slowExclusive)
	<-started // write 已启动
	// 独占运行期间：只读应能启动，并行安全不能。
	se.Add(context.Background(), readOnly)
	se.Add(context.Background(), ToolCall{
		ID: "p1", Name: "parallel", Class: ClassParallel, Input: raw(t, nil),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			started <- "parallel"
			return "ok", nil
		},
	})

	select {
	case s := <-started:
		if s != "read" {
			t.Fatalf("只读工具应在独占工具运行期间启动, got %s", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("只读工具未被调度")
	}
	if len(started) > 0 {
		t.Fatal("并行安全工具不应在独占工具运行期间启动")
	}
	close(block)

	results := se.Wait(context.Background())
	if len(results) != 3 {
		t.Fatalf("应有 3 个结果, got %d", len(results))
	}
}

// 独占工具独占执行：与并行工具同时提交时不重叠。
func TestStreamingExclusiveWaits(t *testing.T) {
	e := New(Config{MaxConcurrency: 4})
	se := NewStreamingExecutor(e)

	release := make(chan struct{})
	started := make(chan string, 8)
	order := []string{}
	var mu sync.Mutex

	se.Add(context.Background(), ToolCall{
		ID: "p1", Name: "par", Class: ClassParallel, Input: raw(t, nil),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			mu.Lock(); order = append(order, "par-start"); mu.Unlock()
			started <- "par"
			<-release
			return "ok", nil
		},
	})
	<-started
	se.Add(context.Background(), ToolCall{
		ID: "w1", Name: "write", Class: ClassExclusive, Input: raw(t, nil),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			mu.Lock(); order = append(order, "write"); mu.Unlock()
			return "ok", nil
		},
	})

	select {
	case s := <-started:
		if s == "write" {
			t.Fatal("独占工具不应在并行工具运行期间启动")
		}
		t.Fatalf("意外启动: %s", s)
	case <-time.After(150 * time.Millisecond):
		// 预期：独占工具被阻塞
	}
	close(release)
	se.Wait(context.Background())
	mu.Lock()
	defer mu.Unlock()
	for i, o := range order {
		if o == "write" && i != len(order)-1 {
			t.Fatalf("独占工具应最后执行, order=%v", order)
		}
	}
}
