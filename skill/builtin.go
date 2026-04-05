// Package skill 实现 Skill 系统。
//
// 对齐 Claude Code 的 /skill 工具和 .yume/commands/ 发现机制。
// 支持内置 skill、项目 skill（.yume/commands/）和用户 skill（~/.claude/commands/）。
package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// SkillProvider 技能提供者接口。
// 用于动态发现和加载技能。
type SkillProvider interface {
	// DiscoverSkills 发现并返回所有可用的技能。
	DiscoverSkills(ctx context.Context) ([]*Skill, error)

	// Name 返回提供者名称。
	Name() string
}

// BuiltinSkillProvider 内置技能提供者。
// 提供内置的默认技能。
type BuiltinSkillProvider struct {
	registry *Registry
}

// NewBuiltinSkillProvider 创建内置技能提供者。
func NewBuiltinSkillProvider() *BuiltinSkillProvider {
	return &BuiltinSkillProvider{}
}

// Name 返回提供者名称。
func (p *BuiltinSkillProvider) Name() string {
	return "builtin"
}

// DiscoverSkills 返回内置技能列表。
func (p *BuiltinSkillProvider) DiscoverSkills(ctx context.Context) ([]*Skill, error) {
	skills := getBuiltinSkills()
	// 注册到内置 registry
	if p.registry != nil {
		for _, s := range skills {
			p.registry.Register(s)
		}
	}
	return skills, nil
}

// SetRegistry 设置关联的 Registry。
func (p *BuiltinSkillProvider) SetRegistry(r *Registry) {
	p.registry = r
}

// FileBasedSkillProvider 基于文件系统的技能提供者。
type FileBasedSkillProvider struct {
	baseDir string
	source  Source
}

// NewFileBasedSkillProvider 创建基于文件的技能提供者。
func NewFileBasedSkillProvider(dir string, source Source) *FileBasedSkillProvider {
	return &FileBasedSkillProvider{
		baseDir: dir,
		source:  source,
	}
}

// Name 返回提供者名称。
func (p *FileBasedSkillProvider) Name() string {
	return "file:" + p.baseDir
}

// DiscoverSkills 扫描目录发现技能。
func (p *FileBasedSkillProvider) DiscoverSkills(ctx context.Context) ([]*Skill, error) {
	var skills []*Skill

	entries, err := os.ReadDir(p.baseDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		filePath := filepath.Join(p.baseDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".md")
		content := string(data)

		skills = append(skills, &Skill{
			Name:        name,
			Description: extractDescription(content),
			Content:     content,
			Source:      p.source,
			Mode:        ModeInline,
			FilePath:    filePath,
		})
	}

	return skills, nil
}

// MCPSkillProvider MCP 技能提供者。
// 从 MCP 服务器发现技能。
type MCPSkillProvider struct {
	name  string
	tools []*Skill
}

// NewMCPSkillProvider 创建 MCP 技能提供者。
func NewMCPSkillProvider(name string, tools []*Skill) *MCPSkillProvider {
	return &MCPSkillProvider{
		name:  name,
		tools: tools,
	}
}

// Name 返回提供者名称。
func (p *MCPSkillProvider) Name() string {
	return "mcp:" + p.name
}

// DiscoverSkills 返回 MCP 技能列表。
func (p *MCPSkillProvider) DiscoverSkills(ctx context.Context) ([]*Skill, error) {
	return p.tools, nil
}

// getBuiltinSkills 返回内置技能列表。
func getBuiltinSkills() []*Skill {
	return []*Skill{
		{
			Name:        "translate",
			Description: "将文本翻译为指定语言",
			Content: `Translate the following text to {{language}}.

Rules:
- Preserve formatting and code blocks
- Maintain technical accuracy
- Keep the original tone

Text to translate:
$ARGUMENTS`,
			Source: SourceBuiltin,
			Mode:   ModeInline,
		},
		{
			Name:        "explain",
			Description: "解释代码或概念",
			Content: `Explain the following code or concept in detail.

$ARGUMENTS

Provide:
1. What it does
2. How it works
3. Common use cases`,
			Source: SourceBuiltin,
			Mode:   ModeInline,
		},
		{
			Name:        "review",
			Description: "代码审查",
			Content: `Review the following code and provide feedback.

$ARGUMENTS

Focus on:
- Code quality and readability
- Potential bugs or issues
- Performance concerns
- Security considerations`,
			Source: SourceBuiltin,
			Mode:   ModeInline,
		},
		{
			Name:        "refactor",
			Description: "代码重构建议",
			Content: `Suggest refactoring for the following code.

$ARGUMENTS

Provide:
1. Current issues
2. Refactored version
3. Benefits of changes`,
			Source: SourceBuiltin,
			Mode:   ModeInline,
		},
		{
			Name:        "test",
			Description: "生成测试用例",
			Content: `Generate test cases for the following code.

$ARGUMENTS

Cover:
- Happy path cases
- Edge cases
- Error cases
- Use table-driven tests where appropriate`,
			Source: SourceBuiltin,
			Mode:   ModeInline,
		},
	}
}

// RegisterBuiltinSkills 注册所有内置技能到 Registry。
func RegisterBuiltinSkills(r *Registry) {
	for _, s := range getBuiltinSkills() {
		r.Register(s)
	}
}
