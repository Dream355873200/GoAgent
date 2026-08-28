package goagent

import (
	"context"
	"testing"
)

// PipelineConfig.OnEvent 应透出节点的 progress（含 StatusKey）/thinking 事件——
// 此前这两类在 runLightweight 的事件 switch 里被静默丢弃，嵌入方无法展示
// 节点的 429 限流等待和思考过程。
func TestPipelineOnEvent(t *testing.T) {
	events := make(chan PipelineEvent, 64)
	app := New(WithMaxTurns(1))
	app.UsePipeline(PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name:    "solo",
				Message: "do something", // 无 Message 的节点队列空转直接 done，agent 不会跑
				Agent: &PipelineAgentDef{
					Name:        "solo",
					Instruction: "test",
				},
			},
		},
		OnEvent: func(ev PipelineEvent) {
			select {
			case events <- ev:
			default: // 不阻塞流水线
			}
		},
	})
	// 无 provider：节点走空响应错误路径，error 事件应经 OnEvent 透出。
	if err := app.RunPipeline(context.Background()); err == nil {
		t.Fatal("无 provider 应返回错误")
	}
	close(events)
	var gotError bool
	for ev := range events {
		if ev.Node != "solo" {
			t.Fatalf("事件节点名错误: %+v", ev)
		}
		if ev.Type == "error" {
			gotError = true
		}
	}
	if !gotError {
		t.Fatal("error 事件未透出（空响应应产生 error 事件）")
	}
}
