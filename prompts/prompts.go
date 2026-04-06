// Package prompts 提供 GoAgent 的嵌入式提示词资源。
//
// 对齐 Claude Code 的提示词系统，包含：
//   - 系统身份声明
//   - 系统规则（权限模式、压缩、hooks）
//   - 执行任务指令
//   - 谨慎执行操作
//   - 工具使用策略
//   - 语气和风格
//   - 输出效率
//   - 上下文压缩提示词
//   - YOLO 分类器提示词
//
// 所有提示词已翻译为中文，精确对齐 Claude Code 源码。
// 支持通过 ExportDefaults 导出到磁盘供用户查看和修改。
package prompts

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed *.md
var FS embed.FS

// 提示词文件名常量。对齐 Claude Code 的 prompt sections。
const (
	Identity         = "system-identity.prompt.md"
	Reminder         = "system-reminder.prompt.md"
	Compact          = "compact.prompt.md"
	DoingTasks       = "system-doing-tasks.prompt.md"
	Actions          = "system-actions.prompt.md"
	UsingTools       = "system-using-tools.prompt.md"
	ToneStyle        = "system-tone-style.prompt.md"
	OutputEfficiency = "system-output-efficiency.prompt.md"
	YoloClassifier   = "yolo-classifier.prompt.md"
)

// MustLoad 读取嵌入的提示词文件，文件不存在时 panic。
func MustLoad(name string) string {
	data, err := FS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("prompts: 无法加载提示词 %q: %v", name, err))
	}
	return strings.TrimSpace(string(data))
}

// Load 读取嵌入的提示词文件。
func Load(name string) (string, error) {
	data, err := FS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("prompts: 无法加载提示词 %q: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// LoadWithVars 读取提示词并替换模板变量。
// 变量格式为 $key，通过 vars map 传入替换值。
func LoadWithVars(name string, vars map[string]string) string {
	content := MustLoad(name)
	for k, v := range vars {
		content = strings.ReplaceAll(content, "$"+k, v)
	}
	return content
}

// DefaultFiles 返回所有嵌入的提示词文件名列表。
func DefaultFiles() []string {
	return []string{
		Identity, Reminder, Compact,
		DoingTasks, Actions, UsingTools,
		ToneStyle, OutputEfficiency, YoloClassifier,
	}
}

// ExportDefaults 将所有嵌入的默认提示词导出到指定目录。
// 目录不存在则自动创建。已存在的文件不覆盖。
// 返回导出的文件绝对路径列表。
func ExportDefaults(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("prompts: 无法创建目录 %q: %w", dir, err)
	}

	var exported []string
	for _, name := range DefaultFiles() {
		dest := filepath.Join(dir, name)
		// 确保父目录存在。
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return nil, fmt.Errorf("prompts: 无法创建目录 %q: %w", filepath.Dir(dest), err)
		}

		// 已存在则跳过。
		if _, err := os.Stat(dest); err == nil {
			abs, _ := filepath.Abs(dest)
			exported = append(exported, abs)
			continue
		}

		data, err := FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("prompts: 无法读取嵌入文件 %q: %w", name, err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return nil, fmt.Errorf("prompts: 无法写入文件 %q: %w", dest, err)
		}
		abs, _ := filepath.Abs(dest)
		exported = append(exported, abs)
	}
	return exported, nil
}
