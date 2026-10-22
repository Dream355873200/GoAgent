// Package permission — 权限规则引擎。
//
// RuleSet 支持 allow/deny/ask 三种规则类型，
// 每种规则可以通过工具名称模式和输入模式匹配。
//
// 对齐 Claude Code 的权限规则系统。
package permission

import "strings"

// Rule 定义单个权限规则。
type Rule struct {
	// ToolPattern 是工具名称匹配模式。
	// 支持精确匹配和通配符：
	//   "Bash"          — 精确匹配 Bash 工具
	//   "Bash(git *)"   — 匹配 Bash 工具且命令以 "git " 开头（按子命令判定）
	//   "*"             — 匹配所有工具
	ToolPattern string

	// InputPattern 是可选的入参匹配模式，语义与括号内的模式相同：作用于
	// 从入参中提取的命令/路径/URL，* 为通配符，不带 * 时按词前缀匹配。
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
// 优先级：deny > ask > allow。如果没有规则匹配，返回 nil。
//
// 匹配对象是从工具入参中提取的值（见 extractSubject）。shell 命令会先
// 拆成子命令（含命令替换内部的命令）：
//   - deny / ask：任一子命令匹配即命中；不带 * 的模式还会对整条命令做
//     子串匹配（宁可多拦）。
//   - allow：每个子命令都要被某条 allow 规则覆盖才放行；含命令替换、
//     输出重定向的命令不会被带入参模式的 allow 规则放行。
func (rs *RuleSet) Evaluate(toolName, input string) *PermissionResult {
	subj := extractSubject(input)

	var bestAsk *PermissionResult
	for _, rule := range rs.rules {
		if rule.Behavior == BehaviorAllow || !rs.matchesBroad(rule, toolName, subj) {
			continue
		}
		result := &PermissionResult{Behavior: rule.Behavior, Reason: rule.Reason, Source: "rule"}
		if rule.Behavior == BehaviorDeny {
			return result
		}
		if bestAsk == nil {
			bestAsk = result
		}
	}
	if bestAsk != nil {
		return bestAsk
	}
	return rs.allowCovers(toolName, subj)
}

// rulePatterns 返回规则的入参模式（工具模式括号里的和 InputPattern），
// 以及工具名模式。
func rulePatterns(rule Rule) (name string, patterns []string) {
	name, inner := splitToolPattern(rule.ToolPattern)
	if inner != "" {
		patterns = append(patterns, inner)
	}
	if rule.InputPattern != "" {
		patterns = append(patterns, rule.InputPattern)
	}
	return name, patterns
}

// matchesBroad 用于 deny/ask：每个入参模式都要在任一子命令上命中。
func (rs *RuleSet) matchesBroad(rule Rule, toolName string, subj subject) bool {
	name, patterns := rulePatterns(rule)
	if !matchToolName(name, toolName) {
		return false
	}
	for _, p := range patterns {
		hit := false
		for _, v := range subj.values {
			if matchValue(p, v) {
				hit = true
				break
			}
		}
		if !hit && !strings.Contains(p, "*") && strings.Contains(subj.raw, p) {
			hit = true
		}
		if !hit {
			return false
		}
	}
	return true
}

// allowCovers 判断每个子命令是否都被某条 allow 规则覆盖，返回首条用到的规则结果。
func (rs *RuleSet) allowCovers(toolName string, subj subject) *PermissionResult {
	var first *PermissionResult
	for _, v := range subj.values {
		var covered *PermissionResult
		for _, rule := range rs.rules {
			if rule.Behavior != BehaviorAllow {
				continue
			}
			name, patterns := rulePatterns(rule)
			if !matchToolName(name, toolName) {
				continue
			}
			if len(patterns) == 0 {
				// 不带入参模式的 allow 规则放行该工具的一切调用。
				return &PermissionResult{Behavior: BehaviorAllow, Reason: rule.Reason, Source: "rule"}
			}
			if subj.risky {
				continue
			}
			ok := true
			for _, p := range patterns {
				if !matchValue(p, v) {
					ok = false
					break
				}
			}
			if ok {
				covered = &PermissionResult{Behavior: BehaviorAllow, Reason: rule.Reason, Source: "rule"}
				break
			}
		}
		if covered == nil {
			return nil
		}
		if first == nil {
			first = covered
		}
	}
	return first
}
