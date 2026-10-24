package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/internal/safename"
	"github.com/Dream355873200/GoAgent/statedir"
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
//   - 节点失败即终止（重规划是 L2，见 design/BACKLOG.md）
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
	// 动态 DSL 契约：节点完全未声明 depends_on 时，自动等其全部上游
	// 生产者（向它 injects 的节点）完成——LLM 缺省扇入依赖时的安全缺省；
	// 显式声明了（哪怕只写一个）则完全按声明（可做提前启动的流式消费）。
	// 精确正交的调度/队列语义请直接用 PipelineConfig。
	DependsOn []string `json:"depends_on,omitempty" desc:"依赖的节点名列表（未声明时自动等待所有上游 injects 生产者完成）"`
	// Injects 可推送队列的下游节点名列表（数据流动方向）。
	// 交付义务：每个声明的下游必须至少推送一条任务（无产出也要推送说明），
	// 构建器会把该义务写进节点 instruction。
	Injects []string `json:"injects,omitempty" desc:"可向哪些下游节点推送任务（每个下游至少推送一条）"`
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

// PipelineNodeEvent 是动态 pipeline 节点生命周期的自定义事件名
// （custom 帧；值 {node, status, text}）。与 subagent_progress 并行下发，
// 宿主可不依赖 agent_id=node 约定直接消费类型化状态。
const PipelineNodeEvent = "goagent.pipeline_node"

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

	// 节点名唯一性。节点名会成为轨迹文件名与运行快照的键，先做文件名安全校验。
	seen := make(map[string]bool, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.Name == "" {
			return PipelineConfig{}, fmt.Errorf("存在 name 为空的节点")
		}
		if err := safename.Check("节点名", n.Name); err != nil {
			return PipelineConfig{}, fmt.Errorf("%w（节点名请只用字母、数字、下划线或中文）", err)
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
	// 先收集「下游节点 → 上游生产者」映射：injects 蕴含调度依赖（下游必须
	// 等所有上游完成）——否则扇入节点没写 depends_on 就直接开跑，无输入
	// 空转；上游漏推的下游空队列秒「完成」（实测踩坑）。
	producersOf := map[string][]string{}
	for _, n := range spec.Nodes {
		for _, inj := range n.Injects {
			producersOf[inj] = append(producersOf[inj], n.Name)
		}
	}
	cfg := PipelineConfig{Nodes: make([]PipelineNode, 0, len(spec.Nodes))}
	for _, n := range spec.Nodes {
		// MD 载体约定：output 字段拼接进本节点 instruction（约束产出格式），
		// 并追加到所有直接下游的 instruction（告知消费格式）。
		instruction := n.Instruction
		if n.Output != "" {
			instruction += "\n\n[产出格式约定] 你的产出将以 " + n.Output +
				" 格式交给下游节点，严格遵循该格式。"
		}
		// injects 蕴含调度依赖——但只补「完全没写 depends_on」的下游：
		// 模型显式声明了依赖（哪怕只写一个）就是调度意图，尊重之；一个没写
		// 才按直觉「等所有上游生产者完成」。全量并集会毁掉显式的流式消费
		// 意图（提前启动边生产边消费），完全不蕴含又让扇入节点无输入空转。
		deps := append([]string{}, n.DependsOn...)
		if len(deps) == 0 {
			deps = append(deps, producersOf[n.Name]...)
		}
		node := PipelineNode{
			Name:      n.Name,
			DependsOn: deps,
			Injects:   n.Injects,
			Message:   n.Message,
			Agent: &PipelineAgentDef{
				Name:        n.Name,
				Instruction: instruction,
			},
		}
		// 根节点（无上游生产者）的 message 省略时，以 instruction 作为任务
		// 种子——否则队列空转：节点不跑一次 LLM 就「完成」，整条流水线
		// 瞬间假完成（实测踩坑：LLM 提交的 spec 普遍省略 message）。
		hasUpstream := len(producersOf[n.Name]) > 0
		if node.Message == "" && !hasUpstream {
			node.Message = n.Instruction
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
				"」格式逐条推送给下游节点（多次调用）。交付义务：以下每个下游节点" +
				"你都必须至少推送一条——确实没有产出的，也要推送一条简要说明，" +
				"否则该下游会空等。全部推完后结束。下游清单: " + strings.Join(n.Injects, " / ") + "。"
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
	return a.RunDynPipelineTrace(ctx, spec, onEvent, "")
}

// RunDynPipelineTrace 同 RunDynPipeline，另把节点运行轨迹（思考/工具/文本）
// 以 JSONL 追加写入 traceDir（每节点一个 <node>.jsonl）——宿主据此提供
// 节点的只读工作过程时间线（对齐主 agent 的会话回放）。
func (a *App) RunDynPipelineTrace(ctx context.Context, spec *DynPipelineSpec, onEvent func(ev PipelineEvent), traceDir string) error {
	cfg, err := a.BuildDynPipeline(spec)
	if err != nil {
		return err
	}
	cfg.OnEvent = onEvent
	cfg.TraceDir = traceDir
	// 独立的运行实例：不经 UsePipeline，不会覆盖宿主配置的 pipeline，并发的
	// 多次调用之间也互不干扰。模型写出的编排一律过检查链——在 agent 运行中
	// 启动时继承该运行的策略，否则按 App 配置新建。
	pl := newPipeline(cfg, a)
	scope, release := a.subRunScope(ctx)
	defer release()
	pl.scope = scope
	return a.runPipeline(ctx, pl)
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
		"末端节点（没有 injects）的最终产出会节选进工具结果，需要完整交付的应让末端节点用 Write 写入文件。" +
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
					// 容错：模型常把 nodes 数组直接当 spec 提交（漏了外层对象）。
					var nodes []DynNodeSpec
					if err2 := json.Unmarshal(in.Spec, &nodes); err2 == nil && len(nodes) > 0 {
						spec = DynPipelineSpec{Nodes: nodes}
					} else {
						return "", fmt.Errorf("spec JSON 解析失败: %w（spec 须为 {\"nodes\":[...]} 对象）", err)
					}
				}

				// 运行快照落盘 <workdir>/<状态目录>/pipelines/<runID>/run.json：节点状态与
				// 产出随事件增量重写——宿主面板可回看历史运行（运行结束定格）。
				runID := strconv.FormatInt(time.Now().UnixMilli(), 10)
				type runNode struct {
					Name        string   `json:"name"`
					Instruction string   `json:"instruction,omitempty"`
					Tools       []string `json:"tools,omitempty"`
					DependsOn   []string `json:"depends_on,omitempty"`
					Injects     []string `json:"injects,omitempty"`
					Status      string   `json:"status"`
					Outputs     []string `json:"outputs,omitempty"`
				}
				type runSnapshot struct {
					ID         string    `json:"id"`
					StartedAt  int64     `json:"started_at"`
					FinishedAt int64     `json:"finished_at,omitempty"`
					Nodes      []runNode `json:"nodes"`
				}
				snap := runSnapshot{ID: runID, StartedAt: time.Now().UnixMilli(), Nodes: make([]runNode, 0, len(spec.Nodes))}
				for _, n := range spec.Nodes {
					snap.Nodes = append(snap.Nodes, runNode{Name: n.Name, Instruction: n.Instruction, Tools: n.Tools, DependsOn: n.DependsOn, Injects: n.Injects, Status: "pending"})
				}
				runDir := ""
				workDir := ctx.WorkDir
				if workDir == "" {
					// 未绑定会话工作目录时回退进程 cwd——快照与轨迹不该因为
					// 宿主没配 WithSessionWorkDir 就整体消失。
					if wd, err := os.Getwd(); err == nil {
						workDir = wd
					}
				}
				if workDir != "" {
					// 运行目录：<workdir>/<状态目录>/pipelines/<runID>/（状态目录默认 .yume），内含
					// run.json（快照）与各节点轨迹 <node>.jsonl。
					runDir = statedir.Join(workDir, "pipelines", runID)
					if err := os.MkdirAll(runDir, 0o755); err == nil {
						defer func() { // 兜底定格：异常路径也留下快照
							b, _ := json.MarshalIndent(snap, "", "  ")
							_ = os.WriteFile(filepath.Join(runDir, "run.json"), b, 0o644)
						}()
					} else {
						runDir = ""
					}
				}
				saveSnap := func() {
					if runDir == "" {
						return
					}
					b, _ := json.MarshalIndent(snap, "", "  ")
					_ = os.WriteFile(filepath.Join(runDir, "run.json"), b, 0o644)
				}
				nodeByID := map[string]*runNode{}
				for i := range snap.Nodes {
					nodeByID[snap.Nodes[i].Name] = &snap.Nodes[i]
				}

				outputs := map[string][]string{}
				// 节点生命周期与产出事件经 subagent_progress 透出到主对话流的
				// create_pipeline 工具卡（前端实时渲染节点进度）；同时发结构化
				// 自定义事件 goagent.pipeline_node（{node,status,text}），宿主
				// 可不依赖 agent_id=node 约定直接消费类型化状态。
				onEvent := func(ev PipelineEvent) {
					status := "running"
					text := ev.Text
					switch ev.Type {
					case "node_started":
						text = "开始执行: " + ev.Node
						if rn := nodeByID[ev.Node]; rn != nil && rn.Status == "pending" {
							rn.Status = "running"
							saveSnap()
						}
					case "node_done":
						text, status = "执行完成: "+ev.Node, "done"
						if rn := nodeByID[ev.Node]; rn != nil {
							rn.Status = "done"
							saveSnap()
						}
					case "node_output":
						const maxOut = 4000
						out := ev.Text
						if len(out) > maxOut {
							out = tracePreview(out, maxOut) + "（截断）"
						}
						outputs[ev.Node] = append(outputs[ev.Node], ev.Text)
						if rn := nodeByID[ev.Node]; rn != nil {
							rn.Outputs = append(rn.Outputs, out)
							saveSnap()
						}
						return // 产出不进进度行（太长），进最终摘要
					case "error":
						if rn := nodeByID[ev.Node]; rn != nil {
							rn.Status = "error"
							saveSnap()
						}
					}
					if text == "" {
						return
					}
					if b, err := json.Marshal(map[string]string{"node": ev.Node, "status": status, "text": text}); err == nil {
						loop.EmitFromTool(ctx, loop.Event{
							Type:        loop.EvtCustom,
							CustomName:  PipelineNodeEvent,
							CustomValue: b,
						})
					}
					loop.EmitFromTool(ctx, loop.Event{
						Type:          loop.EvtSubAgentProgress,
						AgentID:       ev.Node,
						AgentDesc:     ev.Node,
						AgentStatus:   status,
						AgentActivity: text,
					})
				}
				if err := a.RunDynPipelineTrace(ctx, &spec, onEvent, runDir); err != nil {
					return "", err
				}
				snap.FinishedAt = time.Now().UnixMilli()
				for i := range snap.Nodes {
					if snap.Nodes[i].Status == "running" || snap.Nodes[i].Status == "pending" {
						snap.Nodes[i].Status = "done"
					}
				}
				saveSnap()
				summary := fmt.Sprintf("pipeline 执行完成（%d 个节点）", len(spec.Nodes))
				if len(outputs) > 0 {
					summary += "\n\n[节点产出]"
					for _, n := range spec.Nodes {
						for _, out := range outputs[n.Name] {
							tail := out
							if len(tail) > 800 {
								tail = tracePreview(tail, 800) + "（截断，全文见节点产出的文件/队列交付）"
							}
							summary += "\n\n### " + n.Name + "\n" + tail
						}
					}
				}
				return summary, nil
			},
		},
	}
}
