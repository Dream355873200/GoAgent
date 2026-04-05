// Package sessionmem 实现会话内定期记忆提取。
//
// 对齐 Claude Code 的 src/services/SessionMemory/：
//   - 在长对话中定期 fork 轻量 agent 提取关键信息到 session_memory.md
//   - 双阈值触发：token 增长 + 工具调用次数
//   - 防止重要上下文在 autocompact 时丢失
//   - 与 compaction 集成：可作为轻量级替代方案
//
// 使用方式：由 loop 在每次 API 响应后调用 SessionMemory.MaybeExtract()。
package sessionmem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic-community/goagent/message"
	"github.com/anthropic-community/goagent/provider"
)

// Config 配置 SessionMemory 的提取阈值。
type Config struct {
	// MinTokensToInit 是首次激活 SessionMemory 所需的最低 token 数。默认 10000。
	MinTokensToInit int
	// MinTokensBetweenUpdate 是两次提取之间所需的最小 token 增长。默认 5000。
	MinTokensBetweenUpdate int
	// ToolCallsBetweenUpdates 是两次提取之间所需的最小工具调用次数。默认 3。
	ToolCallsBetweenUpdates int
	// MemoryDir 是会话记忆文件存放目录。
	MemoryDir string
	// MaxSectionTokens 是单个 section 的最大 token 数。默认 2000。
	MaxSectionTokens int
	// MaxTotalTokens 是整个记忆文件的最大 token 数。默认 12000。
	MaxTotalTokens int
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		MinTokensToInit:         10000,
		MinTokensBetweenUpdate:  5000,
		ToolCallsBetweenUpdates: 3,
		MaxSectionTokens:        2000,
		MaxTotalTokens:          12000,
	}
}

// SessionMemory 管理会话内的定期记忆提取。
type SessionMemory struct {
	mu    sync.Mutex
	cfg   Config
	prov  provider.Provider
	model string // 可选：指定提取模型

	// 状态追踪。
	initialized          bool
	tokensAtLastExtract  int
	lastExtractMsgIndex  int // 上次提取时的消息数
	extractionInProgress bool
	extractionStartedAt  time.Time
}

// New 创建一个新的 SessionMemory。
func New(prov provider.Provider, cfg Config) *SessionMemory {
	if cfg.MinTokensToInit == 0 {
		cfg.MinTokensToInit = DefaultConfig().MinTokensToInit
	}
	if cfg.MinTokensBetweenUpdate == 0 {
		cfg.MinTokensBetweenUpdate = DefaultConfig().MinTokensBetweenUpdate
	}
	if cfg.ToolCallsBetweenUpdates == 0 {
		cfg.ToolCallsBetweenUpdates = DefaultConfig().ToolCallsBetweenUpdates
	}
	if cfg.MaxSectionTokens == 0 {
		cfg.MaxSectionTokens = DefaultConfig().MaxSectionTokens
	}
	if cfg.MaxTotalTokens == 0 {
		cfg.MaxTotalTokens = DefaultConfig().MaxTotalTokens
	}
	return &SessionMemory{
		cfg:  cfg,
		prov: prov,
	}
}

// WithModel 指定用于提取的模型。
func (sm *SessionMemory) WithModel(model string) *SessionMemory {
	sm.model = model
	return sm
}

// MaybeExtract 检查是否需要提取记忆，如果需要则异步执行。
// 在每次 API 响应+工具执行后调用。
// 非阻塞：在后台 goroutine 中执行提取。
func (sm *SessionMemory) MaybeExtract(ctx context.Context, messages []message.Message) {
	if sm == nil || sm.prov == nil {
		return
	}

	sm.mu.Lock()
	if sm.extractionInProgress {
		// 已有提取在进行中，跳过。
		// 超过 1 分钟的提取视为过期。
		if time.Since(sm.extractionStartedAt) < time.Minute {
			sm.mu.Unlock()
			return
		}
		// 过期的提取，重置状态。
		sm.extractionInProgress = false
	}

	if !sm.shouldExtract(messages) {
		sm.mu.Unlock()
		return
	}

	sm.extractionInProgress = true
	sm.extractionStartedAt = time.Now()
	sm.mu.Unlock()

	// 异步执行提取。
	go func() {
		defer func() {
			sm.mu.Lock()
			sm.extractionInProgress = false
			sm.mu.Unlock()
		}()
		_ = sm.extract(ctx, messages)
	}()
}

