// Package permission — 权限规则引擎。
//
// RuleSet 支持 allow/deny/ask 三种规则类型，
// 每种规则可以通过工具名称模式和输入模式匹配。
//
// 对齐 Claude Code 的权限规则系统。
package permission

// Rule 定义单个权限规则。
type Rule struct {
	// ToolPattern 是工具名称匹配模式。
	// 支持精确匹配和通配符：
	//   "Bash"          — 精确匹配 Bash 工具
	//   "Bash(git *)"   — 匹配 Bash 工具且输入以 "git " 开头
	//   "*"             — 匹配所有工具
	ToolPattern string

	// InputPattern 是可选的输入内容匹配模式。
	// 如果为空，匹配所有输入。
	InputPattern string

	// Behavior 是此规则的行为：allow、deny 或 ask。
	Behavior PermissionBehavior

	// Reason 描述此规则的原因。
	Reason string
}

// RuleSet 管理一组权限规则。
// 评估优先级：deny > ask > allow。
type RuleSet struct {
	rules []Rule
}

// NewRuleSet 创建一个新的规则集。
func NewRuleSet() *RuleSet {
	return &RuleSet{}
}

// AddRule 添加一条权限规则。
func (rs *RuleSet) AddRule(rule Rule) {
	rs.rules = append(rs.rules, rule)
}

// AddAllowRule 添加一条允许规则。
func (rs *RuleSet) AddAllowRule(toolPattern, inputPattern, reason string) {
	rs.AddRule(Rule{
		ToolPattern:  toolPattern,
		InputPattern: inputPattern,
		Behavior:     BehaviorAllow,
		Reason:       reason,
	})
}

// AddDenyRule 添加一条拒绝规则。
func (rs *RuleSet) AddDenyRule(toolPattern, inputPattern, reason string) {
	rs.AddRule(Rule{
		ToolPattern:  toolPattern,
		InputPattern: inputPattern,
		Behavior:     BehaviorDeny,
		Reason:       reason,
	})
}

// AddAskRule 添加一条询问规则。
func (rs *RuleSet) AddAskRule(toolPattern, inputPattern, reason string) {
	rs.AddRule(Rule{
		ToolPattern:  toolPattern,
		InputPattern: inputPattern,
		Behavior:     BehaviorAsk,
		Reason:       reason,
	})
}

// Evaluate 评估规则集并返回匹配的最高优先级结果。
// 优先级：deny > ask > allow。
// 如果没有规则匹配，返回 nil。
func (rs *RuleSet) Evaluate(toolName, input string) *PermissionResult {
	var bestAllow, bestAsk *PermissionResult

	for _, rule := range rs.rules {
		if !matchToolCall(rule.ToolPattern, rule.InputPattern, toolName, input) {
			continue
		}

		result := &PermissionResult{
			Behavior: rule.Behavior,
			Reason:   rule.Reason,
			Source:   "rule",
		}

		switch rule.Behavior {
		case BehaviorDeny:
			// deny 优先级最高，直接返回。
			return result
		case BehaviorAsk:
			if bestAsk == nil {
				bestAsk = result
			}
		case BehaviorAllow:
			if bestAllow == nil {
				bestAllow = result
			}
		}
	}

	// 按优先级返回：ask > allow。
	if bestAsk != nil {
		return bestAsk
	}
	if bestAllow != nil {
		return bestAllow
	}

	return nil
}
