package hooks

import (
	"context"
	"testing"
)

// 双轨合并后的 stop hook 契约测试：
// RunStop 现场信息透传 + NewStopHook 函数包装 + 内置截断检测。

// RunStop 应把退出现场信息（stopReason/lastText/messages）透传给 hook。
func TestRunStopContextPassing(t *testing.T) {
	mgr := NewManager()

	var gotLast string
	var gotReason string
	mgr.Register(NewStopHook("probe", func(_ context.Context, hctx *HookContext) (bool, string) {
		gotLast = hctx.LastAssistantText
		gotReason = hctx.StopReason
		return false, ""
	}))

	_, err := mgr.RunStop(context.Background(), "sess-1", "end_turn", "hello world", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotLast != "hello world" || gotReason != "end_turn" {
		t.Fatalf("现场信息未透传: last=%q reason=%q", gotLast, gotReason)
	}
}

// block=true 应立即生效且 message 可携带修正内容。
func TestRunStopBlock(t *testing.T) {
	mgr := NewManager()
	mgr.Register(NewStopHook("guard", func(_ context.Context, hctx *HookContext) (bool, string) {
		if hctx.LastAssistantText == "没写完" {
			return true, "任务未完成，请继续"
		}
		return false, ""
	}))

	// 未触发
	res, err := mgr.RunStop(context.Background(), "", "end_turn", "已完成", nil)
	if err != nil || res != nil {
		t.Fatalf("完成态不应阻止: %v %v", res, err)
	}
	// 触发
	res, err = mgr.RunStop(context.Background(), "", "end_turn", "没写完", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Block || res.Message != "任务未完成，请继续" {
		t.Fatalf("阻止结果错误: %+v", res)
	}
}

// 内置截断检测：奇数个 ``` 视为代码块未闭合 → block；恢复上限生效。
func TestMaxOutputTokensStopHook(t *testing.T) {
	hook := MaxOutputTokensStopHook(2)

	run := func(last string) bool {
		res, _ := NewManagerWithHook(hook).RunStop(context.Background(), "", "end_turn", last, nil)
		return res != nil && res.Block
	}

	if run("完整回复，无代码块") {
		t.Fatal("无代码块不应触发")
	}
	if run("```go\nfmt.Println(1)\n```\n说明文字") {
		t.Fatal("闭合代码块不应触发")
	}
	if !run("```go\nfunc main() {") {
		t.Fatal("未闭合代码块应触发截断恢复")
	}
	if !run("```go\nfunc main() {") {
		t.Fatal("第二次仍在恢复上限内应触发")
	}
	if run("```go\nfunc main() {") {
		t.Fatal("超过 maxRecoveries=2 后应放行（防死循环）")
	}
}

// NewManagerWithHook 便利构造。
func NewManagerWithHook(h Hook) *Manager {
	m := NewManager()
	m.Register(h)
	return m
}
