// Example: Executor Only
//
// This demonstrates how to use the executor package independently,
// without importing the full goagent framework. Useful when you want
// Claude Code-grade tool execution (concurrent/serial partitioning,
// streaming execution) in your own agent loop.
//
// Usage:
//
//	go run main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Dream355873200/GoAgent/executor"
)

func main() {
	// Create an executor — no goagent import needed.
	exec := executor.New(executor.Config{MaxConcurrency: 5})

	fmt.Println("=== Batch Execution ===")
	batchDemo(exec)

	fmt.Println("\n=== Streaming Execution ===")
	streamingDemo(exec)
}

func batchDemo(exec *executor.Executor) {
	// Define some tool calls — mix of concurrent and serial.
	calls := []executor.ToolCall{
		{
			ID: "1", Name: "read_file",
			Input:      json.RawMessage(`{"path":"main.go"}`),
			Concurrent: true,
			Execute:    mockTool("read_file", 100*time.Millisecond),
		},
		{
			ID: "2", Name: "read_file",
			Input:      json.RawMessage(`{"path":"go.mod"}`),
			Concurrent: true,
			Execute:    mockTool("read_file", 80*time.Millisecond),
		},
		{
			ID: "3", Name: "run_command",
			Input:      json.RawMessage(`{"cmd":"go test ./..."}`),
			Concurrent: false, // serial — not safe to run in parallel
			Execute:    mockTool("run_command", 200*time.Millisecond),
		},
		{
			ID: "4", Name: "read_file",
			Input:      json.RawMessage(`{"path":"README.md"}`),
			Concurrent: true,
			Execute:    mockTool("read_file", 50*time.Millisecond),
		},
	}

	start := time.Now()
	results := exec.Execute(context.Background(), calls)
	elapsed := time.Since(start)

	for _, r := range results {
		status := "ok"
		if r.IsError {
			status = "error"
		}
		fmt.Printf("  [%s] %s: %s (%s)\n", r.ToolUseID, r.Name, r.Content, status)
	}
	fmt.Printf("  Total time: %v (concurrent tools ran in parallel)\n", elapsed)
}

func streamingDemo(exec *executor.Executor) {
	// Simulate streaming: tools arrive one by one as the LLM generates them.
	se := executor.NewStreamingExecutor(exec)
	ctx := context.Background()

	// Simulate LLM streaming — tool calls arrive with delays.
	fmt.Println("  Simulating LLM stream...")

	se.Add(ctx, executor.ToolCall{
		ID: "s1", Name: "read_file",
		Input:      json.RawMessage(`{"path":"config.yaml"}`),
		Concurrent: true,
		Execute:    mockTool("read_file", 150*time.Millisecond),
	})
	fmt.Println("  → Added read_file (concurrent)")

	time.Sleep(50 * time.Millisecond) // simulate streaming delay

	se.Add(ctx, executor.ToolCall{
		ID: "s2", Name: "read_file",
		Input:      json.RawMessage(`{"path":"schema.sql"}`),
		Concurrent: true,
		Execute:    mockTool("read_file", 100*time.Millisecond),
	})
	fmt.Println("  → Added read_file (concurrent)")

	// Poll for completed results while "streaming" continues.
	completed := se.Poll()
	fmt.Printf("  Poll: %d results ready\n", len(completed))

	// Simulate end of LLM stream — wait for all remaining.
	remaining := se.Wait(ctx)
	fmt.Printf("  Wait: %d remaining results\n", len(remaining))

	all := append(completed, remaining...)
	for _, r := range all {
		fmt.Printf("  [%s] %s: %s\n", r.ToolUseID, r.Name, r.Content)
	}
}

// mockTool creates a tool function that simulates work with a given delay.
func mockTool(name string, delay time.Duration) executor.ExecuteFn {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		time.Sleep(delay)
		return fmt.Sprintf("%s completed in %v", name, delay), nil
	}
}
