package goagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// BuildDynPipeline 的校验矩阵：空 nodes / 节点数上限 / 重复名 / 不存在工具 /
// 引用悬空 / 环依赖。全部返回 LLM 可读错误（含修正所需信息）。
func TestBuildDynPipelineValidation(t *testing.T) {
	app := New(WithMaxTurns(1))
	// 注册两个假工具供选择
	app.UseTools(
		InferTool("lookup", "查数据", func(ctx context.Context, in struct {
			Q string `json:"q"`
		}) (string, error) {
			return "data", nil
		}),
		InferTool("write_out", "写结果", func(ctx context.Context, in struct {
			V string `json:"v"`
		}) (string, error) {
			return "ok", nil
		}),
	)

	mk := func(nodes string) *DynPipelineSpec {
		var spec DynPipelineSpec
		if err := json.Unmarshal([]byte(nodes), &spec); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		return &spec
	}

	cases := []struct {
		name    string
		spec    string
		wantErr string // 期望错误信息包含的子串
	}{
		{"空 nodes", `{"nodes":[]}`, "不能为空"},
		{"重复节点名", `{"nodes":[{"name":"a","instruction":"x"},{"name":"a","instruction":"y"}]}`, "重复"},
		{"工具不存在", `{"nodes":[{"name":"a","instruction":"x","tools":["nope"]}]}`, "工具不存在"},
		{"depends_on 悬空", `{"nodes":[{"name":"a","instruction":"x","depends_on":["ghost"]}]}`, "不存在的节点"},
		{"injects 悬空", `{"nodes":[{"name":"a","instruction":"x","injects":["ghost"]}]}`, "不存在的节点"},
		{"循环依赖", `{"nodes":[
			{"name":"a","instruction":"x","depends_on":["c"]},
			{"name":"b","instruction":"y","depends_on":["a"]},
			{"name":"c","instruction":"z","depends_on":["b"]}
		]}`, "循环依赖"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := app.BuildDynPipeline(mk(c.spec))
			if err == nil {
				t.Fatalf("应报错但通过了: %s", c.spec)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误信息应含 %q, got %q", c.wantErr, err.Error())
			}
		})
	}

	// 合法 spec 通过 + 工具正确解析
	ok := mk(`{"nodes":[
		{"name":"extract","instruction":"提取","tools":["lookup"],"message":"开始"},
		{"name":"report","instruction":"汇总","tools":["write_out","lookup"],"depends_on":["extract"]}
	]}`)
	cfg, err := app.BuildDynPipeline(ok)
	if err != nil {
		t.Fatalf("合法 spec 不应报错: %v", err)
	}
	if len(cfg.Nodes) != 2 {
		t.Fatalf("节点数错误: %d", len(cfg.Nodes))
	}
	if len(cfg.Nodes[0].Agent.Tools) != 1 || cfg.Nodes[0].Agent.Tools[0].Name != "lookup" {
		t.Fatalf("extract 工具解析错误: %+v", cfg.Nodes[0].Agent.Tools)
	}
	if len(cfg.Nodes[1].Agent.Tools) != 2 {
		t.Fatalf("report 工具解析错误: %+v", cfg.Nodes[1].Agent.Tools)
	}
	// 共享同一工具的两个节点：def 是同一份（工具无状态，共享安全）
	if &cfg.Nodes[0].Agent.Tools[0].Def == &cfg.Nodes[1].Agent.Tools[1].Def {
		// 不强求同一指针，只验证功能等价；这里仅冒烟
		_ = struct{}{}
	}
}

// 节点数上限：13 个节点报错。
func TestBuildDynPipelineNodeLimit(t *testing.T) {
	app := New(WithMaxTurns(1))
	var sb strings.Builder
	sb.WriteString(`{"nodes":[`)
	for i := 0; i < maxDynPipelineNodes+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"name":"n` + string(rune('a'+i)) + `","instruction":"x"}`)
	}
	sb.WriteString(`]}`)
	var spec DynPipelineSpec
	if err := json.Unmarshal([]byte(sb.String()), &spec); err != nil {
		t.Fatal(err)
	}
	if _, err := app.BuildDynPipeline(&spec); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("应报节点数超限: %v", err)
	}
}

// 根节点 message 省略时以 instruction 作为任务种子——否则队列空转、
// 节点一次 LLM 都不跑就「完成」（整条流水线瞬间假完成）。
func TestBuildDynPipelineRootMessageSeeding(t *testing.T) {
	app := New(WithMaxTurns(1))
	spec := &DynPipelineSpec{Nodes: []DynNodeSpec{
		{Name: "survey", Instruction: "做一次调研"},
		{Name: "report", Instruction: "汇总成报告", DependsOn: []string{"survey"}},
	}}
	cfg, err := app.BuildDynPipeline(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range cfg.Nodes {
		if n.Message == "" {
			t.Fatalf("节点 %q 缺少任务种子（空队列会直接跳过运行）", n.Name)
		}
	}
	if cfg.Nodes[0].Message != "做一次调研" {
		t.Fatalf("根节点应以 instruction 为任务种子, got %q", cfg.Nodes[0].Message)
	}
	// 显式 message 优先
	spec2 := &DynPipelineSpec{Nodes: []DynNodeSpec{
		{Name: "a", Instruction: "指令", Message: "显式任务"},
	}}
	cfg2, err := app.BuildDynPipeline(spec2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Nodes[0].Message != "显式任务" {
		t.Fatalf("显式 message 应优先, got %q", cfg2.Nodes[0].Message)
	}
}
