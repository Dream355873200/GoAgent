// Example: Compaction Only
//
// This demonstrates how to use the compaction package independently,
// without importing the full goagent framework. Useful when you have
// your own agent loop but want Claude Code-grade context compression.
//
// Usage:
//
//	go run main.go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/message"
)

func main() {
	// Create a mock summarizer. In a real app, this would call an LLM.
	summarizer := func(ctx context.Context, text string) (string, error) {
		// Simple mock: just take the first 200 chars as "summary".
		if len(text) > 200 {
			return text[:200] + "...", nil
		}
		return text, nil
	}

	// Create the compaction manager — no goagent import needed.
	mgr := compaction.NewManager(compaction.Config{
		Summarizer:           summarizer, // nil to disable Layer 4
		AutoCompactThreshold: 0.8,
		MaxResultSize:        50000,
	})

	// Simulate a conversation that's getting too long.
	messages := buildLongConversation(50)
	contextWindow := 8000 // small window to trigger compression

	fmt.Printf("Before compression: %d messages\n", len(messages))
	fmt.Printf("Estimated tokens: %d\n", estimateTotal(messages))

	// Apply compression.
	compressed, freed := mgr.Apply(context.Background(), messages, contextWindow)
	fmt.Printf("\nAfter compression: %d messages\n", len(compressed))
	fmt.Printf("Estimated tokens: %d\n", estimateTotal(compressed))
	fmt.Printf("Tokens freed: %d\n", freed)

	// Demonstrate overflow recovery (413 handling).
	fmt.Println("\n--- Overflow Recovery ---")
	recovered, ok := mgr.HandleOverflow(context.Background(), compressed, contextWindow)
	fmt.Printf("Recovery succeeded: %v\n", ok)
	if ok {
		fmt.Printf("Messages after recovery: %d\n", len(recovered))
	}
}

func buildLongConversation(turns int) []message.Message {
	var messages []message.Message
	for i := 0; i < turns; i++ {
		messages = append(messages,
			message.NewUserMessage(fmt.Sprintf("User message %d: %s", i, strings.Repeat("lorem ipsum ", 20))),
			message.NewAssistantMessage(fmt.Sprintf("Assistant response %d: %s", i, strings.Repeat("dolor sit amet ", 20))),
		)
	}
	return messages
}

func estimateTotal(messages []message.Message) int {
	total := 0
	for _, msg := range messages {
		total += message.EstimateTokens(msg)
	}
	return total
}