// Extract 同步执行一次记忆提取（用于手动触发 /summary）。
func (sm *SessionMemory) Extract(ctx context.Context, messages []message.Message) error {
	return sm.extract(ctx, messages)
}

// shouldExtract 判断是否需要提取。
// 调用前必须持有 sm.mu 锁。
func (sm *SessionMemory) shouldExtract(messages []message.Message) bool {
	if len(messages) < 3 {
		return false
	}

	currentTokens := estimateMessagesTokens(messages)

	// 初始化阶段：等到 token 数达到阈值。
	if !sm.initialized {
		if currentTokens < sm.cfg.MinTokensToInit {
			return false
		}
		sm.initialized = true
	}

	// 检查 token 增长阈值。
	tokenGrowth := currentTokens - sm.tokensAtLastExtract
	hasMetTokenThreshold := tokenGrowth >= sm.cfg.MinTokensBetweenUpdate

	if !hasMetTokenThreshold {
		return false // token 阈值是硬性要求
	}

	// 检查工具调用次数阈值。
	toolCallsSince := countToolCallsSince(messages, sm.lastExtractMsgIndex)
	hasMetToolCallThreshold := toolCallsSince >= sm.cfg.ToolCallsBetweenUpdates

	// 检查最后一轮是否有工具调用（自然断点）。
	hasToolCallsInLastTurn := hasToolCallsInLastAssistantTurn(messages)

	// 触发条件：
	// 1. token 阈值 AND 工具调用阈值 都满足，或
	// 2. token 阈值满足 AND 最后一轮无工具调用（自然会话断点）
	return (hasMetTokenThreshold && hasMetToolCallThreshold) ||
		(hasMetTokenThreshold && !hasToolCallsInLastTurn)
}

// extract 执行实际的记忆提取。
func (sm *SessionMemory) extract(ctx context.Context, messages []message.Message) error {
	memoryPath := sm.memoryFilePath()

	// 确保目录存在。
	dir := filepath.Dir(memoryPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建会话记忆目录失败: %w", err)
	}

	// 读取已有记忆内容。
	currentMemory := ""
	if data, err := os.ReadFile(memoryPath); err == nil {
		currentMemory = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取会话记忆失败: %w", err)
	}

	// 如果没有已有记忆，使用模板。
	if currentMemory == "" {
		currentMemory = defaultTemplate
	}

	// 构建提取 prompt。
	prompt := buildUpdatePrompt(currentMemory, memoryPath, sm.cfg)

	// 调用 LLM 提取。
	req := &provider.Request{
		SystemPrompt: extractSystemPrompt,
		Messages: []message.Message{
			// 注入对话上下文。
			buildConversationContext(messages),
			// 提取指令。
			message.NewUserMessage(prompt),
		},
		MaxTokens: 4096,
	}
	if sm.model != "" {
		req.Model = sm.model
	}

	resp, err := sm.prov.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("LLM 提取会话记忆失败: %w", err)
	}

	extracted := message.ExtractText(resp.Message)
	if strings.TrimSpace(extracted) == "" {
		return nil
	}

	// 解析 LLM 输出：它应该是更新后的完整 markdown 文件。
	// 如果输出看起来像完整的 markdown（含 # 标题），直接使用。
	// 否则尝试将其合并到现有模板。
	updatedMemory := extracted
	if !strings.Contains(extracted, "# Session Title") && !strings.Contains(extracted, "# Current State") {
		// LLM 没有返回完整模板，尝试用编辑指令更新。
		updatedMemory = applyEdits(currentMemory, extracted)
	}

	// 截断超长 section。
	updatedMemory = truncateForLimits(updatedMemory, sm.cfg.MaxSectionTokens, sm.cfg.MaxTotalTokens)

	// 写入文件。
	if err := os.WriteFile(memoryPath, []byte(updatedMemory), 0644); err != nil {
		return fmt.Errorf("写入会话记忆失败: %w", err)
	}

	// 更新状态。
	sm.mu.Lock()
	sm.tokensAtLastExtract = estimateMessagesTokens(messages)
	sm.lastExtractMsgIndex = len(messages)
	sm.mu.Unlock()

	return nil
}

