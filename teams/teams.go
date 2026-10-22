// Package teams 实现多 agent 团队协作运行时（GoAgent Teams L1）。
//
// 模型：leader（队长）+ members（成员）。leader 的工具面板里，每个成员
// 注入为一个 assign 工具（并发安全）——leader 发起调用即把任务分派给该
// 成员；成员是轻量 loop（角色卡 + 工具子集 + 微/自动压缩），其
// thinking/tool/progress 事件经 EmitFromTool 以 subagent_progress 透出
//（前端实时可见），最终产出作为 tool 结果交回 leader，由 leader 汇总后
// 向用户报告。
//
// 与 pipeline 的分工（见 YCode docs/pipeline-teams-plan.md 判据）：
// pipeline 承载同构数据项的变换流（map/reduce）；teams 承载需要探索决策、
// 成员间协作的目标导向任务。两者可嵌套：team lead 对同构子任务建 pipeline。
//
// L1 边界：成员按分派同步执行（无自主循环）、无成员间点对点通信、
// 无跨 run 常驻记忆。这些是 L2/L3 的内容。
package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

// MemberRuntime 一个成员（leader 或 member）的可运行描述。
type MemberRuntime struct {
	// Name 成员唯一标识（leader 固定为 "leader"；成员限 [a-z][a-z0-9_]{0,31}）。
	Name string
	// Role 角色卡：身份/职责/输出规范（作为成员 system prompt）。
	Role string
	// Tools 成员可用工具（宿主从注册表解析后的运行条目）。
	Tools []loop.ToolEntry
	// MaxTurns 单任务最大轮次（默认 40）。
	MaxTurns int
	// Provider 必填（宿主按成员/团队填充）。
	Provider provider.Provider
}

// Event 团队事件（带成员归属，宿主透出到 UI）。
type Event struct {
	Member   string
	Type     string // thinking / tool / progress / error
	Text     string
	ToolName string
}

// Config 团队运行配置。
type Config struct {
	Leader  MemberRuntime
	Members []MemberRuntime
	// SharedContext 团队共享背景（注入 leader 与所有成员 prompt）。
	SharedContext string
	// OnEvent 团队事件回调（成员归属的事件流）。
	OnEvent func(Event)
}

var memberNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Run 运行团队：leader 以 task 为主任务启动循环，通过分派工具驱动成员；
// leader 的最终文本即向用户的汇报。在某次 agent 运行之中调用时（ctx 来自
// 该运行的工具执行），队长与成员的循环继承该运行的作用域（见
// loop.NewChild）：每次工具调用按工具声明的权限级别过同一条检查链，观测
// 数据归属同一会话。
func Run(ctx context.Context, cfg Config, task string) (string, error) {
	if err := validate(cfg); err != nil {
		return "", err
	}

	// leader 工具 = 声明工具 + 每成员一个分派工具（并发安全：一轮可同时
	// 分派多个成员，成员互不阻塞地并行执行）。
	leaderTools := make([]loop.ToolEntry, 0, len(cfg.Leader.Tools)+len(cfg.Members))
	leaderTools = append(leaderTools, cfg.Leader.Tools...)
	for _, m := range cfg.Members {
		member := m
		leaderTools = append(leaderTools, loop.ToolEntry{
			Name:        "assign_" + m.Name,
			Description: fmt.Sprintf("把任务分派给成员 %s（职责：%s）。任务说明必须自包含：目标、上下文、产出格式。成员完成后其产出作为本工具结果返回。", m.Name, firstLine(m.Role)),
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{"type": "string", "description": "分派给成员的任务说明（自包含）"},
				},
				"required": []string{"task"},
			},
			// 分派本身没有副作用；成员的每次工具调用各自过检查链。
			Permission: permission.LevelReadOnly,
			Concurrent: true,
			ExecuteFn: func(mctx context.Context, input json.RawMessage) (string, error) {
				var args struct {
					Task string `json:"task"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return "", fmt.Errorf("分派参数解析失败: %w", err)
				}
				return runMember(mctx, cfg, member, args.Task)
			},
		})
	}

	leader := cfg.Leader
	leader.Tools = leaderTools
	l := buildLoop(ctx, leader, cfg)

	var final strings.Builder
	var lastErr error
	for ev := range l.Run(ctx, task) {
		switch ev.Type {
		case loop.EvtTextDelta:
			final.WriteString(ev.Text)
			notify(cfg, "leader", "text", ev.Text)
		case loop.EvtThinking:
			notify(cfg, "leader", "thinking", ev.Thinking)
		case loop.EvtToolStart:
			notify(cfg, "leader", "tool", ev.ToolName)
		case loop.EvtError:
			lastErr = ev.Error
			notify(cfg, "leader", "error", fmt.Sprint(ev.Error))
		}
	}
	return finish(ctx, "队长", final.String(), lastErr)
}

// finish 统一运行结果：被取消时返回 ctx 的错误；模型报错且没有任何产出时
// 返回该错误（此前错误被吞掉，调用方拿到一个空字符串当作成功）。
func finish(ctx context.Context, who, text string, lastErr error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if lastErr != nil && strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s执行失败: %w", who, lastErr)
	}
	return text, nil
}

// runMember 运行单个成员的轻量循环（角色卡 + 共享背景 + 微/自动压缩）。
func runMember(ctx context.Context, cfg Config, member MemberRuntime, task string) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("任务说明不能为空")
	}
	l := buildLoop(ctx, member, cfg)

	var final strings.Builder
	var lastErr error
	for ev := range l.Run(ctx, task) {
		switch ev.Type {
		case loop.EvtThinking:
			notify(cfg, member.Name, "thinking", ev.Thinking)
		case loop.EvtProgress:
			notify(cfg, member.Name, "progress", ev.Text)
		case loop.EvtToolStart:
			notify(cfg, member.Name, "tool", ev.ToolName)
		case loop.EvtToolDone:
			preview := ev.ToolResult
			if len(preview) > 300 {
				preview = preview[:300] + "…"
			}
			notify(cfg, member.Name, "tool_done", preview)
		case loop.EvtError:
			lastErr = ev.Error
			notify(cfg, member.Name, "error", fmt.Sprint(ev.Error))
		case loop.EvtTextDelta:
			final.WriteString(ev.Text)
		}
	}
	return finish(ctx, "成员 "+member.Name+" ", final.String(), lastErr)
}

// notify 团队事件透出（OnEvent 回调 + 主对话流 subagent_progress 双通道）。
func notify(cfg Config, member, typ, text string) {
	if cfg.OnEvent != nil {
		cfg.OnEvent(Event{Member: member, Type: typ, Text: text})
	}
}

// buildLoop 构建成员/队长的轻量循环：角色卡 + 共享背景 + 微/自动压缩，
// 检查链与观测出口继承 ctx 所在的运行。微压缩裁剪过期工具结果（长任务
// 质量/成本关键）；图片结果由压缩层的统一回收策略保护，不会被截断损坏。
func buildLoop(ctx context.Context, m MemberRuntime, cfg Config) *loop.Loop {
	shared := cfg.SharedContext
	prov, maxTurns := resolve(m)

	compCfg := compaction.Config{
		AutoCompactThreshold: 0.8,
		Layers:               []compaction.Layer{compaction.LayerMicro, compaction.LayerAuto},
	}
	compMgr := compaction.NewManager(compCfg)
	compMgr.SetProvider(prov)

	sys := m.Role
	if shared != "" {
		sys += "\n\n[团队共享背景]\n" + shared
	}

	return loop.NewChild(ctx, loop.Config{
		Provider:     prov,
		Tools:        m.Tools,
		SystemPrompt: sys,
		MaxTurns:     maxTurns,
		Compaction:   compMgr,
	})
}

func resolve(m MemberRuntime) (provider.Provider, int) {
	prov := m.Provider
	maxTurns := m.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 40
	}
	return prov, maxTurns
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func validate(cfg Config) error {
	if strings.TrimSpace(cfg.Leader.Role) == "" {
		return fmt.Errorf("队长缺少角色定义")
	}
	if cfg.Leader.Provider == nil {
		return fmt.Errorf("队长缺少 provider")
	}
	seen := map[string]bool{"leader": true}
	for i, m := range cfg.Members {
		if !memberNameRe.MatchString(m.Name) {
			return fmt.Errorf("成员 %d 名字 %q 不合法（限 [a-z][a-z0-9_]，≤32 字符）", i, m.Name)
		}
		if seen[m.Name] {
			return fmt.Errorf("成员名 %q 重复", m.Name)
		}
		seen[m.Name] = true
		if strings.TrimSpace(m.Role) == "" {
			return fmt.Errorf("成员 %s 缺少角色定义", m.Name)
		}
		if m.Provider == nil {
			return fmt.Errorf("成员 %s 缺少 provider", m.Name)
		}
	}
	return nil
}
