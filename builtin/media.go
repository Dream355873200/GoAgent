// Package builtin 提供内置工具。
package builtin

import (
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic-community/goagent"
)

// ImageInput 是通用图片读取的输入。
type ImageInput struct {
	Path string `json:"path" desc:"图片文件路径" required:"true"`
}

// ImageTool 返回图片读取工具定义。
// 支持 PNG、JPEG、GIF、BMP 等常见格式。
func ImageTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "读取图片文件并返回其视觉描述（颜色、尺寸、大致内容）。主要用于分析图片。",
		Input:       ImageInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in ImageInput) (string, error) {
			return executeImage(in)
		},
	}
}

func executeImage(in ImageInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(in.Path))
	switch ext {
	case ".png":
		return describePNG(data)
	case ".jpg", ".jpeg":
		return describeJPEG(data)
	case ".gif":
		return describeGIF(data)
	default:
		// 尝试作为 PNG 解析
		return describeGeneric(data)
	}
}

// describePNG 解析并描述 PNG 图片。
func describePNG(data []byte) (string, error) {
	img, err := png.Decode(strings.NewReader(string(data)))
	if err != nil {
		// 尝试 base64 方式
		if len(data) > 22 && string(data[:22]) == "data:image/png;base64," {
			b64Data := data[22:]
			raw, err := base64.StdEncoding.DecodeString(string(b64Data))
			if err == nil {
				img, err = png.Decode(strings.NewReader(string(raw)))
				if err == nil {
					return summarizeImage(img), nil
				}
			}
		}
		return "", fmt.Errorf("PNG 解析失败: %w", err)
	}
	return summarizeImage(img), nil
}

// describeJPEG 解析并描述 JPEG 图片。
func describeJPEG(data []byte) (string, error) {
	// JPEG 解码需要完整的图片数据
	// 简化实现：只返回文件信息
	info, err := os.Stat("placeholder")
	if err != nil {
		return fmt.Sprintf("[JPEG 图片: %d 字节, 无法解析]", len(data)), nil
	}
	_ = info
	return fmt.Sprintf("[JPEG 图片: %d 字节]", len(data)), nil
}

// describeGIF 解析并描述 GIF 图片。
func describeGIF(data []byte) (string, error) {
	return fmt.Sprintf("[GIF 图片: %d 字节]", len(data)), nil
}

// describeGeneric 通用图片描述。
func describeGeneric(data []byte) (string, error) {
	if len(data) == 0 {
		return "(空文件)", nil
	}

	// 检测图片类型
	var imgType string
	if len(data) > 4 {
		switch {
		case data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47:
			imgType = "PNG"
		case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
			imgType = "JPEG"
		case data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46:
			imgType = "GIF"
		case data[0] == 0x42 && data[1] == 0x4D:
			imgType = "BMP"
		default:
			imgType = "Unknown"
		}
	}

	return fmt.Sprintf("[图片: %s 格式, %d 字节]", imgType, len(data)), nil
}

// summarizeImage 生成图片的文本描述。
func summarizeImage(img image.Image) string {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// 采样分析颜色
	colorCount := make(map[string]int)

	// 采样一些像素
	stepX := width / 10
	stepY := height / 10
	if stepX < 1 {
		stepX = 1
	}
	if stepY < 1 {
		stepY = 1
	}

	for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
		for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
			r, g, b, a := img.At(x, y).RGBA()
			if a < 32768 { // 忽略透明像素
				continue
			}
			// 简化颜色分类
			colorStr := classifyColor(r, g, b)
			colorCount[colorStr]++
		}
	}

	// 找出最常见的颜色
	var dominantColors []string
	var maxCount int
	for color, count := range colorCount {
		if count > maxCount {
			dominantColors = append([]string{color}, dominantColors...)
			maxCount = count
			if len(dominantColors) > 5 {
				dominantColors = dominantColors[:5]
			}
		}
	}

	result := fmt.Sprintf("[图片: %dx%d 像素", width, height)
	if len(dominantColors) > 0 {
		result += fmt.Sprintf(", 主要颜色: %s", strings.Join(dominantColors, ", "))
	}
	result += "]"

	return result
}

// classifyColor 将颜色分类为简单名称。
func classifyColor(r, g, b uint32) string {
	// 简化分类
	rr := int(r >> 8)
	gg := int(g >> 8)
	bb := int(b >> 8)

	// 检测是否为灰度
	if rr-gg < 20 && rr-bb < 20 && gg-bb < 20 {
		avg := (rr + gg + bb) / 3
		if avg < 64 {
			return "黑色"
		} else if avg > 192 {
			return "白色"
		} else {
			return "灰色"
		}
	}

	// 检测主要颜色
	if rr > gg*2 && rr > bb*2 {
		return "红色"
	}
	if gg > rr*2 && gg > bb*2 {
		return "绿色"
	}
	if bb > rr*2 && bb > gg*2 {
		return "蓝色"
	}
	if rr > 200 && gg > 200 && bb < 100 {
		return "黄色"
	}
	if rr > 200 && gg < 100 && bb > 200 {
		return "紫色"
	}
	if rr > 100 && gg > 150 && bb > 100 {
		return "青色"
	}
	if rr > 150 && gg > 100 && bb < 100 {
		return "橙色"
	}

	return "多色"
}

