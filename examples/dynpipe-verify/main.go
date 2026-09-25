// dynpipe-verify — RunDynPipeline 端到端验证（独立 main，不入发布）。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	goagent "github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/builtin"
)

func main() {
	app := goagent.New(
		goagent.ProviderConfig{
			Type:    "openai",
			Model:   os.Getenv("FLAI_MODEL"),
			APIKey:  os.Getenv("FLAI_API_KEY"),
			BaseURL: os.Getenv("FLAI_BASE_URL"),
		},
		goagent.WithToolKits(builtin.FileKit()),
	)

	spec := &goagent.DynPipelineSpec{Nodes: []goagent.DynNodeSpec{
		{
			Name: "writer", Concurrency: 1,
			Instruction: "你是写手：把收到的每个水果清单任务整理成 3 行水果清单（苹果/香蕉/樱桃 各一行），用 push_task 推给下游后结束。",
			Tools:       []string{"push_task"},
			Message:     "整理水果清单（苹果/香蕉/樱桃）",
			Output:      "Markdown：水果清单（每行一个）",
			Injects:     []string{"translator"},
		},
		{
			Name: "translator", DependsOn: []string{"writer"}, Concurrency: 1,
			Instruction: "你是翻译官：收到的上游内容是水果清单 MD，把水果名翻译成英文写进 fruits_en.md（Write 工具）。",
			Tools:       []string{"Read", "Write"},
		},
	}}

	err := app.RunDynPipeline(context.Background(), spec, func(ev goagent.PipelineEvent) {
		fmt.Printf("[event] node=%s type=%s text=%.80s\n", ev.Node, ev.Type, ev.Text)
	})
	if err != nil {
		fmt.Println("ERR:", err)
		os.Exit(1)
	}
	time.Sleep(500 * time.Millisecond)
	fmt.Println("PIPELINE-OK")
	if _, err := os.Stat("fruits_en.md"); err != nil {
		fmt.Println("FAIL: fruits_en.md 不存在")
		os.Exit(1)
	}
	b, _ := os.ReadFile("fruits_en.md")
	fmt.Println("fruits_en.md:", string(b)[:min(120, len(b))])
	fmt.Println("VERIFY-OK")
	os.Exit(0)
}
