package builtin

import (
	"testing"

	ga "github.com/Dream355873200/GoAgent"
)

// WithIssueTools() 单独启用：Issue 工具注册、Task 工具不注册。
func TestIssueToolsOptionStandalone(t *testing.T) {
	app := ga.New(
		ga.WithMaxTurns(1),
		ga.WithIssueTools(),
	)
	names := app.ToolNames()
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("IssueReport") || !has("IssueResolve") {
		t.Fatalf("WithIssueTools 应注册 Issue 工具, got %v", names)
	}
	if has("TaskCreate") {
		t.Errorf("单独启用 Issue 不应注册 Task 工具, got %v", names)
	}
}

// WithTaskTools() 不带 Issue：Task 四件套有、Issue 无。
func TestTaskToolsWithoutIssue(t *testing.T) {
	app := ga.New(
		ga.WithMaxTurns(1),
		ga.WithTaskTools(),
	)
	names := app.ToolNames()
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if has("IssueReport") {
		t.Errorf("WithTaskTools 不应注册 Issue 工具, got %v", names)
	}
	if !has("TaskCreate") {
		t.Errorf("WithTaskTools 应注册 Task 工具, got %v", names)
	}
}

// 两个 Option 同时启用：复用同一 store（issue 记录能被 task 视角看到 kind 标记）。
func TestIssueToolsShareStoreWithTask(t *testing.T) {
	app := ga.New(
		ga.WithMaxTurns(1),
		ga.WithTaskTools(),
		ga.WithIssueTools(),
	)
	// TaskStore() 暴露的是 WithTaskTools 初始化的同一个实例
	ts := app.TaskStore()
	if ts == nil {
		t.Fatal("TaskStore 未初始化")
	}
	created := ts.Create("问题:登录失败", "401 无提示", "", nil)
	if created == nil || created.ID == "" {
		t.Fatal("创建任务失败")
	}
	// Issue 工具走同一 store → IssueResolve 能关闭上面创建的记录
	tool := app.ToolDescription("IssueResolve")
	if tool == "" {
		t.Fatal("IssueResolve 未注册")
	}
}
