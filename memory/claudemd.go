// Package memory — CLAUDE.md 三层配置系统。
//
// CLAUDE.md 是 Claude Code 的项目级配置文件，支持三层加载：
//  1. 用户级 (~/.claude/CLAUDE.md) — 全局偏好
//  2. 项目级 (<project>/CLAUDE.md) — 项目规范
//  3. 本地级 (<project>/.claude/CLAUDE.md) — 本地覆盖
//
// 三层内容按顺序拼接注入系统提示。
//
// 对齐 Claude Code 的 CLAUDE.md 三层加载逻辑。
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeMDConfig 配置 CLAUDE.md 三层加载。
type ClaudeMDConfig struct {
	// UserPath 是用户级 CLAUDE.md 路径（通常是 ~/.claude/CLAUDE.md）。
	UserPath string

	// ProjectPath 是项目级 CLAUDE.md 路径（通常是 <project>/CLAUDE.md）。
	ProjectPath string

	// LocalPath 是本地级 CLAUDE.md 路径（通常是 <project>/.claude/CLAUDE.md）。
	LocalPath string
}

// ClaudeMDLayer 表示一层加载的 CLAUDE.md 内容。
type ClaudeMDLayer struct {
	// Source 是内容来源的路径。
	Source string
	// Content 是文件内容。
	Content string
	// Level 描述层级（"user"、"project"、"local"）。
	Level string
}

// DefaultClaudeMDConfig 从工作目录和用户主目录推导默认的三层路径。
func DefaultClaudeMDConfig(workDir, homeDir string) *ClaudeMDConfig {
	return &ClaudeMDConfig{
		UserPath:    filepath.Join(homeDir, ".claude", "CLAUDE.md"),
		ProjectPath: filepath.Join(workDir, "CLAUDE.md"),
		LocalPath:   filepath.Join(workDir, ".claude", "CLAUDE.md"),
	}
}

// LoadClaudeMD 加载三层 CLAUDE.md 配置。
// 返回所有成功加载的层。
func (cfg *ClaudeMDConfig) LoadClaudeMD() []ClaudeMDLayer {
	var layers []ClaudeMDLayer

	// 第 1 层：用户级。
	if cfg.UserPath != "" {
		if content, err := readFileIfExists(cfg.UserPath); err == nil && content != "" {
			layers = append(layers, ClaudeMDLayer{
				Source:  cfg.UserPath,
				Content: content,
				Level:   "user",
			})
		}
	}

	// 第 2 层：项目级。
	if cfg.ProjectPath != "" {
		if content, err := readFileIfExists(cfg.ProjectPath); err == nil && content != "" {
			layers = append(layers, ClaudeMDLayer{
				Source:  cfg.ProjectPath,
				Content: content,
				Level:   "project",
			})
		}
	}

	// 第 3 层：本地级。
	if cfg.LocalPath != "" {
		if content, err := readFileIfExists(cfg.LocalPath); err == nil && content != "" {
			layers = append(layers, ClaudeMDLayer{
				Source:  cfg.LocalPath,
				Content: content,
				Level:   "local",
			})
		}
	}

	return layers
}

// FormatForSystemPrompt 将三层 CLAUDE.md 内容格式化为系统提示段落。
func (cfg *ClaudeMDConfig) FormatForSystemPrompt() string {
	layers := cfg.LoadClaudeMD()
	if len(layers) == 0 {
		return ""
	}

	var parts []string
	parts = append(parts, "# CLAUDE.md 指令")

	for _, layer := range layers {
		header := fmt.Sprintf("## %s 级配置 (%s)", layerDisplayName(layer.Level), layer.Source)
		parts = append(parts, header+"\n\n"+layer.Content)
	}

	return strings.Join(parts, "\n\n")
}

// layerDisplayName 返回层级的显示名称。
func layerDisplayName(level string) string {
	switch level {
	case "user":
		return "用户"
	case "project":
		return "项目"
	case "local":
		return "本地"
	default:
		return level
	}
}

// readFileIfExists 读取文件内容，如果文件不存在返回空字符串和 nil 错误。
func readFileIfExists(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}
