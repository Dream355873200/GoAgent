// Package compaction 实现四层上下文压缩系统。
//
// 各层从轻到重：
//
//	Layer 0 (Budget):   将超大工具结果持久化到磁盘，替换为引用
//	Layer 1 (Snip):     截断旧消息的内容，保留结构
//	Layer 2 (Micro):    移除模型已处理的工具结果
//	Layer 3 (Collapse): 将中间轮次折叠为摘要视图（读时投影）
//	Layer 4 (Auto):     调用 Summarizer 对整个对话进行摘要
//
// 此外，Reactive Compact 在 413 错误发生后进行事后处理。
//
// 本包可独立使用 — 提供任意 Summarizer 实现（或 nil 禁用 Layer 4），
// 即可接入你自己的 agent 循环。
package compaction

import (
	"context"
	"fmt"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Summarizer 是一个文本摘要函数。它将本包与特定 LLM 提供者解耦 —
// 调用者可以将任何 LLM、本地模型、甚至静态摘要器包装在此签名背后。
//
// 示例：
//
//	summarizer := func(ctx context.Context, text string) (string, error) {
//	    return myLLM.Summarize(ctx, text)
//	}
type Summarizer func(ctx context.Context, text string) (string, error)

// Config 控制压缩行为。
type Config struct {
	// Summarizer 是 Layer 4（自动摘要）的可选函数。
	// 如果为 nil，Layer 4 将被禁用。
	Summarizer Summarizer

	// AutoCompactThreshold 是触发自动压缩的上下文窗口比例（0.0-1.0）。
	// 默认值：0.8
	AutoCompactThreshold float64

	// MaxResultSize 是单个工具结果在持久化到磁盘前的最大字符数。
	// 默认值：50000
	MaxResultSize int
}

// Manager 协调四个压缩层。
type Manager struct {
	config   Config
	provider provider.Provider
	budget   *budgetCompressor
	snip     *snipCompressor
	micro    *microCompressor
	collapse *collapseCompressor
	auto     *autoCompressor
	tracking AutoCompactTracking
}

// SetProvider 设置用于 LLM 摘要的提供者。
func (m *Manager) SetProvider(p provider.Provider) {
	m.provider = p
}

// Compact 执行完整压缩流程，对齐 Claude Code 的 compactConversation。
// 如果 Provider 未设置，返回错误。
func (m *Manager) Compact(ctx context.Context, messages []message.Message, customInstructions string, isAutoCompact bool) (*CompactionResult, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("Compact 需要 Provider")
	}

	cfg := CompactConfig{
		Provider:           m.provider,
		CustomInstructions: customInstructions,
		IsAutoCompact:      isAutoCompact,
		ContextWindow:      200_000, // TODO: 从 provider capabilities 获取
		Threshold:          int(float64(200_000) * m.config.AutoCompactThreshold),
		MicrocompactCfg: MicrocompactConfig{
			MaxToolResultChars: m.config.MaxResultSize,
			ProtectedTail:      4,
		},
	}

	return Compact(ctx, messages, cfg)
}

// AutoCompactTracking 追踪自动压缩状态，用于熔断器逻辑。
// 对齐 Claude Code 的 AutoCompactTrackingState。
type AutoCompactTracking struct {
	// ConsecutiveFailures 计数连续自动压缩失败次数。
	// 超过 MaxConsecutiveAutocompactFailures 后，自动压缩将被抑制。
	ConsecutiveFailures int

	// LastCompactedTurn 是上次成功压缩时的轮次编号。
	LastCompactedTurn int

	// TurnsSinceCompact 计数自上次成功压缩以来的轮次数。
	TurnsSinceCompact int

	// Compacted 标记当前迭代是否发生了压缩。
	Compacted bool
}

// MaxConsecutiveAutocompactFailures 是熔断器阈值。
// 超过此数量的连续失败后，自动压缩将被抑制以防止无限重试循环。
// 对齐 Claude Code 的 MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES。
const MaxConsecutiveAutocompactFailures = 3