// memoryFilePath 返回会话记忆文件路径。
func (sm *SessionMemory) memoryFilePath() string {
	if sm.cfg.MemoryDir != "" {
		return filepath.Join(sm.cfg.MemoryDir, "MEMORY.md")
	}
	return filepath.Join(".yume", ".session-memory", "MEMORY.md")
}

// LoadContent 返回当前会话记忆内容。
func (sm *SessionMemory) LoadContent() string {
	data, err := os.ReadFile(sm.memoryFilePath())
	if err != nil {
		return ""
	}
	return string(data)
}

// IsEmpty 检查会话记忆是否为空（匹配模板）。
func (sm *SessionMemory) IsEmpty() bool {
	content := sm.LoadContent()
	return content == "" || strings.TrimSpace(content) == strings.TrimSpace(defaultTemplate)
}

// WaitForExtraction 等待正在进行的提取完成（最多 15 秒）。
func (sm *SessionMemory) WaitForExtraction(timeout time.Duration) {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		sm.mu.Lock()
		inProgress := sm.extractionInProgress
		sm.mu.Unlock()
		if !inProgress || time.Now().After(deadline) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TruncateForCompact 截断超长 section 用于 compact 插入。
// 返回截断后内容和是否发生了截断。
func TruncateForCompact(content string, maxSectionChars int) (string, bool) {
	if maxSectionChars == 0 {
		maxSectionChars = 2000 * 4 // 对应 ~2000 tokens
	}

	lines := strings.Split(content, "\n")
	var outputLines []string
	var currentSectionHeader string
	var currentSectionLines []string
	wasTruncated := false

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			// 刷新上一个 section。
			out, truncated := flushSection(currentSectionHeader, currentSectionLines, maxSectionChars)
			outputLines = append(outputLines, out...)
			wasTruncated = wasTruncated || truncated
			currentSectionHeader = line
			currentSectionLines = nil
		} else {
			currentSectionLines = append(currentSectionLines, line)
		}
	}
	// 刷新最后一个 section。
	out, truncated := flushSection(currentSectionHeader, currentSectionLines, maxSectionChars)
	outputLines = append(outputLines, out...)
	wasTruncated = wasTruncated || truncated

	return strings.Join(outputLines, "\n"), wasTruncated
}

// ── 内部辅助函数 ──────────────────────────────────────────────────

// estimateMessagesTokens 估算消息列表的 token 数。
func estimateMessagesTokens(messages []message.Message) int {
	total := 0
	for _, msg := range messages {
		total += message.EstimateTokens(msg)
	}
	return total
}

// countToolCallsSince 统计 sinceIndex 之后的工具调用次数。
func countToolCallsSince(messages []message.Message, sinceIndex int) int {
	count := 0
	for i := sinceIndex; i < len(messages); i++ {
		if messages[i].Role != message.RoleAssistant {
			continue
		}
		for _, block := range messages[i].Content {
			if block.Type == "tool_use" {
				count++
			}
		}
	}
	return count
}

// hasToolCallsInLastAssistantTurn 检查最后一个 assistant 轮次是否有工具调用。
func hasToolCallsInLastAssistantTurn(messages []message.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.RoleAssistant {
			continue
		}
		for _, block := range messages[i].Content {
			if block.Type == "tool_use" {
				return true
			}
		}
		return false // 找到最后一个 assistant 消息，检查完毕
	}
	return false
}

