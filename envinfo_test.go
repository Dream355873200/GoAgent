// envinfo_test.go 验证环境信息段（平台/Shell/工作目录）注入 system prompt。
//
// 背景：会话工作目录只存在于 ctx value 时模型「能力存在但不可见」——
// system prompt 不告知工作目录，模型会猜路径（如 /workspace）并误用
// 当前平台不存在的命令（Windows cmd 下跑 pwd）。见 buildSystemPrompt。
package goagent

import (
	"os"
	"strings"
	"testing"
)

func TestBuildSystemPromptInjectsWorkDir(t *testing.T) {
	app := New(WithMaxTurns(1))

	const dir = `E:\proj\todo-app` // run 级注入的会话工作目录（任意值即可）
	prompt := app.buildSystemPrompt(app.config, nil, dir)
	if !strings.Contains(prompt, "Working directory: "+dir) {
		t.Errorf("system prompt 未包含会话工作目录 %s:\n%.500s", dir, prompt)
	}
	if !strings.Contains(prompt, "Platform:") {
		t.Errorf("system prompt 缺少 Platform 行:\n%.500s", prompt)
	}
}

func TestBuildSystemPromptWorkDirFallsBackToCwd(t *testing.T) {
	app := New(WithMaxTurns(1))

	prompt := app.buildSystemPrompt(app.config, nil, "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Skipf("无法获取 cwd: %v", err)
	}
	if !strings.Contains(prompt, "Working directory: "+cwd) {
		t.Errorf("空 workDir 应回退进程 cwd %s:\n%.500s", cwd, prompt)
	}
}

// 沙箱根覆盖会话目录时（run 内 workDir = 沙箱根），prompt 必须告知沙箱根
// 而非进程 cwd——否则模型按 prompt 里的旧路径寻址，被沙箱拒之门外。
func TestBuildSystemPromptSandboxRootOverrides(t *testing.T) {
	app := New(WithMaxTurns(1))

	cwd, _ := os.Getwd()
	prompt := app.buildSystemPrompt(app.config, nil, cwd+string(os.PathSeparator)+"sandbox-root")
	if strings.Contains(prompt, "Working directory: "+cwd+"\n") {
		t.Errorf("沙箱根在场时不应回显进程 cwd:\n%.500s", prompt)
	}
	if !strings.Contains(prompt, "sandbox-root") {
		t.Errorf("应包含沙箱根路径:\n%.500s", prompt)
	}
}