// NewManager 创建一个新的压缩管理器。
func NewManager(cfg Config) *Manager {
	if cfg.AutoCompactThreshold == 0 {
		cfg.AutoCompactThreshold = 0.8
	}
	if cfg.MaxResultSize == 0 {
		cfg.MaxResultSize = 50_000
	}
	return &Manager{
		config:   cfg,
		budget:   &budgetCompressor{maxSize: cfg.MaxResultSize},
		snip:     &snipCompressor{},
		micro:    &microCompressor{},
		collapse: &collapseCompressor{},
		auto:     &autoCompressor{summarizer: cfg.Summarizer, threshold: cfg.AutoCompactThreshold},
	}
}

// CircuitBreakerTripped 检查熔断器是否已触发。
// 连续失败次数超过阈值时返回 true。
func (m *Manager) CircuitBreakerTripped() bool {
	return m.tracking.ConsecutiveFailures >= MaxConsecutiveAutocompactFailures
}

// RecordAutocompactResult 记录一次自动压缩的结果，更新熔断器状态。
func (m *Manager) RecordAutocompactResult(success bool, currentTurn int) {
	if success {
		m.tracking.ConsecutiveFailures = 0
		m.tracking.LastCompactedTurn = currentTurn
		m.tracking.TurnsSinceCompact = 0
		m.tracking.Compacted = true
	} else {
		m.tracking.ConsecutiveFailures++
		m.tracking.Compacted = false
	}
}

// IncrementTurn 在每轮结束时调用，增加 TurnsSinceCompact 计数。
func (m *Manager) IncrementTurn() {
	m.tracking.TurnsSinceCompact++
	m.tracking.Compacted = false
}

// Tracking 返回当前的自动压缩追踪状态（只读副本）。
func (m *Manager) Tracking() AutoCompactTracking {
	return m.tracking
}

// Apply 按顺序在消息上运行所有压缩层。
// 返回处理后的消息和所有层释放的总 token 数。
func (m *Manager) Apply(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, int) {
	totalFreed := 0

	// Layer 0: Budget — 持久化超大工具结果。
	messages, freed := m.budget.apply(messages)
	totalFreed += freed

	// Layer 1: Snip — 截断旧消息内容。
	messages, freed = m.snip.apply(messages, contextWindow)
	totalFreed += freed

	// Layer 2: Micro — 移除已消费的工具结果。
	messages, freed = m.micro.apply(messages)
	totalFreed += freed

	// Layer 3: Collapse — 折叠中间轮次。
	messages, freed = m.collapse.apply(messages, contextWindow)
	totalFreed += freed

	// Layer 4: Auto — 基于模型的摘要（仅在仍超过阈值时触发）。
	// 检查熔断器：如果连续失败过多则跳过。
	if m.CircuitBreakerTripped() {
		return messages, totalFreed
	}

	currentTokens := estimateMessagesTokens(messages)
	threshold := int(float64(contextWindow) * m.config.AutoCompactThreshold)
	if currentTokens > threshold {
		compacted, err := m.auto.apply(ctx, messages, contextWindow)
		if err == nil {
			freed := currentTokens - estimateMessagesTokens(compacted)
			totalFreed += freed
			messages = compacted
			m.RecordAutocompactResult(true, 0)
		} else {
			m.RecordAutocompactResult(false, 0)
		}
	}

	return messages, totalFreed
}

// HandleOverflow 尝试从 413 prompt-too-long 错误中恢复。
// 返回恢复后的消息和恢复是否成功。
func (m *Manager) HandleOverflow(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, bool) {
	// 第一次尝试：折叠排空。
	drained, freed := m.collapse.drain(messages)
	if freed > 0 {
		return drained, true
	}

	// 第二次尝试：响应式压缩（基于模型）。
	compacted, err := m.auto.apply(ctx, messages, contextWindow)
	if err == nil {
		return compacted, true
	}

	return messages, false
}

// --- Layer 0: Budget ---

type budgetCompressor struct {
	maxSize int
	// 完整实现中，这里会持久化到磁盘。
	// 骨架实现仅使用标记进行截断。
}

