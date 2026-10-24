package compaction

import (
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// tailBudgetTextMsg 构造一条约 60 token 的文本消息（200 字符文本 + 10 overhead）。
func tailBudgetTextMsg(role message.Role, marker string) message.Message {
	return message.Message{
		Role: role,
		Content: []message.ContentBlock{
			{Type: "text", Text: strings.Repeat(marker, 200)},
		},
	}
}

func tailBudgetMessages(n int) []message.Message {
	msgs := make([]message.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, tailBudgetTextMsg(message.RoleUser, string(rune('a'+i%26))))
	}
	return msgs
}

// 保护条数改为按 token 预算后，大窗口/大预算时应保留远多于固定 6 条的
// 近期原文：折叠范围明显缩小，尾部原文不被截断。
func TestCollapseTailBudgetProtectsMore(t *testing.T) {
	msgs := tailBudgetMessages(20)
	c := &collapseCompressor{tailBudgetRatio: 0.5, protectTail: 6}
	got, _ := c.apply(msgs, 1000) // 预算 = 500 token ≈ 8 条，超过 6 条下限

	folded := len(msgs) - (len(got) - 1) // 被折叠进摘要的消息数
	if folded >= len(msgs)-6 {
		t.Fatalf("预算模式应保护超过 6 条: folded=%d protected=%d", folded, len(got)-1)
	}
	if len(got) < 8 {
		t.Fatalf("尾部应保留至少预算内的原文: got %d 条", len(got))
	}
	if !got[0].Compacted || got[0].Role != message.RoleUser {
		t.Fatalf("首条应为折叠摘要: %+v", got[0])
	}
	// 尾部原文原样保留
	for i := 1; i < len(got); i++ {
		if got[i].Content[0].Text != msgs[len(msgs)-len(got)+i].Content[0].Text {
			t.Fatalf("尾部第 %d 条被改写", i)
		}
	}
}


// 默认装配（NewManager + 零值 Config）即启用尾部预算：10 万窗口、5 万占用时，
// 预算 = 10% = 1 万 token，保护区约 20 条（远超 6 条下限）。
// 变异验证：把默认 ratio 改回 -1（固定 6 条）时本测试失败。
func TestDefaultManagerUsesTailBudget(t *testing.T) {
	msgs := make([]message.Message, 0, 100)
	for i := 0; i < 100; i++ {
		msgs = append(msgs, message.Message{Role: message.RoleUser,
			Content: []message.ContentBlock{{Type: "text", Text: strings.Repeat("x", 2000)}}})
	}
	m := NewManager(Config{}) // 零值 = 默认 TailBudgetRatio 0.1
	got, _ := m.collapse.apply(msgs, 100000)

	if len(got) != 21 { // 1 条摘要 + 预算内保护的 20 条
		t.Fatalf("默认配置应按预算保护约 20 条: got %d 条", len(got))
	}
	if !got[0].Compacted {
		t.Fatal("首条应为折叠摘要")
	}
}

// 下限兜底：预算过小时仍至少保护 protectTail 条。
func TestCollapseTailBudgetFloor(t *testing.T) {
	msgs := tailBudgetMessages(20)
	c := &collapseCompressor{tailBudgetRatio: 0.01, protectTail: 6} // 预算 = 10 token
	got, _ := c.apply(msgs, 1000)

	if len(got) != 7 { // 1 条摘要 + 保护的 6 条
		t.Fatalf("预算过小时应回落到 6 条下限: got %d 条", len(got))
	}
}

// 负 ratio = 关闭预算，回到 v0.14 的固定条数行为（保护最后 6 条）。
func TestCollapseLegacyFixedTail(t *testing.T) {
	msgs := tailBudgetMessages(20)
	c := &collapseCompressor{tailBudgetRatio: -1, protectTail: 6}
	got, _ := c.apply(msgs, 1000)

	if len(got) != 7 { // 1 条摘要 + 保护的 6 条
		t.Fatalf("legacy 模式应保护最后 6 条: got %d 条", len(got))
	}
}

// L1 snip 同样按预算扩展保护区：大预算时近期原文不被截断。
func TestSnipTailBudgetProtectsMore(t *testing.T) {
	// 300 字符：超过 L1 的 200 字符截断线，且每条约 85 token
	msgs := make([]message.Message, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, message.Message{Role: message.RoleUser,
			Content: []message.ContentBlock{{Type: "text", Text: strings.Repeat("x", 300)}}})
	}
	s := &snipCompressor{tailBudgetRatio: 2.0, protectTail: 4}
	got, freed := s.apply(msgs, 1000) // 预算 = 2000 token，全部受保护

	if freed != 0 {
		t.Fatalf("大预算下近期原文不应被截断, freed=%d", freed)
	}
	for i := range got {
		if got[i].Content[0].Text != msgs[i].Content[0].Text {
			t.Fatalf("第 %d 条被截断", i)
		}
	}

	s2 := &snipCompressor{tailBudgetRatio: -1, protectTail: 4}
	got2, freed2 := s2.apply(msgs, 1000)
	if freed2 == 0 {
		t.Fatal("legacy 模式下旧消息长文本应被截断")
	}
	if len(got2) != len(msgs) {
		t.Fatal("snip 不应增删消息")
	}
}
