// Package userinput 实现用户输入预处理流水线。
//
// 对齐 Claude Code 的 src/utils/processUserInput/：
//   - 斜杠命令解析（/help, /clear, /compact, /skill-name 等）
//   - Skill 模板触发与参数替换
//   - @mention 文件注入
//
// 使用方式：在用户输入进入 loop 之前调用 Process()。
package userinput

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Dream355873200/GoAgent/skill"
)

// Action 是输入处理后的动作类型。
type Action int

const (
	// ActionSendToLLM 将处理后的文本发送给 LLM。
	ActionSendToLLM Action = iota
	// ActionBuiltinCommand 已执行内置命令，不需要发送给 LLM。
	ActionBuiltinCommand
	// ActionSkill 触发了 skill，文本已被 skill 模板替换。
	ActionSkill
)

// Result 是输入处理的结果。
type Result struct {
	// Action 表示后续应如何处理。
	Action Action
	// Text 是处理后的文本（可能被 skill 模板替换）。
	Text string
	// CommandOutput 是内置命令的输出（仅 ActionBuiltinCommand 有效）。
	CommandOutput string
	// SkillName 是触发的 skill 名称（仅 ActionSkill 有效）。
	SkillName string
}

// BuiltinCommandHandler 处理内置命令。
// 返回输出文本和 true 表示已处理；返回 "", false 表示未识别。
type BuiltinCommandHandler func(ctx context.Context, command string, args string) (output string, handled bool)

// Processor 是输入预处理器。
type Processor struct {
	// skills 是 skill 注册表。
	skills *skill.Registry

	// builtins 是内置命令处理器列表。
	builtins []BuiltinCommandHandler
}

// NewProcessor 创建一个新的输入预处理器。
func NewProcessor(skills *skill.Registry) *Processor {
	p := &Processor{skills: skills}
	// 注册默认内置命令。
	p.builtins = append(p.builtins, defaultBuiltinHandler)
	return p
}

// AddBuiltinHandler 添加自定义内置命令处理器。
func (p *Processor) AddBuiltinHandler(h BuiltinCommandHandler) {
	p.builtins = append(p.builtins, h)
}

// Process 处理用户输入。
func (p *Processor) Process(ctx context.Context, input string) Result {
	input = strings.TrimSpace(input)

	// 空输入。
	if input == "" {
		return Result{Action: ActionSendToLLM, Text: input}
	}

	// 处理 @mention 文件注入。
	input = expandMentions(input)

	// 不是斜杠命令，直接发给 LLM。
	if !strings.HasPrefix(input, "/") {
		return Result{Action: ActionSendToLLM, Text: input}
	}

	// 解析斜杠命令。
	command, args := parseSlashCommand(input)

	// 尝试内置命令。
	for _, handler := range p.builtins {
		if output, handled := handler(ctx, command, args); handled {
			return Result{
				Action:        ActionBuiltinCommand,
				CommandOutput: output,
			}
		}
	}

	// 尝试 skill 匹配。
	if p.skills != nil {
		if sk := p.skills.Get(command); sk != nil {
			text, err := p.skills.Execute(command, args)
			if err != nil {
				return Result{
					Action:        ActionBuiltinCommand,
					CommandOutput: fmt.Sprintf("skill %q 执行失败: %s", command, err),
				}
			}
			return Result{
				Action:    ActionSkill,
				Text:      text,
				SkillName: command,
			}
		}
	}

	// 未识别的斜杠命令，作为普通文本发给 LLM。
	return Result{Action: ActionSendToLLM, Text: input}
}

// parseSlashCommand 解析 "/command args" 格式。
func parseSlashCommand(input string) (command, args string) {
	// 去掉开头的 /
	input = strings.TrimPrefix(input, "/")
	parts := strings.SplitN(input, " ", 2)
	command = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return
}

// expandMentions 展开 @file_path 引用，注入文件内容。
func expandMentions(input string) string {
	// 简单实现：匹配 @filepath 模式（路径中无空格）。
	// 更复杂的实现可以处理引号路径。
	words := strings.Fields(input)
	var changed bool
	for i, w := range words {
		if !strings.HasPrefix(w, "@") || len(w) < 2 {
			continue
		}
		path := w[1:]
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 文件不存在，保留原文
		}
		words[i] = fmt.Sprintf("\n<file path=%q>\n%s\n</file>\n", path, string(data))
		changed = true
	}
	if changed {
		return strings.Join(words, " ")
	}
	return input
}

// defaultBuiltinHandler 处理默认的内置命令。
func defaultBuiltinHandler(_ context.Context, command, args string) (string, bool) {
	switch command {
	case "help":
		return formatHelp(), true
	case "clear":
		return "[会话已清空]", true
	case "compact":
		return "[触发上下文压缩]", true
	case "config":
		return "[配置管理]", true
	case "status":
		return "[状态信息]", true
	default:
		return "", false
	}
}

func formatHelp() string {
	return `可用命令：
  /help     - 显示此帮助
  /clear    - 清空会话历史
  /compact  - 手动触发上下文压缩
  /model    - 查看/切换模型 (用法: /model <model_name>)
  /tools    - 列出已注册工具
  /cost     - 显示 token 用量
  /tasks    - 列出任务
  /bg       - 列出后台任务
  /sessions - 列出历史会话
  /resume   - 恢复历史会话 (用法: /resume <session_id>)
  /config   - 管理配置
  /status   - 显示状态信息
  /exit     - 退出
  /<skill>  - 运行已注册的 skill`
}
