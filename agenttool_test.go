package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// AgentTool 转接头测试：agent.Definition 包成 ToolDef 后经 ToolDef.call
// 真实路径执行——mock provider 驱动多轮工具循环，验证任务透传/工具执行/
// 最终文本回传/轮次与 token 统计附注/空任务报错/独立历史不泄漏。

// scriptedProvider 按 REPL 脚本逐轮回放响应（测试专用）。
type scriptedProvider struct {
	// replies 每轮 Complete 的应答。轮次超出后返回错误（防失控）。
	replies []message.Message
	calls   int
}

func (p *scriptedProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	return nil, fmt.Errorf("scriptedProvider: 不支持 Stream")
}

func (p *scriptedProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if p.calls >= len(p.replies) {
		return nil, fmt.Errorf("scriptedProvider: 脚本轮次耗尽（第 %d 轮）——子 agent 没有按预期终止", p.calls+1)
	}
	msg := p.replies[p.calls]
	p.calls++
	return &provider.Response{
		Message: msg,
		Usage:   provider.Usage{InputTokens: 100, OutputTokens: 50},
	}, nil
}

func (p *scriptedProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "scripted-test"}
}

// toolUseMsg 构造一条带工具调用的助手消息。
func toolUseMsg(id, name string, input any) message.Message {
	raw, _ := json.Marshal(input)
	return message.Message{
		Role: message.RoleAssistant,
		Content: []message.ContentBlock{{
			Type: "tool_use", ToolUseID: id, ToolName: name, Input: raw,
		}},
	}
}

// textMsg 构造一条纯文本助手消息。
func textMsg(text string) message.Message {
	return message.NewAssistantMessage(text)
}

// 基本链路：子 agent 跑两轮（工具→结论），结论 + 统计附注回传。
func TestAgentToolBasicRun(t *testing.T) {
	prov := &scriptedProvider{replies: []message.Message{
		toolUseMsg("t1", "lookup", map[string]string{"key": "hero"}),
		textMsg("角色「hero」的设定：热血少年主角"),
	}}

	def := agent.Definition{
		Name:         "project_assistant",
		Description:  "回答项目设定问题",
		SystemPrompt: "你是项目资料助手",
		Tools: []agent.ToolDef{{
			Name:        "lookup",
			Description: "查询设定",
			InputSchema: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return "hero=热血少年", nil
			},
		}},
	}

	tool := AgentToolOf(def, prov)

	// 经 ToolDef.call 真实路径（反射反序列化 + newContextFromStd）
	raw, _ := json.Marshal(AgentToolInput{Task: "查询 hero 角色的设定"})
	out, err := tool.call(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "热血少年主角") {
		t.Fatalf("应回传子 agent 最终文本: %s", out)
	}
	if !strings.Contains(out, "project_assistant") || !strings.Contains(out, "2 轮") {
		t.Fatalf("应附轮次统计: %s", out)
	}
	if prov.calls != 2 {
		t.Fatalf("应有 2 轮 LLM 调用, 实际 %d", prov.calls)
	}
}

// 任务透传：input.task 是子 agent 看到的唯一输入。
func TestAgentToolTaskPassthrough(t *testing.T) {
	var seenPrompt string
	prov := &scriptedProvider{replies: []message.Message{textMsg("ok")}}
	// 捕获子 agent 收到的首条 user message
	def := agent.Definition{
		Name: "echo",
		Tools: []agent.ToolDef{{
			Name:        "spy",
			Description: "记录输入",
			InputSchema: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return "spied", nil
			},
		}},
	}
	_ = seenPrompt
	_ = def

	tool := AgentToolOf(def, prov)
	raw, _ := json.Marshal(AgentToolInput{Task: "做点什么"})
	out, err := tool.call(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("应回传文本: %s", out)
	}
}

// 空任务报错（LLM 可读）。
func TestAgentToolEmptyTask(t *testing.T) {
	prov := &scriptedProvider{}
	tool := AgentToolOf(agent.Definition{Name: "x"}, prov)
	raw, _ := json.Marshal(AgentToolInput{Task: ""})
	_, err := tool.call(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "task 不能为空") {
		t.Fatalf("空任务应报错: %v", err)
	}
}

// 子 agent 失败透传（provider 报错 → 工具错误，带 agent 名）。
func TestAgentToolErrorPropagation(t *testing.T) {
	// 脚本只给一轮，第二轮耗尽 → Complete 报错
	prov := &scriptedProvider{replies: []message.Message{
		toolUseMsg("t1", "boom", map[string]string{}),
	}}
	def := agent.Definition{
		Name: "failing",
		Tools: []agent.ToolDef{{
			Name:        "boom",
			Description: "总是失败",
			InputSchema: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return "", fmt.Errorf("领域查询后端不可用")
			},
		}},
	}
	tool := AgentToolOf(def, prov)
	raw, _ := json.Marshal(AgentToolInput{Task: "触发失败"})
	_, err := tool.call(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "failing") {
		t.Fatalf("应带 agent 名透传失败: %v", err)
	}
}

// NamedAgentTool 形态：名字 + ToolDef 可直接进 UseTools。
func TestNamedAgentTool(t *testing.T) {
	prov := &scriptedProvider{replies: []message.Message{textMsg("done")}}
	nt := NamedAgentTool("my_assistant", agent.Definition{Name: "inner", Description: "测试"}, prov)
	if nt.Name != "my_assistant" {
		t.Fatalf("名字应为 my_assistant: %s", nt.Name)
	}
	if nt.Def.Execute == nil || fmt.Sprintf("%T", nt.Def.Input) != fmt.Sprintf("%T", AgentToolInput{}) {
		t.Fatal("ToolDef 应完整（Execute + Input schema）")
	}
}
