package compaction

import (
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// 超长的内联图片结果：近期的必须原样保留（截断会产出损坏 base64，端点 400），
// 早期的整体回收为占位符；两种情况都不能出现 head+tail 截断。
func imageResultMessages(total, imageAt int) []message.Message {
	img := "[IMAGE jpeg " + strings.Repeat("A", 70_000) + "]\n联系表说明"
	msgs := make([]message.Message, total)
	for i := range msgs {
		msgs[i] = message.Message{Role: message.RoleUser, Content: []message.ContentBlock{{Type: "text", Text: "x"}}}
	}
	msgs[imageAt] = message.Message{Role: message.RoleUser, Content: []message.ContentBlock{{Type: "tool_result", ForToolUseID: "t1", Text: img}}}
	return msgs
}

func TestOversizedImageResultNeverTruncated(t *testing.T) {
	compressors := map[string]func([]message.Message) []message.Message{
		"budget": func(m []message.Message) []message.Message {
			out, _ := (&budgetCompressor{maxSize: 60_000}).apply(m)
			return out
		},
		"micro": func(m []message.Message) []message.Message {
			out, _ := (&microCompressor{}).apply(m, 200_000, 0)
			return out
		},
	}
	for name, apply := range compressors {
		recent := imageResultMessages(10, 8)
		original := recent[8].Content[0].Text
		if got := apply(recent)[8].Content[0].Text; got != original {
			t.Errorf("%s: 近期图片结果被改写（len %d → %d）", name, len(original), len(got))
		}

		old := imageResultMessages(imgTailMessages+10, 1)
		got := apply(old)[1].Content[0].Text
		if strings.Contains(got, "截断") || strings.HasPrefix(got, "[IMAGE") {
			t.Errorf("%s: 早期图片结果应回收为占位符，得到 %.60q", name, got)
		}
	}
}
