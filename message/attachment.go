// Package message 实现消息类型。
package message

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AttachmentConfig 附件配置。
type AttachmentConfig struct {
	// MaxSize 最大附件大小（字节）。
	MaxSize int64
	// AllowedTypes 允许的附件类型。
	AllowedTypes []string
	// TempDir 临时存储目录。
	TempDir string
}

// DefaultAttachmentConfig 返回默认配置。
func DefaultAttachmentConfig() AttachmentConfig {
	return AttachmentConfig{
		MaxSize:      10 * 1024 * 1024, // 10MB
		AllowedTypes: []string{"image/png", "image/jpeg", "image/gif", "application/pdf", "text/plain"},
		TempDir:      "./tmp/attachments",
	}
}

// Attachment 附件。
type Attachment struct {
	// ID 附件唯一 ID。
	ID string
	// Type MIME 类型。
	Type string
	// Name 文件名。
	Name string
	// Path 存储路径。
	Path string
	// Size 大小（字节）。
	Size int64
	// Content 内存内容（如果有）。
	Content []byte
}

// NewAttachment 从文件创建附件。
func NewAttachment(path string, cfg AttachmentConfig) (*Attachment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取文件: %w", err)
	}

	if info.Size() > cfg.MaxSize {
		return nil, fmt.Errorf("文件超过大小限制: %d > %d", info.Size(), cfg.MaxSize)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取文件: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	mimeType := extToMime(ext)

	if len(cfg.AllowedTypes) > 0 && !isAllowedType(mimeType, cfg.AllowedTypes) {
		return nil, fmt.Errorf("不允许的附件类型: %s", mimeType)
	}

	// 确保临时目录存在
	if err := os.MkdirAll(cfg.TempDir, 0755); err != nil {
		return nil, fmt.Errorf("无法创建临时目录: %w", err)
	}

	// 复制到临时目录
	tempPath := filepath.Join(cfg.TempDir, fmt.Sprintf("%d_%s", info.Size(), filepath.Base(path)))
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return nil, fmt.Errorf("无法保存附件: %w", err)
	}

	return &Attachment{
		ID:      generateAttachmentID(),
		Type:    mimeType,
		Name:    filepath.Base(path),
		Path:    tempPath,
		Size:    info.Size(),
		Content: data,
	}, nil
}

// NewAttachmentFromData 从数据创建附件。
func NewAttachmentFromData(name string, data []byte, cfg AttachmentConfig) (*Attachment, error) {
	if int64(len(data)) > cfg.MaxSize {
		return nil, fmt.Errorf("数据超过大小限制: %d > %d", len(data), cfg.MaxSize)
	}

	ext := strings.ToLower(filepath.Ext(name))
	mimeType := extToMime(ext)

	if len(cfg.AllowedTypes) > 0 && !isAllowedType(mimeType, cfg.AllowedTypes) {
		return nil, fmt.Errorf("不允许的附件类型: %s", mimeType)
	}

	// 确保临时目录存在
	if err := os.MkdirAll(cfg.TempDir, 0755); err != nil {
		return nil, fmt.Errorf("无法创建临时目录: %w", err)
	}

	tempPath := filepath.Join(cfg.TempDir, fmt.Sprintf("%d_%s", len(data), name))
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return nil, fmt.Errorf("无法保存附件: %w", err)
	}

	return &Attachment{
		ID:      generateAttachmentID(),
		Type:    mimeType,
		Name:    name,
		Path:    tempPath,
		Size:    int64(len(data)),
		Content: data,
	}, nil
}

// ToContentBlock 转换为消息内容块。
func (a *Attachment) ToContentBlock() (ContentBlock, error) {
	switch {
	case strings.HasPrefix(a.Type, "image/"):
		return ContentBlock{
			Type:      "image",
			Data:      string(a.Content),
			MediaType: a.Type,
		}, nil
	case a.Type == "application/pdf":
		return ContentBlock{
			Type: "text",
			Text: fmt.Sprintf("[PDF 附件: %s, %d 字节]", a.Name, a.Size),
		}, nil
	default:
		return ContentBlock{
			Type: "text",
			Text: fmt.Sprintf("[附件: %s, %d 字节]", a.Name, a.Size),
		}, nil
	}
}

