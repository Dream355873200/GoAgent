package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

func TestToolResultImageBecomesImageBlock(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10})
	blk := toolResultBlock(message.ContentBlock{Type: "tool_result", ForToolUseID: "t1", Text: "[IMAGE jpeg " + b64 + "]\n#1 帧 64"})
	data, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"type":"image"`, `"media_type":"image/jpeg"`, `"data":"` + b64 + `"`, `"text":"#1 帧 64"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %s：%s", want, s)
		}
	}
	if strings.Contains(s, "[IMAGE") {
		t.Fatalf("标记不应以文本形式发出：%s", s)
	}
}

func TestToolResultBrokenImageDegradesToText(t *testing.T) {
	blk := toolResultBlock(message.ContentBlock{Type: "tool_result", ForToolUseID: "t1", Text: "[IMAGE png not*base64]\n说明"})
	data, _ := json.Marshal(blk)
	if s := string(data); strings.Contains(s, `"type":"image"`) || !strings.Contains(s, "数据无效") {
		t.Fatalf("坏图应降级为文本：%s", s)
	}
}

func TestPlainToolResultUnchanged(t *testing.T) {
	data, _ := json.Marshal(toolResultBlock(message.ContentBlock{Type: "tool_result", ForToolUseID: "t1", Text: "ok"}))
	if !strings.Contains(string(data), `"text":"ok"`) {
		t.Fatalf("普通结果应原样：%s", data)
	}
}
