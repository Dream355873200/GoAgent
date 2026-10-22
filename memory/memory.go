// Package memory 管理项目上下文加载和跨会话持久化内存。
package memory

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxInjectBytes 单个注入系统提示词的文件的字节上限，超出部分截断。
const maxInjectBytes = 64 << 10

// readForPrompt 读取要注入系统提示词的文件并去掉首尾空白；超过
// maxInjectBytes 时只保留开头部分，并在末尾追加一行截断说明。
func readForPrompt(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// 多读 1 字节用于判断是否超限，不把超大文件整个读进内存。
	data, err := io.ReadAll(io.LimitReader(f, maxInjectBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) <= maxInjectBytes {
		return strings.TrimSpace(string(data)), nil
	}

	size := int64(len(data))
	if fi, err := f.Stat(); err == nil && fi.Size() > size {
		size = fi.Size()
	}
	kept := strings.TrimSpace(string(data[:cutPoint(data, maxInjectBytes)]))
	note := fmt.Sprintf("…（文件过大已截断：原始 %d 字节，仅保留前 %d KiB）", size, maxInjectBytes>>10)
	return strings.TrimSpace(kept + "\n" + note), nil
}

// cutPoint 返回 data 不超过 limit 字节的截断位置（要求 len(data) > limit）。
// 优先截在后半段的最后一个换行处，不留下半行（例如被截断的 @import 路径）；
// 否则退到 UTF-8 字符边界。
func cutPoint(data []byte, limit int) int {
	if i := bytes.LastIndexByte(data[:limit+1], '\n'); i >= limit/2 {
		return i
	}
	cut := limit
	// 合法 UTF-8 中字符起始字节之前最多 3 个续字节；非法编码不再继续回退。
	for k := 0; k < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(data[cut]); k++ {
		cut--
	}
	return cut
}

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
// 用于注入到系统提示中。单个文件超过 64 KiB 时截断。
func (m *Manager) LoadProjectContext() string {
	var parts []string

	for _, path := range m.config.ProjectContext {
		content, err := readForPrompt(path)
		if err != nil {
			continue
		}
		parts = append(parts, content)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// LoadPersistentMemory 从内存目录读取 MEMORY.md 文件，超过 64 KiB 时截断。
// 如果内存被禁用或文件不存在，返回空字符串。
func (m *Manager) LoadPersistentMemory() string {
	if m.config.MemoryDir == "" {
		return ""
	}

	content, err := readForPrompt(filepath.Join(m.config.MemoryDir, "MEMORY.md"))
	if err != nil {
		return ""
	}
	return content
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
