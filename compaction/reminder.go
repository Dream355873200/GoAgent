// reminder.go 压缩后重注入（post-compact reminder）。
//
// 压缩（Layer 4 摘要 / 413 响应式压缩）把旧轮次折叠成摘要，伴随两类
// 不可接受的失忆：
//  1. 会话早期读过的文件内容被摘要掉——模型再编辑时凭摘要里的残句
//     拼凑 old_string，失败率大增；
//  2. 宿主维护的关键工件（任务规范、最新测试结论等）随原文一起消失。
//
// ReminderSource 让宿主/库内模块在压缩完成后重新注入提醒：每条返回值
// 独立成一条 user 消息追加在压缩边界之后，随事件流正常持久化。
package compaction

import (
	"context"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/reminder"
)

// ReminderSource 压缩后重注入源。
//
// Reminders 在压缩完成后调用（每次实际压缩各调一次），返回要注入的
// 文本列表；空串条目会被跳过，返回 nil 表示无提醒。实现必须并发安全。
// 典型实现：已读文件重水合（内容重注）、宿主固定资产（规范/报告）。
type ReminderSource interface {
	Reminders(ctx context.Context) []string
}

// ReminderSourceFunc 函数适配器。
type ReminderSourceFunc func(ctx context.Context) []string

func (f ReminderSourceFunc) Reminders(ctx context.Context) []string { return f(ctx) }

// appendReminderMessages 把提醒追加为 user 消息（空条目跳过）。
// 统一经 system-reminder 包装（source=post-compact）：重注入文本是
// 环境提醒而非用户发言，模型与回放端都要能辨识。
func appendReminderMessages(msgs []message.Message, reminders []string) []message.Message {
	for _, r := range reminders {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		msgs = append(msgs, message.NewUserMessage(reminder.Wrap(reminder.SourcePostCompact, r)))
	}
	return msgs
}