// buildConversationContext 构建对话上下文消息（截断过长的单条消息）。
func buildConversationContext(messages []message.Message) message.Message {
	var sb strings.Builder
	sb.WriteString("以下是当前对话的记录，请从中提取关键信息更新会话记忆。\n\n")

	for _, msg := range messages {
		role := string(msg.Role)
		text := message.ExtractText(msg)
		if text == "" {
			// 对于工具调用/结果，也包含简要信息。
			for _, block := range msg.Content {
				switch block.Type {
				case "tool_use":
					sb.WriteString(fmt.Sprintf("[%s] 调用工具: %s\n", role, block.ToolName))
				case "tool_result":
					result := block.Text
					if len(result) > 500 {
						result = result[:500] + "..."
					}
					sb.WriteString(fmt.Sprintf("[tool_result] %s\n", result))
				}
			}
			continue
		}
		if len(text) > 3000 {
			text = text[:3000] + "..."
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", role, text))
	}

	return message.NewUserMessage(sb.String())
}

// flushSection 刷新一个 section，必要时截断。
func flushSection(header string, lines []string, maxChars int) ([]string, bool) {
	if header == "" {
		return lines, false
	}

	content := strings.Join(lines, "\n")
	if len(content) <= maxChars {
		result := make([]string, 0, len(lines)+1)
		result = append(result, header)
		result = append(result, lines...)
		return result, false
	}

	// 截断到行边界。
	var keptLines []string
	keptLines = append(keptLines, header)
	charCount := 0
	for _, line := range lines {
		if charCount+len(line)+1 > maxChars {
			break
		}
		keptLines = append(keptLines, line)
		charCount += len(line) + 1
	}
	keptLines = append(keptLines, "\n[... section 因长度被截断 ...]")
	return keptLines, true
}

// truncateForLimits 截断超限的 section。
func truncateForLimits(content string, maxSectionTokens, maxTotalTokens int) string {
	maxSectionChars := maxSectionTokens * 4
	truncated, _ := TruncateForCompact(content, maxSectionChars)

	// 检查总 token 数。
	totalChars := len(truncated)
	maxTotalChars := maxTotalTokens * 4
	if totalChars > maxTotalChars {
		// 需要进一步截断，保留前 maxTotalChars 个字符。
		truncated = truncated[:maxTotalChars] + "\n\n[... 记忆文件因总长度限制被截断 ...]"
	}
	return truncated
}

// applyEdits 尝试将 LLM 的编辑输出合并到已有模板。
// 如果 LLM 返回了 section 更新片段，尝试匹配并替换。
func applyEdits(existing, edits string) string {
	// 简单策略：如果编辑内容包含 section 标题，按 section 替换。
	editSections := parseSections(edits)
	if len(editSections) == 0 {
		return existing // 无法解析，返回原内容
	}

	existingSections := parseSections(existing)
	for header, content := range editSections {
		existingSections[header] = content
	}

	return rebuildFromSections(existing, existingSections)
}

// parseSections 解析 markdown 的 section。
func parseSections(content string) map[string]string {
	sections := make(map[string]string)
	lines := strings.Split(content, "\n")
	var currentHeader string
	var currentLines []string

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			if currentHeader != "" {
				sections[currentHeader] = strings.Join(currentLines, "\n")
			}
			currentHeader = line
			currentLines = nil
		} else {
			currentLines = append(currentLines, line)
		}
	}
	if currentHeader != "" {
		sections[currentHeader] = strings.Join(currentLines, "\n")
	}
	return sections
}

// rebuildFromSections 按原始顺序重建 markdown。
func rebuildFromSections(original string, sections map[string]string) string {
	lines := strings.Split(original, "\n")
	var result []string
	var currentHeader string
	skipUntilNextHeader := false

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			currentHeader = line
			if content, ok := sections[currentHeader]; ok {
				result = append(result, currentHeader)
				result = append(result, content)
				skipUntilNextHeader = true
				delete(sections, currentHeader)
			} else {
				skipUntilNextHeader = false
				result = append(result, line)
			}
		} else if !skipUntilNextHeader {
			result = append(result, line)
		}
	}

	// 添加新增的 section（不在原始内容中的）。
	for header, content := range sections {
		result = append(result, header)
		result = append(result, content)
	}

	return strings.Join(result, "\n")
}

// ── 模板和 Prompt ──────────────────────────────────────────────────

