package goagent

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 根包不链接终端界面库：只有导入 cli 包的程序才会带上 Bubble Tea 等依赖。
func TestFix_RootPackageDoesNotLinkTUI(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go 工具不可用")
	}
	out, err := exec.Command(goBin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list 失败: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "charmbracelet") || strings.HasSuffix(dep, "/GoAgent/tui") || strings.HasSuffix(dep, "/GoAgent/cli") {
			t.Fatalf("根包不应依赖终端界面库: %s", dep)
		}
	}
}

// 没有导入 cli 包时 RunCLI 提示后立即返回，而不是阻塞或 panic。
func TestFix_RunCLIWithoutCLIPackageReturns(t *testing.T) {
	if cliRunner != nil {
		t.Skip("cli 包已注册终端界面")
	}
	done := make(chan struct{})
	go func() {
		New().RunCLI()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("未导入 cli 包时 RunCLI 应立即返回")
	}
}
