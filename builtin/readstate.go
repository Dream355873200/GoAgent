// readstate.go 文件读取状态跟踪（对齐 Claude Code 的 readFileState）。
//
// 会话级记录「哪些文件被 Read 过、内容指纹是什么」，支撑三个质量行为：
//  1. Edit/Write 前置校验：未读过的文件直接改 → 返回错误提示先 Read
//    （防止模型凭旧印象/想象编辑，old_string 匹配失败率大增）
//  2. 重复 Read 短路：文件指纹未变时返回「自上次读取无变化」
//    （防循环读取——模型拿到确定性答案就不再重试）
//  3. Write 覆盖保护：覆盖已读文件时正常放行（模型有最新视图）
//
// 状态按会话（SessionID）隔离，随会话生命周期存续。
package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// readState 全局读取状态表：sessionID → (绝对路径 → 内容 hash)。
type readState struct {
	mu    sync.Mutex
	files map[string]map[string]string
}

var reads = &readState{files: make(map[string]map[string]string)}

// sessionKey 会话键（空 session 归入 "" 分区）。
// 参数直接用 string 避免接口抽象。
func sessionKey(sessionID string) string { return sessionID }

// markRead 记录「本会话已读取该文件的当前内容指纹」。
func markRead(sessionID, path, content string) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	m := reads.files[sessionID]
	if m == nil {
		m = make(map[string]string)
		reads.files[sessionID] = m
	}
	m[path] = hashOf(content)
}

// hasRead 本会话是否读取过该文件（任意版本）。
func hasRead(sessionID, path string) bool {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	_, ok := reads.files[sessionID][path]
	return ok
}

// unchangedSinceRead 文件当前内容是否与会话上次读取时一致。
// 未读取过返回 false。
func unchangedSinceRead(sessionID, path, currentContent string) bool {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	h, ok := reads.files[sessionID][path]
	return ok && h == hashOf(currentContent)
}

// 写操作后使指纹失效（Edit/Write 改了文件，下次 Read 不能短路）。
func invalidate(sessionID, path string) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	delete(reads.files[sessionID], path)
}

func hashOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:8])
}
