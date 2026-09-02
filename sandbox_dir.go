package goagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ---------------------------------------------------------------------------
// DirSandbox（Tier 1）：目录沙箱。
//
// 每次 Enter 在 BaseDir 下建一个独立临时目录作为沙箱根，工具的相对路径
// 解析、Bash/GitCommit 的工作目录全部落到该根内；绝对路径做白名单策略
// 检查（最长前缀优先，无匹配拒绝）。
//
// 局限（Tier 1 边界，防意外不防恶意）：
//   - 符号链接逃逸不防护（词法前缀检查可被 symlink 指到根外）
//   - Net/Env 携带但不强制
//   - 工具自身的 Go 代码绕过 Context.Sandbox 直接 os.* 不受约束
// ---------------------------------------------------------------------------

// DirSandbox 目录沙箱。零值可用（BaseDir 空 = os.TempDir()）。
type DirSandbox struct {
	// BaseDir 沙箱根的父目录；空 = os.TempDir()。
	BaseDir string
}

// NewDirSandbox 创建目录沙箱。baseDir 空串表示用系统临时目录。
func NewDirSandbox(baseDir string) *DirSandbox {
	return &DirSandbox{BaseDir: baseDir}
}

// Enter 创建沙箱根目录并返回会话。
func (s *DirSandbox) Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error) {
	base := s.BaseDir
	if base == "" {
		base = os.TempDir()
	}
	prefix := "goagent-sb-"
	if n := sanitizeName(sessionID); n != "" {
		prefix = "goagent-sb-" + n + "-"
	}
	root, err := os.MkdirTemp(base, prefix+"*")
	if err != nil {
		return nil, fmt.Errorf("sandbox: 创建沙箱根失败: %w", err)
	}

	// 策略归一化：空 FS → 根读写；相对规则路径按根解析。
	if len(policy.FS) == 0 {
		policy.FS = []FSRule{{Path: root, Access: FSReadWrite}}
	} else {
		for i := range policy.FS {
			if policy.FS[i].Path == "" {
				policy.FS[i].Path = root
			} else if !filepath.IsAbs(policy.FS[i].Path) {
				policy.FS[i].Path = filepath.Join(root, policy.FS[i].Path)
			}
		}
	}

	return &dirSandboxSession{
		root:   root,
		base:   base,
		policy: policy,
	}, nil
}

type dirSandboxSession struct {
	root   string
	base   string
	policy Policy
}

func (s *dirSandboxSession) Root() string       { return s.root }
func (s *dirSandboxSession) CommandDir() string { return s.root }
func (s *dirSandboxSession) Policy() Policy     { return s.policy }

// ResolvePath 相对路径按根解析 + Clean（词法捕获 `..` 逃逸），再按策略检查。
func (s *dirSandboxSession) ResolvePath(p string, op FSOp) (string, error) {
	if p == "" {
		return s.root, nil
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(s.root, p))
	}
	if err := checkPath(s.policy.FS, abs, op); err != nil {
		return "", err
	}
	return abs, nil
}

// Close 删除沙箱根。防御性校验：根必须仍在 base 之下（防归一化逻辑
// 出错时误删任意目录）。
func (s *dirSandboxSession) Close() error {
	if s.root == "" {
		return nil
	}
	if !pathContains(s.base, s.root) {
		return fmt.Errorf("sandbox: 拒绝删除沙箱根 %s（不在 %s 下）", s.root, s.base)
	}
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("sandbox: 清理沙箱根失败: %w", err)
	}
	s.root = ""
	return nil
}
