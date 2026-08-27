package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
)

// GitCommit：真实仓库提交、无变更短路、非仓库报错。
func TestGitCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")

	tool := GitCommitTool()
	exec1 := tool.Execute.(func(goagent.Context, gitCommitInput) (string, error))
	ctx := goagent.Context{WorkDir: dir}

	// 无变更：短路
	out, err := exec1(ctx, gitCommitInput{Message: "空"})
	if err != nil || !strings.Contains(out, "无变更") {
		t.Fatalf("无变更应短路: %v %q", err, out)
	}

	// 有变更：提交成功
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = exec1(ctx, gitCommitInput{Message: "feat: 初始文件"})
	if err != nil || !strings.Contains(out, "已提交") {
		t.Fatalf("提交失败: %v %q", err, out)
	}

	// 再提交（无新变更）→ 短路
	out, err = exec1(ctx, gitCommitInput{Message: "again"})
	if err != nil || !strings.Contains(out, "无变更") {
		t.Fatalf("二次提交应短路: %v %q", err, out)
	}

	// 非仓库目录：明确报错
	outside := t.TempDir()
	if _, err := exec1(goagent.Context{WorkDir: outside}, gitCommitInput{Message: "x"}); err == nil {
		t.Fatalf("非仓库应报错，得到 %q", out)
	}

	// 空 message：报错
	if _, err := exec1(ctx, gitCommitInput{}); err == nil {
		t.Fatalf("空 message 应报错")
	}
}
