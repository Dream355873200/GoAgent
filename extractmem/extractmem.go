// Package extractmem 实现对话结束后的自动记忆提取。
//
// 对齐 Claude Code 的 src/services/extractMemories/：
//   - 对话结束时 fork 一个轻量 agent 分析本轮对话
//   - 自动将有价值的信息（用户偏好、项目约定、调试经验等）写入 auto memory
//   - 增量追加，不覆盖已有记忆
//   - 去重：过滤与已有记忆重复的内容
//
// 触发点：作为 Stop hook 或在 EventDone 后由业务层调用。
package extractmem

import (
	"context"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/memory"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Extractor 负责从对话中提取记忆。
type Extractor struct {
	prov    provider.Provider
	autoMem *memory.AutoMemory
	model   string // 可选：指定用于提取的模型
}

// NewExtractor 创建一个新的记忆提取器。
func NewExtractor(prov provider.Provider, autoMem *memory.AutoMemory) *Extractor {
	return &Extractor{
		prov:    prov,
		autoMem: autoMem,
	}
}

// WithModel 指定用于提取的模型（默认使用 provider 自带模型）。
func (e *Extractor) WithModel(model string) *Extractor {
	e.model = model
	return e
}

// Extract 从对话消息中提取记忆。
// 返回提取到的记忆条目数。
func (e *Extractor) Extract(ctx context.Context, messages []message.Message) (int, error) {
	if e.autoMem == nil || e.prov == nil {
		return 0, nil
	}

	if len(messages) < 3 {
		return 0, nil // 对话太短，没有什么可提取的
	}

	// 加载已有记忆（用于去重）。
	existingMemory := e.autoMem.LoadMain()

	// 构建提取 prompt。
	prompt := buildExtractPrompt(messages, existingMemory)

	// 调用 LLM 提取。
	req := &provider.Request{
		SystemPrompt: extractSystemPrompt,
		Messages:     []message.Message{message.NewUserMessage(prompt)},
		MaxTokens:    2048,
	}
	if e.model != "" {
		req.Model = e.model
	}

	resp, err := e.prov.Complete(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("LLM 提取记忆失败: %w", err)
	}

	extracted := message.ExtractText(resp.Message)
	if strings.TrimSpace(extracted) == "" || strings.Contains(extracted, "[无需保存]") {
		return 0, nil
	}

	// 解析提取结果。
	entries := parseExtractedMemories(extracted)
	if len(entries) == 0 {
		return 0, nil
	}

	// 追加到 MEMORY.md。
	current := e.autoMem.LoadMain()
	var newContent strings.Builder
	if current != "" {
		newContent.WriteString(current)
		newContent.WriteString("\n\n")
	}

	added := 0
	for _, entry := range entries {
		// 简单去重：检查是否已存在。
		if current != "" && strings.Contains(strings.ToLower(current), strings.ToLower(entry.Key)) {
			continue
		}
		newContent.WriteString(fmt.Sprintf("- **%s**: %s\n", entry.Key, entry.Value))
		added++
	}

	if added > 0 {
		if err := e.autoMem.SaveMain(newContent.String()); err != nil {
			return 0, fmt.Errorf("保存记忆失败: %w", err)
		}
	}

	return added, nil
}

// MemoryEntry 是一条记忆。
type MemoryEntry struct {
	Key   string // 简短的标签
	Value string // 详细内容
}

// parseExtractedMemories 从 LLM 输出中解析记忆条目。
func parseExtractedMemories(text string) []MemoryEntry {
	var entries []MemoryEntry
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")

		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}

		// 尝试 "key: value" 或 "**key**: value" 格式。
		if idx := strings.Index(line, ": "); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			key = strings.Trim(key, "*")
			value := strings.TrimSpace(line[idx+2:])
			if key != "" && value != "" {
				entries = append(entries, MemoryEntry{Key: key, Value: value})
				continue
			}
		}

		// 整行作为一条记忆。
		if len(line) > 10 {
			entries = append(entries, MemoryEntry{Key: "备注", Value: line})
		}
	}
	return entries
}

// buildExtractPrompt 构建提取 prompt。
func buildExtractPrompt(messages []message.Message, existingMemory string) string {
	var sb strings.Builder
	sb.WriteString("以下是一段对话记录。请从中提取值得跨会话记住的信息。\n\n")
	sb.WriteString("提取规则：\n")
	sb.WriteString("1. 只提取稳定的模式和偏好，不提取临时任务细节\n")
	sb.WriteString("2. 包括：用户偏好、项目约定、架构决策、调试经验、工具使用习惯\n")
	sb.WriteString("3. 不包括：当前任务进度、临时变量、一次性操作\n")
	sb.WriteString("4. 如果没有值得记忆的内容，返回 [无需保存]\n")
	sb.WriteString("5. 每条记忆用 \"- **标签**: 内容\" 格式输出\n\n")

	if existingMemory != "" {
		sb.WriteString("已有记忆（避免重复）：\n")
		sb.WriteString(existingMemory)
		sb.WriteString("\n\n")
	}

	sb.WriteString("对话记录：\n")
	for _, msg := range messages {
		role := string(msg.Role)
		text := message.ExtractText(msg)
		if text == "" {
			continue
		}
		// 截断过长的单条消息。
		if len(text) > 2000 {
			text = text[:2000] + "..."
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", role, text))
	}

	return sb.String()
}

const extractSystemPrompt = `你是一个记忆提取助手。你的任务是从对话中提取值得长期记忆的信息。
只输出提取的记忆条目（格式：- **标签**: 内容），或者 [无需保存]。
不要添加任何解释或前言。保持简洁。`
