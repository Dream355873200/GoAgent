package goagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Dream355873200/GoAgent/worktree"
)

// ---------------------------------------------------------------------------
// WorktreeSandbox（Tier 1）：git worktree 沙箱。
//
// 每次 Enter 创建一个 git worktree 作为沙箱根（走 Manager.Create——
// 不做进程全局 chdir、不占 single-current 状态，多会话可并发持有）。
// 工作副本天然可回滚、可 diff：跑完可以 diff 沙箱内的改动收割产物，
// 这是 benchmark 自迭代（防 agent 改坏仓库）和 L4 代码执行的首选形态。
//
// 默认策略只开放 worktree 根读写；agent 要读原仓库需显式加 FSRule
// （如 {Path: RepoRoot, Access: FSReadOnly}）。
// ---------------------------------------------------------------------------

// WorktreeSandbox git worktree 沙箱。零值不可用，须经 NewWorktreeSandbox。
type WorktreeSandbox struct {
	// RepoRoot git 仓库根目录。
	RepoRoot string

	// KeepOnClose true = Close 保留 worktree（宿主自行 diff/收割后清理）；
	// false（默认）= force 删除 worktree + 分支（沙箱即抛即弃）。
	KeepOnClose bool

	mu      sync.Mutex
	manager *worktree.Manager
}

// NewWorktreeSandbox 创建 worktree 沙箱。
func NewWorktreeSandbox(repoRoot string) *WorktreeSandbox {
	return &WorktreeSandbox{RepoRoot: repoRoot}
}

// Enter 创建一个 worktree 作为沙箱根。
func (s *WorktreeSandbox) Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error) {
	s.mu.Lock()
	if s.manager == nil {
		s.manager = worktree.NewManager(s.RepoRoot)
	}
	mgr := s.manager
	s.mu.Unlock()

	name := sanitizeName(sessionID)
	if name == "" {
		name = "run"
	}
	wt, err := mgr.Create("sb-" + name + "-" + randomSuffix())
	if err != nil {
		return nil, fmt.Errorf("sandbox: 创建 worktree 失败: %w", err)
	}

	// 策略归一化：空 FS → worktree 根读写；空 Path 规则 = 根；
	// 相对规则路径按根解析。
	if len(policy.FS) == 0 {
		policy.FS = []FSRule{{Path: wt.Path, Access: FSReadWrite}}
	} else {
		for i := range policy.FS {
			if policy.FS[i].Path == "" {
				policy.FS[i].Path = wt.Path
			}
		}
	}

	return &worktreeSandboxSession{
		mgr:         mgr,
		wt:          wt,
		policy:      policy,
		keepOnClose: s.KeepOnClose,
	}, nil
}

type worktreeSandboxSession struct {
	mgr         *worktree.Manager
	wt          *worktree.Worktree
	policy      Policy
	keepOnClose bool
}

func (s *worktreeSandboxSession) Root() string       { return s.wt.Path }
func (s *worktreeSandboxSession) CommandDir() string { return s.wt.Path }
func (s *worktreeSandboxSession) Policy() Policy     { return s.policy }

func (s *worktreeSandboxSession) ResolvePath(p string, op FSOp) (string, error) {
	if p == "" {
		return s.wt.Path, nil
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(s.wt.Path, p))
	}
	if err := checkPath(s.policy.FS, abs, op); err != nil {
		return "", err
	}
	return abs, nil
}

func (s *worktreeSandboxSession) Close() error {
	if s.keepOnClose {
		return nil
	}
	return s.mgr.Remove(s.wt, true)
}

// randomSuffix 生成短随机后缀区分并发会话。
func randomSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}
