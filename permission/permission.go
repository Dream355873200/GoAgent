// Package permission 实现权限门控，将 Permission 枚举级别映射为实际的
// 允许/拒绝决定，追踪"始终允许"授权，并与 Approver 接口交互。
package permission

import (
	"context"
	"fmt"
	"sync"

	"github.com/Dream355873200/GoAgent/message"
)

// Level 对应公共的 goagent.Permission 枚举。
type Level int

const (
	LevelReadOnly        Level = iota // 只读工具，自动允许
	LevelNormal                       // 普通工具，询问一次，可"始终允许"
	LevelRequireApproval              // 需要审批，每次询问
	LevelDangerous                    // 危险操作，警告+每次询问，不可"始终允许"
)

// String returns the string name of the permission level.
func (l Level) String() string {
	switch l {
	case LevelReadOnly:
		return "read_only"
	case LevelNormal:
		return "normal"
	case LevelRequireApproval:
		return "require_approval"
	case LevelDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// PermissionBehavior 是权限检查的三态结果。
// 对齐 Claude Code 的 allow/deny/ask 三态。
type PermissionBehavior int

const (
	// BehaviorAllow 允许工具执行。
	BehaviorAllow PermissionBehavior = iota
	// BehaviorDeny 拒绝工具执行。
	BehaviorDeny
	// BehaviorAsk 需要询问用户。
	BehaviorAsk
)

// String 返回行为名称。
func (b PermissionBehavior) String() string {
	switch b {
	case BehaviorAllow:
		return "allow"
	case BehaviorDeny:
		return "deny"
	case BehaviorAsk:
		return "ask"
	default:
		return "unknown"
	}
}

// PermissionMode 定义权限模式。
// 对齐 Claude Code 的 5 种权限模式。
type PermissionMode int

const (
	// ModeDefault 使用默认权限逻辑。
	ModeDefault PermissionMode = iota
	// ModeBypassPermissions 绕过所有权限检查（全部允许）。
	ModeBypassPermissions
	// ModeAcceptEdits 自动接受编辑操作。
	ModeAcceptEdits
	// ModePlan 规划模式，只允许只读操作。
	ModePlan
	// ModeDontAsk 不询问用户，对需要权限的操作直接拒绝。
	ModeDontAsk
)

// PermissionResult 是权限检查的完整结果。
type PermissionResult struct {
	// Behavior 是最终的权限行为。
	Behavior PermissionBehavior
	// Reason 描述决定的原因。
	Reason string
	// Source 标识决定来源（"rule"、"mode"、"approver"、"tool" 等）。
	Source string
}

// Approver 是 Gate 用来提示用户的接口。
// 对应公共的 goagent.Approver 接口。
type Approver interface {
	Approve(toolName string, input string, permission Level) (allow bool, alwaysAllow bool)
}

// Gate 管理工具调用的权限决定。
type Gate struct {
	approver          Approver
	mu                sync.RWMutex
	alwaysAllow       map[string]bool // toolName -> 用户是否授权"始终允许"
	deniedCache       map[string]bool // toolName+input -> 用户是否最近拒绝
	mode              PermissionMode
	rules             *RuleSet
	classifier        *YoloClassifier   // YOLO LLM 分类器（对齐 Claude Code）
	currentTranscript []message.Message // 由 loop 每轮更新，供 YOLO 分类器使用
}

// NewGate 使用给定的 Approver 创建一个新的权限 Gate。
// 如果 approver 为 nil，所有非 ReadOnly 工具将被拒绝（SDK 模式的安全默认）。
func NewGate(approver Approver) *Gate {
	return &Gate{
		approver:    approver,
		alwaysAllow: make(map[string]bool),
		deniedCache: make(map[string]bool),
		mode:        ModeDefault,
	}
}

// SetMode 设置权限模式。
func (g *Gate) SetMode(mode PermissionMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
}

// SetRules 设置权限规则集。
func (g *Gate) SetRules(rules *RuleSet) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = rules
}

// SetClassifier 设置 YOLO LLM 分类器。
// 对齐 Claude Code 的 YOLO classifier 集成。
// 设置后，非 ReadOnly 工具在 allow 规则通过后会经过 YOLO 分类。
func (g *Gate) SetClassifier(c *YoloClassifier) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.classifier = c
}

// SetTranscript 更新当前对话记录，供 YOLO 分类器构建上下文。
// 由 loop 每轮迭代时调用。
func (g *Gate) SetTranscript(msgs []message.Message) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.currentTranscript = msgs
}

