package goagent

import (
	"fmt"

	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/provider"
)

// ---------------------------------------------------------------------------
// AgentTool 转接头：把一整个 agent 循环包成一个普通工具（ToolDef）。
//
// 与 SubAgent（WithSubAgents）是同一机制的两个朝向：
//   - SubAgent 是**运行时委派行为**——LLM 在对话中自主决定叫哪个子 agent
//     （WithSubAgents 注册成 Agent_<name> 工具，调不调由模型现场判断）
//   - AgentTool 是**构造时组合零件**——开发者写代码时把某个 agent 钉死成
//     一个固定名字的工具，可进 app.Tool() 注册表、pipeline 节点工具集、
//     动态 Pipeline 的按名解析工具箱、路由表等一切 ToolDef 能出现的位置
//
// 典型用途：领域助手模式——主 agent 对话里挂一个「项目助手」工具，内部
// 是带独立工具集的多轮 agent 循环（如 RAG 检索 + 领域查询），跑完只把
// 最终文本回传主对话，主 agent 上下文不被检索过程撑爆。
//
// 执行复用 agent.Runner（与 SubAgent 同一条路径）：独立消息历史、独立
// SystemPrompt、独立工具集、MaxTurns 上限。
// ---------------------------------------------------------------------------

// AgentToolInput 是 AgentTool 的输入 schema。
type AgentToolInput struct {
	// Task 是交给子 agent 的任务描述（会成为子 agent 的首条 user message）。
	Task string `json:"task" desc:"交给此 agent 的完整任务描述（背景+目标+产出要求），它是子 agent 看到的唯一输入" required:"true"`
}

// AgentToolOf 把一个 agent.Definition 包装成 ToolDef。
// 每次工具调用 = 一次完整的隔离 agent 运行（独立历史，跑完即弃）。
//
// prov 是子 agent 的 LLM provider——传 nil 时延迟到 NewWithSubAgentDeps
// 注入的 provider（见 WithAgentToolProv）；两者都为空则调用时报错。
//
// 示例：
//
//	app.Tool("project_assistant", goagent.AgentToolOf(
//	    agent.Definition{
//	        Name:         "project_assistant",
//	        Description:  "回答漫剧项目的设定与进度问题",
//	        SystemPrompt: "你是项目资料助手…",
//	        Tools:        []agent.ToolDef{…},   // 检索世界观/角色/分镜
//	        MaxTurns:     8,
//	    },
//	    prov,
//	))
func AgentToolOf(def agent.Definition, prov provider.Provider) ToolDef {
	runner := agent.NewRunner(prov)
	return ToolDef{
		Description: fmt.Sprintf("启动专属 agent「%s」执行任务并返回其结论。%s\n"+
			"该 agent 拥有独立的工具集和多轮推理循环——适合需要多步检索/分析的领域任务；"+
			"返回值是它的最终结论（推理过程不回传，不占用你的上下文）。",
			def.Name, def.Description),
		Input:      AgentToolInput{},
		Permission: Normal,
		Concurrent: true,
		Execute: func(ctx Context, in AgentToolInput) (string, error) {
			if in.Task == "" {
				return "", fmt.Errorf("task 不能为空")
			}
			result, err := runner.Run(ctx, def, in.Task)
			if err != nil {
				return "", fmt.Errorf("agent「%s」执行失败: %w", def.Name, err)
			}
			return fmt.Sprintf("%s\n\n--- agent「%s」: %d 轮, %d+%d tokens ---",
				result.FinalText, def.Name, result.TurnCount,
				result.Usage.InputTokens, result.Usage.OutputTokens), nil
		},
	}
}

// NamedAgentTool 是 AgentToolOf 的 NamedTool 形态（UseTools/ToolKit/节点
// Tools 字段直接吃这个）。
func NamedAgentTool(name string, def agent.Definition, prov provider.Provider) NamedTool {
	return NamedTool{Name: name, Def: AgentToolOf(def, prov)}
}
