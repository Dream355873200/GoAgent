// teamtool.go — run_team 工具：主 agent 运行时组建并运行一个多 agent 团队
//（GoAgent teams 包的动态入口，对齐 create_pipeline 的定位）。
//
// 主 agent 从任务推导团队编制（队长指令 + 成员角色 + 成员工具按名选），
// 提交 spec + 任务 → 引擎校验 → leader 循环驱动成员执行（成员的
// thinking/tool/progress 事件经 EmitFromTool 以 subagent_progress 实时
// 透出到对话流的 run_team 工具卡）→ leader 汇总文本作为工具结果返回。
//
// 数据载体约定（对齐 create_pipeline）：成员间交付默认 Markdown 片段；
// 供 UI 渲染的结构化字段用 MD 内嵌 JSON 围栏。
package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/teams"
)

// DynamicTeamMember 团队成员声明。
type DynamicTeamMember struct {
	// Name 成员唯一标识（[a-z][a-z0-9_]，如 tester / reviewer / writer）。
	Name string `json:"name" desc:"成员唯一标识（小写字母开头）"`
	// Role 角色卡：身份/职责/输出规范。
	Role string `json:"role" desc:"角色卡：身份、职责、产出格式" required:"true"`
	// Tools 成员可用工具名列表（从主 agent 工具箱按名选）。
	Tools []string `json:"tools,omitempty" desc:"成员可用工具名列表（只能从可用工具里选）"`
	// MaxTurns 成员单任务最大轮次（默认 40）。
	MaxTurns int `json:"max_turns,omitempty" desc:"成员单任务最大轮次，默认 40"`
}

// DynamicTeamSpec 动态团队声明。
type DynamicTeamSpec struct {
	// LeaderInstruction 队长指令（任务拆解/分派策略/验收标准/汇报格式）。
	LeaderInstruction string `json:"leader_instruction" desc:"队长指令：任务拆解、分派策略、验收标准、汇报格式" required:"true"`
	// SharedContext 团队共享背景（注入队长与所有成员）。
	SharedContext string `json:"shared_context,omitempty" desc:"团队共享背景（目标/约束/已知信息）"`
	// Members 成员列表（1~8 个）。
	Members []DynamicTeamMember `json:"members" desc:"团队成员列表" required:"true"`
}

// CreateTeamTool 返回 run_team 工具：主 agent 组队执行大型协作任务。
// 成员的执行过程（thinking/tool）经 subagent_progress 实时透出到对话流。
func (a *App) CreateTeamTool() NamedTool {
	desc := "组建并运行一个多 agent 团队：leader（队长）拆解任务并向成员分派，" +
		"成员完成后向 leader 汇报，leader 汇总后向用户报告。" +
		"成员的执行过程实时可见。适合需要多种职责协作的大任务" +
		"（如调研+分析+写作、测试+修复+验证）。" +
		"成员间交付默认用 Markdown；供 UI 渲染的结构化字段用内嵌 JSON 围栏。" +
		"成员的 tools 只能从可用工具里选。"
	if names := a.ToolNames(); len(names) > 0 {
		desc += "\n可用工具（成员的 tools 只能从这里选）: " + strings.Join(names, ", ")
	}
	return NamedTool{
		Name: "run_team",
		Def: ToolDef{
			Description: desc,
			Input:       runTeamInput{},
			Permission:  Normal,
		},
	}
}

type runTeamInput struct {
	Spec DynamicTeamSpec `json:"spec" desc:"团队编制（队长指令 + 成员列表）"`
	Task string          `json:"task" desc:"团队的总任务"`
}

// buildTeamRuntimes 校验 spec 并把成员工具从注册表解析为运行条目。
// 与 pipeline 节点同规则：成员内工具不经过权限门（团队边界在 run_team 处审批）。
func (a *App) buildTeamRuntimes(spec DynamicTeamSpec) ([]teams.MemberRuntime, error) {
	if len(spec.Members) == 0 || len(spec.Members) > 8 {
		return nil, fmt.Errorf("成员数量须在 1~8 之间")
	}
	runtimes := make([]teams.MemberRuntime, 0, len(spec.Members))
	for _, m := range spec.Members {
		if strings.HasPrefix(m.Name, "assign_") {
			return nil, fmt.Errorf("成员名不能以 assign_ 开头")
		}
		rt := teams.MemberRuntime{
			Name:     m.Name,
			Role:     m.Role + "\n[团队规范] 你向队长汇报；产出用 Markdown；任务完成后即汇报，不要越权联系用户。",
			MaxTurns: m.MaxTurns,
			Provider: a.provider,
		}
		for _, tn := range m.Tools {
			a.mu.RLock()
			t, ok := a.tools[tn]
			a.mu.RUnlock()
			if !ok {
				return nil, fmt.Errorf("成员 %s 的工具 %q 不存在（只能从可用工具里选）", m.Name, tn)
			}
			rt.Tools = append(rt.Tools, loop.ToolEntry{
				Name:        tn,
				Description: t.def.Description,
				InputSchema: t.inputSchema,
				// 与 pipeline 节点同规则：成员内跳过权限门（团队边界已审批）。
				Permission: permission.LevelReadOnly,
				Concurrent: t.def.Concurrent,
				ExecuteFn: func(ctx context.Context, input json.RawMessage) (string, error) {
					return t.def.call(ctx, input)
				},
			})
		}
		runtimes = append(runtimes, rt)
	}
	return runtimes, nil
}

// runTeam 执行动态团队（引擎内部入口，供 run_team 工具与测试使用）。
// OnEvent 团队事件经 EmitFromTool 以 subagent_progress 透出到主对话流，
// 由 run_team 工具卡实时渲染（成员归属见事件 Member 字段）。
func (a *App) runTeam(ctx Context, spec DynamicTeamSpec, task string) (string, error) {
	runtimes, err := a.buildTeamRuntimes(spec)
	if err != nil {
		return "", err
	}
	leader := teams.MemberRuntime{
		Name:     "leader",
		Role:     spec.LeaderInstruction,
		Provider: a.provider,
	}
	return teams.Run(ctx, teams.Config{
		Leader:        leader,
		Members:       runtimes,
		SharedContext: spec.SharedContext,
		OnEvent: func(ev teams.Event) {
			loop.EmitFromTool(ctx, loop.Event{
				Type:          loop.EvtSubAgentProgress,
				AgentID:       ev.Member,
				AgentDesc:     ev.Member,
				AgentStatus:   "running",
				AgentActivity: fmt.Sprintf("[%s] %s", ev.Type, firstLineOf(ev.Text)),
			})
		},
	}, task)
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
