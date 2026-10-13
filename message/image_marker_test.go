package message

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestImageMarkerTokenEstimate(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 30_000)))
	msg := Message{Role: RoleUser, Content: []ContentBlock{{Type: "tool_result", Text: "[IMAGE jpeg " + b64 + "]\n说明文字"}}}
	if got := EstimateMessageTokens(msg); got > ImageTokenEstimate*2 {
		t.Fatalf("内联图片应按图片估算，得到 %d tokens（base64 %d 字符）", got, len(b64))
	}
}

func TestSplitImageMarker(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	img, ok := SplitImageMarker("[IMAGE jpg " + b64 + "]\n正文")
	if !ok || !img.Valid || img.Data != b64 || img.Body != "正文" || img.MIME() != "image/jpeg" {
		t.Fatalf("解析错误: %+v ok=%v", img, ok)
	}
	if bad, ok := SplitImageMarker("[IMAGE png abc*]\n"); !ok || bad.Valid {
		t.Fatalf("损坏数据应 ok 且 Valid=false: %+v", bad)
	}
	if _, ok := SplitImageMarker("普通文本"); ok {
		t.Fatal("非标记文本不应识别")
	}
}
