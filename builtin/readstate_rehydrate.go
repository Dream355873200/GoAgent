// readstate_rehydrate.go 压缩后已读文件重水合（post-compact reminder）。
//
// 全量压缩把旧轮次连同 Read 的文件内容一起折叠成摘要——模型再编辑
// 这些文件时只能凭摘要里的残句拼凑，old_string 匹配失败率大增。
// 本模块作为 compaction.ReminderSource 在压缩完成后重新注入最近读过的
// 文件：
//   - 小文件：以「Read 工具调用回放」格式带行号重注全文（模型等同
//     重新读过——指纹随之更新，后续 Edit/Write 前置校验照常放行）
//   - 大文件：注入「内容已移出上下文，操作前必须重新 Read」提醒
//
// 会话标识经 context 提取（SessionIDFromContext）；文件内容现读磁盘，
// 期间被外部修改的文件按当前磁盘内容重注（保证视图最新）。
package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Dream355873200/GoAgent"
)

// 重水合默认参数。
const (
	rehydrateMaxFiles     = 5     // 最多重注的文件数（最近读的优先）
	rehydrateFullMaxChars = 12000 // 全文重注的单文件上限（超过退化为提醒）
	rehydrateTotalBudget  = 40000 // 全文重注的总预算（超出即止）
)

// ReadStateRehydrater 已读文件重注入源。
type ReadStateRehydrater struct {
	// MaxFiles 最多重注的文件数（0 = 默认 5）。
	MaxFiles int
	// FullMaxChars 单文件全文重注上限（0 = 默认 12000）。
	FullMaxChars int
}

// NewReadStateRehydrater 创建重注入源（宿主经 WithPostCompactReminder 注册）。
func NewReadStateRehydrater() *ReadStateRehydrater { return &ReadStateRehydrater{} }

func (r *ReadStateRehydrater) limits() (maxFiles, fullMax int) {
	maxFiles, fullMax = r.MaxFiles, r.FullMaxChars
	if maxFiles <= 0 {
		maxFiles = rehydrateMaxFiles
	}
	if fullMax <= 0 {
		fullMax = rehydrateFullMaxChars
	}
	return
}

// Reminders 实现 compaction.ReminderSource。
func (r *ReadStateRehydrater) Reminders(ctx context.Context) []string {
	sessionID := goagent.SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil
	}
	maxFiles, fullMax := r.limits()

	var out []string
	budget := rehydrateTotalBudget
	for _, path := range recentReadPaths(sessionID, maxFiles) {
		content, err := os.ReadFile(path)
		if err != nil {
			// 文件已删除：提示即可（模型操作时会自然发现）。
			out = append(out, fmt.Sprintf("[上下文重注] 已读文件 %s 在压缩后被移出上下文，当前读取失败（可能已删除）——操作前请确认其状态。", path))
			continue
		}
		text := string(content)
		if len(text) <= fullMax && len(text) <= budget {
			budget -= len(text)
			out = append(out, fmt.Sprintf("[上下文重注] 压缩移除了此前的文件读取记录，以下是你最近读取的文件最新内容（等同重新 Read，可直接编辑）:\n\n"+
				"Called the Read tool with the following input: {\"file_path\": %q}\n"+
				"Result of calling the Read tool:\n%s", path, withLineNumbers(text)))
		} else {
			out = append(out, fmt.Sprintf("[上下文重注] 你最近读取过 %s（约 %d 字符），其内容已随上下文压缩移出。"+
				"再次编辑该文件前必须重新 Read 拿到最新内容——凭记忆构造 old_string 会匹配失败。", path, len(text)))
		}
	}
	return out
}

// recentReadPaths 该会话最近读取的文件路径（最新优先，最多 n 个）。
func recentReadPaths(sessionID string, n int) []string {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	order := reads.order[sessionID]
	if len(order) == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := len(order) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, order[i])
	}
	return out
}

// withLineNumbers 给文本加行号前缀（对齐 Read 工具的输出格式）。
func withLineNumbers(content string) string {
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var b strings.Builder
	line := 0
	for sc.Scan() {
		line++
		b.WriteString(fmt.Sprintf("%6d\t%s\n", line, sc.Text()))
	}
	return strings.TrimRight(b.String(), "\n")
}
