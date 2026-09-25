package goagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 同一轮的多个工具调用并行执行：两个工具互相等待对方启动，串行会死锁超时。
func TestSubAgentParallelToolsAndProgress(t *testing.T) {
	both := make(chan struct{})
	var once sync.Once
	var started sync.WaitGroup
	started.Add(2)
	go func() { started.Wait(); once.Do(func() { close(both) }) }()

	wait := func(ctx context.Context, _ json.RawMessage) (string, error) {
		started.Done()
		select {
		case <-both:
			return "ok", nil
		case <-time.After(2 * time.Second):
			return "", context.DeadlineExceeded
		}
	}
	two := message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{
		{Type: "tool_use", ToolUseID: "a", ToolName: "w1", Input: json.RawMessage(`{"path":"x.go"}`)},
		{Type: "tool_use", ToolUseID: "b", ToolName: "w2", Input: json.RawMessage(`{}`)},
	}}
	prov := &scriptedProvider{replies: []message.Message{two, textMsg("完成")}}
	def := agent.Definition{Name: "par", Tools: []agent.ToolDef{
		{Name: "w1", Execute: wait}, {Name: "w2", Execute: wait},
	}}

	var mu sync.Mutex
	var acts []string
	res, err := agent.NewRunner(prov).OnProgress(func(p agent.Progress) {
		mu.Lock()
		acts = append(acts, p.Activity)
		mu.Unlock()
	}).Run(context.Background(), def, "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "完成" {
		t.Fatalf("最终文本: %q", res.FinalText)
	}
	for _, m := range res.Messages[2:4] {
		if m.Content[0].IsError {
			t.Fatalf("工具应并行成功: %+v", m.Content[0])
		}
	}
	if strings.Join(acts, "|") != "w1 x.go|w2" {
		t.Fatalf("进度活动: %v", acts)
	}
}

// 后台运行：立即返回 task_id，完成后进入终态并触发 OnTaskDone。
func TestAgentToolBackground(t *testing.T) {
	mgr := bgtask.NewManager(t.TempDir())
	done := make(chan bgtask.TaskState, 1)
	mgr.OnTaskDone = func(s bgtask.TaskState) { done <- s }

	prov := &scriptedProvider{replies: []message.Message{textMsg("后台结论")}}
	tool := agentToolDef(agent.Definition{Name: "bg"}, func() provider.Provider { return prov },
		func() *bgtask.Manager { return mgr })

	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(AgentToolInput{Task: "跑", Background: true})
	out, err := tool.call(ctx, raw)
	cancel() // 本轮结束（中断）不应取消后台任务
	if err != nil || !strings.Contains(out, "task_id=") {
		t.Fatalf("应立即返回 task_id: %q %v", out, err)
	}
	select {
	case s := <-done:
		if s.Status != bgtask.StatusCompleted || s.Result != "后台结论" {
			t.Fatalf("终态: %s %q %s", s.Status, s.Result, s.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("后台任务未完成")
	}
}
