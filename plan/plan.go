// Package plan 实现 Plan Mode 系统。
//
// 对齐 Claude Code 的 EnterPlanMode/ExitPlanMode 工具。
// Plan mode 下 agent 只能使用读取类工具，不能执行写操作。
// Plan 文件存储在 .yume/plans/ 目录下。
package plan

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// State 表示 plan mode 的状态。
type State int

const (
	// StateInactive plan mode 未激活。
	StateInactive State = iota
	// StateActive plan mode 已激活，正在编写计划。
	StateActive
	// StateApproved 计划已被用户批准，准备执行。
	StateApproved
)

// String 返回状态名称。
func (s State) String() string {
	switch s {
	case StateInactive:
		return "inactive"
	case StateActive:
		return "active"
	case StateApproved:
		return "approved"
	default:
		return "unknown"
	}
}

// Manager 管理 plan mode 的生命周期。
type Manager struct {
	mu sync.RWMutex

	// state 是当前 plan mode 状态。
	state State

	// filePath 是当前 plan 文件路径。
	filePath string

	// content 是当前 plan 内容。
	content string

	// plansDir 是 plan 文件存储目录。
	plansDir string
}

// NewManager 创建一个新的 plan 管理器。
// plansDir 是 plan 文件存储目录（通常是 .yume/plans/）。
func NewManager(plansDir string) *Manager {
	return &Manager{
		plansDir: plansDir,
	}
}

// Enter 进入 plan mode。
// 创建一个新的 plan 文件并返回文件路径。
func (m *Manager) Enter() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == StateActive {
		return m.filePath, nil // 已经在 plan mode 中
	}

	// 确保目录存在。
	if err := os.MkdirAll(m.plansDir, 0755); err != nil {
		return "", fmt.Errorf("创建 plans 目录失败: %w", err)
	}

	// 生成随机文件名。
	name := generatePlanName()
	m.filePath = filepath.Join(m.plansDir, name+".md")
	m.state = StateActive
	m.content = ""

	return m.filePath, nil
}

// Exit 退出 plan mode。
// 返回 plan 文件内容供用户审核。
func (m *Manager) Exit() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != StateActive {
		return "", fmt.Errorf("未在 plan mode 中")
	}

	// 读取 plan 文件内容。
	if m.filePath != "" {
		data, err := os.ReadFile(m.filePath)
		if err == nil {
			m.content = string(data)
		}
	}

	m.state = StateApproved
	return m.content, nil
}

// Cancel 取消 plan mode，不执行计划。
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = StateInactive
	m.filePath = ""
	m.content = ""
}

// Complete 完成 plan mode（计划已执行完毕）。
func (m *Manager) Complete() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = StateInactive
}

// IsActive 返回是否在 plan mode 中。
func (m *Manager) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == StateActive
}

// State 返回当前状态。
func (m *Manager) GetState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// FilePath 返回当前 plan 文件路径。
func (m *Manager) FilePath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.filePath
}

// Content 返回当前 plan 内容。
func (m *Manager) Content() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.content
}

// WritePlan 写入 plan 内容到文件。
func (m *Manager) WritePlan(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != StateActive {
		return fmt.Errorf("未在 plan mode 中")
	}

	m.content = content
	if m.filePath != "" {
		return os.WriteFile(m.filePath, []byte(content), 0644)
	}
	return nil
}

// ReadPlan 从文件读取 plan 内容。
func (m *Manager) ReadPlan() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.filePath == "" {
		return m.content, nil
	}

	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return m.content, nil // 文件不存在时返回内存中的内容
	}
	return string(data), nil
}

// 形容词列表用于生成随机 plan 名称。
var adjectives = []string{
	"smooth", "bright", "gentle", "swift", "calm",
	"bold", "warm", "cool", "sharp", "soft",
	"clear", "deep", "light", "quick", "steady",
}

// 名词列表。
var nouns = []string{
	"tumbling", "sailing", "dancing", "running", "flying",
	"gliding", "spinning", "drifting", "soaring", "leaping",
}

// 动物列表。
var animals = []string{
	"unicorn", "phoenix", "falcon", "dolphin", "tiger",
	"eagle", "wolf", "bear", "hawk", "lynx",
}

// generatePlanName 生成一个随机的 plan 名称。
func generatePlanName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)

	idx1 := int(b[0]) % len(adjectives)
	idx2 := int(b[1]) % len(nouns)
	idx3 := int(b[2]) % len(animals)

	return fmt.Sprintf("%s-%s-%s", adjectives[idx1], nouns[idx2], animals[idx3])
}

// SetFilePath 手动设置 plan 文件路径（用于恢复 session）。
func (m *Manager) SetFilePath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filePath = path
}

// LoadExisting 从文件路径加载已存在的 plan。
func (m *Manager) LoadExisting(filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("读取 plan 文件失败: %w", err)
	}

	m.filePath = filePath
	m.content = string(data)
	m.state = StateActive
	return nil
}

// AllowedPermissions 是 plan mode 下允许的权限级别列表。
// Plan mode 下只允许 ReadOnly 权限的工具。
var AllowedPermissions = []string{"ReadOnly"}

// IsToolAllowed 检查在 plan mode 下是否允许使用指定工具。
// prefix: 空字符串表示使用内部状态判断。
func (m *Manager) IsToolAllowed(toolPermission string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.state != StateActive {
		return true // 不在 plan mode 中，所有工具都允许
	}

	for _, allowed := range AllowedPermissions {
		if toolPermission == allowed {
			return true
		}
	}
	return false
}

// FormatForSystemPrompt 返回 plan mode 的系统提示片段。
func (m *Manager) FormatForSystemPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.state != StateActive {
		return ""
	}

	hint := "You are currently in PLAN MODE. In this mode:\n"
	hint += "- Only use read-only tools (Read, Glob, Grep, etc.)\n"
	hint += "- Do NOT use write tools (Edit, Write, Bash with side effects)\n"
	hint += "- Write your plan to the plan file\n"
	hint += "- Use ExitPlanMode when your plan is ready for review\n"

	if m.filePath != "" {
		hint += fmt.Sprintf("\nPlan file: %s\n", m.filePath)
	}

	if m.content != "" {
		hint += fmt.Sprintf("\nCurrent plan content:\n%s\n", m.content)
	}

	return hint
}

// HexString 生成指定长度的十六进制随机字符串。
func hexString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
