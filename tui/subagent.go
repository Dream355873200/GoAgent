package tui

// 本文件实现子 Agent 并发状态追踪和树形渲染。
//
// 对齐 Claude Code 的 Agent 并发显示风格：
//   ● Running 2 Explore agents… (ctrl+o to expand)
//      ├─ Explore reference prompts · 13 tool uses · 24.6k tokens
//      │  ⎿  Searching for 1 pattern, reading 12 files…
//      └─ Explore our current prompts · 12 tool uses · 24.6k tokens
//         ⎿  Done

import (
	"fmt"
	"strings"
	"sync"
)

// subAgentState 表示一个正在运行的子 agent 的状态。
type subAgentState struct {
	ID       string // 唯一标识（通常是 ToolUseID）
	Desc     string // 描述（如 "Explore reference prompts"）
	Status   string // "running" / "done"
	Activity string // 当前活动（如 "Searching for 1 pattern, reading 12 files…"）
	ToolUses int    // 工具调用次数
	Tokens   int    // token 消耗
}

// subAgentTracker 追踪当前正在运行的子 agent 集合。
// 用于在 TUI 中渲染树形状态显示。
type subAgentTracker struct {
	mu     sync.Mutex
	agents map[string]*subAgentState // agentID → state
	order  []string                  // 保持插入顺序
}

func newSubAgentTracker() *subAgentTracker {
	return &subAgentTracker{
		agents: make(map[string]*subAgentState),
	}
}

// Update 更新或创建子 agent 状态。
func (t *subAgentTracker) Update(id, desc, status, activity string, toolUses, tokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.agents[id]; !ok {
		t.order = append(t.order, id)
		t.agents[id] = &subAgentState{ID: id}
	}

	s := t.agents[id]
	if desc != "" {
		s.Desc = desc
	}
	if status != "" {
		s.Status = status
	}
	if activity != "" {
		s.Activity = activity
	}
	if toolUses > 0 {
		s.ToolUses = toolUses
	}
	if tokens > 0 {
		s.Tokens = tokens
	}
}

// Remove 移除一个子 agent。
func (t *subAgentTracker) Remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.agents, id)
	for i, oid := range t.order {
		if oid == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

// Clear 清空所有子 agent 状态。
func (t *subAgentTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.agents = make(map[string]*subAgentState)
	t.order = nil
}

// Count 返回正在运行的子 agent 数量。
func (t *subAgentTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	count := 0
	for _, s := range t.agents {
		if s.Status == "running" {
			count++
		}
	}
	return count
}

// HasAny 返回是否有任何子 agent（包括已完成的）。
func (t *subAgentTracker) HasAny() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.agents) > 0
}

// RenderTree 渲染 Claude Code 风格的子 agent 树形显示。
//
// 输出格式：
//
//	● Running 2 Explore agents…
//	   ├─ Explore reference prompts · 13 tool uses · 24.6k tokens
//	   │  ⎿  Searching for 1 pattern, reading 12 files…
//	   └─ Explore our current prompts · 12 tool uses · 24.6k tokens
//	      ⎿  Done
func (t *subAgentTracker) RenderTree() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.agents) == 0 {
		return ""
	}

	var b strings.Builder

	// 统计运行中的数量和类型。
	running := 0
	agentTypes := make(map[string]int)
	for _, s := range t.agents {
		if s.Status == "running" {
			running++
		}
		parts := strings.SplitN(s.Desc, " ", 2)
		if len(parts) > 0 {
			agentTypes[parts[0]]++
		}
	}

	// 标题行：● Running N agents…
	total := len(t.agents)
	if running > 0 {
		b.WriteString(ActivityDotStyle.Render("●") + " ")
		if running == 1 && total == 1 {
			s := t.agents[t.order[0]]
			b.WriteString(fmt.Sprintf("Running %s…", s.Desc))
		} else {
			b.WriteString(fmt.Sprintf("Running %d agents…", running))
		}
	} else {
		b.WriteString(SuccessDotStyle.Render("●") + " ")
		b.WriteString(fmt.Sprintf("Ran %d agents", total))
	}
	b.WriteString("\n")

	// 树形子项。
	for i, id := range t.order {
		s := t.agents[id]
		isLast := i == len(t.order)-1
		prefix := "   ├─ "
		childPrefix := "   │  "
		if isLast {
			prefix = "   └─ "
			childPrefix = "      "
		}

		// 子 agent 行：├─ desc · N tool uses · Nk tokens
		line := prefix + s.Desc
		var stats []string
		if s.ToolUses > 0 {
			stats = append(stats, fmt.Sprintf("%d tool uses", s.ToolUses))
		}
		if s.Tokens > 0 {
			stats = append(stats, formatTokenCount(s.Tokens)+" tokens")
		}
		if len(stats) > 0 {
			line += " · " + strings.Join(stats, " · ")
		}
		b.WriteString(DimStyle.Render(line) + "\n")

		// 活动行：│  ⎿  activity
		if s.Activity != "" {
			activityLine := childPrefix + "⎿  "
			if s.Status == "done" {
				activityLine += "Done"
			} else {
				activityLine += s.Activity
			}
			b.WriteString(DimStyle.Render(activityLine) + "\n")
		} else if s.Status == "done" {
			activityLine := childPrefix + "⎿  Done"
			b.WriteString(DimStyle.Render(activityLine) + "\n")
		}
	}

	return b.String()
}

// RenderCompletedTree 渲染已完成的子 agent 树形显示（用于写入聊天内容区）。
func (t *subAgentTracker) RenderCompletedTree() string {
	return t.RenderTree()
}
