// Package memory 管理项目上下文加载和跨会话持久化内存。
package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// Config 控制内存行为。
type Config struct {
	// MemoryDir 是跨会话持久化内存的目录。
	// 如果为空，跨会话内存将被禁用。
	MemoryDir string

	// ProjectContext 是要加载为项目上下文的文件列表。
	ProjectContext []string
}

// Manager 处理内存加载和提取。
type Manager struct {
	config     Config
	claudemd   *ClaudeMDConfig
	autoMemory *AutoMemory // auto memory 子系统
}

// NewManager 创建一个新的内存管理器。
func NewManager(cfg Config) *Manager {
	m := &Manager{config: cfg}
	// 如果配置了记忆目录，初始化 AutoMemory 子系统。
	if cfg.MemoryDir != "" {
		m.autoMemory = NewAutoMemory(cfg.MemoryDir)
	}
	return m
}

// SetClaudeMD 设置 CLAUDE.md 三层配置。
func (m *Manager) SetClaudeMD(cfg *ClaudeMDConfig) {
	m.claudemd = cfg
}

// GetAutoMemory 返回 AutoMemory 子系统实例。
// 如果未配置 MemoryDir 则返回 nil。
// 外部可通过此方法调用 Save/Search/Delete 等操作。
func (m *Manager) GetAutoMemory() *AutoMemory {
	return m.autoMemory
}

// LoadProjectContext 读取所有项目上下文文件并返回拼接后的内容，
// 用于注入到系统提示中。
func (m *Manager) LoadProjectContext() string {
	var parts []string

	for _, path := range m.config.ProjectContext {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parts = append(parts, strings.TrimSpace(string(content)))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// LoadPersistentMemory 从内存目录读取 MEMORY.md 文件。
// 如果内存被禁用或文件不存在，返回空字符串。
func (m *Manager) LoadPersistentMemory() string {
	if m.config.MemoryDir == "" {
		return ""
	}

	memFile := filepath.Join(m.config.MemoryDir, "MEMORY.md")
	content, err := os.ReadFile(memFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

// SavePersistentMemory 将内容写入 MEMORY.md 文件。
func (m *Manager) SavePersistentMemory(content string) error {
	if m.config.MemoryDir == "" {
		return nil
	}

	if err := os.MkdirAll(m.config.MemoryDir, 0755); err != nil {
		return err
	}

	memFile := filepath.Join(m.config.MemoryDir, "MEMORY.md")
	return os.WriteFile(memFile, []byte(content), 0644)
}

// BuildSystemPromptSuffix 构建系统提示的内存/上下文部分。
func (m *Manager) BuildSystemPromptSuffix() string {
	var parts []string

	// CLAUDE.md 三层上下文（最高优先级）。
	if m.claudemd != nil {
		if claudeContent := m.claudemd.FormatForSystemPrompt(); claudeContent != "" {
			parts = append(parts, claudeContent)
		}
	}

	// 项目上下文。
	if ctx := m.LoadProjectContext(); ctx != "" {
		parts = append(parts, "# 项目上下文\n\n"+ctx)
	}

	// 持久化内存（优先使用 AutoMemory 的截断逻辑）。
	if m.autoMemory != nil {
		if mem := m.autoMemory.FormatForInjection(); mem != "" {
			parts = append(parts, mem)
		}
	} else if mem := m.LoadPersistentMemory(); mem != "" {
		parts = append(parts, "# 内存（来自之前的会话）\n\n"+mem)
	}

	return strings.Join(parts, "\n\n---\n\n")
}
