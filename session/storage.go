// Package session — JSONL 持久化存储。
//
// 使用 JSONL（JSON Lines）格式进行增量写入，每条记录占一行。
// 支持消息记录、元数据记录和边界记录三种类型。
//
// 对齐 Claude Code 的 JSONL 会话存储。
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anthropic-community/goagent/message"
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
)

// Record 是 JSONL 文件中的一条记录。
type Record struct {
	Type      RecordType      `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// Storage 管理会话的 JSONL 持久化。
type Storage struct {
	dir string // 存储目录
}

// NewStorage 创建一个新的存储管理器。
func NewStorage(dir string) *Storage {
	return &Storage{dir: dir}
}

// sessionPath 返回会话文件的路径。
func (s *Storage) sessionPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".jsonl")
}

// Write 将一条记录追加到会话的 JSONL 文件。
func (s *Storage) Write(sessionID string, record Record) error {
	// 确保目录存在。
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return fmt.Errorf("创建存储目录失败: %w", err)
	}

	path := s.sessionPath(sessionID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开会话文件失败: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("序列化记录失败: %w", err)
	}

	_, err = f.Write(append(data, '\n'))
	return err
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

// ReadAll 读取会话文件的所有记录。
func (s *Storage) ReadAll(sessionID string) ([]Record, error) {
	path := s.sessionPath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开会话文件失败: %w", err)
	}
	defer f.Close()

	var records []Record
	scanner := bufio.NewScanner(f)
	// 增大缓冲区以处理大消息。
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("第 %d 行解析失败: %w", lineNum, err)
		}
		records = append(records, record)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取会话文件失败: %w", err)
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

	var sessions []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if ext := filepath.Ext(name); ext == ".jsonl" {
			sessions = append(sessions, name[:len(name)-len(ext)])
		}
	}

	// 按修改时间排序（最新在前）。
	sort.Slice(sessions, func(i, j int) bool {
		iInfo, _ := os.Stat(s.sessionPath(sessions[i]))
		jInfo, _ := os.Stat(s.sessionPath(sessions[j]))
		if iInfo == nil || jInfo == nil {
			return false
		}
		return iInfo.ModTime().After(jInfo.ModTime())
	})

	return sessions, nil
}

// Delete 删除指定会话的存储文件。
func (s *Storage) Delete(sessionID string) error {
	path := s.sessionPath(sessionID)
	return os.Remove(path)
}

// Exists 检查指定会话是否存在。
func (s *Storage) Exists(sessionID string) bool {
	path := s.sessionPath(sessionID)
	_, err := os.Stat(path)
	return err == nil
}
