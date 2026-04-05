// Package filehistory 实现文件写入前的自动快照备份。
//
// 对齐 Claude Code 的 src/utils/fileHistory.ts：
// - 在 Write/Edit 工具执行前保存文件原始内容到快照目录
// - 支持按文件路径和时间戳恢复
// - 快照存储在 .yume/file_history/ 目录下
//
// 使用方式：作为 Middleware 注册到 App。
package filehistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic-community/goagent"
)

// Snapshot 表示一个文件快照。
type Snapshot struct {
	OriginalPath string    `json:"original_path"` // 原始文件路径
	SnapshotPath string    `json:"snapshot_path"` // 快照文件路径
	Timestamp    time.Time `json:"timestamp"`     // 快照时间
	Size         int64     `json:"size"`          // 文件大小
}

// Store 管理文件快照的存储。
type Store struct {
	mu        sync.Mutex
	dir       string                // 快照目录（如 .yume/file_history/）
	snapshots map[string][]Snapshot // 按原始文件路径索引
}

// NewStore 创建一个新的快照存储。
func NewStore(dir string) *Store {
	return &Store{
		dir:       dir,
		snapshots: make(map[string][]Snapshot),
	}
}

// Save 保存文件的当前内容作为快照。
// 如果文件不存在（新建场景），不保存快照。
func (s *Store) Save(filePath string) (*Snapshot, error) {
	// 规范化路径。
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	// 读取当前文件内容。
	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 新文件，无需快照
		}
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}

	// 确保快照目录存在。
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return nil, fmt.Errorf("创建快照目录失败: %w", err)
	}

	// 生成快照文件名：hash(原始路径)_timestamp。
	now := time.Now()
	hash := sha256.Sum256([]byte(absPath))
	hashStr := hex.EncodeToString(hash[:8])
	ts := now.Format("20060102_150405")
	snapshotName := fmt.Sprintf("%s_%s", hashStr, ts)
	snapshotPath := filepath.Join(s.dir, snapshotName)

	// 写入快照。
	if err := os.WriteFile(snapshotPath, data, 0644); err != nil {
		return nil, fmt.Errorf("写入快照失败: %w", err)
	}

	snap := Snapshot{
		OriginalPath: absPath,
		SnapshotPath: snapshotPath,
		Timestamp:    now,
		Size:         int64(len(data)),
	}

	s.mu.Lock()
	s.snapshots[absPath] = append(s.snapshots[absPath], snap)
	s.mu.Unlock()

	return &snap, nil
}

// Restore 恢复文件到最近的快照。
func (s *Store) Restore(filePath string) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	s.mu.Lock()
	snaps := s.snapshots[absPath]
	s.mu.Unlock()

	if len(snaps) == 0 {
		return fmt.Errorf("无可用快照: %s", absPath)
	}

	// 取最近的快照。
	latest := snaps[len(snaps)-1]
	data, err := os.ReadFile(latest.SnapshotPath)
	if err != nil {
		return fmt.Errorf("读取快照失败: %w", err)
	}

	return os.WriteFile(absPath, data, 0644)
}

// List 返回指定文件的所有快照。
func (s *Store) List(filePath string) []Snapshot {
	absPath, _ := filepath.Abs(filePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Snapshot{}, s.snapshots[absPath]...)
}

// ── Middleware 实现 ──────────────────────────────────────────

// Middleware 是文件快照中间件。
// 在 Write/Edit 工具执行前自动保存文件快照。
type Middleware struct {
	store *Store
}

// NewMiddleware 创建文件快照中间件。
func NewMiddleware(store *Store) *Middleware {
	return &Middleware{store: store}
}

// filePathInput 用于从工具输入中提取 file_path。
type filePathInput struct {
	FilePath string `json:"file_path"`
}

// BeforeTool 在工具执行前保存文件快照。
func (m *Middleware) BeforeTool(ctx goagent.Context, toolName string, input json.RawMessage) *goagent.Decision {
	// 只对写工具做快照。
	if !isWriteTool(toolName) {
		return nil
	}

	// 从输入中提取文件路径。
	var fp filePathInput
	if err := json.Unmarshal(input, &fp); err != nil || fp.FilePath == "" {
		return nil // 无法提取路径，跳过
	}

	// 保存快照（忽略错误，不阻塞工具执行）。
	_, _ = m.store.Save(fp.FilePath)

	return nil // 继续执行工具
}

// AfterTool 工具执行后无操作。
func (m *Middleware) AfterTool(goagent.Context, string, *goagent.Result, error) {}

// isWriteTool 判断工具是否会修改文件。
func isWriteTool(name string) bool {
	lower := strings.ToLower(name)
	return lower == "write" || lower == "edit" || lower == "filewrite" || lower == "fileedit" ||
		lower == "filewritetool" || lower == "fileedittool" || lower == "notebookedit"
}