// AttachmentManager 附件管理器。
type AttachmentManager struct {
	cfg         AttachmentConfig
	attachments map[string]*Attachment
}

// NewAttachmentManager 创建管理器。
func NewAttachmentManager(cfg AttachmentConfig) *AttachmentManager {
	if cfg.MaxSize == 0 {
		cfg = DefaultAttachmentConfig()
	}
	return &AttachmentManager{
		cfg:         cfg,
		attachments: make(map[string]*Attachment),
	}
}

// Add 添加附件。
func (m *AttachmentManager) Add(attachment *Attachment) {
	m.attachments[attachment.ID] = attachment
}

// Get 获取附件。
func (m *AttachmentManager) Get(id string) *Attachment {
	return m.attachments[id]
}

// List 列出所有附件。
func (m *AttachmentManager) List() []*Attachment {
	result := make([]*Attachment, 0, len(m.attachments))
	for _, a := range m.attachments {
		result = append(result, a)
	}
	return result
}

// Remove 移除附件。
func (m *AttachmentManager) Remove(id string) {
	if a, ok := m.attachments[id]; ok {
		os.Remove(a.Path)
		delete(m.attachments, id)
	}
}

// Clear 清除所有附件。
func (m *AttachmentManager) Clear() {
	for _, a := range m.attachments {
		os.Remove(a.Path)
	}
	m.attachments = make(map[string]*Attachment)
}

// ToMessages 转换为消息列表。
func (m *AttachmentManager) ToMessages() []Message {
	var msgs []Message
	for _, a := range m.attachments {
		block, _ := a.ToContentBlock()
		msgs = append(msgs, Message{
			Role:    RoleUser,
			Content: []ContentBlock{block},
		})
	}
	return msgs
}

// generateAttachmentID 生成唯一 ID。
func generateAttachmentID() string {
	return fmt.Sprintf("att_%d", len(os.Args))
}

// extToMime 扩展名转 MIME 类型。
func extToMime(ext string) string {
	mimeMap := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".pdf":  "application/pdf",
		".txt":  "text/plain",
		".md":   "text/markdown",
		".json": "application/json",
		".csv":  "text/csv",
	}
	if mime, ok := mimeMap[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}

// isAllowedType 检查类型是否允许。
func isAllowedType(mime string, allowed []string) bool {
	for _, a := range allowed {
		if a == mime || strings.HasPrefix(mime, strings.TrimSuffix(a, "/*")+"/") {
			return true
		}
	}
	return false
}

// ProcessAttachments 处理消息中的附件引用。
// 附件引用格式: @attachment:attachment_id
func ProcessAttachments(ctx context.Context, text string, mgr *AttachmentManager) ([]ContentBlock, error) {
	var blocks []ContentBlock

	// 简单的文本处理
	remaining := text
	for {
		idx := strings.Index(remaining, "@attachment:")
		if idx == -1 {
			if remaining != "" {
				blocks = append(blocks, ContentBlock{
					Type: "text",
					Text: remaining,
				})
			}
			break
		}

		// 添加 @ 之前的文本
		if idx > 0 {
			blocks = append(blocks, ContentBlock{
				Type: "text",
				Text: remaining[:idx],
			})
		}

		// 提取 attachment ID
		rest := remaining[idx+len("@attachment:"):]
		endIdx := strings.IndexAny(rest, " \n\t")
		if endIdx == -1 {
			endIdx = len(rest)
		}
		attID := rest[:endIdx]

		// 获取附件
		att := mgr.Get(attID)
		if att != nil {
			block, err := att.ToContentBlock()
			if err == nil {
				blocks = append(blocks, block)
			}
		}

		remaining = rest[endIdx:]
	}

	return blocks, nil
}
