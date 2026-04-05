// restore.go 实现压缩后的文件恢复机制。
//
// 对齐 Claude Code 的 POST_COMPACT_MAX_FILES_TO_RESTORE = 5。
// 追踪最近通过 Read 工具读取的文件，压缩后自动注入这些文件的最新内容。
package compaction

import (
	"fmt"
	"os"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
)

// PostCompactMaxFilesToRestore 是压缩后最多恢复的文件数。
const PostCompactMaxFilesToRestore = 5

// FileTracker 追踪最近读取的文件，用于压缩后恢复。
type FileTracker struct {
	mu    sync.Mutex
	files []trackedFile
}

// trackedFile 记录一个被追踪的文件。
type trackedFile struct {
	path   string
	toolID string // 关联的 tool_use ID
}

// NewFileTracker 创建一个新的文件追踪器。
func NewFileTracker() *FileTracker {
	return &FileTracker{}
}

// Track 记录一个文件读取操作。
func (ft *FileTracker) Track(filePath string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	// 去重：如果文件已存在，移到末尾。
	for i, f := range ft.files {
		if f.path == filePath {
			ft.files = append(ft.files[:i], ft.files[i+1:]...)
			break
		}
	}

	ft.files = append(ft.files, trackedFile{path: filePath})

	// 保持列表不超过上限。
	if len(ft.files) > PostCompactMaxFilesToRestore {
		ft.files = ft.files[len(ft.files)-PostCompactMaxFilesToRestore:]
	}
}

// GetRecentFiles 返回最近读取的文件路径列表。
func (ft *FileTracker) GetRecentFiles() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	paths := make([]string, len(ft.files))
	for i, f := range ft.files {
		paths[i] = f.path
	}
	return paths
}

// GenerateRestoreMessages 生成压缩后的文件恢复消息。
// 返回一对消息（assistant tool_use + user tool_result），每个文件一对。
func (ft *FileTracker) GenerateRestoreMessages() []message.Message {
	ft.mu.Lock()
	files := make([]trackedFile, len(ft.files))
	copy(files, ft.files)
	ft.mu.Unlock()

	var messages []message.Message

	for i, f := range files {
		// 尝试读取文件最新内容。
		content, err := os.ReadFile(f.path)
		if err != nil {
			continue // 文件不存在或不可读，跳过
		}

		// 生成唯一的 tool_use ID。
		toolID := fmt.Sprintf("restore_%d", i)

		// 构建 assistant message (tool_use)。
		assistantMsg := message.Message{
			Role: message.RoleAssistant,
			Content: []message.ContentBlock{
				{
					Type:      "tool_use",
					ToolUseID: toolID,
					ToolName:  "Read",
					// Input 是文件路径的 JSON。
					Input: []byte(fmt.Sprintf(`{"file_path":"%s"}`, f.path)),
				},
			},
		}

		// 构建 user message (tool_result)。
		resultContent := string(content)
		// 截断过长的内容。
		if len(resultContent) > 50000 {
			resultContent = resultContent[:50000] + "\n... (truncated for recovery)"
		}

		resultMsg := message.NewToolResultMessage(toolID, resultContent, false)

		messages = append(messages, assistantMsg, resultMsg)
	}

	return messages
}

// Clear 清除所有追踪记录。
func (ft *FileTracker) Clear() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.files = nil
}

// Count 返回当前追踪的文件数。
func (ft *FileTracker) Count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.files)
}
