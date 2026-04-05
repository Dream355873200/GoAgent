package builtin

// 本文件定义框架内置的管理工具 ToolDef。
// 对应 Claude Code 的 Task/Plan/Skill/Cron/Worktree 工具。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic-community/goagent"
	"github.com/anthropic-community/goagent/bgtask"
	"github.com/anthropic-community/goagent/cron"
	"github.com/anthropic-community/goagent/plan"
	"github.com/anthropic-community/goagent/skill"
	"github.com/anthropic-community/goagent/task"
	"github.com/anthropic-community/goagent/worktree"
)

// ManagementTools 返回所有管理类内置工具（Task/Plan/Skill/Cron/Worktree）。
// 需要传入各子系统实例。
func ManagementTools(deps ManagementDeps) []goagent.NamedTool {
	var tools []goagent.NamedTool

	if deps.TaskStore != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "TaskCreate", Def: TaskCreateTool(deps.TaskStore)},
			goagent.NamedTool{Name: "TaskUpdate", Def: TaskUpdateTool(deps.TaskStore)},
			goagent.NamedTool{Name: "TaskGet", Def: TaskGetTool(deps.TaskStore)},
			goagent.NamedTool{Name: "TaskList", Def: TaskListTool(deps.TaskStore)},
		)
	}

	if deps.PlanManager != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "EnterPlanMode", Def: EnterPlanModeTool(deps.PlanManager)},
			goagent.NamedTool{Name: "ExitPlanMode", Def: ExitPlanModeTool(deps.PlanManager)},
		)
	}

	if deps.SkillRegistry != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "Skill", Def: SkillTool(deps.SkillRegistry)},
		)
	}

	if deps.CronScheduler != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "CronCreate", Def: CronCreateTool(deps.CronScheduler)},
			goagent.NamedTool{Name: "CronDelete", Def: CronDeleteTool(deps.CronScheduler)},
			goagent.NamedTool{Name: "CronList", Def: CronListTool(deps.CronScheduler)},
		)
	}

	if deps.WorktreeManager != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "EnterWorktree", Def: EnterWorktreeTool(deps.WorktreeManager)},
			goagent.NamedTool{Name: "ExitWorktree", Def: ExitWorktreeTool(deps.WorktreeManager)},
		)
	}

	if deps.BgTaskManager != nil {
		tools = append(tools,
			goagent.NamedTool{Name: "TaskStop", Def: TaskStopTool(deps.BgTaskManager)},
			goagent.NamedTool{Name: "TaskOutput", Def: TaskOutputTool(deps.BgTaskManager)},
		)
	}

	return tools
}

// ManagementDeps 是管理工具所需的依赖。
type ManagementDeps struct {
	TaskStore       *task.Store
	PlanManager     *plan.Manager
	SkillRegistry   *skill.Registry
	CronScheduler   *cron.Scheduler
	WorktreeManager *worktree.Manager
	BgTaskManager   *bgtask.Manager
}

// ---------- Task 工具 ----------

type taskCreateInput struct {
	Subject     string `json:"subject" desc:"任务简短标题（祈使句）" required:"true"`
	Description string `json:"description" desc:"任务详细描述" required:"true"`
}

func TaskCreateTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "创建一个新任务。用于跟踪多步骤工作的进度。",
		Input:       taskCreateInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in taskCreateInput) (string, error) {
			t := store.Create(in.Subject, in.Description, "", nil)
			return fmt.Sprintf("已创建任务 #%s: %s", t.ID, t.Subject), nil
		},
	}
}

type taskUpdateInput struct {
	TaskID      string `json:"taskId" desc:"任务 ID" required:"true"`
	Status      string `json:"status,omitempty" desc:"新状态: pending/in_progress/completed/deleted"`
	Subject     string `json:"subject,omitempty" desc:"新标题"`
	Description string `json:"description,omitempty" desc:"新描述"`
}

func TaskUpdateTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "更新任务的状态或字段。",
		Input:       taskUpdateInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in taskUpdateInput) (string, error) {
			patch := task.UpdatePatch{
				Subject:     in.Subject,
				Description: in.Description,
			}
			if in.Status != "" {
				patch.Status = task.Status(in.Status)
			}
			t, err := store.Update(in.TaskID, patch)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已更新任务 #%s: 状态=%s", t.ID, t.Status), nil
		},
	}
}

type taskGetInput struct {
	TaskID string `json:"taskId" desc:"任务 ID" required:"true"`
}

func TaskGetTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "获取任务的完整详情。",
		Input:       taskGetInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in taskGetInput) (string, error) {
			t := store.Get(in.TaskID)
			if t == nil {
				return "", fmt.Errorf("任务 #%s 不存在", in.TaskID)
			}
			lines := []string{
				fmt.Sprintf("任务 #%s", t.ID),
				fmt.Sprintf("标题: %s", t.Subject),
				fmt.Sprintf("状态: %s", t.Status),
			}
			if t.Description != "" {
				lines = append(lines, fmt.Sprintf("描述: %s", t.Description))
			}
			if len(t.BlockedBy) > 0 {
				lines = append(lines, fmt.Sprintf("依赖: %s", strings.Join(t.BlockedBy, ", ")))
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

func TaskListTool(store task.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "列出所有任务的摘要。",
		Input:       struct{}{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in struct{}) (string, error) {
			summaries := store.ListSummaries()
			if len(summaries) == 0 {
				return "(无任务)", nil
			}
			var lines []string
			for _, s := range summaries {
				line := fmt.Sprintf("#%s [%s] %s", s.ID, s.Status, s.Subject)
				if len(s.BlockedBy) > 0 {
					line += fmt.Sprintf(" (blocked by: %s)", strings.Join(s.BlockedBy, ","))
				}
				lines = append(lines, line)
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

// ---------- Plan 工具 ----------

func EnterPlanModeTool(store plan.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "进入计划模式。计划模式下只允许使用只读工具（Read, Glob, Grep 等）来探索代码库并设计实施方案。",
		Input:       struct{}{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in struct{}) (string, error) {
			planFile, err := store.Enter()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已进入计划模式。计划文件: %s", planFile), nil
		},
	}
}

func ExitPlanModeTool(store plan.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "退出计划模式，提交计划供用户审批。",
		Input:       struct{}{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in struct{}) (string, error) {
			content, err := store.Exit()
			if err != nil {
				return "", err
			}
			if content == "" {
				return "已退出计划模式（无计划内容）。", nil
			}
			return fmt.Sprintf("已退出计划模式。计划内容:\n%s", content), nil
		},
	}
}

// ---------- Skill 工具 ----------

type skillInput struct {
	Skill string `json:"skill" desc:"技能名称" required:"true"`
	Args  string `json:"args,omitempty" desc:"传递给技能的参数"`
}

func SkillTool(reg *skill.Registry) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "执行一个已注册的技能（slash command）。",
		Input:       skillInput{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in skillInput) (string, error) {
			result, err := reg.Execute(in.Skill, in.Args)
			if err != nil {
				return "", err
			}
			return result, nil
		},
	}
}

// ---------- Cron 工具 ----------

type cronCreateInput struct {
	Cron      string `json:"cron" desc:"标准 5-field cron 表达式: 分 时 日 月 周" required:"true"`
	Prompt    string `json:"prompt" desc:"触发时执行的 prompt" required:"true"`
	Recurring *bool  `json:"recurring,omitempty" desc:"是否循环（默认 true）"`
}

func CronCreateTool(sched *cron.Scheduler) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "创建一个定时任务。使用标准 5-field cron 表达式。",
		Input:       cronCreateInput{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in cronCreateInput) (string, error) {
			recurring := true
			if in.Recurring != nil {
				recurring = *in.Recurring
			}
			job, err := sched.Create(in.Cron, in.Prompt, recurring)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已创建定时任务 %s (cron: %s, 循环: %v)", job.ID, in.Cron, recurring), nil
		},
	}
}

type cronDeleteInput struct {
	ID string `json:"id" desc:"定时任务 ID" required:"true"`
}

func CronDeleteTool(sched *cron.Scheduler) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "删除一个定时任务。",
		Input:       cronDeleteInput{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in cronDeleteInput) (string, error) {
			if err := sched.Delete(in.ID); err != nil {
				return "", err
			}
			return fmt.Sprintf("已删除定时任务 %s", in.ID), nil
		},
	}
}

func CronListTool(sched *cron.Scheduler) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "列出所有定时任务。",
		Input:       struct{}{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in struct{}) (string, error) {
			jobs := sched.List()
			if len(jobs) == 0 {
				return "(无定时任务)", nil
			}
			var lines []string
			for _, j := range jobs {
				lines = append(lines, fmt.Sprintf("%s: cron=%s prompt=%q recurring=%v",
					j.ID, j.Cron, j.Prompt, j.Recurring))
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

// ---------- Worktree 工具 ----------

type enterWorktreeInput struct {
	Name string `json:"name,omitempty" desc:"worktree 名称，留空自动生成"`
}

func EnterWorktreeTool(mgr *worktree.Manager) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "创建一个 git worktree 并切换到其中工作。提供代码隔离环境。",
		Input:       enterWorktreeInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in enterWorktreeInput) (string, error) {
			wt, err := mgr.Enter(in.Name)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已进入 worktree\n路径: %s\n分支: %s", wt.Path, wt.Branch), nil
		},
	}
}

type exitWorktreeInput struct {
	Action         string `json:"action" desc:"keep（保留）或 remove（删除）" required:"true"`
	DiscardChanges bool   `json:"discard_changes,omitempty" desc:"强制删除有未提交更改的 worktree"`
}

func ExitWorktreeTool(mgr *worktree.Manager) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "退出当前 worktree。action=keep 保留文件，action=remove 删除 worktree 和分支。",
		Input:       exitWorktreeInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in exitWorktreeInput) (string, error) {
			err := mgr.Exit(in.Action, in.DiscardChanges)
			if err != nil {
				return "", err
			}
			if in.Action == "keep" {
				return "已退出 worktree（保留）", nil
			}
			return "已退出 worktree（已删除）", nil
		},
	}
}

// ---------- 后台任务工具 ----------

type taskStopInput struct {
	TaskID string `json:"task_id" desc:"要停止的后台任务 ID" required:"true"`
}

// TaskStopTool 返回 TaskStop 工具定义。
func TaskStopTool(store bgtask.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "停止一个正在运行的后台任务（agent 或 shell）。",
		Input:       taskStopInput{},
		Permission:  goagent.Normal,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in taskStopInput) (string, error) {
			return store.ExecuteStop(mustMarshal(in))
		},
	}
}

type taskOutputInput struct {
	TaskID  string `json:"task_id" desc:"后台任务 ID" required:"true"`
	Block   *bool  `json:"block,omitempty" desc:"是否等待任务完成（默认 true）"`
	Timeout *int   `json:"timeout,omitempty" desc:"最大等待时间（毫秒，默认 30000）"`
}

// TaskOutputTool 返回 TaskOutput 工具定义。
func TaskOutputTool(store bgtask.StoreInterface) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "获取后台任务的输出。block=true 时阻塞等待完成。",
		Input:       taskOutputInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in taskOutputInput) (string, error) {
			return store.ExecuteOutput(mustMarshal(in))
		},
	}
}

// mustMarshal 将任意值序列化为 JSON。
func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
