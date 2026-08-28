package goagent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// WithLogger 注入的 logger 应被 pipeline 用于节点运行轨迹——
// 此前 37 处 log.Printf 直写 stderr，宿主日志体系完全看不到流水线现场。
// 用自定义 handler 捕获输出验证注入生效。
func TestWithLoggerPipelineTrace(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	// runLightweight 的轨迹日志挂在 pipeline 上；不实际跑 LLM，
	// 只验证 newPipeline 从 parent App 继承 logger。
	app := New(
		WithMaxTurns(1),
		WithLogger(logger),
	)
	app.UsePipeline(PipelineConfig{
		Nodes: []PipelineNode{
			{
				Name: "solo",
				Agent: &PipelineAgentDef{
					Name:        "solo",
					Instruction: "test",
				},
			},
		},
	})
	if app.pipeline == nil {
		t.Fatal("pipeline 未初始化")
	}
	if app.pipeline.logger != logger {
		t.Fatal("pipeline 未继承 WithLogger 注入的 logger")
	}

	// 跑一次 RunPipeline：轨迹日志（markNodeDone 等）应出现在注入的 handler 里。
	_ = app.RunPipeline(context.Background())
	out := buf.String()
	if !strings.Contains(out, "markNodeDone") {
		t.Fatalf("pipeline 日志未走注入的 logger, output=%q", out)
	}
}