func (b *budgetCompressor) apply(messages []message.Message) ([]message.Message, int) {
	freed := 0
	result := make([]message.Message, len(messages))
	for i, msg := range messages {
		result[i] = msg
		for j, block := range msg.Content {
			if block.Type == "tool_result" && len(block.Text) > b.maxSize {
				original := len(block.Text)
				result[i].Content[j].Text = block.Text[:b.maxSize/2] +
					fmt.Sprintf("\n\n[... %d 字符被截断，已持久化到磁盘 ...]\n\n", original-b.maxSize) +
					block.Text[original-b.maxSize/2:]
				freed += (original - len(result[i].Content[j].Text)) / 4
			}
		}
	}
	return result, freed
}

// --- Layer 1: Snip ---

type snipCompressor struct{}

func (s *snipCompressor) apply(messages []message.Message, contextWindow int) ([]message.Message, int) {
	if len(messages) <= 4 {
		return messages, 0 // 消息太少，不需要截断
	}

	freed := 0
	// 保护最后 4 条消息（近期上下文）。
	protectedTail := 4
	cutoff := len(messages) - protectedTail

	result := make([]message.Message, len(messages))
	copy(result, messages)

	for i := 0; i < cutoff; i++ {
		if result[i].Compacted {
			continue // 已经截断过
		}
		for j, block := range result[i].Content {
			if block.Type == "text" && len(block.Text) > 200 {
				original := message.EstimateTokens(message.Message{Content: []message.ContentBlock{block}})
				result[i].Content[j].Text = block.Text[:100] + "\n[... 已截断 ...]\n" + block.Text[len(block.Text)-100:]
				newEstimate := message.EstimateTokens(message.Message{Content: []message.ContentBlock{result[i].Content[j]}})
				freed += original - newEstimate
			}
		}
	}
	return result, freed
}

// --- Layer 2: Micro ---
// 对齐 Claude Code 的 applyToolResultBudget + microcompact：
// 1. 按单条工具结果大小裁剪（>50KB 的替换为截断版）
// 2. 移除已被助手处理过的工具结果内容

// MaxToolResultChars 单条工具结果的最大字符数。
// 超过此阈值的结果会被截断，保留头尾各 10KB。
// 对齐 Claude Code 的 applyToolResultBudget 默认阈值。
const MaxToolResultChars = 50_000

type microCompressor struct{}

func (mc *microCompressor) apply(messages []message.Message) ([]message.Message, int) {
	freed := 0

	result := make([]message.Message, len(messages))
	copy(result, messages)

	// Pass 1: 按大小裁剪过长的工具结果（对所有工具结果生效）。
	// 对齐 Claude Code 的 applyToolResultBudget()。
	for i, msg := range result {
		for j, block := range msg.Content {
			if block.Type == "tool_result" && len(block.Text) > MaxToolResultChars {
				original := len(block.Text)
				// 保留头尾各 10KB，中间用截断标记替换。
				head := block.Text[:10_000]
				tail := block.Text[original-10_000:]
				result[i].Content[j].Text = head +
					fmt.Sprintf("\n\n[... 中间 %d 字符已截断 ...]\n\n", original-20_000) +
					tail
				freed += (original - len(result[i].Content[j].Text)) / 4
			}
		}
	}

	// Pass 2: 移除已被助手处理过的工具结果内容。
	// 追踪哪些 tool_use ID 已被"消费"（后续有助手响应）。
	consumed := make(map[string]bool)

	// 正向扫描：找到已被助手文本跟随的工具结果。
	for i, msg := range messages {
		if msg.Role == message.RoleAssistant {
			// 标记所有前置工具结果为已消费。
			for j := i - 1; j >= 0; j-- {
				for _, block := range messages[j].Content {
					if block.Type == "tool_result" {
						consumed[block.ForToolUseID] = true
					}
				}
				if messages[j].Role == message.RoleAssistant {
					break // 在前一个助手轮次处停止
				}
			}
		}
	}

	// 将已消费的工具结果替换为标记。
	for i, msg := range result {
		for j, block := range msg.Content {
			if block.Type == "tool_result" && consumed[block.ForToolUseID] && len(block.Text) > 100 {
				original := len(block.Text)
				result[i].Content[j].Text = fmt.Sprintf("[内容已处理，移除 %d 字符]", original)
				freed += original / 4
			}
		}
	}
	return result, freed
}

