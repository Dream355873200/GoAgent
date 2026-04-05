// Package worktree 实现 git worktree 隔离机制。
//
// 对齐 Claude Code 的 EnterWorktree/ExitWorktree 工具。
// 在 .yume/worktrees/ 目录下创建隔离的工作树，
// 每个 worktree 有独立的分支和工作目录。
package worktree

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Worktree 表示一个 git worktree。
type Worktree struct {
	// Name 是 worktree 的名称。
	Name string `json:"name"`

	// Path 是 worktree 的文件系统路径。
	Path string `json:"path"`

	// Branch 是 worktree 使用的分支名。
	Branch string `json:"branch"`

	// OriginalDir 是进入 worktree 前的原始工作目录。
	OriginalDir string `json:"original_dir"`
}

// Manager 管理 worktree 的生命周期。
type Manager struct {
	mu sync.RWMutex

	// current 是当前活跃的 worktree。nil 表示不在 worktree 中。
	current *Worktree

	// repoRoot 是 git 仓库的根目录。
	repoRoot string
}

// NewManager 创建一个新的 worktree 管理器。
// repoRoot 是 git 仓库根目录。
func NewManager(repoRoot string) *Manager {
	return &Manager{repoRoot: repoRoot}
}

// Enter 创建并进入一个新的 worktree。
func (m *Manager) Enter(name string) (*Worktree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current != nil {
		return nil, fmt.Errorf("已在 worktree %q 中，请先退出", m.current.Name)
	}

	// 检查是否在 git 仓库中。
	if !isGitRepo(m.repoRoot) {
		return nil, fmt.Errorf("当前目录不是 git 仓库")
	}

	if name == "" {
		name = generateName()
	}

	// 确保 worktrees 目录存在。
	worktreesDir := filepath.Join(m.repoRoot, ".yume", "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 worktrees 目录失败: %w", err)
	}

	wtPath := filepath.Join(worktreesDir, name)
	branch := "claude-worktree-" + name

	// 创建 worktree。
	cmd := exec.Command("git", "worktree", "add", wtPath, "-b", branch)
	cmd.Dir = m.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("创建 worktree 失败: %s: %w", string(output), err)
	}

	// 获取当前工作目录。
	cwd, err := os.Getwd()
	if err != nil {
		cwd = m.repoRoot
	}

	wt := &Worktree{
		Name:        name,
		Path:        wtPath,
		Branch:      branch,
		OriginalDir: cwd,
	}
	m.current = wt

	return wt, nil
}

// Exit 退出当前 worktree。
// action: "keep" 保留 worktree，"remove" 删除 worktree 及分支。
func (m *Manager) Exit(action string, discardChanges bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return nil // 不在 worktree 中，no-op
	}

	wt := m.current

	if action == "remove" {
		// 检查是否有未提交的更改。
		if !discardChanges && hasUncommittedChanges(wt.Path) {
			return fmt.Errorf("worktree 有未提交的更改，使用 discardChanges=true 强制删除")
		}

		// 删除 worktree。
		cmd := exec.Command("git", "worktree", "remove", wt.Path, "--force")
		cmd.Dir = m.repoRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("删除 worktree 失败: %s: %w", string(output), err)
		}

		// 删除分支。
		cmd = exec.Command("git", "branch", "-D", wt.Branch)
		cmd.Dir = m.repoRoot
		_ = cmd.Run() // 分支删除失败不算致命错误
	}

	m.current = nil
	return nil
}

// IsInWorktree 返回是否在 worktree 中。
func (m *Manager) IsInWorktree() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current != nil
}

// Current 返回当前的 worktree。
func (m *Manager) Current() *Worktree {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// OriginalDir 返回进入 worktree 前的原始目录。
func (m *Manager) OriginalDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current != nil {
		return m.current.OriginalDir
	}
	return ""
}

// isGitRepo 检查指定目录是否是 git 仓库。
func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// hasUncommittedChanges 检查 worktree 是否有未提交的更改。
func hasUncommittedChanges(dir string) bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return true // 出错时保守处理
	}
	return len(strings.TrimSpace(string(output))) > 0
}

// generateName 生成一个随机的 worktree 名称。
func generateName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "wt-" + hex.EncodeToString(b)
}

// ListWorktrees 列出所有 git worktrees。
func ListWorktrees(repoRoot string) ([]string, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("列出 worktrees 失败: %w", err)
	}

	var paths []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths, nil
}
