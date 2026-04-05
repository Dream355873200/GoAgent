// Package sysprompt 实现动态 System Prompt 组装。
//
// 对齐 Claude Code 的 systemPrompt.ts 多段 system prompt 构建。
// 支持 gitStatus/currentDate/env_info 注入、cache_control 段、
// 工具说明注入、CLAUDE.md 注入、MCP instructions 注入。
package sysprompt

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Section 是 system prompt 的一个段落。
type Section struct {
	// Name 是段落的标识名称。
	Name string `json:"name"`

	// Content 是段落的文本内容。
	Content string `json:"content"`

	// CacheControl 是此段的缓存控制。nil 表示不缓存。
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl 是 prompt caching 的控制参数。
type CacheControl struct {
	// Type 是缓存类型。目前只支持 "ephemeral"。
	Type string `json:"type"`
}

// EphemeralCache 返回一个 ephemeral 缓存控制。
func EphemeralCache() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

// Builder 构建动态 system prompt。
type Builder struct {
	// sections 是已添加的段落列表（按顺序）。
	sections []Section

	// 动态数据源。
	gitRoot    string
	projectDir string
}

// NewBuilder 创建一个新的 system prompt 构建器。
func NewBuilder() *Builder {
	return &Builder{}
}

// SetGitRoot 设置 git 仓库根目录（用于 gitStatus 注入）。
func (b *Builder) SetGitRoot(root string) *Builder {
	b.gitRoot = root
	return b
}

// SetProjectDir 设置项目目录。
func (b *Builder) SetProjectDir(dir string) *Builder {
	b.projectDir = dir
	return b
}

// AddSection 添加一个自定义段落。
func (b *Builder) AddSection(name, content string, cacheControl *CacheControl) *Builder {
	b.sections = append(b.sections, Section{
		Name:         name,
		Content:      content,
		CacheControl: cacheControl,
	})
	return b
}

// AddBasePrompt 添加基础系统提示。
func (b *Builder) AddBasePrompt(prompt string) *Builder {
	return b.AddSection("base", prompt, EphemeralCache())
}

// AddEnvironmentInfo 添加环境信息段。
func (b *Builder) AddEnvironmentInfo() *Builder {
	info := buildEnvironmentInfo(b.projectDir)
	return b.AddSection("environment", info, nil)
}

// AddCurrentDate 添加当前日期段。
func (b *Builder) AddCurrentDate() *Builder {
	now := time.Now()
	content := fmt.Sprintf("# currentDate\nToday's date is %s.", now.Format("2006-01-02"))
	return b.AddSection("currentDate", content, nil)
}

// AddGitStatus 添加 git status 段。
func (b *Builder) AddGitStatus() *Builder {
	if b.gitRoot == "" {
		return b
	}

	status := getGitStatus(b.gitRoot)
	if status == "" {
		return b
	}

	content := fmt.Sprintf("gitStatus: %s", status)
	return b.AddSection("gitStatus", content, nil)
}

// AddToolInstructions 添加工具说明段。
func (b *Builder) AddToolInstructions(instructions string) *Builder {
	if instructions == "" {
		return b
	}
	return b.AddSection("toolInstructions", instructions, nil)
}

// AddMemory 添加 memory 段（CLAUDE.md 内容等）。
func (b *Builder) AddMemory(content string) *Builder {
	if content == "" {
		return b
	}
	return b.AddSection("memory", content, EphemeralCache())
}

// AddMCPInstructions 添加 MCP server 说明段。
func (b *Builder) AddMCPInstructions(instructions string) *Builder {
	if instructions == "" {
		return b
	}
	return b.AddSection("mcpInstructions", instructions, nil)
}

// Build 构建最终的 system prompt。
// 返回完整的文本和段落列表（用于 multi-block system prompt）。
func (b *Builder) Build() (string, []Section) {
	// 为最后一个非空段添加 cache_control（如果还没有的话）。
	sections := make([]Section, len(b.sections))
	copy(sections, b.sections)

	if len(sections) > 0 {
		last := &sections[len(sections)-1]
		if last.CacheControl == nil {
			last.CacheControl = EphemeralCache()
		}
	}

	// 拼接完整文本。
	var parts []string
	for _, s := range sections {
		if s.Content != "" {
			parts = append(parts, s.Content)
		}
	}

	return strings.Join(parts, "\n\n"), sections
}

// Sections 返回当前的段落列表（不做 cache_control 自动设置）。
func (b *Builder) Sections() []Section {
	return b.sections
}

// buildEnvironmentInfo 构建环境信息字符串。
func buildEnvironmentInfo(projectDir string) string {
	var parts []string

	// 平台。
	parts = append(parts, fmt.Sprintf("- Platform: %s", runtime.GOOS))

	// OS 版本（简化）。
	parts = append(parts, fmt.Sprintf("- Architecture: %s", runtime.GOARCH))

	// Shell。
	shell := detectShell()
	if shell != "" {
		parts = append(parts, fmt.Sprintf("- Shell: %s", shell))
	}

	// 工作目录。
	if projectDir != "" {
		parts = append(parts, fmt.Sprintf("- Working directory: %s", projectDir))
	}

	return "# Environment\n" + strings.Join(parts, "\n")
}

// detectShell 检测当前 shell。
func detectShell() string {
	if runtime.GOOS == "windows" {
		// Windows 上检查 SHELL 环境变量（Git Bash）或默认 cmd。
		cmd := exec.Command("bash", "--version")
		if output, err := cmd.Output(); err == nil {
			firstLine := strings.Split(string(output), "\n")[0]
			if strings.Contains(firstLine, "bash") {
				return "bash"
			}
		}
		return "cmd"
	}

	// Unix 系统。
	cmd := exec.Command("echo", "$SHELL")
	if output, err := cmd.Output(); err == nil {
		shell := strings.TrimSpace(string(output))
		if shell != "" && shell != "$SHELL" {
			return shell
		}
	}
	return "bash"
}

// getGitStatus 获取 git status 信息。
func getGitStatus(repoRoot string) string {
	// 获取当前分支。
	branchCmd := exec.Command("git", "branch", "--show-current")
	branchCmd.Dir = repoRoot
	branchOutput, err := branchCmd.Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(branchOutput))

	// 获取 status。
	statusCmd := exec.Command("git", "status", "--porcelain", "-u")
	statusCmd.Dir = repoRoot
	statusOutput, err := statusCmd.Output()
	if err != nil {
		return ""
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Current branch: %s\n", branch))

	status := strings.TrimSpace(string(statusOutput))
	if status != "" {
		// 限制输出行数。
		lines := strings.Split(status, "\n")
		if len(lines) > 50 {
			result.WriteString(fmt.Sprintf("Status: (%d files changed, showing first 50)\n", len(lines)))
			lines = lines[:50]
		} else {
			result.WriteString("Status:\n")
		}
		for _, line := range lines {
			result.WriteString(line + "\n")
		}
	} else {
		result.WriteString("Status: clean\n")
	}

	return result.String()
}

// GetGitBranch 获取当前 git 分支名。
func GetGitBranch(repoRoot string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// IsGitRepo 检查目录是否是 git 仓库。
func IsGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// ── 导出的辅助函数（供 prompts 模板变量使用） ──

// DetectPlatform 返回当前操作系统名称。
func DetectPlatform() string {
	return runtime.GOOS
}

// CurrentDate 返回当前日期字符串。
func CurrentDate() string {
	return time.Now().Format("2006-01-02")
}

// DetectGitRoot 检测当前目录的 git 仓库根目录。
// 如果不在 git 仓库中则返回空字符串。
func DetectGitRoot() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// GetGitStatusText 获取格式化的 git status 文本。
func GetGitStatusText(repoRoot string) string {
	return getGitStatus(repoRoot)
}
