package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dream355873200/GoAgent/internal/loop"
)

// ---------------------------------------------------------------------------
// 动态 Pipeline：JSON DSL → PipelineConfig 构建器 + create_pipeline 工具
//
// 让 LLM 在运行时自建编排（TODO「动态 Pipeline」L1 受限模式）：
// agent 拿到大任务 → 现场拆解成 DAG → 从宿主预注册工具箱按名选工具 →
// 执行 → 汇报。不是把 PipelineConfig 整个交给 LLM——闭包部分（回调、
// 事务）它生成不了，只能生成声明式描述，由本构建器解释执行。
//
// L1 范围（本文件）：
//   - 工具按名从 App 注册表解析（LLM 不能发明工具，只能选）
//   - 消息流固定 string（MessageType 不开放）
//   - 无事务、无 OnResult 回调
//   - DAG 校验：无环 / 工具存在 / 节点数与 instruction 上限
//   - 节点失败即终止（重规划是 L2，见 README TODO）
// ---------------------------------------------------------------------------

// DynPipelineSpec 是 LLM 生成的动态 pipeline 声明（JSON DSL）。
type DynPipelineSpec struct {
	// Nodes DAG 节点。至少 1 个，至多 maxDynPipelineNodes 个。
	Nodes []DynNodeSpec `json:"nodes" desc:"DAG 节点列表，按依赖关系组织"`
}

// pushTaskInput push_task 工具的输入。
type pushTaskInput struct {
	To      string `json:"to" desc:"下游节点名（必须是该节点 Injects 声明的目标）"`
	Content string `json:"content" desc:"要推送的产出内容（Markdown）"`
}

// DynNodeSpec 单个节点的声明。
type DynNodeSpec struct {
	// Name 节点唯一标识（DAG 内唯一，被 DependsOn/Injects 引用）。
	Name string `json:"name" desc:"节点唯一标识" required:"true"`
	// Instruction 节点 agent 的 system prompt：角色 + 任务 + 产出要求。
	Instruction string `json:"instruction" desc:"节点 agent 的系统提示词（角色+任务+产出要求）" required:"true"`
	// Tools 此节点可用的工具名列表（从宿主工具箱按名选，不能发明）。
	Tools []string `json:"tools" desc:"节点可用工具名列表（只能从可用工具清单里选）"`
	// Message 初始任务消息（仅无上游生产者的节点作为首条任务 Push）。
	Message string `json:"message" desc:"节点的初始任务消息（无上游输入的节点必填）"`
	// DependsOn 依赖节点名列表（全部完成后本节点才启动）。
	DependsOn []string `json:"depends_on,omitempty" desc:"依赖的节点名列表"`
	// Injects 可推送队列的下游节点名列表（数据流动方向）。
	Injects []string `json:"injects,omitempty" desc:"可向哪些下游节点推送任务"`
	// Concurrency 并发 worker 数（默认 1）。
	Concurrency int `json:"concurrency,omitempty" desc:"并发 worker 数，默认 1"`
	// Output 向下游声明本节点产出的数据格式约定（如 "Markdown，含 小节标题/要点列表"）。
	// 会拼接进本节点 instruction 尾部（约束产出），并注入下游 instruction
	//（告知消费格式）——MD 载体约定的机制化。
	Output string `json:"output,omitempty" desc:"本节点产出格式约定（Markdown 描述）"`
}

// 动态 pipeline 的规模护栏：LLM 建图易犯的错不是拓扑错误，是拆太碎——
// 每个节点都是一次 LLM 调用，5 个节点的串行链成本可能远超一个 SubAgent 直接干。
const (
	maxDynPipelineNodes = 12
	maxDynInstruction   = 8000
)

