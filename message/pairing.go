package message

import (
	"fmt"
	"strings"
)

// orphanResultPrefix 孤儿 tool_result 降级为文本时的前缀。
const orphanResultPrefix = "[工具结果（对应的调用已不在上下文中）]\n"

// missingResultText 补齐缺失 tool_result 时使用的占位文本。
const missingResultText = "[该工具调用没有结果：执行被中断或结果已从上下文中移除，如仍需要请重新调用]"

// SanitizeForRequest 返回一份满足模型 API 配对约束的消息副本，不修改入参：
//
//   - 每个 assistant 的 tool_use，在其后连续的用户消息里都有 tool_result；
//     缺失的就地补一条占位结果（插在已有结果之后、其他用户消息之前）。
//   - 找不到对应 tool_use 的 tool_result（压缩切掉了调用一侧）降级为文本块，
//     内容保留，不再是会让 API 400 的孤儿结果。
//   - 第一条消息必须是 user：以 assistant 开头时补一条占位用户消息。
//
// 消息本身已合法时原样返回入参切片。
func SanitizeForRequest(msgs []Message) []Message {
	if len(msgs) == 0 || ValidatePairing(msgs) == nil {
		return msgs
	}

	out := make([]Message, 0, len(msgs)+2)
	if msgs[0].Role == RoleAssistant {
		out = append(out, Message{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "（更早的对话已压缩）"}}})
	}

	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role != RoleAssistant {
			// 不跟在 assistant 之后的用户消息：其中的 tool_result 都是孤儿。
			out = append(out, demoteToolResults(m, nil))
			i++
			continue
		}

		out = append(out, m)
		i++
		want := toolUseIDs(m)

		// 收集紧随其后的连续用户消息（API 会把它们合并成同一个 user 轮次）。
		j := i
		for j < len(msgs) && msgs[j].Role == RoleUser {
			j++
		}
		run := msgs[i:j]

		// 先放已有的 tool_result 消息（去掉孤儿），再补缺失的，最后放其他用户消息。
		seen := make(map[string]bool, len(want))
		var results, others []Message
		for _, um := range run {
			fixed := demoteToolResults(um, want)
			for _, b := range fixed.Content {
				if b.Type == "tool_result" {
					seen[b.ForToolUseID] = true
				}
			}
			if IsToolResultMessage(fixed) {
				results = append(results, fixed)
			} else {
				others = append(others, fixed)
			}
		}
		out = append(out, results...)
		for _, id := range orderedToolUseIDs(m) {
			if !seen[id] {
				out = append(out, NewToolResultMessage(id, missingResultText, true))
				seen[id] = true
			}
		}
		out = append(out, others...)
		i = j
	}
	return out
}

// ValidatePairing 检查消息序列是否满足 tool_use/tool_result 的配对约束，
// 返回第一处违规。测试用它作为每轮结束时的不变量断言。
func ValidatePairing(msgs []Message) error {
	if len(msgs) > 0 && msgs[0].Role == RoleAssistant {
		return fmt.Errorf("第一条消息是 assistant")
	}
	var pending map[string]bool // 上一条 assistant 发出、尚未见到结果的调用
	inResults := false          // 是否仍处于 assistant 之后的 tool_result 段
	for i, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			if len(pending) > 0 {
				return fmt.Errorf("消息 %d 之前的 tool_use %v 缺少 tool_result", i, keys(pending))
			}
			pending = toolUseIDs(m)
			inResults = len(pending) > 0
		default:
			for _, b := range m.Content {
				if b.Type != "tool_result" {
					continue
				}
				if !inResults || !pending[b.ForToolUseID] {
					return fmt.Errorf("消息 %d 的 tool_result %q 没有对应的 tool_use", i, b.ForToolUseID)
				}
				delete(pending, b.ForToolUseID)
			}
			if !IsToolResultMessage(m) {
				if len(pending) > 0 {
					return fmt.Errorf("消息 %d 之前的 tool_use %v 缺少 tool_result", i, keys(pending))
				}
				inResults = false
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("末尾的 tool_use %v 缺少 tool_result", keys(pending))
	}
	return nil
}

// demoteToolResults 把 ID 不在 valid 中的 tool_result 块降级为文本块。
// 没有需要降级的块时原样返回（不复制）。
func demoteToolResults(m Message, valid map[string]bool) Message {
	needs := false
	for _, b := range m.Content {
		if b.Type == "tool_result" && !valid[b.ForToolUseID] {
			needs = true
			break
		}
	}
	if !needs {
		return m
	}
	m = m.CloneContent()
	for k, b := range m.Content {
		if b.Type == "tool_result" && !valid[b.ForToolUseID] {
			text := b.Text
			if img, ok := SplitImageMarker(text); ok {
				text = "[图片已省略]\n" + img.Body
			}
			m.Content[k] = ContentBlock{Type: "text", Text: orphanResultPrefix + text}
		}
	}
	return m
}

func toolUseIDs(m Message) map[string]bool {
	var ids map[string]bool
	for _, b := range m.Content {
		if b.Type == "tool_use" && b.ToolUseID != "" {
			if ids == nil {
				ids = make(map[string]bool)
			}
			ids[b.ToolUseID] = true
		}
	}
	return ids
}

func orderedToolUseIDs(m Message) []string {
	var ids []string
	for _, b := range m.Content {
		if b.Type == "tool_use" && b.ToolUseID != "" {
			ids = append(ids, b.ToolUseID)
		}
	}
	return ids
}

func keys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ",")
}
