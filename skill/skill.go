// Package skill 实现 Skill 系统。
//
// 对齐 Claude Code 的 /skill 工具和 .yume/commands/ 发现机制。
// 支持内置 skill、项目 skill（.yume/commands/）和用户 skill（~/.claude/commands/）。
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Source 是 skill 的来源。
type Source string

const (
	// SourceBuiltin 内置 skill。
	SourceBuiltin Source = "builtin"
	// SourceProject 项目目录的 skill (.yume/commands/)。
	SourceProject Source = "project"
	// SourceUser 用户目录的 skill (~/.claude/commands/)。
	SourceUser Source = "user"
)

// ExecutionMode 是 skill 的执行模式。
type ExecutionMode string

const (
	// ModeInline 内联执行：将 skill 内容作为 prompt 注入当前对话。
	ModeInline ExecutionMode = "inline"
	// ModeFork fork 执行：在独立的 agent 中执行 skill。
	ModeFork ExecutionMode = "fork"
)

// Skill 表示一个可执行的 skill。
type Skill struct {
	// Name 是 skill 名称（对应文件名，不含扩展名）。
	Name string `json:"name"`

	// Description 是 skill 的简短描述（frontmatter 的 description，缺省回落首行非注释文本）。
	Description string `json:"description,omitempty"`

	// WhenToUse 是触发时机说明（frontmatter 的 when-to-use）——供发现层
	// 判断「什么任务该调用本 skill」，不注入执行 prompt。
	WhenToUse string `json:"when_to_use,omitempty"`

	// AllowedTools 是本 skill 预期用到的工具名列表（frontmatter 的
	// allowed-tools，逗号分隔原始串）——供宿主做工具预授权/裁剪提示。
	AllowedTools string `json:"allowed_tools,omitempty"`

	// Content 是 skill 的完整内容（Markdown prompt，不含 frontmatter）。
	Content string `json:"content"`

	// Source 是 skill 的来源。
	Source Source `json:"source"`

	// Mode 是 skill 的执行模式。
	Mode ExecutionMode `json:"mode"`

	// FilePath 是 skill 文件的完整路径。
	FilePath string `json:"file_path,omitempty"`
}

// Registry 管理 skill 的发现和注册。
type Registry struct {
	mu sync.RWMutex

	// skills 按名称索引。
	skills map[string]*Skill

	// projectDir 是项目目录。
	projectDir string

	// userDir 是用户目录（~/.claude/commands/）。
	userDir string
}

// NewRegistry 创建一个新的 skill 注册表。
func NewRegistry(projectDir, userDir string) *Registry {
	return &Registry{
		skills:     make(map[string]*Skill),
		projectDir: projectDir,
		userDir:    userDir,
	}
}

// Register 注册一个内置 skill。
func (r *Registry) Register(skill *Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[skill.Name] = skill
}

// Discover 扫描项目和用户目录，发现所有 skill 文件。
func (r *Registry) Discover() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 扫描项目 commands 目录。
	if r.projectDir != "" {
		commandsDir := filepath.Join(r.projectDir, ".yume", "commands")
		if err := r.scanDir(commandsDir, SourceProject); err != nil {
			// 目录不存在不算错误。
			if !os.IsNotExist(err) {
				return fmt.Errorf("扫描项目 commands 目录失败: %w", err)
			}
		}
	}

	// 扫描用户 commands 目录。
	if r.userDir != "" {
		if err := r.scanDir(r.userDir, SourceUser); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("扫描用户 commands 目录失败: %w", err)
			}
		}
	}

	return nil
}

// scanDir 扫描指定目录中的 .md 文件。
func (r *Registry) scanDir(dir string, source Source) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".md")
		content := string(data)

		skill := loadSkill(name, filePath, content, source)
		// frontmatter 显式给出 name 时以它为准（注册键同步）。
		key := skill.Name

		// 项目 skill 不覆盖内置 skill。
		if existing, ok := r.skills[key]; ok && existing.Source == SourceBuiltin {
			continue
		}
		r.skills[key] = skill
	}

	return nil
}

// Get 获取指定名称的 skill。
func (r *Registry) Get(name string) *Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skills[name]
}

// List 返回所有注册的 skill。
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		result = append(result, s)
	}
	return result
}

// Execute 执行一个 skill，返回要注入的 prompt。
func (r *Registry) Execute(name string, args string) (string, error) {
	r.mu.RLock()
	skill, ok := r.skills[name]
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("skill %q 不存在", name)
	}

	// 替换 $ARGUMENTS 占位符。
	content := strings.ReplaceAll(skill.Content, "$ARGUMENTS", args)

	return content, nil
}

// extractDescription 从 skill 内容中提取描述（无 frontmatter 时的兜底：
// 首个非空且非标题的行，截断到 100 字符）。
func extractDescription(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 截断过长的描述。
		if len(line) > 100 {
			return line[:100] + "..."
		}
		return line
	}
	return ""
}

// skillMeta 是 skill 文件 YAML frontmatter 的关键字段。
type skillMeta struct {
	Name         string
	Description  string
	WhenToUse    string
	AllowedTools string
}

// parseFrontmatter 解析 skill 文件头部的 YAML frontmatter 块。
// 首行为 `---` 且存在闭合 `---` 行时，块内按行做宽松 `key: value` 解析
//（只取所需字段，不引入完整 YAML 依赖）；返回 meta 与剥掉 frontmatter
// 的正文——正文才是注入执行用的 prompt，YAML 头不应随之进入上下文。
// 无 frontmatter 时返回零值 meta 与原文。
func parseFrontmatter(content string) (skillMeta, string) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return skillMeta{}, content
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return skillMeta{}, content
	}
	meta := skillMeta{}
	for _, line := range lines[1:end] {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
		// 键名归一：小写、下划线/空格折线（兼容 when_to_use/whenToUse）。
		k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", "-"), " ", "-"))
		switch k {
		case "name":
			meta.Name = v
		case "description":
			meta.Description = v
		case "when-to-use", "whentouse":
			meta.WhenToUse = v
		case "allowed-tools", "allowedtools":
			meta.AllowedTools = v
		}
	}
	return meta, strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
}

// loadSkill 从文件内容构建 Skill：frontmatter 提供元数据与剥离后的
// 正文，description 缺省回落正文首行。
func loadSkill(name, filePath, content string, source Source) *Skill {
	meta, body := parseFrontmatter(content)
	desc := meta.Description
	if desc == "" {
		desc = extractDescription(body)
	}
	sname := meta.Name
	if sname == "" {
		sname = name
	}
	return &Skill{
		Name:         sname,
		Description:  desc,
		WhenToUse:    meta.WhenToUse,
		AllowedTools: meta.AllowedTools,
		Content:      body,
		Source:       source,
		Mode:         ModeInline,
		FilePath:     filePath,
	}
}

// Names 返回所有 skill 名称。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.skills))
	for name := range r.skills {
		names = append(names, name)
	}
	return names
}
