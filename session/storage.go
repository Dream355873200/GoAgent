// Package session — JSONL 持久化存储。
//
// 使用 JSONL（JSON Lines）格式进行增量写入，每条记录占一行。
// 支持消息记录、元数据记录和边界记录三种类型。
//
// 对齐 Claude Code 的 JSONL 会话存储。
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/internal/safename"
	"github.com/Dream355873200/GoAgent/message"
)

// RecordType 标识 JSONL 记录的类型。
type RecordType string

const (
	// RecordMessage 消息记录。
	RecordMessage RecordType = "message"
	// RecordMetadata 元数据记录。
	RecordMetadata RecordType = "metadata"
	// RecordBoundary 压缩边界记录。
	RecordBoundary RecordType = "boundary"
	// RecordState 状态变更记录。
	RecordState RecordType = "state"
	// RecordCheckpoint 挂起点检查记录：执行位置 + 挂起原因小块。
	// resume = 历史消息（已存在的 message 记录）+ 本记录的执行位置。
	RecordCheckpoint RecordType = "checkpoint"
)

// Record 是 JSONL 文件中的一条记录。
type Record struct {
	Type      RecordType      `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// Storage 管理会话的 JSONL 持久化。
//
// 同一进程内的写入串行化并逐条 fsync；读取不限制单行长度，损坏的行
// （例如进程被杀时写了一半的末行）被跳过，不会让整个会话不可读。
// 会话 ID 必须能安全地作为文件名（见 internal/safename），否则拒绝。
type Storage struct {
	dir string // 存储目录
	mu  sync.Mutex
}

// NewStorage 创建一个新的存储管理器。
func NewStorage(dir string) *Storage {
	return &Storage{dir: dir}
}

// sessionPath 返回会话文件的路径；会话 ID 不合法（含路径分隔符、".."
// 等）时返回错误。
func (s *Storage) sessionPath(sessionID string) (string, error) {
	return safename.Join("会话 ID", s.dir, sessionID, ".jsonl")
}

// Write 将一条记录追加到会话的 JSONL 文件。
func (s *Storage) Write(sessionID string, record Record) error {
	path, err := s.sessionPath(sessionID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("序列化记录失败: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return fmt.Errorf("创建存储目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开会话文件失败: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("写入会话文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步会话文件失败: %w", err)
	}
	return f.Close()
}

// WriteMessage 写入一条消息记录。
func (s *Storage) WriteMessage(sessionID string, msg message.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return s.Write(sessionID, Record{
		Type:      RecordMessage,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// WriteMetadata 写入一条元数据记录。
func (s *Storage) WriteMetadata(sessionID string, meta Metadata) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return s.Write(sessionID, Record{
		Type:      RecordMetadata,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// WriteState 写入一条状态变更记录。
func (s *Storage) WriteState(sessionID string, state State) error {
	data, err := json.Marshal(map[string]string{"state": state.String()})
	if err != nil {
		return err
	}
	return s.Write(sessionID, Record{
		Type:      RecordState,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// Checkpoint 是挂起点的执行现场小块（消息历史之外的部分）。
// 消息历史本来就在 JSONL 里逐条落盘——checkpoint 只补「执行位置 +
// 挂起原因」，resume = RunWithHistory(历史) + 从此处继续。
type Checkpoint struct {
	// Reason 挂起原因（approval/user）。
	Reason string `json:"reason"`
	// Turn 挂起时的轮次（已完成轮数）。
	Turn int `json:"turn"`
	// PendingStep 挂起时未完成的那步描述（如待审批的工具调用）。
	PendingStep string `json:"pending_step,omitempty"`
	// CreatedAt 挂起发生时间（Unix 毫秒）。
	CreatedAt int64 `json:"created_at"`
}

// WriteCheckpoint 写入一条挂起点检查记录（覆盖语义：ReadLastCheckpoint
// 只取最后一条，最新的执行位置才是恢复点）。
func (s *Storage) WriteCheckpoint(sessionID string, cp Checkpoint) error {
	cp.CreatedAt = time.Now().UnixMilli()
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return s.Write(sessionID, Record{
		Type:      RecordCheckpoint,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// ReadLastCheckpoint 读取最近一条挂起点检查记录。
// 无 checkpoint（会话不存在或从未挂起）返回 nil, nil。
func (s *Storage) ReadLastCheckpoint(sessionID string) (*Checkpoint, error) {
	if !s.Exists(sessionID) {
		return nil, nil
	}
	records, err := s.ReadAll(sessionID)
	if err != nil {
		return nil, err
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Type != RecordCheckpoint {
			continue
		}
		var cp Checkpoint
		if err := json.Unmarshal(records[i].Data, &cp); err != nil {
			return nil, fmt.Errorf("解析 checkpoint 记录失败: %w", err)
		}
		return &cp, nil
	}
	return nil, nil
}

// ReadAll 读取会话文件的所有记录。无法解析的行被跳过（写了一半的末行、
// 外部损坏），其余记录照常返回。
func (s *Storage) ReadAll(sessionID string) ([]Record, error) {
	path, err := s.sessionPath(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开会话文件失败: %w", err)
	}
	defer f.Close()

	var records []Record
	r := bufio.NewReaderSize(f, 1024*1024)
	for {
		line, readErr := r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			var record Record
			if err := json.Unmarshal(line, &record); err == nil {
				records = append(records, record)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("读取会话文件失败: %w", readErr)
		}
	}
	return records, nil
}

// ListSessions 列出存储目录中的所有会话 ID。
func (s *Storage) ListSessions() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取存储目录失败: %w", err)
	}

	type item struct {
		id  string
		mod time.Time
	}
	var items []item
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{id: name[:len(name)-len(ext)], mod: info.ModTime()})
	}

	// 按修改时间排序（最新在前）。
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	sessions := make([]string, len(items))
	for i, it := range items {
		sessions[i] = it.id
	}
	return sessions, nil
}

// Delete 删除指定会话的存储文件。
func (s *Storage) Delete(sessionID string) error {
	path, err := s.sessionPath(sessionID)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// Exists 检查指定会话是否存在（会话 ID 不合法时为 false）。
func (s *Storage) Exists(sessionID string) bool {
	path, err := s.sessionPath(sessionID)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
