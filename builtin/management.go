package builtin

// 本文件定义框架内置的管理工具 ToolDef。
// 对应 Claude Code 的 Task/Plan/Skill/Cron/Worktree 工具。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/cron"
	"github.com/Dream355873200/GoAgent/plan"
	"github.com/Dream355873200/GoAgent/skill"
	"github.com/Dream355873200/GoAgent/task"
	"github.com/Dream355873200/GoAgent/worktree"
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
			// 问题记录：与 task 同存储（metadata.kind=issue），测试发现缺陷时记录、修复后关闭
			goagent.NamedTool{Name: "IssueReport", Def: IssueReportTool(deps.TaskStore)},
			goagent.NamedTool{Name: "IssueResolve", Def: IssueResolveTool(deps.TaskStore)},
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
			t := storeFor(ctx, store).Create(in.Subject, in.Description, "", nil)
			return fmt.Sprintf("已创建任务 #%s: %s", t.ID, t.Subject), nil
		},
	}
}

// storeFor 按会话路由任务存储：store 实现了 SessionRouter（如
// task.SessionStore）时取当前会话的分区——task 跟随 session，不同会话
// 的任务列表互不可见；普通全局存储原样返回（旧行为）。
func storeFor(ctx goagent.Context, store task.StoreInterface) task.StoreInterface {
	if r, ok := store.(task.SessionRouter); ok {
		return r.ForSession(ctx.SessionID)
	}
	return store
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
			t, err := storeFor(ctx, store).Update(in.TaskID, patch)
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
			t := storeFor(ctx, store).Get(in.TaskID)
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
			summaries := storeFor(ctx, store).ListSummaries()
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
	// Skill 名称；特殊值 "list" 列出全部可用 skill（用户问「你有什么技能」时用它）
	Skill string `json:"skill" desc:"技能名称（传 list 可列出全部可用技能）" required:"true"`
	Args  string `json:"args,omitempty" desc:"传递给技能的参数"`
}

// skillDescOf 取 skill 的描述行：注册表提取的 Description，为空时
// 从 Content 现场提取首行非空文本（Register 手动注册的 skill 没有 Description）。
func skillDescOf(s *skill.Skill) string {
	if d := strings.TrimSpace(s.Description); d != "" {
		return d
	}
	if d := extractSkillDesc(s.Content); d != "" {
		return d
	}
	return s.FilePath
}

// skillToolDescription 动态构建描述：内嵌当前可用 skill 清单
// （名称 + 一行描述），让模型在决策时直接看得到有哪些技能可用——
// 否则模型不知道技能存在，永远不会调用。
func skillToolDescription(reg *skill.Registry) string {
	var sb strings.Builder
	sb.WriteString("执行一个已注册的技能（slash command）——技能是领域工作流的完整操作指南（如测试策略、页面生成）。")
	if reg != nil {
		if skills := reg.List(); len(skills) > 0 {
			sb.WriteString(" 可用技能:\n")
			for _, s := range skills {
				if s.Source == skill.SourceBuiltin {
					continue // 内置的不占篇幅
				}
				fmt.Fprintf(&sb, "- %s: %s\n", s.Name, skillDescOf(s))
			}
		}
	}
	sb.WriteString("不确定有哪些技能时传 skill=list 列出。")
	return sb.String()
}

func SkillTool(reg *skill.Registry) goagent.ToolDef {
	return goagent.ToolDef{
		Description: skillToolDescription(reg),
		Input:       skillInput{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in skillInput) (string, error) {
			// list：合并项目目录（会话工作目录）+ 全局注册表的清单。
			if in.Skill == "list" {
				return listSkills(ctx.WorkDir, reg), nil
			}
			// 项目级 skill 优先（对齐 Claude Code：项目 commands 覆盖用户级）。
			// 按会话工作目录现场读取——单进程多会话各自的项目 commands 互不串；
			// 项目没有该 skill 时回落到注册表（全局目录预扫描的结果）。
			if content, ok := readProjectSkill(ctx.WorkDir, in.Skill, in.Args); ok {
				return content, nil
			}
			result, err := reg.Execute(in.Skill, in.Args)
			if err != nil {
				// 报错里带上可用清单：模型能立即自我纠正（打错名 → 看到对的）
				return "", fmt.Errorf("%w。可用技能:\n%s", err, listSkills(ctx.WorkDir, reg))
			}
			return result, nil
		},
	}
}

// listSkills 列出会话可用的全部 skill：项目 .yume/commands/（现场扫描，
// 新建文件立即生效）+ 全局注册表。同名时项目版覆盖全局。
func listSkills(workDir string, reg *skill.Registry) string {
	merged := map[string]string{} // name → 描述行
	if reg != nil {
		for _, s := range reg.List() {
			if s.Source == skill.SourceBuiltin {
				continue
			}
			merged[s.Name] = "- " + s.Name + ": " + skillDescOf(s) + "（全局）"
		}
	}
	// 项目目录现场扫描（注册表预扫描可能滞后）
	if workDir != "" {
		if entries, err := os.ReadDir(filepath.Join(workDir, ".yume", "commands")); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".md")
				if name == "" || strings.ContainsAny(name, `/\`) {
					continue
				}
				data, err := os.ReadFile(filepath.Join(workDir, ".yume", "commands", e.Name()))
				if err != nil {
					continue
				}
				merged[name] = "- " + name + ": " + extractSkillDesc(string(data)) + "（项目）"
			}
		}
	}
	if len(merged) == 0 {
		return "（无可用技能）"
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(merged[n])
		sb.WriteString("\n")
	}
	return sb.String()
}

// extractSkillDesc 取内容第一行非空非标题文本作描述（截断 100 字符）。
func extractSkillDesc(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 100 {
			return line[:100] + "..."
		}
		return line
	}
	return ""
}

// readProjectSkill 读取会话项目目录下的 .yume/commands/<name>.md。
// 文件不存在返回 ok=false；名称做路径消毒（防 "../" 逃逸）。
func readProjectSkill(workDir, name, args string) (string, bool) {
	if workDir == "" || name == "" || strings.ContainsAny(name, `/\`) || name == ".." || name == "." {
		return "", false
	}
	path := filepath.Join(workDir, ".yume", "commands", name+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.ReplaceAll(string(data), "$ARGUMENTS", args), true
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
