package openai

import (
	"encoding/json"
	"testing"

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
