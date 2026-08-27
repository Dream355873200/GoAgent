package builtin

import (
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/task"
)

// Issue 工具按会话路由 + 记录/关闭闭环。
func TestIssueTools(t *testing.T) {
	store := task.NewSessionStore(task.SessionStoreConfig{})
	report := IssueReportTool(store).Execute.(func(goagent.Context, issueReportInput) (string, error))
	resolve := IssueResolveTool(store).Execute.(func(goagent.Context, issueResolveInput) (string, error))

	ctx := goagent.Context{SessionID: "sess-a"}

	out, err := report(ctx, issueReportInput{Title: "登录后白屏", Severity: "error", Detail: "tap 登录后无跳转"})
	if err != nil || !strings.Contains(out, "登录后白屏") {
		t.Fatalf("report 失败: %v %q", err, out)
	}
	// 提取 ID
	id := strings.TrimPrefix(strings.Split(out, "#")[1], " ")
	id = strings.SplitN(id, " ", 2)[0]

	// 校验 metadata 标记 + 会话分区
	s := store.ForSession("sess-a")
	got := s.Get(id)
	if got == nil || got.Metadata[issueKind] != "error" {
		t.Fatalf("问题未正确入存: %+v", got)
	}
	if other := store.ForSession("sess-b").Get(id); other != nil {
		t.Errorf("问题串会话了")
	}

	// 关闭
	out2, err := resolve(ctx, issueResolveInput{IssueID: id, Resolution: "补上 Navigator 跳转"})
	if err != nil || !strings.Contains(out2, "已关闭") {
		t.Fatalf("resolve 失败: %v %q", err, out2)
	}
	if got := s.Get(id); got.Status != task.StatusCompleted {
		t.Errorf("关闭后状态应为 completed: %s", got.Status)
	}

	// 重复关闭报错
	if _, err := resolve(ctx, issueResolveInput{IssueID: id}); err == nil {
		t.Errorf("重复关闭应报错")
	}
}