// BuildDynPipeline 把 LLM 生成的 spec 解释为 PipelineConfig。
// 工具按名从 app 注册表解析；每节点构造独立 agent def。
// 校验失败返回可读错误（供 LLM 自我纠正后重试）。
func (a *App) BuildDynPipeline(spec *DynPipelineSpec) (PipelineConfig, error) {
	if spec == nil || len(spec.Nodes) == 0 {
		return PipelineConfig{}, fmt.Errorf("nodes 不能为空")
	}
	if len(spec.Nodes) > maxDynPipelineNodes {
		return PipelineConfig{}, fmt.Errorf("节点数 %d 超过上限 %d（任务该拆得更粗，或改用子 agent 直接处理）",
			len(spec.Nodes), maxDynPipelineNodes)
	}

	// 节点名唯一性
	seen := make(map[string]bool, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.Name == "" {
			return PipelineConfig{}, fmt.Errorf("存在 name 为空的节点")
		}
		if seen[n.Name] {
			return PipelineConfig{}, fmt.Errorf("节点名 %q 重复", n.Name)
		}
		seen[n.Name] = true
	}

	// 工具存在性（先收集所有缺失名，一次报全——LLM 一次修正）
	a.mu.RLock()
	missing := map[string][]string{} // node → missing tools
	for _, n := range spec.Nodes {
		for _, tn := range n.Tools {
			// push_task 由框架自动注入（见 BuildDynPipeline），不参与校验。
			if tn == "push_task" {
				continue
			}
			if _, ok := a.tools[tn]; !ok {
				missing[n.Name] = append(missing[n.Name], tn)
			}
		}
	}
	a.mu.RUnlock()
	if len(missing) > 0 {
		var sb strings.Builder
		sb.WriteString("以下工具不存在（只能从宿主已注册工具里选，可用工具: ")
		sb.WriteString(strings.Join(a.ToolNames(), ", "))
		sb.WriteString("）: ")
		first := true
		for node, tools := range missing {
			if !first {
				sb.WriteString("; ")
			}
			fmt.Fprintf(&sb, "节点 %s → %s", node, strings.Join(tools, ", "))
			first = false
		}
		return PipelineConfig{}, fmt.Errorf("%s", sb.String())
	}

	// DependsOn / Injects 引用存在性
	for _, n := range spec.Nodes {
		for _, dep := range n.DependsOn {
			if !seen[dep] {
				return PipelineConfig{}, fmt.Errorf("节点 %q 的 depends_on 引用了不存在的节点 %q", n.Name, dep)
			}
		}
		for _, inj := range n.Injects {
			if !seen[inj] {
				return PipelineConfig{}, fmt.Errorf("节点 %q 的 injects 引用了不存在的节点 %q", n.Name, inj)
			}
		}
	}

	// 消息与 instruction 上限
	for _, n := range spec.Nodes {
		if len(n.Instruction) > maxDynInstruction {
			return PipelineConfig{}, fmt.Errorf("节点 %q 的 instruction 超过 %d 字符", n.Name, maxDynInstruction)
		}
	}

	// 构造 PipelineConfig：工具按名解析为 NamedTool。
	// 注意：多个节点选同一个工具是合法的（共享 def，工具本身无状态）。
	cfg := PipelineConfig{Nodes: make([]PipelineNode, 0, len(spec.Nodes))}
	for _, n := range spec.Nodes {
		// MD 载体约定：output 字段拼接进本节点 instruction（约束产出格式），
		// 并追加到所有直接下游的 instruction（告知消费格式）。
		instruction := n.Instruction
		downstreamNote := ""
		if n.Output != "" {
			instruction += "\n\n[产出格式约定] 你的产出将以 " + n.Output +
				" 格式交给下游节点，严格遵循该格式。"
			downstreamNote = "\n\n[输入格式约定] 上游 " + n.Name +
				" 的产出格式为 " + n.Output + "，按该约定消费。"
		}
		node := PipelineNode{
			Name:      n.Name,
			DependsOn: n.DependsOn,
			Injects:   n.Injects,
			Message:   n.Message,
			Agent: &PipelineAgentDef{
				Name:        n.Name,
				Instruction: instruction,
			},
		}
		if n.Concurrency > 0 {
			node.Concurrency = n.Concurrency
		}
		if len(n.Tools) > 0 {
			a.mu.RLock()
			for _, tn := range n.Tools {
				if rt, ok := a.tools[tn]; ok {
					node.Agent.Tools = append(node.Agent.Tools, NamedTool{Name: tn, Def: rt.def})
				}
			}
			a.mu.RUnlock()
		}
		// 下游交付工具：带 Injects 的节点自动注入 push_task——此前只有描述
		// 没有工具，节点 agent 根本无法向下游队列推数据（队列传输断路）。
		if len(n.Injects) > 0 {
			node.Agent.Tools = append(node.Agent.Tools, NamedTool{
				Name: "push_task",
				Def: ToolDef{
					// to 的合法值在描述里逐个列出（模型无法凭空猜下游节点名）。
					Description: "把一份产出推送给下游节点队列（MD 片段）。每个下游产出一项调一次。" +
						"to 的合法值: " + strings.Join(n.Injects, " / "),
					Input:      pushTaskInput{},
					Permission: ReadOnly,
					Concurrent: true,
					Execute: func(ctx Context, in pushTaskInput) (string, error) {
						mp := GetMessageQueue(ctx, in.To)
						if mp == nil {
							return "", fmt.Errorf("下游节点 %q 不可推送（未声明 Injects 或队列已关闭）", in.To)
						}
						mp.Push(in.Content)
						return fmt.Sprintf("已推送给 %s（%d 字符）", in.To, len(in.Content)), nil
					},
				},
			})
			instruction += "\n\n[下游交付] 使用 push_task 工具，把你的每份产出按「" + n.Output +
				"」格式逐条推送给下游节点（多次调用）；全部推完后结束。"
			node.Agent.Instruction = instruction
			for _, dn := range n.Injects {
				for k := range cfg.Nodes {
					if cfg.Nodes[k].Name == dn && cfg.Nodes[k].Agent != nil {
						cfg.Nodes[k].Agent.Instruction += "\n\n[输入格式约定] 上游 " + n.Name +
							" 的产出格式为 " + n.Output + "，按该约定消费。"
					}
				}
			}
		}
		cfg.Nodes = append(cfg.Nodes, node)
		if false {
			for _, dn := range n.Injects {
				for k := range cfg.Nodes {
					if cfg.Nodes[k].Name == dn && cfg.Nodes[k].Agent != nil {
						cfg.Nodes[k].Agent.Instruction += downstreamNote
					}
				}
			}
		}
	}

	// DAG 无环校验：复用 topoSort（在 newPipeline 里做），这里先做一次
	// 快速前置检查，把错误翻译成 LLM 可读的形式。
	if err := a.checkDynAcyclic(spec); err != nil {
		return PipelineConfig{}, err
	}

	return cfg, nil
}

