package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 历史回放：assistant 文本里的伪标签思考块（含交叉配对与未闭合残留）不回传。
func TestConvertMessagesStripsThinkTags(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "text", Text: "<thinking>泄漏的思考</thought>正式回答第一句\n正式回答第二句"},
		}},
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "text", Text: "正常内容\n<think>未闭合的思考一直拖到最后"},
		}},
	}
	out := convertMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("应产出 2 条消息，得到 %d", len(out))
	}
	for i, m := range out {
		data, err := json.Marshal(m.Content)
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		if strings.Contains(s, "泄漏的思考") || strings.Contains(s, "未闭合的思考") {
			t.Fatalf("消息 %d 仍含伪标签内容：%s", i, s)
		}
		if !strings.Contains(s, "正式回答") && !strings.Contains(s, "正常内容") {
			t.Fatalf("消息 %d 正文被误伤：%s", i, s)
		}
	}
}

// 全部内容都是伪标签时，文本块应被整体丢弃（消息可能随之消失）。
func TestConvertMessagesDropsPureThinkText(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: "text", Text: "<thinking>整段都是思考"},
		}},
	}
	out := convertMessages(msgs)
	if len(out) != 0 {
		t.Fatalf("纯伪标签文本应使整条消息消失，得到 %d 条", len(out))
	}
}

// 流式分流：伪标签段走 thinking 事件，正文走 text 事件。
func TestConsumeTextFilterDivertsTags(t *testing.T) {
	f := &message.ThinkStreamFilter{}
	cur := &streamBlock{typ: "text"}
	events := []provider.StreamEvent{}
	out := make(chan provider.StreamEvent, 64)
	go func() {
		defer close(out)
		emitFilteredText(f, cur, "开头<thinki", out)
		emitFilteredText(f, cur, "ng>跨块思考", out)
		emitFilteredText(f, cur, "</thinking>结尾", out)
		if text, think := f.Flush(); text != "" || think != "" {
			t.Errorf("不应有悬挂残留：text=%q think=%q", text, think)
		}
	}()
	for e := range out {
		events = append(events, e)
	}
	var text, think strings.Builder
	for _, e := range events {
		switch e.Type {
		case provider.EventTextDelta:
			text.WriteString(e.Text)
		case provider.EventThinkingDelta:
			think.WriteString(e.Thinking)
		default:
			t.Fatalf("意外事件类型：%v", e.Type)
		}
	}
	if text.String() != "开头结尾" {
		t.Fatalf("正文=%q", text.String())
	}
	if think.String() != "跨块思考" {
		t.Fatalf("思考=%q", think.String())
	}
	if cur.text.String() != "开头结尾" || cur.thinking.String() != "跨块思考" {
		t.Fatalf("块累计不一致：text=%q thinking=%q", cur.text.String(), cur.thinking.String())
	}
}
