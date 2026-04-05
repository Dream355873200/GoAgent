// Package hooks — Shell 命令 Hook 实现。
//
// CommandHook 通过执行 shell 命令来实现 hook 逻辑。
// 命令的退出码决定 hook 结果：
//   - 0: 允许（不阻止）
//   - 非 0: 阻止
//
// 命令通过环境变量接收上下文信息。
//
// 对齐 Claude Code 的 command hook 配置。
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// CommandHook 通过执行 shell 命令实现 hook 逻辑。
type CommandHook struct {
	// HookName 是 hook 的名称。
	HookName string

	// Command 是要执行的 shell 命令。
	Command string

	// HookEvents 是此 hook 关注的事件列表。
	HookEvents []HookEvent

	// Timeout 是命令执行超时时间（秒）。0 表示无限制。
	Timeout int

	// Shell 是使用的 shell 程序。默认为 "bash"。
	Shell string
}

// Name 返回 hook 名称。
func (ch *CommandHook) Name() string {
	return ch.HookName
}

// Events 返回关注的事件列表。
func (ch *CommandHook) Events() []HookEvent {
	return ch.HookEvents
}

// Execute 执行 shell 命令。
func (ch *CommandHook) Execute(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	shell := ch.Shell
	if shell == "" {
		shell = "bash"
	}

	// 构建命令。
	cmd := exec.CommandContext(ctx, shell, "-c", ch.Command)

	// 设置环境变量传递上下文信息。
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("HOOK_EVENT=%s", hctx.Event.String()),
		fmt.Sprintf("HOOK_SESSION_ID=%s", hctx.SessionID),
	)

	if hctx.ToolName != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HOOK_TOOL_NAME=%s", hctx.ToolName))
	}
	if len(hctx.ToolInput) > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HOOK_TOOL_INPUT=%s", string(hctx.ToolInput)))
	}
	if hctx.ToolResult != "" {
		// 截断过长的结果以避免环境变量溢出。
		result := hctx.ToolResult
		if len(result) > 4096 {
			result = result[:4096] + "...[已截断]"
		}
		cmd.Env = append(cmd.Env, fmt.Sprintf("HOOK_TOOL_RESULT=%s", result))
	}

	// 执行命令。
	output, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(output))

	if err != nil {
		// 非 0 退出码表示阻止。
		if _, ok := err.(*exec.ExitError); ok {
			return &HookResult{
				Block:   true,
				Message: outputStr,
			}, nil
		}
		return nil, fmt.Errorf("执行命令失败: %w", err)
	}

	return &HookResult{
		Block:   false,
		Message: outputStr,
	}, nil
}

// FuncHook 通过函数实现 hook 逻辑（便捷包装）。
type FuncHook struct {
	HookName   string
	HookEvents []HookEvent
	Fn         func(ctx context.Context, hctx *HookContext) (*HookResult, error)
}

// Name 返回 hook 名称。
func (fh *FuncHook) Name() string {
	return fh.HookName
}

// Events 返回关注的事件列表。
func (fh *FuncHook) Events() []HookEvent {
	return fh.HookEvents
}

// Execute 执行函数。
func (fh *FuncHook) Execute(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	return fh.Fn(ctx, hctx)
}

// NewFuncHook 创建一个函数 hook 的便捷构造器。
func NewFuncHook(name string, events []HookEvent, fn func(ctx context.Context, hctx *HookContext) (*HookResult, error)) *FuncHook {
	return &FuncHook{
		HookName:   name,
		HookEvents: events,
		Fn:         fn,
	}
}

// ParseCommandHookConfig 从 JSON 配置解析 CommandHook。
//
// JSON 格式：
//
//	{
//	  "name": "my-hook",
//	  "command": "echo $HOOK_TOOL_NAME",
//	  "events": ["pre_tool_use"],
//	  "timeout": 30
//	}
func ParseCommandHookConfig(data json.RawMessage) (*CommandHook, error) {
	var cfg struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Events  []string `json:"events"`
		Timeout int      `json:"timeout"`
		Shell   string   `json:"shell"`
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 hook 配置失败: %w", err)
	}

	// 解析事件类型。
	var events []HookEvent
	for _, e := range cfg.Events {
		switch e {
		case "pre_tool_use":
			events = append(events, EventPreToolUse)
		case "post_tool_use":
			events = append(events, EventPostToolUse)
		case "stop":
			events = append(events, EventStop)
		case "permission_request":
			events = append(events, EventPermissionRequest)
		case "session_start":
			events = append(events, EventSessionStart)
		default:
			return nil, fmt.Errorf("未知的 hook 事件: %q", e)
		}
	}

	return &CommandHook{
		HookName:   cfg.Name,
		Command:    cfg.Command,
		HookEvents: events,
		Timeout:    cfg.Timeout,
		Shell:      cfg.Shell,
	}, nil
}
