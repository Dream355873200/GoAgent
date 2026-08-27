// issue.go 问题记录工具：测试/开发过程中发现缺陷时记录下来，
// 修复后关闭——与 Task 系统同存储（metadata.kind=issue 区分），
// 天然继承会话隔离、落盘、闲置回收。
//
// 语义对齐 Claude Code 的「发现问题 → 记录 → 修复 → 勾掉」工作流：
// AI 在测试过程中发现 bug（tap 后白屏、断言失败、logcat 有 crash），
// 调 IssueReport 记录；修完调 IssueResolve 关闭。前端「问题」页签
// 与 analyze 静态问题合并展示。
package builtin

import (
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/task"
)

// issueKind metadata 键值：task 的 kind 元数据标为 issue（前端区分展示）。
const issueKind = "issue"

type issueReportInput struct {
	Title string `json:"title" desc:"问题简述（一句话，如：登录后白屏）" required:"true"`
	// Severity 严重度：error=功能不可用/崩溃；warning=功能可用但有缺陷；info=体验问题
	Severity string `json:"severity" desc:"严重度" enum:"error,warning,info" required:"true"`
	// Detail 复现路径与证据（步骤、截图路径、logcat 摘录）
	Detail string `json:"detail,omitempty" desc:"复现步骤与证据（截图路径/logcat 摘录）"`
	// Evidence 截图文件路径（逗号分隔多个）
	Evidence string `json:"evidence,omitempty" desc:"证据截图路径（screenshot 工具返回的路径，逗号分隔）"`
}

// IssueReportTool 报告一个问题（测试发现缺陷时用）。
func IssueReportTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "报告一个缺陷/问题（测试或开发中发现）。测试过程中发现问题（页面白屏、断言失败、logcat crash、" +
			"analyze error）时用本工具记录——问题进入「问题」页签，修复后用 IssueResolve 关闭。" +
			"注意：跨多轮的工作项用 TaskCreate（任务），当场能修的缺陷用本工具（问题）。",
		Input:      issueReportInput{},
		Permission: goagent.ReadOnly,
		Concurrent: true,
		Execute: func(ctx goagent.Context, in issueReportInput) (string, error) {
			sev := strings.ToLower(in.Severity)
			switch sev {
			case "error", "warning", "info":
			default:
				return "", fmt.Errorf("severity 必须是 error/warning/info")
			}
			meta := map[string]any{issueKind: sev}
			if in.Evidence != "" {
				meta["evidence"] = in.Evidence
			}
			t := storeFor(ctx, store).Create(in.Title, in.Detail, "修复问题："+in.Title, meta)
			return fmt.Sprintf("已记录问题 #%s [%s]: %s（修复后调用 IssueResolve 关闭）", t.ID, sev, in.Title), nil
		},
	}
}

type issueResolveInput struct {
	// IssueID 问题任务 ID
	IssueID string `json:"issueId" desc:"问题 ID（IssueReport 返回的编号）" required:"true"`
	// Resolution 修复说明（怎么修的）
	Resolution string `json:"resolution,omitempty" desc:"修复说明（做了什么改动）"`
}

// IssueResolveTool 关闭一个已修复的问题。
func IssueResolveTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "标记一个问题为已解决（IssueReport 记录的）。修复完成并验证通过后调用。" +
			"已解决的问题保留在「问题」页签（半透明+已解决标记），历史可查——它证明这个问题存在过且被修过。" +
			"流程：阻塞测试的问题 → 当场修复 → 验证 → 本工具标记；不阻塞的 → 测完一并修复标记。",
		Input:      issueResolveInput{},
		Permission: goagent.ReadOnly,
		Concurrent: true,
		Execute: func(ctx goagent.Context, in issueResolveInput) (string, error) {
			s := storeFor(ctx, store)
			t := s.Get(in.IssueID)
			if t == nil {
				return "", fmt.Errorf("问题 #%s 不存在", in.IssueID)
			}
			if t.Status != task.StatusPending && t.Status != task.StatusInProgress {
				return "", fmt.Errorf("问题 #%s 状态是 %s，无需关闭", in.IssueID, t.Status)
			}
			// 修复说明追加进描述（历史可查）
			patch := task.UpdatePatch{Status: task.StatusCompleted}
			if in.Resolution != "" {
				patch.Description = t.Description + "\n[修复] " + in.Resolution
			}
			if _, err := s.Update(in.IssueID, patch); err != nil {
				return "", err
			}
			return fmt.Sprintf("问题 #%s 已关闭: %s", in.IssueID, t.Subject), nil
		},
	}
}
