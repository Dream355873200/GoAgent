package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func truncNote(size int) string {
	return fmt.Sprintf("…（文件过大已截断：原始 %d 字节，仅保留前 64 KiB）", size)
}

// splitTruncated 拆出截断说明之前保留的正文，并校验其为原文前缀且 UTF-8 合法。
func splitTruncated(t *testing.T, got, original string) string {
	t.Helper()
	suffix := "\n" + truncNote(len(original))
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("缺少截断说明，结尾为 %q", got[max(0, len(got)-120):])
	}
	kept := strings.TrimSuffix(got, suffix)
	if !utf8.ValidString(kept) {
		t.Fatal("截断后的内容不是合法 UTF-8")
	}
	if len(kept) > maxInjectBytes {
		t.Fatalf("保留了 %d 字节，超过上限 %d", len(kept), maxInjectBytes)
	}
	if !strings.HasPrefix(original, kept) {
		t.Fatal("保留内容不是原文前缀")
	}
	return kept
}

func TestClaudeMDTruncatedAtUTF8Boundary(t *testing.T) {
	// 不同前缀长度让 64 KiB 边界分别落在 3 字节汉字的首、中、尾字节。
	for _, prefix := range []string{"", "a", "ab"} {
		path := filepath.Join(t.TempDir(), "CLAUDE.md")
		content := prefix + strings.Repeat("中文内容", 3<<20/12) // 约 3 MB，单行
		writeFile(t, path, content)

		layers := (&ClaudeMDConfig{ProjectPath: path}).LoadClaudeMD()
		if len(layers) != 1 {
			t.Fatalf("前缀 %q: 加载到 %d 层", prefix, len(layers))
		}
		kept := splitTruncated(t, layers[0].Content, content)
		if len(kept) < maxInjectBytes-(utf8.UTFMax-1) {
			t.Fatalf("前缀 %q: 只保留了 %d 字节，回退过多", prefix, len(kept))
		}

		prompt := (&ClaudeMDConfig{ProjectPath: path}).FormatForSystemPrompt()
		if len(prompt) > maxInjectBytes+1024 {
			t.Fatalf("前缀 %q: 系统提示词长度 %d 未被限制", prefix, len(prompt))
		}
	}
}

// 截断落在整行边界：跨越 64 KiB 的行（例如 @import 行）要么完整保留要么整行丢弃。
func TestTruncationKeepsWholeLines(t *testing.T) {
	const importLine = "@docs/architecture/very-long-import-path.md"
	var b strings.Builder
	for b.Len() < maxInjectBytes-200 {
		b.WriteString("普通内容行\n")
	}
	b.WriteString(strings.Repeat("x", maxInjectBytes-10-b.Len()-1) + "\n")
	b.WriteString(importLine + "\n") // 从 64 KiB-10 处开始，跨越上限
	for b.Len() < 2*maxInjectBytes {
		b.WriteString("后续内容行\n")
	}
	content := b.String()
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	writeFile(t, path, content)

	got, err := readFileIfExists(path)
	if err != nil {
		t.Fatal(err)
	}
	kept := splitTruncated(t, got, content)
	if strings.Contains(kept, "@docs") {
		t.Fatal("跨越上限的 @import 行被截成了半行")
	}
	if content[len(kept)] != '\n' {
		t.Fatalf("截断点不在行尾: %q", content[len(kept)-10:len(kept)+10])
	}
}

func TestInjectedFilesTruncated(t *testing.T) {
	dir := t.TempDir()
	bigCtx := strings.Repeat("项目约定第一条。\n", 20000)
	writeFile(t, filepath.Join(dir, "big.md"), bigCtx)
	writeFile(t, filepath.Join(dir, "small.md"), "\n  小文件  \n")

	memDir := filepath.Join(dir, "mem")
	m := NewManager(Config{
		MemoryDir:      memDir,
		ProjectContext: []string{filepath.Join(dir, "big.md"), filepath.Join(dir, "small.md")},
	})

	ctx := m.LoadProjectContext()
	parts := strings.Split(ctx, "\n\n---\n\n")
	if len(parts) != 2 || parts[1] != "小文件" {
		t.Fatalf("项目上下文拼接异常: %d 段，末段 %q", len(parts), parts[len(parts)-1])
	}
	splitTruncated(t, parts[0], bigCtx)

	// 单行超长的 MEMORY.md 不触发 200 行截断，只能靠字节上限。
	bigMem := strings.Repeat("记", 100000)
	if err := m.GetAutoMemory().SaveMain(bigMem); err != nil {
		t.Fatal(err)
	}
	splitTruncated(t, m.GetAutoMemory().LoadMain(), bigMem)
	splitTruncated(t, strings.TrimPrefix(m.GetAutoMemory().FormatForInjection(), "# auto memory\n\n"), bigMem)
	splitTruncated(t, m.LoadPersistentMemory(), bigMem)

	if n := len(m.BuildSystemPromptSuffix()); n > 2*maxInjectBytes+4096 {
		t.Fatalf("系统提示词后缀 %d 字节，未被限制", n)
	}
}

func TestSmallFilesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	writeFile(t, path, "\n\n  内容\n第二行  \n\n")
	if got, err := readFileIfExists(path); err != nil || got != "内容\n第二行" {
		t.Fatalf("readFileIfExists = %q, %v", got, err)
	}
	if got, err := readFileIfExists(filepath.Join(dir, "missing.md")); err != nil || got != "" {
		t.Fatalf("不存在的文件应返回空且无错误: %q, %v", got, err)
	}

	exact := strings.Repeat("a", maxInjectBytes)
	writeFile(t, path, exact)
	if got, _ := readFileIfExists(path); got != exact {
		t.Fatal("恰好 64 KiB 的文件不应被截断")
	}
}

func TestLoadMainLineLimit(t *testing.T) {
	am := NewAutoMemory(t.TempDir())
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "第 %d 行\n", i)
	}
	if err := am.SaveMain(b.String()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(am.LoadMain(), "\n")
	if len(lines) != 201 || lines[199] != "第 199 行" || lines[200] != "... (truncated)" {
		t.Fatalf("行数截断异常: %d 行，末行 %q", len(lines), lines[len(lines)-1])
	}

	// 恰好 200 行且以换行结尾时不应被判为超长。
	exact := strings.Repeat("一行\n", 200)
	if err := am.SaveMain(exact); err != nil {
		t.Fatal(err)
	}
	if got := am.LoadMain(); got != strings.TrimSpace(exact) {
		t.Fatalf("200 行的主文件被截断，末尾为 %q", got[len(got)-30:])
	}
}

func TestCutPoint(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		limit int
		want  int
	}{
		{"4 字节字符跨越边界", []byte("ab😀😀"), 4, 2},
		{"换行恰在上限处", []byte("abcd\nefgh"), 4, 4},
		{"后半段有换行", []byte("abcdef\nghijk"), 8, 6},
		{"换行只在前半段时按字符边界截", []byte("a\nbcdefghij"), 8, 8},
		{"非法 UTF-8 最多回退 3 字节", bytes.Repeat([]byte{0x80}, 16), 8, 5},
	}
	for _, c := range cases {
		if got := cutPoint(c.data, c.limit); got != c.want {
			t.Errorf("%s: cutPoint = %d, want %d", c.name, got, c.want)
		}
	}
}