const defaultTemplate = `
# Session Title
_简短且有区分度的 5-10 个词描述会话标题_

# Current State
_当前正在做什么？尚未完成的待办任务。下一步计划。_

# Task specification
_用户要求构建什么？设计决策或其他背景信息_

# Files and Functions
_重要的文件有哪些？它们包含什么内容，为什么相关？_

# Workflow
_通常运行哪些命令？以什么顺序？如何解读输出？_

# Errors & Corrections
_遇到的错误及修复方式。用户纠正了什么？哪些方案失败了不应再尝试？_

# Codebase and System Documentation
_重要的系统组件有哪些？它们如何工作/配合？_

# Learnings
_什么有效？什么无效？应该避免什么？不要与其他 section 重复_

# Key results
_如果用户要求特定输出（如答案、表格或文档），在此处重复精确结果_

# Worklog
_逐步记录尝试了什么、做了什么。每步非常简洁的总结_
`

const extractSystemPrompt = `你是一个会话记忆助手。你的任务是根据对话记录更新会话记忆文件。

规则：
1. 保持文件的精确结构，所有 section 标题和斜体描述必须保持不变
2. 只更新每个 section 中斜体描述之后的实际内容
3. 不要添加新的 section 或删除现有 section
4. 写入详细、信息密集的内容——包括文件路径、函数名、错误信息、具体命令等
5. 每个 section 保持在 ~2000 tokens 以内
6. 重点是可操作的、具体的信息
7. 始终更新 "Current State" 以反映最新工作状态
8. 如果某个 section 没有新信息，不要填充无用内容，保持空白即可
9. 直接输出完整的更新后的 markdown 文件，不要添加解释或前言`

// buildUpdatePrompt 构建提取更新 prompt。
func buildUpdatePrompt(currentMemory, memoryPath string, cfg Config) string {
	var sb strings.Builder

	sb.WriteString("请根据上面的对话记录，更新会话记忆文件。\n\n")
	sb.WriteString(fmt.Sprintf("记忆文件路径: %s\n\n", memoryPath))
	sb.WriteString("当前记忆内容:\n")
	sb.WriteString("<current_notes_content>\n")
	sb.WriteString(currentMemory)
	sb.WriteString("\n</current_notes_content>\n\n")

	// 分析 section 大小，生成提醒。
	sectionSizes := analyzeSectionSizes(currentMemory)
	totalTokens := len(currentMemory) / 4 // 粗略估算

	if totalTokens > cfg.MaxTotalTokens {
		sb.WriteString(fmt.Sprintf("\n注意：当前记忆文件约 %d tokens，超出最大限制 %d tokens。请精简内容。\n",
			totalTokens, cfg.MaxTotalTokens))
	}

	var oversized []string
	for section, tokens := range sectionSizes {
		if tokens > cfg.MaxSectionTokens {
			oversized = append(oversized, fmt.Sprintf("- %q 约 %d tokens (限制: %d)", section, tokens, cfg.MaxSectionTokens))
		}
	}
	if len(oversized) > 0 {
		sb.WriteString("\n以下 section 超长，请精简:\n")
		sb.WriteString(strings.Join(oversized, "\n"))
		sb.WriteString("\n")
	}

	sb.WriteString("\n请直接输出完整的更新后 markdown 文件内容。")
	return sb.String()
}

// analyzeSectionSizes 分析每个 section 的 token 大小。
func analyzeSectionSizes(content string) map[string]int {
	sections := make(map[string]int)
	lines := strings.Split(content, "\n")
	var currentSection string
	var currentContent []string

	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			if currentSection != "" && len(currentContent) > 0 {
				sectionContent := strings.Join(currentContent, "\n")
				sections[currentSection] = len(strings.TrimSpace(sectionContent)) / 4
			}
			currentSection = line
			currentContent = nil
		} else {
			currentContent = append(currentContent, line)
		}
	}
	if currentSection != "" && len(currentContent) > 0 {
		sectionContent := strings.Join(currentContent, "\n")
		sections[currentSection] = len(strings.TrimSpace(sectionContent)) / 4
	}
	return sections
}
