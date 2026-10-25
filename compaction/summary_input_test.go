package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// 摘要输入必须包含工具证据链：tool_use（工具名+参数要点）与 tool_result
//（正文，含错误标记）。此前只取 text 块，摘要模型看不到任何执行证据，
// 压缩后模型基于失真的进度认知继续工作——输出错误内容或提前收轮。
func TestBuildConversationTextIncludesToolEvidence(t *testing.T) {
	b64 := strings.Repeat("A", 200) // 假 base64，触发图片标记剥离
	msgs := []message.Message{
		message.NewUserMessage("做一只 30 秒的鸟群短片"),
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "text", Text: "先写场景。"},
			{Type: "tool_use", ToolUseID: "t1", ToolName: "Write", Input: []byte(`{"file_path":"src/flock.tsx","content":"export default ..."}`)},
			{Type: "tool_use", ToolUseID: "t2", ToolName: "Bash", Input: []byte(`{"command":"npm run build"}`)},
		}},
		{Role: message.RoleUser, Content: []message.ContentBlock{
			{Type: "tool_result", ForToolUseID: "t1", Text: "已写入 src/flock.tsx"},
			{Type: "tool_result", ForToolUseID: "t2", Text: "构建失败: TS2305", IsError: true},
		}},
		{Role: message.RoleUser, Content: []message.ContentBlock{
			{Type: "tool_result", ForToolUseID: "t3", Text: "[IMAGE jpeg " + b64 + "]联系表"},
		}},
	}
	out := buildConversationText(msgs)

	for _, want := range []string{
		"调用工具 Write", "file_path", // 工具名 + 参数要点
		"调用工具 Bash", "npm run build",
		"工具结果: 已写入 src/flock.tsx",
		"工具结果: [错误] 构建失败", // 错误标记保留
		"联系表", // 图片标记剥离后正文保留
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("摘要输入缺工具证据 %q，实际：\n%s", want, out)
		}
	}
	if strings.Contains(out, b64) {
		t.Fatalf("base64 不应进入摘要输入")
	}
	// 超长 tool_result 截断
	huge := message.NewUserMessage("")
	huge.Content = []message.ContentBlock{{Type: "tool_result", ForToolUseID: "t9", Text: strings.Repeat("x", 5000)}}
	out2 := buildConversationText([]message.Message{huge})
	if got := len([]rune(strings.Split(out2, "\n")[0])); got > summaryBlockMax+200 {
		t.Fatalf("超长结果应截断，实际 %d 字符", got)
	}
}

// 手动 compact 的尾部保留必须按轮次对齐：assistant(tool_use) + user(tool_result)
// 是不可拆的最小单元，盲取最后 2 条会把切点落在配对中间留下孤儿结果。
func TestBuildCompactedMessagesKeepsToolPairing(t *testing.T) {
	recent := []message.Message{
		{Role: message.RoleUser, Content: []message.ContentBlock{{Type: "text", Text: "继续"}}},
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "tool_use", ToolUseID: "t1", ToolName: "Edit", Input: []byte(`{}`)},
		}},
		{Role: message.RoleUser, Content: []message.ContentBlock{
			{Type: "tool_result", ForToolUseID: "t1", Text: "已修改"},
		}},
	}
	out := buildCompactedMessages("摘要", recent)
	// 切点应回退到轮次起点（"继续"），配对完整保留
	if len(out) != 1+len(recent) {
		t.Fatalf("尾部应保留完整轮次（%d 条），实际 %d 条", 1+len(recent), len(out))
	}
	found := 0
	for _, m := range out[1:] {
		for _, b := range m.Content {
			if b.Type == "tool_use" || b.Type == "tool_result" {
				found++
			}
		}
	}
	if found != 2 {
		t.Fatalf("tool_use/tool_result 应成对保留，实际 %d 个相关块", found)
	}
}

// 每轮压缩后必须带内置的「继续未完成任务」重定向：压缩边界是提前收轮
// （stop=end_turn）的高发区。
func TestInjectPostCompactRemindersIncludesContinuation(t *testing.T) {
	m := NewManager(Config{}) // 无宿主提醒源：内置提醒依然要有
	out := m.injectPostCompactReminders(context.Background(), nil)
	if len(out) == 0 {
		t.Fatalf("内置继续提醒缺失")
	}
	joined := out[len(out)-1].Content[0].Text
	if !strings.Contains(joined, "继续") || !strings.Contains(joined, "压缩") {
		t.Fatalf("内置提醒应指向继续压缩前的任务，实际：%s", joined)
	}
}
