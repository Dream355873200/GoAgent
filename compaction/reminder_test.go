package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// fakeSummarizer 返回固定摘要（Layer 4 驱动用）。
func fakeSummarizer(ctx context.Context, text string) (string, error) {
	return "压缩摘要", nil
}

// bigHistory 构造超过压缩阈值的消息历史（中文 ≈1 token/字）。
func bigHistory(chars int) []message.Message {
	msgs := []message.Message{message.NewUserMessage("开始")}
	for i := 0; len(msgs) < 6 || i < 2; i++ {
		msgs = append(msgs,
			message.NewUserMessage(fmt.Sprintf("第 %d 轮请求", i)),
			message.NewUserMessage(strings.Repeat("内容", chars/3)),
		)
		if i >= 3 {
			break
		}
	}
	return msgs
}

// Apply：Layer 4 摘要成功后应调用重注入源，提醒以 user 消息追加在
// 压缩边界之后；空条目跳过。
func TestApply_PostCompactRemindersInjected(t *testing.T) {
	called := 0
	m := NewManager(Config{
		Summarizer:           fakeSummarizer,
		AutoCompactThreshold: 0.01, // 极低阈值：任何历史都触发压缩
		PostCompact: []ReminderSource{
			ReminderSourceFunc(func(ctx context.Context) []string {
				called++
				return []string{"关键上下文：SPEC 在 SPEC.md", "", "  ", "最新测试报告：全部通过"}
			}),
		},
	})

	msgs := bigHistory(4000)
	out, freed := m.Apply(context.Background(), msgs, 100_000)
	if freed <= 0 {
		t.Fatalf("应发生压缩并释放 token, freed=%d", freed)
	}
	if called != 1 {
		t.Fatalf("重注入源应被调用 1 次, got %d", called)
	}

	// 提醒应为 user 消息（空条目被跳过 → 2 条有效提醒）。
	var reminders []string
	for _, m := range out {
		if m.Role == message.RoleUser {
			txt := message.ExtractText(m)
			if strings.Contains(txt, "SPEC 在 SPEC.md") || strings.Contains(txt, "测试报告") {
				reminders = append(reminders, txt)
			}
		}
	}
	if len(reminders) != 2 {
		t.Fatalf("应注入 2 条非空提醒, got %d: %v", len(reminders), reminders)
	}
	// 提醒必须在压缩摘要之后。
	lastIsReminder := strings.Contains(message.ExtractText(out[len(out)-1]), "测试报告")
	if !lastIsReminder {
		t.Fatalf("最后一条消息应为提醒: %q", message.ExtractText(out[len(out)-1]))
	}
}

// 压缩未实际发生（token 未超阈值）时不应调用重注入源。
func TestApply_NoCompactNoReminders(t *testing.T) {
	called := 0
	m := NewManager(Config{
		Summarizer:           fakeSummarizer,
		AutoCompactThreshold: 0.99,
		PostCompact: []ReminderSource{
			ReminderSourceFunc(func(ctx context.Context) []string {
				called++
				return nil
			}),
		},
	})
	m.Apply(context.Background(), bigHistory(10), 1_000_000)
	if called != 0 {
		t.Fatalf("未压缩不应调用重注入源, called=%d", called)
	}
}

// HandleOverflow 成功恢复后同样应注入提醒。
func TestHandleOverflow_PostCompactRemindersInjected(t *testing.T) {
	m := NewManager(Config{
		Summarizer: fakeSummarizer,
		PostCompact: []ReminderSource{
			ReminderSourceFunc(func(ctx context.Context) []string {
				return []string{"413 恢复后的提醒"}
			}),
		},
	})
	out, ok := m.HandleOverflow(context.Background(), bigHistory(4000), 100_000)
	if !ok {
		t.Fatal("HandleOverflow 应成功恢复")
	}
	found := false
	for _, msg := range out {
		if strings.Contains(message.ExtractText(msg), "413 恢复后的提醒") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("HandleOverflow 恢复后应注入提醒")
	}
}