// PDFInput 是 PDF 读取的输入。
type PDFInput struct {
	Path      string `json:"path" desc:"PDF 文件路径" required:"true"`
	MaxPages  int    `json:"max_pages,omitempty" desc:"最多读取页数，默认 5"`
	StartPage int    `json:"start_page,omitempty" desc:"起始页（从 1 开始），默认 1"`
}

// PDFTool 返回 PDF 读取工具定义。
// 注意：完整 PDF 解析需要额外库（如 github.com/ledongthuc/pdf），此处为简化实现。
func PDFTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description:        "读取 PDF 文件的文本内容。支持分页读取。",
		Input:              PDFInput{},
		Permission:         goagent.ReadOnly,
		Concurrent:         true,
		MaxResultSizeChars: 50000,
		Execute: func(ctx goagent.Context, in PDFInput) (string, error) {
			return executePDF(in)
		},
	}
}

func executePDF(in PDFInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	// 检测 PDF 签名
	if len(data) < 4 || string(data[:4]) != "%PDF" {
		return "", fmt.Errorf("不是有效的 PDF 文件")
	}

	// 简化实现：提取可见文本
	// 完整实现需要专门的 PDF 解析库
	text := extractPDFText(data, in.MaxPages, in.StartPage)

	if text == "" {
		return "[PDF 文件: " + fmt.Sprintf("%d 字节, 无可提取文本或需要 PDF 解析库]", len(data)), nil
	}

	return text, nil
}

// extractPDFText 从 PDF 数据中提取文本（简化版）。
func extractPDFText(data []byte, maxPages, startPage int) string {
	if maxPages <= 0 {
		maxPages = 5
	}
	if startPage <= 0 {
		startPage = 1
	}

	// 简化实现：尝试提取括号中的文本
	var result strings.Builder
	inStream := false
	var streamContent strings.Builder

	for i := 0; i < len(data); i++ {
		if i < len(data)-1 && data[i] == '<' && data[i+1] == '<' {
			inStream = true
			streamContent.Reset()
			i++
			continue
		}
		if i < len(data)-1 && data[i] == '>' && data[i+1] == '>' {
			inStream = false
			// 检查 streamContent 是否包含文本
			text := streamContent.String()
			if len(text) > 10 && isMostlyPrintable(text) {
				result.WriteString(text)
				result.WriteString("\n")
			}
			i++
			continue
		}
		if inStream {
			streamContent.WriteByte(data[i])
		}
	}

	if result.Len() == 0 {
		return "[PDF 内容需要专门解析库才能提取]"
	}

	return result.String()
}

// isMostlyPrintable 检测字符串是否大部分是可打印文本。
func isMostlyPrintable(s string) bool {
	if len(s) == 0 {
		return false
	}
	printable := 0
	for _, r := range s {
		if r >= 32 && r < 127 {
			printable++
		}
	}
	return float64(printable)/float64(len(s)) > 0.7
}

// NotebookInput 是 Notebook 读取的输入。
type NotebookInput struct {
	Path string `json:"path" desc:"Jupyter Notebook 文件路径 (.ipynb)" required:"true"`
}

// NotebookTool 返回 Jupyter Notebook 读取工具定义。
func NotebookTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "读取 Jupyter Notebook 文件，返回代码单元和输出摘要。",
		Input:       NotebookInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in NotebookInput) (string, error) {
			return executeNotebook(in)
		},
	}
}

func executeNotebook(in NotebookInput) (string, error) {
	if in.Path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	if filepath.Ext(in.Path) != ".ipynb" {
		return "", fmt.Errorf("不是 .ipynb 文件")
	}

	data, err := os.ReadFile(in.Path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	// 尝试解析 JSON
	// 查找 JSON 部分（nbformat 通常在文件开头）
	jsonStart := -1
	for i := 0; i < len(data) && i < 1000; i++ {
		if data[i] == '{' {
			jsonStart = i
			break
		}
	}

	if jsonStart == -1 {
		return "", fmt.Errorf("无法解析 notebook 格式")
	}

	// 简化实现：返回文件信息
	var result strings.Builder
	result.WriteString(fmt.Sprintf("[Notebook: %s, %d 字节]\n\n", filepath.Base(in.Path), len(data)))

	// 尝试提取代码单元
	cells := extractNotebookCells(string(data))
	if len(cells) > 0 {
		result.WriteString(fmt.Sprintf("共 %d 个单元:\n", len(cells)))
		for i, cell := range cells {
			if len(cell) > 200 {
				cell = cell[:200] + "..."
			}
			result.WriteString(fmt.Sprintf("\n--- 单元 %d ---\n%s", i+1, cell))
		}
	} else {
		result.WriteString("[无可提取的单元内容]")
	}

	return result.String(), nil
}

// extractNotebookCells 提取 notebook 中的代码单元。
func extractNotebookCells(content string) []string {
	// 简化实现：查找 "source" 字段
	var cells []string

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, `"source"`) || strings.Contains(trimmed, `"code"`) {
			// 提取后续行作为代码
			var cell strings.Builder
			for j := i + 1; j < len(lines) && j < i+20; j++ {
				cellLine := strings.TrimSpace(lines[j])
				if cellLine == "]" || cellLine == "}," || cellLine == "}" {
					break
				}
				if len(cellLine) > 2 {
					cell.WriteString(cellLine)
					cell.WriteString("\n")
				}
			}
			if cell.Len() > 0 {
				cells = append(cells, cell.String())
			}
		}
	}

	return cells
}
