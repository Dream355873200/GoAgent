package retrieval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ChunkTransformer 定长滑窗分块器（Transformer 参考实现）。
// Size 按字符（rune）计；Overlap 是相邻块的重叠字符数（< Size）。
// 每个块继承原文档 Metadata 并追加 chunk_index/chunk_total——
// 引用审计时能定位到原文档的第几块。
type ChunkTransformer struct {
	Size    int // 每块字符数，<=0 时取默认 500
	Overlap int // 相邻块重叠字符数，<0 视为 0，>= Size 视为 Size/4
}

// Transform 实现 Transformer。
func (c ChunkTransformer) Transform(_ context.Context, docs []Document) ([]Document, error) {
	size := c.Size
	if size <= 0 {
		size = 500
	}
	overlap := c.Overlap
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}

	var out []Document
	for _, d := range docs {
		runes := []rune(d.Content)
		if len(runes) <= size {
			out = append(out, d)
			continue
		}
		total := (len(runes)-overlap-1)/(size-overlap) + 1
		for start, i := 0, 0; start < len(runes); start, i = start+size-overlap, i+1 {
			end := start + size
			if end > len(runes) {
				end = len(runes)
			}
			chunk := d
			chunk.Content = string(runes[start:end])
			chunk.Metadata = cloneMetadata(d.Metadata)
			chunk.Embedding = nil // 块与原文档向量不同，必须重算
			out = append(out, chunk.WithMetadata("chunk_index", i).
				WithMetadata("chunk_total", total))
			if end == len(runes) {
				break
			}
		}
	}
	return out, nil
}

// cloneMetadata 深拷贝 map（分块产出各自独立，改一块不污染兄弟块）。
func cloneMetadata(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// FileLoader 本地文件/目录加载器（Loader 参考实现）。
// source 是文件或目录：目录时递归加载 .md/.txt（其他扩展名跳过）。
// 每个文件一个 Document，Content 为全文，Metadata 带 source/title。
type FileLoader struct{}

// Load 实现 Loader。
func (FileLoader) Load(_ context.Context, source string) ([]Document, error) {
	if source == "" {
		return nil, fmt.Errorf("retrieval: source 不能为空")
	}
	st, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("retrieval: 无法访问 %s: %w", source, err)
	}

	if !st.IsDir() {
		return loadFile(source)
	}

	var files []string
	err = filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err // 目录不可访问时如实上抛
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".txt":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: 遍历 %s 失败: %w", source, err)
	}

	var docs []Document
	for _, f := range files {
		d, err := loadFile(f)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d...)
	}
	return docs, nil
}

func loadFile(path string) ([]Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("retrieval: 读取 %s 失败: %w", path, err)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("retrieval: %s 不是 UTF-8 文本（二进制/GBK 文件不支持）", path)
	}
	return []Document{{
		Content:  string(data),
		Metadata: map[string]any{"source": path, "title": filepath.Base(path)},
	}}, nil
}
