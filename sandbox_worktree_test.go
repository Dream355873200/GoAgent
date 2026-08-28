package goagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// WorktreeSandbox：Enter 创建 worktree 作为沙箱根（无全局 chdir）、
// 默认策略根读写、Close force 删除、KeepOnClose 保留。
// 无 git 时 skip（Windows CI 无 git 环境防护）。
func TestWorktreeSandbox(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	repo := t.TempDir()
	if err := initGitRepo(t, repo); err != nil {
		t.Skipf("git 仓库初始化失败: %v", err)
	}

	cwd, _ := os.Getwd()

	sb := NewWorktreeSandbox(repo)
	sess, err := sb.Enter(context.Background(), "bench", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	root := sess.Root()
	if !pathContains(filepath.Join(repo, ".yume", "worktrees"), root) {
		t.Fatalf("沙箱根应位于 .yume/worktrees 下: %q", root)
	}

	// 根内写放行，文件真实落盘
	p, err := sess.ResolvePath("out.txt", OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 原仓库根默认不在白名单内（只开放 worktree 根）
	if _, err := sess.ResolvePath(filepath.Join(repo, "README.md"), OpRead); err == nil {
		t.Fatal("原仓库根默认应不可读（需显式加 FSRule）")
	}

	// 全局 cwd 未被改动（无 chdir 是多会话安全的前提）
	if now, _ := os.Getwd(); now != cwd {
		t.Fatalf("Enter 不应改变进程 cwd: %q -> %q", cwd, now)
	}

	// Close（force）删除 worktree
	if err := sess.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("Close 后 worktree 应被删除")
	}
}

// KeepOnClose：Close 保留 worktree 供宿主收割产物。
func TestWorktreeSandboxKeepOnClose(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	repo := t.TempDir()
	if err := initGitRepo(t, repo); err != nil {
		t.Skipf("git 仓库初始化失败: %v", err)
	}

	sb := &WorktreeSandbox{RepoRoot: repo, KeepOnClose: true}
	sess, err := sb.Enter(context.Background(), "", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	root := sess.Root()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("KeepOnClose 下 worktree 应保留")
	}
	// 清理（测试卫生）
	_ = exec.Command("git", "worktree", "remove", root, "--force").Run()
}

// initGitRepo 建一个带初始提交的临时 git 仓库。
func initGitRepo(t *testing.T, dir string) error {
	t.Helper()
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("git %v: %s", args, strings.TrimSpace(string(out)))
		}
		return err
	}
	if err := run("init"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0o644); err != nil {
		return err
	}
	if err := run("add", "-A"); err != nil {
		return err
	}
	return run("commit", "-m", "init")
}
