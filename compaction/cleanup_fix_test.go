package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// 图片占位符：路径只从图片标记之后的正文里取（Windows 与 POSIX 路径都支持），
// base64 数据中的 "/" 不能被当成路径；文案不依赖宿主特有的工具名。
func TestFix_ImagePlaceholderPath(t *testing.T) {
	b64 := strings.Repeat("ab/cd+ef", 40)
	cases := []struct {
		name, body, want string
	}{
		{"windows", "截图保存在 C:\\shots\\a.png", "C:\\shots\\a.png"},
		{"posix", "截图保存在 /tmp/shots/a.png，共 1 张", "/tmp/shots/a.png"},
		{"no path", "（图片: a.png，123 字节）", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imagePlaceholder("[IMAGE png " + b64 + "]\n" + tc.body)
			if strings.Contains(got, "vision_ask") {
				t.Fatalf("占位文案不应引用宿主特有的工具: %s", got)
			}
			if tc.want == "" {
				if strings.Contains(got, "原图位于") {
					t.Fatalf("正文没有路径时不应编造路径: %s", got)
				}
				return
			}
			if !strings.Contains(got, "原图位于 "+tc.want+"；") {
				t.Fatalf("应带上原图路径 %q: %s", tc.want, got)
			}
		})
	}
}

// 手动压缩（Compact → MicrocompactMessages）按 tool_use 反查工具名：tool_result
// 块本身不带工具名，此前"可压缩工具"判断恒为 false，微压缩什么都不做；同时
// 改写必须是写时复制，不能篡改调用方的历史。
func TestFix_MicrocompactResolvesToolNamesAndCopiesOnWrite(t *testing.T) {
	big := strings.Repeat("x", 60_000)
	msgs := []message.Message{
		message.NewUserMessage("读一下文件"),
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{
			Type: "tool_use", ToolUseID: "t1", ToolName: "Read", Input: json.RawMessage(`{"file_path":"a.txt"}`),
		}}},
		message.NewToolResultMessage("t1", big, false),
		message.NewAssistantMessage("读完了"),
	}

	res := MicrocompactMessages(msgs, MicrocompactConfig{MaxToolResultChars: 50_000})

	if res.TokensFreed == 0 {
		t.Fatal("Read 的超大结果应被压缩")
	}
	got := res.Messages[2].Content[0].Text
	if len(got) >= len(big) {
		t.Fatalf("压缩后的结果应变短，实际 %d 字符", len(got))
	}
	if msgs[2].Content[0].Text != big {
		t.Fatal("压缩改写了调用方的历史消息")
	}
}
