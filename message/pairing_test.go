package message

import (
	"strings"
	"testing"
)

func asst(text string, toolIDs ...string) Message {
	m := Message{Role: RoleAssistant}
	if text != "" {
		m.Content = append(m.Content, ContentBlock{Type: "text", Text: text})
	}
	for _, id := range toolIDs {
		m.Content = append(m.Content, ContentBlock{Type: "tool_use", ToolUseID: id, ToolName: "t"})
	}
	return m
}

func TestValidatePairing(t *testing.T) {
	ok := []Message{
		NewUserMessage("hi"),
		asst("", "a", "b"),
		NewToolResultMessage("a", "ra", false),
		NewToolResultMessage("b", "rb", false),
		NewUserMessage("插话"),
		asst("done"),
	}
	if err := ValidatePairing(ok); err != nil {
		t.Fatalf("合法序列被判非法: %v", err)
	}
	if got := SanitizeForRequest(ok); &got[0] != &ok[0] {
		t.Fatal("合法序列应原样返回")
	}
}

func TestSanitizeForRequest_OrphanResultsAtHead(t *testing.T) {
	// collapse/auto 压缩后的典型形态：摘要后紧跟孤儿 tool_result。
	msgs := []Message{
		NewUserMessage("[摘要]"),
		NewToolResultMessage("q1", "结果1", false),
		NewToolResultMessage("q2", "结果2", false),
		asst("继续"),
	}
	if ValidatePairing(msgs) == nil {
		t.Fatal("孤儿 tool_result 应被判非法")
	}
	got := SanitizeForRequest(msgs)
	if err := ValidatePairing(got); err != nil {
		t.Fatalf("修复后仍非法: %v", err)
	}
	if !strings.Contains(got[1].Content[0].Text, "结果1") {
		t.Fatalf("孤儿结果的内容应保留为文本: %+v", got[1])
	}
	if msgs[1].Content[0].Type != "tool_result" {
		t.Fatal("SanitizeForRequest 不得修改入参")
	}
}

func TestSanitizeForRequest_MissingResult(t *testing.T) {
	msgs := []Message{
		NewUserMessage("hi"),
		asst("", "t1", "t2"),
		NewToolResultMessage("t2", "r2", false),
		NewUserMessage("插话"),
	}
	got := SanitizeForRequest(msgs)
	if err := ValidatePairing(got); err != nil {
		t.Fatalf("修复后仍非法: %v", err)
	}
	// 补齐的占位结果必须在插话之前。
	last := got[len(got)-1]
	if last.Content[0].Text != "插话" {
		t.Fatalf("其他用户消息应排在结果之后: %+v", got)
	}
}

func TestSanitizeForRequest_LeadingAssistant(t *testing.T) {
	got := SanitizeForRequest([]Message{asst("x"), NewUserMessage("y")})
	if got[0].Role != RoleUser {
		t.Fatal("首条消息应补成 user")
	}
}

func TestEstimateTextTokens_CJK(t *testing.T) {
	zh := strings.Repeat("中", 1000)
	if n := EstimateTextTokens(zh, CharsPerToken); n < 1000 {
		t.Fatalf("1000 个汉字估成 %d token，明显低估", n)
	}
	if n := EstimateContentBlockTokens(ContentBlock{Type: "thinking", Thinking: strings.Repeat("思", 3000)}); n < 3000 {
		t.Fatalf("thinking 块估成 %d token（此前读错字段恒为 0）", n)
	}
}
