package openai

import (
	"encoding/json"
	"testing"
)

// 流式拼接的 tool_use arguments 可能非法（模型输出截断/坏转义——实测出现过
// 值后跟裸反斜杠的碎片）：必须降级为可解析的占位参数，否则坏 RawMessage 入
// 历史后随每轮请求回传，端点 400 invalid_argument 且会话落盘失败。
func TestToolCallInvalidArgsDegradeToValidJSON(t *testing.T) {
	bad := `{"code": "import bpy` + `\` + `"}` // 值后跟裸反斜杠的碎片
	args := sanitizeToolCallArgs(bad)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("降级后应为合法 JSON: %v", err)
	}
	if _, ok := parsed["_invalid_arguments"]; !ok {
		t.Error("原文应保留在 _invalid_arguments 内")
	}
	if sanitizeToolCallArgs("") != "{}" {
		t.Error("空参数应归一为 {}")
	}
	if sanitizeToolCallArgs(`{"a":1}`) != `{"a":1}` {
		t.Error("合法参数应原样透传")
	}
}