// checkDynAcyclic DFS 检查 DAG 无环。环存在时报出环上的节点序列，
// 让 LLM 看得懂哪里循环了。
func (a *App) checkDynAcyclic(spec *DynPipelineSpec) error {
	// 邻接表
	adj := make(map[string][]string, len(spec.Nodes))
	for _, n := range spec.Nodes {
		adj[n.Name] = n.DependsOn
	}
	// 三色标记 DFS
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(spec.Nodes))
	var path []string

	var visit func(name string) bool // true = 发现环
	visit = func(name string) bool {
		color[name] = gray
		path = append(path, name)
		for _, dep := range adj[name] {
			switch color[dep] {
			case gray:
				// 找到环：dep 已在当前路径上
				path = append(path, dep)
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		color[name] = black
		return false
	}

	for _, n := range spec.Nodes {
		if color[n.Name] == white {
			path = nil
			if visit(n.Name) {
				return fmt.Errorf("DAG 存在循环依赖: %s（请调整 depends_on 打破循环）", strings.Join(path, " → "))
			}
		}
	}
	return nil
}

// RunDynPipeline 构建 + 执行动态 pipeline（同步阻塞直到完成或失败）。
// 供宿主在工具 Execute 里调用；OnEvent（若设置）透出节点事件。
func (a *App) RunDynPipeline(ctx context.Context, spec *DynPipelineSpec, onEvent func(ev PipelineEvent)) error {
	cfg, err := a.BuildDynPipeline(spec)
	if err != nil {
		return err
	}
	cfg.OnEvent = onEvent
	a.UsePipeline(cfg)
	return a.RunPipeline(ctx)
}

// CreatePipelineInput 是 create_pipeline 工具的输入。
type CreatePipelineInput struct {
	// Spec 是动态 pipeline 的 JSON 声明。
	Spec json.RawMessage `json:"spec" desc:"pipeline 声明 JSON：{\"nodes\":[{name,instruction,tools,message,depends_on,injects,concurrency}]}" required:"true"`
}

// CreatePipelineTool 返回 create_pipeline 工具：LLM 提交 spec → 构建校验 →
// 执行 → 返回结果摘要。校验失败返回可读错误让 LLM 修正后重试。
//
// 工具描述内嵌当前可用工具清单（模型看得见才知道能选什么——对齐 SkillTool
// 的清单内嵌做法）。
func (a *App) CreatePipelineTool() NamedTool {
	desc := "构建并执行一个动态 DAG 流水线（大任务拆解成多个节点并行/串行执行）。" +
		"节点间数据流：上游节点用 push_task 类工具向 Injects 声明的下游队列推任务。" +
		"只适合依赖关系【任务固有】的多阶段任务（如先提取→再润色→再审核）；" +
		"依赖关系需要你自己现场猜的任务请直接自己做或用子 agent。" +
		"数据载体约定：队列任务与节点产出默认用 Markdown 片段（LLM-to-LLM），" +
		"仅在下游是代码/需要 UI 渲染时用 JSON；每个节点的 output 字段向下游声明" +
		"自己的产出格式，下游按该约定消费。" +
		"校验失败会返回具体原因，修正 spec 后可重试。"
	if names := a.ToolNames(); len(names) > 0 {
		desc += "\n可用工具（节点的 tools 只能从这里选）: " + strings.Join(names, ", ")
	}
	return NamedTool{
		Name: "create_pipeline",
		Def: ToolDef{
			Description: desc,
			Input:       CreatePipelineInput{},
			Permission:  Normal,
			Execute: func(ctx Context, in CreatePipelineInput) (string, error) {
				var spec DynPipelineSpec
				if err := json.Unmarshal(in.Spec, &spec); err != nil {
					return "", fmt.Errorf("spec JSON 解析失败: %w", err)
				}
				// 节点生命周期事件经 subagent_progress 透出到主对话流的
				// create_pipeline 工具卡（前端实时渲染节点进度）。
				onEvent := func(ev PipelineEvent) {
					status := "running"
					text := ev.Text
					switch ev.Type {
					case "node_started":
						text = "▶ " + ev.Node
					case "node_done":
						text, status = "✓ "+ev.Node, "done"
					}
					if text == "" {
						return
					}
					loop.EmitFromTool(ctx, loop.Event{
						Type:          loop.EvtSubAgentProgress,
						AgentID:       ev.Node,
						AgentDesc:     ev.Node,
						AgentStatus:   status,
						AgentActivity: text,
					})
				}
				if err := a.RunDynPipeline(ctx, &spec, onEvent); err != nil {
					return "", err
				}
				return fmt.Sprintf("pipeline 执行完成（%d 个节点）", len(spec.Nodes)), nil
			},
		},
	}
}
