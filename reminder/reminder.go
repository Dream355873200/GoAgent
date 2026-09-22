// Package reminder 统一 system-reminder 通道。
//
// 注入给模型的「非对话文本」——运行中插话、压缩后重注入、工具结果
// 内联提示——统一经 Wrap 打上 <system-reminder> 标记：
//
//   - 模型可辨识其工具性质（不是用户在说话，是指令性环境提醒）；
//   - 回放/前端可按标记过滤（不渲染成用户气泡）；
//   - 注入点不必各自发明前缀协议（如旧的 "[系统通知]"/"[confirm]{json}"
//     文本前缀）。
//
// 来源分类（source 参数，写入开标签的 source 属性）：
//
//	steer         插话通道注入（运行中插话、外部事件通知）
//	post-compact  压缩后重注入（ReminderSource 返回的提醒）
//	tool          工具结果内联提醒
//	host          其他宿主注入
//
// 注入位置规范：统一通道的文本只能出现在三类位置——工具批结束边界
// 的 user 消息（steer）、压缩边界的 user 消息（post-compact）、工具
// 结果尾部（tool）。不得作为普通用户输入进入对话。
//
// 防嵌套：内容中若出现闭合标签（无论大小写、含空白变体），一律转义
// 为 <\/system-reminder>——模型仍能读懂，但无法提前闭合外层标记。
package reminder

import (
	"strings"
)

// 来源常量（source 属性值）。
const (
	SourceSteer       = "steer"
	SourcePostCompact = "post-compact"
	SourceTool        = "tool"
	SourceHost        = "host"
)

const (
	openTag  = "<system-reminder>"
	closeTag = "</system-reminder>"
)

// escapeReminder 防嵌套转义：大小写不敏感地把内容中的闭合标签换成
// 转义形式（反斜杠不参与任何语法，纯视觉分隔，模型可正常理解）。
func escapeReminder(s string) string {
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "</system-reminder") {
		return s
	}
	const open = "</system-reminder" // 不含闭合 >（转义保留原标签的属性部分）
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+len(open) <= len(s) && strings.EqualFold(s[i:i+len(open)], open) {
			b.WriteString(`<\/system-reminder`)
			i += len(open)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Wrap 把 text 包装成 system-reminder 标记文本。空文本原样返回
// （调用方负责跳过空提醒）。source 为空时省略 source 属性。
func Wrap(source, text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	if source != "" {
		return `<system-reminder source="` + source + `">` + "\n" + escapeReminder(t) + "\n" + closeTag
	}
	return openTag + "\n" + escapeReminder(t) + "\n" + closeTag
}

// Inline 把提醒追加到工具结果尾部（source 固定为 tool——调用方是
// 工具本身，向模型补充环境状态/约束提醒）。result 为空时只返回提醒。
func Inline(result, text string) string {
	w := Wrap(SourceTool, text)
	if w == "" {
		return result
	}
	if strings.TrimSpace(result) == "" {
		return w
	}
	return result + "\n\n" + w
}
