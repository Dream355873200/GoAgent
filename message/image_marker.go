package message

import (
	"encoding/base64"
	"strings"
)

// 工具结果内联图片的约定：结果文本以 "[IMAGE <media> <base64>]\n" 开头，后面是正文。
// 视觉类工具用它把截图交给模型；各 provider 负责转成自己的多模态格式。

// ImageMarker 解析出的内联图片
type ImageMarker struct {
	Media string // 子类型：png / jpeg / webp / gif
	Data  string // base64
	Body  string // 标记之后的正文（可空）
	Valid bool   // base64 可解码；损坏的标记（截断等）应降级为纯文本，发给端点必 400
}

// MIME 完整媒体类型（jpg 归一为 image/jpeg）
func (m ImageMarker) MIME() string {
	media := strings.ToLower(m.Media)
	if media == "jpg" {
		media = "jpeg"
	}
	return "image/" + media
}

// SplitImageMarker 识别工具结果开头的图片标记；不是该格式时 ok=false
func SplitImageMarker(s string) (img ImageMarker, ok bool) {
	const prefix = "[IMAGE "
	if !strings.HasPrefix(s, prefix) {
		return img, false
	}
	rest := s[len(prefix):]
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return img, false
	}
	img.Media = rest[:sp]
	rest = rest[sp+1:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return img, false
	}
	// 右括号属于标记而非数据——不去掉的话 base64 解码必然失败
	img.Data = strings.TrimSuffix(strings.TrimSpace(rest[:nl]), "]")
	if img.Data == "" {
		return img, false
	}
	img.Body = strings.TrimSpace(rest[nl+1:])
	_, err := base64.StdEncoding.DecodeString(img.Data)
	img.Valid = err == nil
	return img, true
}