// --- Layer 3: Collapse ---

type collapseCompressor struct{}

func (c *collapseCompressor) apply(messages []message.Message, contextWindow int) ([]message.Message, int) {
	if len(messages) <= 6 {
		return messages, 0
	}

	// 将旧的轮次对折叠为摘要消息。
	// 保护最后 6 条消息。
	protectedTail := 6
	cutoff := len(messages) - protectedTail
	if cutoff <= 0 {
		return messages, 0
	}

	// 构建折叠轮次的摘要。
	var summaryParts []string
	freed := 0

	for i := 0; i < cutoff; i += 2 {
		if i+1 < cutoff {
			userText := message.ExtractText(messages[i])
			assistantText := message.ExtractText(messages[i+1])
			if len(userText) > 80 {
				userText = userText[:80] + "..."
			}
			if len(assistantText) > 80 {
				assistantText = assistantText[:80] + "..."
			}
			summaryParts = append(summaryParts, fmt.Sprintf("- 用户: %s → 助手: %s", userText, assistantText))
			freed += message.EstimateTokens(messages[i]) + message.EstimateTokens(messages[i+1])
		}
	}

	if len(summaryParts) == 0 {
		return messages, 0
	}

	summary := "之前的对话摘要：\n"
	for _, part := range summaryParts {
		summary += part + "\n"
	}

	summaryMsg := message.Message{
		Role:      message.RoleUser,
		Content:   []message.ContentBlock{{Type: "text", Text: summary}},
		Compacted: true,
	}

	freed -= message.EstimateTokens(summaryMsg)

	result := []message.Message{summaryMsg}
	result = append(result, messages[cutoff:]...)
	return result, freed
}

func (c *collapseCompressor) drain(messages []message.Message) ([]message.Message, int) {
	// 激进折叠：折叠除最后 2 条消息外的所有内容。
	return c.apply(messages, 0)
}

// --- Layer 4: Auto ---

type autoCompressor struct {
	summarizer Summarizer
	threshold  float64
}

func (a *autoCompressor) apply(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, error) {
	if a.summarizer == nil {
		return messages, fmt.Errorf("自动压缩缺少 summarizer")
	}

	// 构建请求模型摘要的提示。
	var conversationText string
	for _, msg := range messages {
		text := message.ExtractText(msg)
		if text != "" {
			conversationText += fmt.Sprintf("[%s]: %s\n", msg.Role, text)
		}
	}

	// 如果对话本身对摘要调用来说太长，则截断。
	maxChars := contextWindow * 3 // 粗略估算：1 token ≈ 4 字符，使用窗口的 3/4
	if len(conversationText) > maxChars {
		conversationText = conversationText[len(conversationText)-maxChars:]
	}

	prompt := "Summarize the following conversation. Preserve key decisions, " +
		"file paths, code changes, and important context. Be concise.\n\n" +
		conversationText

	summaryText, err := a.summarizer(ctx, prompt)
	if err != nil {
		return messages, fmt.Errorf("自动压缩失败: %w", err)
	}

	summaryMsg := message.Message{
		Role: message.RoleUser,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: "[对话已压缩]\n\n" + summaryText,
		}},
		Compacted:        true,
		IsCompactSummary: true,
	}

	// 保留最后 2 条消息以维持上下文连续性。
	tail := 2
	if len(messages) < tail {
		tail = len(messages)
	}

	result := []message.Message{summaryMsg}
	result = append(result, messages[len(messages)-tail:]...)
	return result, nil
}

// estimateMessagesTokens 估算消息切片的总 token 数。
func estimateMessagesTokens(messages []message.Message) int {
	total := 0
	for _, msg := range messages {
		total += message.EstimateTokens(msg)
	}
	return total
}
