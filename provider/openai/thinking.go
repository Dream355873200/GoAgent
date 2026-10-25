// thinking.go 思考档位：规范档位 → 模型家族实际请求形态。
// 对齐 ZCode 的声明式 optionSpecs：每个模型家族声明自己支持的档位集
//（UI 按它渲染，不同模型档位不同），请求期把档位翻译成本家参数。
// 不认识的家族走默认档 + 端点侧 400 自适应（maybeDisableThinkingKnob）。
// 另含思考伪标签防线：历史回放把思考包成 <thinking> 进上下文后，模型
// 会模仿着把思考手写进 content（实测泄漏），这里在流式层改道、回放层清洗。
package openai

import (
	"bytes"
	"regexp"
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

// ---------- 思考伪标签防线：content 通道混入 <think>/<thinking>/<thought> 时改道 ----------

// thinkOpens / thinkCloses 是模型手写思考时用过的全部标签变体；开闭不保证
// 同名配对（实测出现过 <thinking> 开、</thought> 收），所以两侧独立认变体。
var (
	thinkOpens  = []string{"<think>", "<thinking>", "<thought>"}
	thinkCloses = []string{"</think>", "</thinking>", "</thought>"}
)

// thinkStreamFilter 增量分流器：把 content 流里的手写思考段从 text 通道
// 改道到 thinking 通道。标签可能跨 chunk 断裂，用悬挂缓冲识别前缀；
// 非标签的 '<'（如 "a < b"）只延迟一个 chunk，随后原样透传。
type thinkStreamFilter struct {
	buf     []byte
	inThink bool
	flushed bool
}

// Feed 喂入一段 content 增量，返回应走 text 与 thinking 通道的部分。
func (f *thinkStreamFilter) Feed(s string) (text, think string) {
	f.buf = append(f.buf, s...)
	for {
		if f.inThink {
			best, tag := findEarliest(f.buf, thinkCloses)
			if best >= 0 {
				think += string(f.buf[:best])
				f.buf = f.buf[best+len(tag):]
				f.inThink = false
				continue
			}
			keep := partialTagLen(f.buf, thinkCloses)
			if keep < len(f.buf) {
				think += string(f.buf[:len(f.buf)-keep])
				f.buf = f.buf[len(f.buf)-keep:]
			}
			return text, think
		}
		best, tag := findEarliest(f.buf, thinkOpens)
		if best >= 0 {
			text += string(f.buf[:best])
			f.buf = f.buf[best+len(tag):]
			f.inThink = true
			continue
		}
		keep := partialTagLen(f.buf, thinkOpens)
		if keep < len(f.buf) {
			text += string(f.buf[:len(f.buf)-keep])
			f.buf = f.buf[len(f.buf)-keep:]
		}
		return text, think
	}
}

// Flush 在流结束时调用：吐出悬挂缓冲（未闭合标签按当前通道归属）。
// 幂等，只生效一次。
func (f *thinkStreamFilter) Flush() (text, think string) {
	if f.flushed {
		return "", ""
	}
	f.flushed = true
	s := string(f.buf)
	f.buf = nil
	if f.inThink {
		return "", s
	}
	return s, ""
}

// findEarliest 返回 tags 中任一标签在 buf 里最早的出现位置与对应标签。
func findEarliest(buf []byte, tags []string) (int, string) {
	best, bestTag := -1, ""
	for _, t := range tags {
		if i := bytes.Index(buf, []byte(t)); i >= 0 && (best < 0 || i < best) {
			best, bestTag = i, t
		}
	}
	return best, bestTag
}

// partialTagLen 返回 buf 末尾与任一标签真前缀吻合的最长字节数
//（可能是被 chunk 边界截断的标签，需继续悬挂等待）。
func partialTagLen(buf []byte, tags []string) int {
	max := 0
	for _, t := range tags {
		for l := len(t) - 1; l > max; l-- {
			if l <= len(buf) && bytes.HasSuffix(buf, []byte(t[:l])) {
				if l > max {
					max = l
				}
				break
			}
		}
	}
	return max
}

// thinkTagBlockRe 匹配完整的伪标签思考块（开闭变体独立出现，容忍交叉配对）。
var thinkTagBlockRe = regexp.MustCompile(`(?s)<(?:think|thinking|thought)>.*?</(?:think|thinking|thought)>`)

// unclosedThinkRe 匹配未闭合的伪标签块（从开标签吞到串尾）。
var unclosedThinkRe = regexp.MustCompile(`(?s)<(?:think|thinking|thought)>.*`)

// stripThinkTags 清洗完整文本里的手写思考块：新流已被 thinkStreamFilter
// 拦截，这里兜底处理历史会话里已落盘的泄漏残留。
func stripThinkTags(s string) string {
	s = thinkTagBlockRe.ReplaceAllString(s, "")
	s = unclosedThinkRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

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
