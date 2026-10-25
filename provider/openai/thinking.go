// thinking.go 思考档位：规范档位 → 模型家族实际请求形态。
// 对齐 ZCode 的声明式 optionSpecs：每个模型家族声明自己支持的档位集
//（UI 按它渲染，不同模型档位不同），请求期把档位翻译成本家参数。
// 不认识的家族走默认档 + 端点侧 400 自适应（maybeDisableThinkingKnob）。
package openai

import (
	"strings"
)

// ThinkingLevels 模型家族支持的规范思考档位；nil = 该模型没有思考旋钮
//（UI 隐藏档位选择）。空串「默认」档恒可用，不入列。
func ThinkingLevels(modelID string) []string {
	switch thinkingFamily(modelID) {
	case thinkingSwitch:
		// GLM/Qwen 等开关型：只有开与关
		return []string{"off", "on"}
	case thinkingNone:
		// DeepSeek R1 类：思考恒开，无旋钮
		return nil
	case thinkingOpenAI:
		// o-series/gpt-5 系：无「关」，档位即 effort
		return []string{"low", "medium", "high", "max"}
	case thinkingClaude:
		// Claude 经兼容层暴露 reasoning_effort（中转内部翻译 thinking 预算）
		return []string{"low", "high", "max"}
	default:
		// 未知家族：沿用历史五档（off 不再映射 "none"——o-series 会拒收）
		return []string{"off", "low", "medium", "high"}
	}
}

type thinkingFamilyID int

const (
	thinkingDefault thinkingFamilyID = iota
	thinkingSwitch
	thinkingNone
	thinkingOpenAI
	thinkingClaude
)

func thinkingFamily(modelID string) thinkingFamilyID {
	m := strings.ToLower(modelID)
	switch {
	case strings.Contains(m, "deepseek"):
		return thinkingNone
	case strings.Contains(m, "glm") || strings.Contains(m, "chatglm") || strings.Contains(m, "qwen"):
		return thinkingSwitch
	case strings.Contains(m, "claude") || strings.Contains(m, "opus") || strings.Contains(m, "sonnet") || strings.Contains(m, "haiku"):
		return thinkingClaude
	case strings.Contains(m, "gpt-5") || strings.Contains(m, "gpt-6") || strings.Contains(m, "o3") || strings.Contains(m, "o4"):
		return thinkingOpenAI
	}
	return thinkingDefault
}

// resolveThinking 规范档位 → 请求参数：effort 写 reasoning_effort；
// enableThinking 写 enable_thinking 开关（GLM/Qwen 家族，档位不进 effort）。
// 空档位 = 不干预。
func resolveThinking(modelID, level string) (effort string, enableThinking *bool) {
	if level == "" {
		return "", nil
	}
	switch thinkingFamily(modelID) {
	case thinkingSwitch:
		return "", boolPtr(level != "off")
	case thinkingNone:
		return "", nil // 无旋钮：档位静默忽略
	case thinkingOpenAI, thinkingClaude:
		return level, nil
	default:
		if level == "off" {
			return "", nil
		}
		return level, nil
	}
}

func boolPtr(v bool) *bool { return &v }

// ---------- 400 自适应：端点不认档位参数时停发 ----------

// markThinkingKnobRejected 端点对档位参数返回校验错（reasoning_effort /
// enable_thinking 被拒）时记忆该 端点|模型：之后不再发任何思考参数。
// 与图片通道自适应同构——一次探测，永久自愈。
func (p *Provider) markThinkingKnobRejected(model string) {
	key := p.imgFormatKey(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.thinkingKnobOff == nil {
		p.thinkingKnobOff = map[string]bool{}
	}
	p.thinkingKnobOff[key] = true
}

func (p *Provider) thinkingKnobDisabled(model string) bool {
	key := p.imgFormatKey(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.thinkingKnobOff[key]
}

// maybeDisableThinkingKnob 判断错误是否为思考参数被端点拒绝，是则记忆并
// 返回 true 表示应不带档位参数重试一次。
func (p *Provider) maybeDisableThinkingKnob(model string, err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !(strings.Contains(msg, "reasoning_effort") || strings.Contains(msg, "enable_thinking") ||
		strings.Contains(msg, "reasoning.effort")) {
		return false
	}
	p.markThinkingKnobRejected(model)
	return true
}
