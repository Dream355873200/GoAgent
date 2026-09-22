package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 无 frontmatter 的文件：meta 零值、正文原样、描述回落首行。
func TestParseFrontmatter_Absent(t *testing.T) {
	meta, body := parseFrontmatter("# 标题\n\n第一行描述。\n\n正文")
	if meta != (skillMeta{}) {
		t.Fatalf("无 frontmatter 应返回零值 meta, got %+v", meta)
	}
	if body != "# 标题\n\n第一行描述。\n\n正文" {
		t.Fatalf("正文应原样保留, got %q", body)
	}
	if desc := extractDescription(body); desc != "第一行描述。" {
		t.Fatalf("描述兜底应取首个非标题行, got %q", desc)
	}
}

// 标准 frontmatter：字段全提取、正文剥离、键名归一（下划线/大小写兼容）。
func TestParseFrontmatter_Standard(t *testing.T) {
	src := "---\nname: testing\ndescription: 复合分层自动化测试\nwhen_to_use: 修改代码后需设备验证时\nallowed-tools: tap, screenshot, screen_diff\n---\n\n# 正文标题\n\n正文内容"
	meta, body := parseFrontmatter(src)
	if meta.Name != "testing" || meta.Description != "复合分层自动化测试" {
		t.Fatalf("name/description 提取错误: %+v", meta)
	}
	if meta.WhenToUse != "修改代码后需设备验证时" {
		t.Fatalf("when_to_use 兼容解析失败: %q", meta.WhenToUse)
	}
	if meta.AllowedTools != "tap, screenshot, screen_diff" {
		t.Fatalf("allowed-tools 提取错误: %q", meta.AllowedTools)
	}
	if body != "# 正文标题\n\n正文内容" {
		t.Fatalf("正文应剥离 frontmatter, got %q", body)
	}
}

// 驼峰 whenToUse 也兼容；frontmatter 未闭合时视为无 frontmatter。
func TestParseFrontmatter_Edges(t *testing.T) {
	meta, _ := parseFrontmatter("---\nwhenToUse: 收尾交付时\n---\n正文")
	if meta.WhenToUse != "收尾交付时" {
		t.Fatalf("驼峰键应兼容: %q", meta.WhenToUse)
	}
	_, body := parseFrontmatter("---\nname: x\n无闭合")
	if !strings.Contains(body, "name: x") {
		t.Fatal("未闭合的 frontmatter 应视为正文")
	}
}

// loadSkill：name 缺省回落文件名；description 缺省回落正文首行。
func TestLoadSkill_Fallbacks(t *testing.T) {
	s := loadSkill("from-file", "/tmp/x.md", "---\n---\n# 标题\n首行描述\n", SourceProject)
	if s.Name != "from-file" {
		t.Fatalf("name 应回落文件名, got %q", s.Name)
	}
	if s.Description != "首行描述" {
		t.Fatalf("description 应回落正文首行, got %q", s.Description)
	}
	if strings.Contains(s.Content, "---") {
		t.Fatal("Content 不应包含 frontmatter 分隔符")
	}
}

// 集成：临时目录扫描 → frontmatter 元数据生效、注册键用 frontmatter name。
func TestScanDir_Frontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.md"), []byte(
		"---\nname: demo-skill\ndescription: 演示技能\n---\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(dir, "")
	if err := r.scanDir(dir, SourceProject); err != nil {
		t.Fatal(err)
	}
	s := r.Get("demo-skill")
	if s == nil {
		t.Fatal("应以 frontmatter name 注册")
	}
	if s.Description != "演示技能" || s.Content != "正文" {
		t.Fatalf("元数据不符: %+v", s)
	}
}
