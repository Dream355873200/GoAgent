// automemory.go 实现 Auto Memory 系统。
//
// 对齐 Claude Code 的 auto memory 机制:
// - MEMORY.md 主文件 + 话题子文件 (debugging.md, patterns.md 等)
// - 语义检索：按关键词搜索相关记忆文件
// - Session 开始时自动注入 MEMORY.md 到 system prompt
// - 去重：过滤重复的记忆内容
package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Dream355873200/GoAgent/internal/safename"
)

// mainName 是 MEMORY.md 主文件的记忆名（不含扩展名）。
const mainName = "MEMORY"

// AutoMemory 管理自动记忆系统。
//
// 锁约定：每个公开方法只加一次锁，内部统一调用不加锁的 *Locked 实现。
// sync.RWMutex 不可重入，且有写者排队时会阻塞新读者，嵌套加锁（包括
// 嵌套 RLock）都会死锁。
type AutoMemory struct {
	mu sync.RWMutex

	// dir 是记忆文件目录。
	dir string

	// maxMainFileLines 是 MEMORY.md 主文件的最大行数（超出截断）。
	maxMainFileLines int
}

// NewAutoMemory 创建一个新的 AutoMemory 实例。
func NewAutoMemory(dir string) *AutoMemory {
	return &AutoMemory{
		dir:              dir,
		maxMainFileLines: 200,
	}
}

// MemoryFile 表示一个记忆文件。
type MemoryFile struct {
	// Name 是文件名（不含路径和扩展名）。
	Name string `json:"name"`
	// Path 是文件的完整路径。
	Path string `json:"path"`
	// Content 是文件内容。
	Content string `json:"content"`
}

// filePath 返回记忆名对应的文件路径。name 必须能作为单个文件名
// （拒绝 ".."、路径分隔符等），防止读写记忆目录之外的文件。
func (am *AutoMemory) filePath(name string) (string, error) {
	return safename.Join("记忆名", am.dir, name, ".md")
}

// LoadAll 加载目录中所有 .md 记忆文件。
func (am *AutoMemory) LoadAll() ([]MemoryFile, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.loadAllLocked()
}

func (am *AutoMemory) loadAllLocked() ([]MemoryFile, error) {
	if am.dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(am.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []MemoryFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(am.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".md")
		files = append(files, MemoryFile{
			Name:    name,
			Path:    path,
			Content: string(data),
		})
	}

	return files, nil
}

// LoadMain 加载 MEMORY.md 主文件，自动截断超长内容（超过 64 KiB 或
// maxMainFileLines 行）。返回值用于注入提示词，不应原样写回文件。
func (am *AutoMemory) LoadMain() string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.loadMainLocked()
}

func (am *AutoMemory) loadMainLocked() string {
	if am.dir == "" {
		return ""
	}

	content, err := readForPrompt(filepath.Join(am.dir, mainName+".md"))
	if err != nil {
		return ""
	}

	// 截断超过 maxMainFileLines 的内容。
	lines := strings.Split(content, "\n")
	if len(lines) > am.maxMainFileLines {
		content = strings.Join(lines[:am.maxMainFileLines], "\n")
		content += "\n... (truncated)"
	}

	return strings.TrimSpace(content)
}

// Save 将内容保存到指定的记忆文件。name 不合法（如含 ".." 或路径分隔符）
// 时返回错误。
func (am *AutoMemory) Save(name, content string) error {
	am.mu.Lock()
	defer am.mu.Unlock()
	return am.saveLocked(name, content)
}

func (am *AutoMemory) saveLocked(name, content string) error {
	path, err := am.filePath(name)
	if err != nil {
		return err
	}
	if am.dir == "" {
		return nil
	}

	if err := os.MkdirAll(am.dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// SaveMain 保存 MEMORY.md 主文件。
func (am *AutoMemory) SaveMain(content string) error {
	am.mu.Lock()
	defer am.mu.Unlock()
	return am.saveLocked(mainName, content)
}

// Search 按关键词搜索相关的记忆文件。
// 返回内容包含任意关键词的文件列表。
func (am *AutoMemory) Search(keywords []string) ([]MemoryFile, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	all, err := am.loadAllLocked()
	if err != nil {
		return nil, err
	}

	var matched []MemoryFile
	for _, f := range all {
		contentLower := strings.ToLower(f.Content)
		for _, kw := range keywords {
			if strings.Contains(contentLower, strings.ToLower(kw)) {
				matched = append(matched, f)
				break
			}
		}
	}

	return matched, nil
}

// FormatForInjection 将记忆内容格式化为可注入 system prompt 的文本。
func (am *AutoMemory) FormatForInjection() string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	main := am.loadMainLocked()
	if main == "" {
		return ""
	}

	return "# auto memory\n\n" + main
}

// FilterDuplicates 从 items 中过滤掉与 existing 重复的内容。
// 使用内容前 100 字符作为指纹进行去重。
func FilterDuplicates(existing []string, items []MemoryFile) []MemoryFile {
	fingerprints := make(map[string]bool, len(existing))
	for _, s := range existing {
		fp := fingerprint(s)
		if fp != "" {
			fingerprints[fp] = true
		}
	}

	var unique []MemoryFile
	for _, item := range items {
		fp := fingerprint(item.Content)
		if fp != "" && fingerprints[fp] {
			continue // 重复，跳过
		}
		unique = append(unique, item)
		if fp != "" {
			fingerprints[fp] = true
		}
	}

	return unique
}

// fingerprint 取内容前 100 个字符作为指纹。
func fingerprint(content string) string {
	content = strings.TrimSpace(content)
	if len(content) == 0 {
		return ""
	}
	if len(content) > 100 {
		content = content[:100]
	}
	return strings.ToLower(content)
}

// Exists 检查指定名称的记忆文件是否存在。name 不合法时返回 false。
func (am *AutoMemory) Exists(name string) bool {
	path, err := am.filePath(name)
	if err != nil || am.dir == "" {
		return false
	}
	am.mu.RLock()
	defer am.mu.RUnlock()
	_, err = os.Stat(path)
	return err == nil
}

// Delete 删除指定名称的记忆文件。name 不合法时返回错误。
func (am *AutoMemory) Delete(name string) error {
	path, err := am.filePath(name)
	if err != nil {
		return err
	}
	if am.dir == "" {
		return nil
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	return os.Remove(path)
}