// CheckResult 执行完整的权限检查流程并返回 PermissionResult。
// 优先级：deny 规则 > ask 规则 > mode > allow 规则 > 默认。
func (g *Gate) CheckResult(toolName string, input string, level Level) PermissionResult {
	g.mu.RLock()
	mode := g.mode
	rules := g.rules
	g.mu.RUnlock()

	// 第 1 步：检查 deny 规则（最高优先级）。
	if rules != nil {
		if result := rules.Evaluate(toolName, input); result != nil {
			if result.Behavior == BehaviorDeny {
				return *result
			}
			// 记录 allow/ask 规则结果，稍后可能用到。
		}
	}

	// 第 2 步：检查权限模式。
	switch mode {
	case ModeBypassPermissions:
		return PermissionResult{
			Behavior: BehaviorAllow,
			Reason:   "绕过权限模式",
			Source:   "mode",
		}
	case ModePlan:
		if level != LevelReadOnly {
			return PermissionResult{
				Behavior: BehaviorDeny,
				Reason:   "规划模式仅允许只读操作",
				Source:   "mode",
			}
		}
		return PermissionResult{
			Behavior: BehaviorAllow,
			Reason:   "规划模式，只读允许",
			Source:   "mode",
		}
	case ModeDontAsk:
		if level == LevelReadOnly {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "只读工具",
				Source:   "level",
			}
		}
		return PermissionResult{
			Behavior: BehaviorDeny,
			Reason:   "不询问模式，拒绝需要权限的操作",
			Source:   "mode",
		}
	case ModeAcceptEdits:
		if level == LevelReadOnly || level == LevelNormal {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "自动接受编辑模式",
				Source:   "mode",
			}
		}
	}

	// 第 3 步：检查 allow 规则。
	if rules != nil {
		if result := rules.Evaluate(toolName, input); result != nil && result.Behavior == BehaviorAllow {
			return *result
		}
	}

	// 第 3.5 步：YOLO LLM 分类器（对齐 Claude Code 的 classifyYoloAction 位置）。
	// 在 allow 规则通过后、默认级别检查前，使用 LLM 子模型判断操作安全性。
	if g.classifier != nil && level > LevelReadOnly {
		g.mu.RLock()
		clf := g.classifier
		transcript := g.currentTranscript
		g.mu.RUnlock()

		if IsSafeYoloTool(toolName) {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "安全工具白名单",
				Source:   "yolo_whitelist",
			}
		}

		result := clf.Classify(context.Background(), transcript, toolName, input)

		if result.Unavailable {
			// API 不可用，fail-open → 退回到正常权限逻辑（step 4）。
		} else if result.TranscriptTooLong {
			// 上下文过长，退回到正常权限逻辑（step 4）。
		} else if result.ShouldBlock {
			reason := "YOLO 分类器阻止"
			if result.Reason != "" {
				reason += ": " + result.Reason
			}
			return PermissionResult{
				Behavior: BehaviorDeny,
				Reason:   reason,
				Source:   "yolo_classifier",
			}
		} else {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "YOLO 分类器允许",
				Source:   "yolo_classifier",
			}
		}
	}

	// 第 4 步：回退到默认级别检查。
	switch level {
	case LevelReadOnly:
		return PermissionResult{
			Behavior: BehaviorAllow,
			Reason:   "只读工具",
			Source:   "level",
		}
	default:
		return PermissionResult{
			Behavior: BehaviorAsk,
			Reason:   fmt.Sprintf("需要用户确认 (级别: %d)", level),
			Source:   "level",
		}
	}
}

// Check 决定是否允许工具调用。
// 返回 (allow bool, reason string)。
// 保持向后兼容。
func (g *Gate) Check(toolName string, input string, level Level) (bool, string) {
	result := g.CheckResult(toolName, input, level)

	switch result.Behavior {
	case BehaviorAllow:
		return true, result.Reason
	case BehaviorDeny:
		return false, result.Reason
	case BehaviorAsk:
		// 检查"始终允许"缓存。
		g.mu.RLock()
		if g.alwaysAllow[toolName] && level == LevelNormal {
			g.mu.RUnlock()
			return true, "始终允许"
		}
		g.mu.RUnlock()

		// 询问用户。
		return g.ask(toolName, input, level)
	default:
		return false, "未知权限行为"
	}
}

// ask 通过 Approver 提示用户。
func (g *Gate) ask(toolName string, input string, level Level) (bool, string) {
	if g.approver == nil {
		return false, "未配置审批者，默认拒绝"
	}

	// 检查拒绝缓存，防止提示疲劳。
	cacheKey := toolName + ":" + input
	g.mu.RLock()
	if g.deniedCache[cacheKey] {
		g.mu.RUnlock()
		return false, "之前已拒绝"
	}
	g.mu.RUnlock()

	allow, alwaysAllow := g.approver.Approve(toolName, input, level)

	if allow {
		if alwaysAllow && level == LevelNormal {
			g.mu.Lock()
			g.alwaysAllow[toolName] = true
			g.mu.Unlock()
		}
		return true, "用户批准"
	}

	// 记录拒绝。
	g.mu.Lock()
	g.deniedCache[cacheKey] = true
	g.mu.Unlock()

	return false, "用户拒绝"
}

// Reset 清除所有缓存的权限（例如新会话开始时）。
func (g *Gate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.alwaysAllow = make(map[string]bool)
	g.deniedCache = make(map[string]bool)
}
