// Package prompts 提供 GoAgent 的嵌入式提示词资源。
//
// 对齐 Claude Code 的提示词系统，包含：
//   - 系统身份声明
//   - 工作流指令（语气/主动性/约定/工具策略）
//   - 系统提醒
//   - 上下文压缩提示词
//
// 所有提示词已翻译为中文。
package prompts

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed *.md
var FS embed.FS

// 提示词文件名常量。对齐 Claude Code 的 prompt sections。
const (
	Identity         = "system-identity.prompt.md"
	Workflow         = "system-workflow.prompt.md"
	Reminder         = "system-reminder.prompt.md"
	Compact          = "compact.prompt.md"
	DoingTasks       = "system-doing-tasks.prompt.md"
	Actions          = "system-actions.prompt.md"
	UsingTools       = "system-using-tools.prompt.md"
	ToneStyle        = "system-tone-style.prompt.md"
	OutputEfficiency = "system-output-efficiency.prompt.md"
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
