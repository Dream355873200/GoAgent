package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent"
)

// BashInput 是 Bash 工具的输入。
type BashInput struct {
	Command     string `json:"command" desc:"要执行的 shell 命令" required:"true"`
	Description string `json:"description,omitempty" desc:"命令的简短描述"`
	Timeout     int    `json:"timeout,omitempty" desc:"超时时间（毫秒），默认 120000（2分钟）"`
}

// BashTool 返回 shell 命令执行工具定义。
func BashTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "执行 shell 命令并返回输出。支持 timeout 和工作目录配置。" +
			"用于运行构建、测试、git 操作、文件管理等系统命令。",
		Input:              BashInput{},
		Permission:         goagent.Normal,
		MaxResultSizeChars: 50000,
		InterruptMode:      "block",
		Execute: func(ctx goagent.Context, in BashInput) (string, error) {
			return executeBash(ctx, in)
		},
	}
}

func executeBash(ctx context.Context, in BashInput) (string, error) {
	if in.Command == "" {
		return "", fmt.Errorf("command 不能为空")
	}

	// 设置超时。
	timeoutMs := in.Timeout
	if timeoutMs <= 0 {
		timeoutMs = 120_000 // 默认 2 分钟。
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 根据平台选择 shell。
	shell := "bash"
	flag := "-c"
	if runtime.GOOS == "windows" {
		shell = "cmd"
		flag = "/c"
	}

	cmd := exec.CommandContext(childCtx, shell, flag, in.Command)
	output, err := cmd.CombinedOutput()
	result := strings.TrimRight(string(output), "\n\r ")

	if childCtx.Err() == context.DeadlineExceeded {
		if result != "" {
			return fmt.Sprintf("(命令超时 %dms)\n%s", timeoutMs, result), nil
		}
		return "", fmt.Errorf("命令超时 (%dms)", timeoutMs)
	}

	if err != nil {
		if result != "" {
			return fmt.Sprintf("(退出码非零)\n%s", result), nil
		}
		return "", fmt.Errorf("命令执行失败: %w", err)
	}

	if result == "" {
		return "(无输出)", nil
	}
	return result, nil
}

// runCommand 是内部辅助函数，执行命令并返回输出。
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(childCtx, name, args...)
	output, err := cmd.CombinedOutput()
	return strings.TrimRight(string(output), "\n\r "), err
}

// parseJSON 是内部辅助函数，解析 JSON 输入。
func parseJSON(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
