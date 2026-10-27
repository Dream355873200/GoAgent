// think_tags.go 思考伪标签防线：部分模型（GLM 类实测）会把
// <think>/<thinking>/<thought> 手写思考混进 text 通道——流式层用
// ThinkStreamFilter 增量改道，历史回放/非流式响应用 StripThinkTags 清洗。
// 实现放在 message 包，openai 与 anthropic 两个通道共用。
package message

import (
	"bytes"
	"regexp"
	"strings"
)

// thinkOpens / thinkCloses 是模型手写思考时用过的全部标签变体；开闭不保证
// 同名配对（实测出现过 <thinking> 开、</thought> 收），所以两侧独立认变体。
var (
	thinkOpens  = []string{"<think>", "<thinking>", "<thought>"}
	thinkCloses = []string{"</think>", "</thinking>", "</thought>"}
)

// ThinkStreamFilter 增量分流器：把 content 流里的手写思考段从 text 通道
// 改道到 thinking 通道。标签可能跨 chunk 断裂，用悬挂缓冲识别前缀；
// 非标签的 '<'（如 "a < b"）只延迟一个 chunk，随后原样透传。
type ThinkStreamFilter struct {
	buf     []byte
	inThink bool
	flushed bool
}

// Feed 喂入一段 content 增量，返回应走 text 与 thinking 通道的部分。
func (f *ThinkStreamFilter) Feed(s string) (text, think string) {
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
func (f *ThinkStreamFilter) Flush() (text, think string) {
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
// （可能是被 chunk 边界截断的标签，需继续悬挂等待）。
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

// StripThinkTags 清洗完整文本里的手写思考块：新流已被 ThinkStreamFilter
// 拦截，这里兜底处理历史会话里已落盘的泄漏残留。
func StripThinkTags(s string) string {
	s = thinkTagBlockRe.ReplaceAllString(s, "")
	s = unclosedThinkRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
