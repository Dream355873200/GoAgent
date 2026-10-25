package openai

import (
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// feedChunks 按给定分片逐个喂入过滤器，拼接最终 text/think（含 Flush 尾巴）。
func feedChunks(chunks []string) (text, think string) {
	f := &thinkStreamFilter{}
	for _, c := range chunks {
		t, th := f.Feed(c)
		text += t
		think += th
	}
	ft, fth := f.Flush()
	return text + ft, think + fth
}

func TestThinkFilterBasicDivert(t *testing.T) {
	text, think := feedChunks([]string{
		"<thinking>\nLet me analyze.\n</thinking>\n\n五支手绘向的参考图。",
	})
	if think != "\nLet me analyze.\n" {
		t.Fatalf("think = %q", think)
	}
	if text != "\n\n五支手绘向的参考图。" {
		t.Fatalf("text = %q", text)
	}
}

func TestThinkFilterMismatchedTags(t *testing.T) {
	// 实测泄漏形态：<thinking> 开、</thought> 收
	text, think := feedChunks([]string{
		"<thinking>\nUser wants posters.\n</thought>五支手绘向的参考图。",
	})
	if think != "\nUser wants posters.\n" {
		t.Fatalf("think = %q", think)
	}
	if text != "五支手绘向的参考图。" {
		t.Fatalf("text = %q", text)
	}
}

func TestThinkFilterTagSplitAcrossChunks(t *testing.T) {
	// 在任意字节处切成两段喂入，结果都必须与整段一致
	full := "<thinking>\nhidden\n</thinking>visible"
	for cut := 1; cut < len(full); cut++ {
		text, think := feedChunks([]string{full[:cut], full[cut:]})
		if think != "\nhidden\n" || text != "visible" {
			t.Fatalf("cut=%d text=%q think=%q", cut, text, think)
		}
	}
}

func TestThinkFilterCloseSplitAcrossChunks(t *testing.T) {
	full := "<think>abc</thought>x"
	for cut := 1; cut < len(full); cut++ {
		text, think := feedChunks([]string{full[:cut], full[cut:]})
		if think != "abc" || text != "x" {
			t.Fatalf("cut=%d text=%q think=%q", cut, text, think)
		}
	}
}

func TestThinkFilterPlainTextPassthrough(t *testing.T) {
	// 数学不等号、普通尖括号不受影响
	text, think := feedChunks([]string{
		"if a < b then a <max> b",
	})
	if think != "" {
		t.Fatalf("think = %q", think)
	}
	if text != "if a < b then a <max> b" {
		t.Fatalf("text = %q", text)
	}
}

func TestThinkFilterMultipleBlocks(t *testing.T) {
	text, think := feedChunks([]string{
		"<think>A</think>mid<thought>B</thought>end",
	})
	if think != "AB" || text != "midend" {
		t.Fatalf("text=%q think=%q", text, think)
	}
}

func TestThinkFilterUnclosedAtEnd(t *testing.T) {
	// 流结束标签未闭合：已进思考通道的内容归思考，不漏进正文
	text, think := feedChunks([]string{
		"<think>still reasoning to the very end",
	})
	if text != "" {
		t.Fatalf("text = %q", text)
	}
	if think != "still reasoning to the very end" {
		t.Fatalf("think = %q", think)
	}
}

func TestThinkFilterIdempotentFlush(t *testing.T) {
	f := &thinkStreamFilter{}
	if ft, _ := f.Feed("hello<"); ft != "hello" {
		t.Fatalf("feed text = %q", ft)
	}
	// 悬挂的 '<' 在 Flush 时按当前通道吐出
	if ft, fth := f.Flush(); ft != "<" || fth != "" {
		t.Fatalf("first flush = %q %q", ft, fth)
	}
	if ft, fth := f.Flush(); ft != "" || fth != "" {
		t.Fatalf("second flush = %q %q", ft, fth)
	}
}

func TestStripThinkTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<thinking>\nabc\n</thinking>\n\n正文", "正文"},
		{"<thinking>\nabc\n</thought>正文", "正文"},
		{"<thought>\nabc\n</think> 正文", "正文"},
		{"无标签正文", "无标签正文"},
		{"<think>未闭合一路吞到底", ""},
	}
	for _, c := range cases {
		if got := stripThinkTags(c.in); got != c.want {
			t.Errorf("stripThinkTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestToOpenAIMessagesNoThinkingReplay 历史回放不再携带 <thinking> 包裹，
// 且正文里的泄漏残留被清洗。
func TestToOpenAIMessagesNoThinkingReplay(t *testing.T) {
	p := &Provider{imgFormats: map[string]string{}}
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "thinking", Text: "internal reasoning"},
			{Type: "text", Text: "最终答复"},
		}},
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			// 历史泄漏残留：伪标签已落进 text 块
			{Type: "text", Text: "<thinking>\nleaked reasoning\n</thought>清洗后的正文"},
		}},
	}
	out := p.toOpenAIMessages("", true, "", msgs)
	first, _ := out[0].Content.(string)
	if strings.Contains(first, "<thinking>") || strings.Contains(first, "internal reasoning") {
		t.Fatalf("历史回放不应携带思考（包裹或原文），实际 %q", first)
	}
	if first != "最终答复" {
		t.Fatalf("正文应原样保留，实际 %q", first)
	}
	second, _ := out[1].Content.(string)
	if second != "清洗后的正文" {
		t.Fatalf("泄漏残留应被清洗，实际 %q", second)
	}
}
