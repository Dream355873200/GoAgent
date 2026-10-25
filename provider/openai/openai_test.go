package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 思考档位按模型家族翻译（thinking.go）：未知家族 off=省略字段（旧版映射
// "none" 会被 o-series 拒收）；开关型走 enable_thinking；端点拒收过档位
// 参数的（thinkingKnobOff）停发全部思考参数。
func TestBuildChatRequestReasoningEffort(t *testing.T) {
	p := New(Config{BaseURL: "http://x/v1", Model: "m"})

	// 未知家族：默认五档，off 省略字段
	cases := map[string]string{
		"off":  "",
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

	// 序列化后 off 不携带 reasoning_effort
	b, _ := json.Marshal(p.buildChatRequest(&provider.Request{ReasoningEffort: "off"}, false))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Errorf("off 应省略 reasoning_effort，得到 %v", m["reasoning_effort"])
	}

	// 开关型（GLM/Qwen）：档位翻成 enable_thinking，不写 reasoning_effort
	pc := New(Config{BaseURL: "http://x/v1", Model: "glm-5"})
	cr := pc.buildChatRequest(&provider.Request{ReasoningEffort: "on"}, false)
	if cr.EnableThinking == nil || !*cr.EnableThinking || cr.ReasoningEffort != "" {
		t.Fatalf("glm-5 的 on 应为 enable_thinking=true 且无 effort，实际 %+v", cr)
	}
	cr = pc.buildChatRequest(&provider.Request{ReasoningEffort: "off"}, false)
	if cr.EnableThinking == nil || *cr.EnableThinking {
		t.Fatalf("glm-5 的 off 应为 enable_thinking=false，实际 %+v", cr)
	}

	// Claude 家族：max 档原样透传给兼容层
	pa := New(Config{BaseURL: "http://x/v1", Model: "claude-opus-4"})
	cr = pa.buildChatRequest(&provider.Request{ReasoningEffort: "max"}, false)
	if cr.ReasoningEffort != "max" {
		t.Fatalf("claude max 应透传 reasoning_effort=max，实际 %q", cr.ReasoningEffort)
	}

	// 端点拒收过档位参数：停发全部思考参数
	p.markThinkingKnobRejected("m")
	cr = p.buildChatRequest(&provider.Request{ReasoningEffort: "high"}, false)
	if cr.ReasoningEffort != "" || cr.EnableThinking != nil {
		t.Fatalf("knobOff 后不应携带思考参数，实际 %+v", cr)
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
	out := p.toOpenAIMessages("", true, "", msgs)
	parts, isParts := out[len(out)-1].Content.([]chatContentPart)
	if !isParts || len(parts) != 2 || parts[1].ImageURL == nil || !strings.HasSuffix(parts[1].ImageURL.URL, b64) {
		t.Fatalf("tool 消息应为 text+image_url 多模态内容，实际 %#v", out[len(out)-1].Content)
	}
	if parts[0].Text != "第 3 帧：标题溢出" {
		t.Fatalf("图片说明应随图送达，实际 %q", parts[0].Text)
	}

	// toolImgs=false（Bedrock 类通道）时同一标记必须降级为文本占位，不发 image_url 块
	out = p.toOpenAIMessages("", false, "", msgs)
	if parts, ok := out[len(out)-1].Content.([]chatContentPart); ok {
		t.Fatalf("降级后 tool 消息不应是多模态内容，实际 %#v", parts)
	}
	if s, _ := out[len(out)-1].Content.(string); !strings.Contains(s, "图片已省略") || !strings.Contains(s, "第 3 帧") {
		t.Fatalf("降级后应为文本占位+说明，实际 %q", out[len(out)-1].Content)
	}
}

// 通道拒收工具内嵌图片的自适应识别：image_url 块只可能来自本框架的工具结果，
// 任何 400 引用它都应触发该 端点|模型 降级记忆；编码类错误（不含 image_url）
// 不应误触发。
func TestMaybeDisableToolImages(t *testing.T) {
	p := &Provider{imgFormats: map[string]string{}, toolImgOff: map[string]bool{}}

	bedrock := "API 错误 (HTTP 400): {\"error\":{\"message\":\"InvokeModelWithResponseStream: ValidationException: tool_result.content.1: Input tag 'image_url' found using 'type' does not match any of the expected tags: 'image', 'text'\"}}"
	if !p.maybeDisableToolImages("opus-5.5", fmt.Errorf("API 错误 (HTTP 400): %s", bedrock)) {
		t.Fatalf("Bedrock 拒收 image_url 应触发降级")
	}
	if !p.toolImagesOff("opus-5.5") {
		t.Fatalf("降级应按 端点|模型 记忆")
	}
	if p.toolImagesOff("glm-5") {
		t.Fatalf("记忆应按模型隔离，不得影响其他模型")
	}

	if p.maybeDisableToolImages("glm-5", fmt.Errorf("API 错误 (HTTP 400): 图片输入格式/解析错误")) {
		t.Fatalf("编码类错误（不含 image_url）不应触发降级")
	}
}
