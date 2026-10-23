// Package permission 实现权限门控，将 Permission 枚举级别映射为实际的
// 允许/拒绝决定，追踪"始终允许"授权，并与 Approver 接口交互。
package permission

import (
	"context"
	"fmt"
	"sync"
	"time"

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
type PermissionMode int

const (
	// ModeDefault 使用默认权限逻辑：只读工具放行，其余询问用户。
	ModeDefault PermissionMode = iota
	// ModeBypassPermissions 不询问用户：普通操作直接放行，高风险操作
	// （见 HighRisk，以及命中 ask 规则的调用）交给 YOLO 分类器判定。
	ModeBypassPermissions
	// ModeAcceptEdits 自动接受编辑操作（高风险的编辑除外）。
	ModeAcceptEdits
	// ModePlan 规划模式，只允许只读操作。
	ModePlan
	// ModeDontAsk 不询问用户，对需要权限的操作直接拒绝。
	ModeDontAsk
	// ModeAuto 自动模式：每个非只读、非白名单的工具调用都交给 YOLO 分类器
	// 判定（allow 规则命中的直接放行，ask 规则命中的仍询问用户）。
	ModeAuto
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

// ContextApprover 是可选的扩展接口：实现它的审批者会收到本次工具调用的
// ctx——其中带有会话 ID（用于把审批请求路由到正确的会话），ctx 取消时
// 应尽快返回拒绝，使用户中断能打断审批等待。
type ContextApprover interface {
	ApproveContext(ctx context.Context, toolName string, input string, permission Level) (allow bool, alwaysAllow bool)
}

// ReasonApprover 是可选的扩展接口：在 ContextApprover 之上，拒绝时给出原因。
// 原因写进交给模型的工具结果（"权限被拒绝: <原因>"），模型据此调整做法。
type ReasonApprover interface {
	ApproveReason(ctx context.Context, toolName string, input string, permission Level) (allow bool, alwaysAllow bool, reason string)
}

// classifyTimeout 是单次 YOLO 分类调用的超时。
const classifyTimeout = 30 * time.Second

// Gate 管理工具调用的权限决定。
type Gate struct {
	approver          Approver
	mu                sync.RWMutex
	grants            *Grants         // "始终允许"授权（可在多个权限门之间共享，见 SetGrants）
	deniedCache       map[string]bool // toolName+input -> 用户是否最近拒绝
	mode              PermissionMode
	rules             *RuleSet
	guards            []Guard           // 外部策略链（单调：可拒不可放，见 Guard 注释）
	classifier        Classifier        // 分类器（默认 YOLO LLM 分类器，对齐 Claude Code）
	currentTranscript []message.Message // 由 loop 每轮更新，供 YOLO 分类器使用
	editTools         map[string]bool   // AcceptEdits 模式下自动放行的编辑类工具
}

// NewGate 使用给定的 Approver 创建一个新的权限 Gate。
// 如果 approver 为 nil，所有非 ReadOnly 工具将被拒绝（SDK 模式的安全默认）。
func NewGate(approver Approver) *Gate {
	return &Gate{
		approver:    approver,
		grants:      NewGrants(),
		deniedCache: make(map[string]bool),
		mode:        ModeDefault,
	}
}

// SetGrants 让权限门使用给定的"始终允许"授权集合（nil 表示换成一份新的）。
// 同一份 Grants 交给多个权限门时，任一权限门上记下的授权对其他权限门同样
// 生效。
func (g *Gate) SetGrants(grants *Grants) {
	if grants == nil {
		grants = NewGrants()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants = grants
}

// Grants 返回权限门当前使用的授权集合。
func (g *Gate) Grants() *Grants {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.grants
}

// SetEditTools 设置编辑类工具（写文件、改文件）。AcceptEdits 模式只自动
// 放行只读工具和这些工具；shell、代码执行等其他工具仍需确认。
func (g *Gate) SetEditTools(names []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.editTools = make(map[string]bool, len(names))
	for _, n := range names {
		g.editTools[n] = true
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

// Guard 是挂在 Gate 上的外部策略（对齐 DeepSeek harness 的 monotonic guard：
// 任何 Guard 都可以拒绝一次工具调用，但没有任何 Guard 能强行放行已被其他
// 策略拒绝的调用）。拒绝是终点；返回 nil 或非 Deny 结果等于把决定交还
// 给后续策略链（模式 / allow 规则 / 分类器 / 默认级别）。宿主用它接入
// 域内策略（审批升级、场景包规则、企业红线），不必改 Gate 本体。
type Guard struct {
	// Name 策略名（幂等注册：同名覆盖；结果 Source 标注 guard:<name>）。
	Name string
	// Fn 返回 nil = 不干预；返回 Deny = 拒绝（最终决定）；其余行为忽略。
	Fn func(toolName string, input string, level Level) *PermissionResult
}

// AddGuard 注册一条策略（同名覆盖）。
func (g *Gate) AddGuard(gd Guard) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.guards {
		if g.guards[i].Name == gd.Name {
			g.guards[i] = gd
			return
		}
	}
	g.guards = append(g.guards, gd)
}

// RemoveGuard 注销一条策略。
func (g *Gate) RemoveGuard(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.guards {
		if g.guards[i].Name == name {
			g.guards = append(g.guards[:i], g.guards[i+1:]...)
			return
		}
	}
}

// Classifier 裁决一次工具调用：auto 模式下判定每个非只读、非白名单的调用，
// bypass 模式下判定高风险调用与命中 ask 规则的调用，其他模式不调用。
//
// 返回 BehaviorAllow 放行、BehaviorDeny 拒绝（Reason 写进交给模型的工具结果）、
// BehaviorAsk 改为询问用户。返回错误表示分类器不可用，同样改为询问用户；
// 没有审批者时拒绝。每次调用有 30 秒超时（ctx）。
//
// 默认实现是 YoloClassifier（LLM 两阶段判定）。宿主可以换成规则引擎、外部
// 策略服务，或在 YOLO 之前加一道快速判断的组合。
type Classifier interface {
	ClassifyCall(ctx context.Context, req ClassifyRequest) (Classification, error)
}

// ClassifyRequest 是待裁决的工具调用。
type ClassifyRequest struct {
	ToolName string
	// Input 是调用参数（JSON）。
	Input string
	Level Level
	// Why 说明为何需要裁决，如 "auto 模式下的操作"、"高风险操作（写入系统目录）"。
	Why string
	// Transcript 是调用所在对话的记录（构建上下文用）。
	Transcript []message.Message
}

// Classification 是分类器的裁决。
type Classification struct {
	Behavior PermissionBehavior
	Reason   string
}

// ClassifierFunc 让普通函数实现 Classifier。
type ClassifierFunc func(ctx context.Context, req ClassifyRequest) (Classification, error)

// ClassifyCall 实现 Classifier。
func (f ClassifierFunc) ClassifyCall(ctx context.Context, req ClassifyRequest) (Classification, error) {
	return f(ctx, req)
}

// SetClassifier 设置分类器（nil 表示不用分类器）。分类器未设置或调用失败
// 时，需要裁决的调用改为询问用户（没有审批者则拒绝）。
func (g *Gate) SetClassifier(c Classifier) {
	if y, ok := c.(*YoloClassifier); ok && y == nil {
		c = nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.classifier = c
}

// SetTranscript 更新当前对话记录，供 YOLO 分类器构建上下文。
// 由 loop 每轮迭代时调用。ctx 中带有 WithTranscript 设置的记录时优先使用后者。
func (g *Gate) SetTranscript(msgs []message.Message) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.currentTranscript = msgs
}

type transcriptKey struct{}

// WithTranscript 把本次工具调用所在对话的记录放进 ctx，供分类器构建上下文。
// 同一个权限门被多个并发循环共用时（pipeline 的多个 worker、团队成员），
// SetTranscript 设置的共享记录可能属于另一个循环；ctx 中的记录总是本次调用的。
func WithTranscript(ctx context.Context, msgs []message.Message) context.Context {
	return context.WithValue(ctx, transcriptKey{}, msgs)
}

// CheckResult 执行完整的权限检查流程并返回 PermissionResult。
// 优先级：deny 规则 > Guard > ask 规则 > 模式 > allow 规则 > 默认级别。
func (g *Gate) CheckResult(toolName string, input string, level Level) PermissionResult {
	return g.checkResult(context.Background(), toolName, input, level)
}

func (g *Gate) checkResult(ctx context.Context, toolName string, input string, level Level) PermissionResult {
	g.mu.RLock()
	mode := g.mode
	rules := g.rules
	clf := g.classifier
	transcript := g.currentTranscript
	isEdit := g.editTools[toolName]
	guards := append([]Guard(nil), g.guards...)
	g.mu.RUnlock()
	if t, ok := ctx.Value(transcriptKey{}).([]message.Message); ok {
		transcript = t
	}

	var ruleRes *PermissionResult
	if rules != nil {
		ruleRes = rules.Evaluate(toolName, input)
	}

	// 第 1 步：deny 规则（最高优先级）。
	if ruleRes != nil && ruleRes.Behavior == BehaviorDeny {
		return *ruleRes
	}

	// 第 1.5 步：外部 Guard 链（单调：可拒不可放）。deny 规则之后、模式
	// 判定之前——宿主策略可以拦下任何调用（包括 bypass 模式），但不能
	// 强行放行（返回的 Allow/Ask 一律忽略）。
	for _, gd := range guards {
		if gd.Fn == nil {
			continue
		}
		if r := gd.Fn(toolName, input, level); r != nil && r.Behavior == BehaviorDeny {
			out := *r
			if out.Source == "" {
				out.Source = "guard:" + gd.Name
			}
			return out
		}
	}

	// 第 2 步：显式 ask 规则。ask 规则对只读工具和 AcceptEdits、auto 模式
	// 同样生效；bypass 模式不询问用户，命中 ask 规则的调用改由分类器判定。
	askRule := ruleRes != nil && ruleRes.Behavior == BehaviorAsk
	if askRule {
		switch mode {
		case ModeBypassPermissions:
			// 交给下面的 bypass 分支。
		case ModeDontAsk:
			return PermissionResult{Behavior: BehaviorDeny, Reason: "不询问模式，拒绝需要确认的操作", Source: "mode"}
		case ModePlan:
			if level != LevelReadOnly {
				return PermissionResult{Behavior: BehaviorDeny, Reason: "规划模式仅允许只读操作", Source: "mode"}
			}
			return *ruleRes
		default:
			return *ruleRes
		}
	}

	// 第 3 步：检查权限模式。
	switch mode {
	case ModeBypassPermissions:
		reason := ""
		switch {
		case askRule:
			reason = "命中 ask 规则"
		case level > LevelReadOnly:
			reason = HighRisk(toolName, input, level)
		}
		if reason == "" {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "绕过权限模式",
				Source:   "mode",
			}
		}
		// 高风险操作被显式 allow 规则覆盖时，用户已经明确授权，不再判定。
		if !askRule && ruleRes != nil && ruleRes.Behavior == BehaviorAllow {
			return *ruleRes
		}
		return judge(ctx, clf, transcript, toolName, input, level, "高风险操作（"+reason+"）")
	case ModeAuto:
		if level == LevelReadOnly || IsSafeYoloTool(toolName) {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "只读或白名单工具",
				Source:   "mode",
			}
		}
		if ruleRes != nil && ruleRes.Behavior == BehaviorAllow {
			return *ruleRes
		}
		return judge(ctx, clf, transcript, toolName, input, level, "auto 模式下的操作")
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
		if ruleRes != nil && ruleRes.Behavior == BehaviorAllow {
			return *ruleRes
		}
		return PermissionResult{
			Behavior: BehaviorDeny,
			Reason:   "不询问模式，拒绝需要权限的操作",
			Source:   "mode",
		}
	case ModeAcceptEdits:
		// 高风险的编辑（写入系统目录、凭据、shell 启动文件等）不自动接受。
		if level == LevelReadOnly || (isEdit && level <= LevelNormal && HighRisk(toolName, input, level) == "") {
			return PermissionResult{
				Behavior: BehaviorAllow,
				Reason:   "自动接受编辑模式",
				Source:   "mode",
			}
		}
	}

	// 第 4 步：allow 规则。
	if ruleRes != nil && ruleRes.Behavior == BehaviorAllow {
		return *ruleRes
	}

	// 第 5 步：回退到默认级别检查。
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

// judge 把调用交给分类器裁决；分类器未设置或不可用时退回询问用户。
func judge(ctx context.Context, clf Classifier, transcript []message.Message, toolName, input string, level Level, what string) PermissionResult {
	if clf != nil {
		if r, decided := classify(ctx, clf, ClassifyRequest{
			ToolName: toolName, Input: input, Level: level, Why: what, Transcript: transcript,
		}); decided {
			return r
		}
	}
	return PermissionResult{
		Behavior: BehaviorAsk,
		Reason:   what + "需要确认（分类器不可用）",
		Source:   "classifier_unavailable",
	}
}

// classify 调用分类器。decided 为 false 表示分类器不可用（返回错误或无效的
// 裁决），由调用方决定退路。
func classify(ctx context.Context, clf Classifier, req ClassifyRequest) (PermissionResult, bool) {
	cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()
	c, err := clf.ClassifyCall(cctx, req)
	if err != nil {
		return PermissionResult{}, false
	}
	source := "classifier"
	if _, ok := clf.(*YoloClassifier); ok {
		source = "yolo_classifier"
	}
	switch c.Behavior {
	case BehaviorAllow, BehaviorDeny, BehaviorAsk:
		return PermissionResult{Behavior: c.Behavior, Reason: c.Reason, Source: source}, true
	}
	return PermissionResult{}, false
}

// Check 决定是否允许工具调用。
// 返回 (allow bool, reason string)。
func (g *Gate) Check(toolName string, input string, level Level) (bool, string) {
	return g.CheckContext(context.Background(), toolName, input, level)
}

// CheckContext 同 Check，但审批与分类都在 ctx 下进行：ctx 携带的会话 ID
// 交给实现了 ContextApprover 的审批者路由，ctx 取消时立即拒绝。
func (g *Gate) CheckContext(ctx context.Context, toolName string, input string, level Level) (bool, string) {
	result := g.checkResult(ctx, toolName, input, level)

	switch result.Behavior {
	case BehaviorAllow:
		return true, result.Reason
	case BehaviorDeny:
		return false, result.Reason
	case BehaviorAsk:
		if level == LevelNormal && g.Grants().covers(toolName, input) {
			return true, "始终允许"
		}
		return g.ask(ctx, toolName, input, level)
	default:
		return false, "未知权限行为"
	}
}

// ask 通过 Approver 提示用户。
func (g *Gate) ask(ctx context.Context, toolName string, input string, level Level) (bool, string) {
	if g.approver == nil {
		return false, "未配置审批者，默认拒绝"
	}
	if ctx.Err() != nil {
		return false, "已取消"
	}

	// 检查拒绝缓存，防止提示疲劳。
	cacheKey := toolName + ":" + input
	g.mu.RLock()
	if g.deniedCache[cacheKey] {
		g.mu.RUnlock()
		return false, "之前已拒绝"
	}
	g.mu.RUnlock()

	var allow, alwaysAllow bool
	var reason string
	switch a := g.approver.(type) {
	case ReasonApprover:
		allow, alwaysAllow, reason = a.ApproveReason(ctx, toolName, input, level)
	case ContextApprover:
		allow, alwaysAllow = a.ApproveContext(ctx, toolName, input, level)
	default:
		allow, alwaysAllow = g.approver.Approve(toolName, input, level)
	}

	if allow {
		if alwaysAllow && level == LevelNormal {
			if key, ok := alwaysAllowKey(toolName, input); ok {
				g.Grants().add(key)
			}
		}
		return true, "用户批准"
	}
	if ctx.Err() != nil {
		return false, "已取消"
	}

	// 记录拒绝。
	g.mu.Lock()
	g.deniedCache[cacheKey] = true
	g.mu.Unlock()

	if reason != "" {
		return false, reason
	}
	return false, "用户拒绝"
}

// Reset 清除所有缓存的权限（例如新会话开始时）：撤销"始终允许"授权（与其他
// 权限门共享时一并撤销），清空拒绝缓存。
func (g *Gate) Reset() {
	g.Grants().Clear()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deniedCache = make(map[string]bool)
}

// Mode 返回当前权限模式。
func (g *Gate) Mode() PermissionMode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode
}
