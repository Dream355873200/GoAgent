package builtin

import (
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/task"
)

// Task 工具按会话路由：同一全局 SessionStore 注入，两个会话各建任务后
// TaskList 只看到自己的（task 跟随 session）。
func TestTaskToolsSessionRouting(t *testing.T) {
	store := task.NewSessionStore(task.SessionStoreConfig{})
	createTool := TaskCreateTool(store)
	listTool := TaskListTool(store)

	execCreate := createTool.Execute.(func(goagent.Context, taskCreateInput) (string, error))
	execList := listTool.Execute.(func(goagent.Context, struct{}) (string, error))

	ctxA := goagent.Context{SessionID: "sess-a"}
	ctxB := goagent.Context{SessionID: "sess-b"}

	if _, err := execCreate(ctxA, taskCreateInput{Subject: "A的任务"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execCreate(ctxA, taskCreateInput{Subject: "A的任务2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execCreate(ctxB, taskCreateInput{Subject: "B的任务"}); err != nil {
		t.Fatal(err)
	}

	outA, err := execList(ctxA, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "A的任务2"; !strings.Contains(outA, want) {
		t.Errorf("sess-a 列表应含 %q，得到:\n%s", want, outA)
	}
	if want := "B的任务"; strings.Contains(outA, want) {
		t.Errorf("sess-a 列表不应看到 B 的任务（会话隔离失效）:\n%s", outA)
	}

	outB, err := execList(ctxB, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outB, "B的任务") || strings.Contains(outB, "A的任务") {
		t.Errorf("sess-b 列表串了别的会话的任务:\n%s", outB)
	}
}
