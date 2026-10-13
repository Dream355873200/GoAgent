package openai

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 思考强度档位 → 请求体 reasoning_effort 的映射约定：
// off → "none"（关闭思考），low/medium/high 原样透传，空 = 不发字段。
func TestBuildChatRequestReasoningEffort(t *testing.T) {
	p := New(Config{BaseURL: "http://x/v1", Model: "m"})

	cases := map[string]string{
		"off":  "none",
		"low":  "low",
		"high": "high",
		"":     "",
	}
	for in, want := range cases {
		cr := p.buildChatRequest(&provider.Request{ReasoningEffort: in}, false)
		if cr.ReasoningEffort != want {
			t.Errorf("ReasoningEffort %q → %q, 期望 %q", in, cr.ReasoningEffort, want)
		}
	}

	// 序列化后字段名与 omitempty 行为
	b, _ := json.Marshal(p.buildChatRequest(&provider.Request{ReasoningEffort: "off"}, false))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_effort"] != "none" {
		t.Errorf("off 应序列化为 reasoning_effort=none，得到 %v", m["reasoning_effort"])
	}
	b2, _ := json.Marshal(p.buildChatRequest(&provider.Request{}, false))
	var m2 map[string]any
	_ = json.Unmarshal(b2, &m2)
	if _, ok := m2["reasoning_effort"]; ok {
		t.Error("空档位不应携带 reasoning_effort 字段")
	}
}

// ExtraBody 逃生舱：浅合并进序列化请求体，ExtraBody 优先。
func TestApplyExtraBody(t *testing.T) {
	p := New(Config{BaseURL: "http://x/v1", Model: "m", ExtraBody: map[string]any{
		"enable_thinking": false,
		"model":           "override", // 私有参数可覆盖标准字段
	}})
	b, err := p.applyExtraBody([]byte(`{"model":"m","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["enable_thinking"] != false {
		t.Errorf("ExtraBody 键未合并: %v", m)
	}
	if m["model"] != "override" {
		t.Errorf("ExtraBody 应覆盖标准字段: %v", m["model"])
	}
}

// 工具结果里的内联图片标记 "[IMAGE png <base64>]\n正文" 必须转成多模态 tool 消息，
// 且正文要跟着图片一起送达（此前只发「[截图已附上]」，图片说明被丢掉）。
func TestToolResultImageMarker(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	b64 := base64.StdEncoding.EncodeToString(raw)

	p := &Provider{imgFormats: map[string]string{}}
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "tool_use", ToolUseID: "t1", ToolName: "look", Input: []byte(`{}`)}}},
		{Role: message.RoleUser, Content: []message.ContentBlock{{Type: "tool_result", ForToolUseID: "t1", Text: "[IMAGE png " + b64 + "]\n第 3 帧：标题溢出"}}},
	}
	out := p.toOpenAIMessages("", "", msgs)
	parts, isParts := out[len(out)-1].Content.([]chatContentPart)
	if !isParts || len(parts) != 2 || parts[1].ImageURL == nil || !strings.HasSuffix(parts[1].ImageURL.URL, b64) {
		t.Fatalf("tool 消息应为 text+image_url 多模态内容，实际 %#v", out[len(out)-1].Content)
	}
	if parts[0].Text != "第 3 帧：标题溢出" {
		t.Fatalf("图片说明应随图送达，实际 %q", parts[0].Text)
	}
}
