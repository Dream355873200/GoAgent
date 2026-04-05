// Package agent 实现 agent 核心机制。
package agent

import (
	"context"
	"fmt"
	"sync"
)

// SwarmConfig 配置 Swarm 多 agent 协调。
type SwarmConfig struct {
	// Agents 参与协调的 agent 定义。
	Agents []AgentDefinition
	// Coordinator 主协调器。
	Coordinator string
	// MaxHandoffs 最大交接次数。
	MaxHandoffs int
	// OnHandoff 交接回调。
	OnHandoff func(from, to string, context map[string]any)
}

// SwarmAgent 协调多个 agent 的 Swarm 系统。
// 对齐 Claude Code 的 swarm/multi-agent 协调机制。
type SwarmAgent struct {
	config         SwarmConfig
	registry       *SubAgentManager
	activeHandoffs int
	mu             sync.Mutex
}

// NewSwarmAgent 创建 Swarm agent。
func NewSwarmAgent(cfg SwarmConfig, registry *SubAgentManager) *SwarmAgent {
	return &SwarmAgent{
		config:   cfg,
		registry: registry,
	}
}

// HandoffResult agent 交接结果。
type HandoffResult struct {
	// From 源 agent。
	From string
	// To 目标 agent。
	To string
	// Context 交接上下文。
	Context map[string]any
	// Success 是否成功。
	Success bool
	// Error 错误信息。
	Error error
}

// Handoff 进行 agent 之间的交接。
func (s *SwarmAgent) Handoff(ctx context.Context, fromAgent, toAgent string, context map[string]any) (*HandoffResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := &HandoffResult{
		From:    fromAgent,
		To:      toAgent,
		Context: context,
	}

	// 检查是否超过最大交接次数
	if s.config.MaxHandoffs > 0 && s.activeHandoffs >= s.config.MaxHandoffs {
		result.Success = false
		result.Error = fmt.Errorf("超过最大交接次数 %d", s.config.MaxHandoffs)
		return result, result.Error
	}

	// 查找目标 agent
	toDef := s.registry.GetAgent(toAgent)
	if toDef == nil {
		result.Success = false
		result.Error = fmt.Errorf("未找到 agent: %s", toAgent)
		return result, result.Error
	}

	// 执行回调
	if s.config.OnHandoff != nil {
		s.config.OnHandoff(fromAgent, toAgent, context)
	}

	s.activeHandoffs++

	result.Success = true
	return result, nil
}

// AgentCapability agent 能力描述。
type AgentCapability struct {
	Name        string
	Description string
	Tools       []string
	MaxTurns    int
}

// GetCapabilities 返回所有 agent 的能力。
func (s *SwarmAgent) GetCapabilities() []AgentCapability {
	var caps []AgentCapability
	for _, def := range s.config.Agents {
		caps = append(caps, AgentCapability{
			Name:        def.AgentType,
			Description: def.Description,
			Tools:       def.Tools,
			MaxTurns:    def.MaxTurns,
		})
	}
	return caps
}

// RoutingStrategy 路由策略。
type RoutingStrategy int

const (
	// RoutingByCapability 根据能力路由。
	RoutingByCapability RoutingStrategy = iota
	// RoutingByLoad 根据负载路由。
	RoutingByLoad
	// RoutingByRoundRobin 轮询路由。
	RoutingByRoundRobin
)

// Router agent 路由器。
type Router struct {
	strategy   RoutingStrategy
	registry   *SubAgentManager
	roundRobin int
	mu         sync.Mutex
}

// NewRouter 创建路由器。
func NewRouter(strategy RoutingStrategy, registry *SubAgentManager) *Router {
	return &Router{
		strategy: strategy,
		registry: registry,
	}
}

// Route 根据策略选择最适合的 agent。
func (r *Router) Route(requiredTools []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.strategy {
	case RoutingByCapability:
		return r.routeByCapability(requiredTools)
	case RoutingByRoundRobin:
		return r.routeByRoundRobin()
	default:
		return "general-purpose", nil
	}
}

// routeByCapability 根据能力选择 agent。
func (r *Router) routeByCapability(requiredTools []string) (string, error) {
	if len(requiredTools) == 0 {
		return "general-purpose", nil
	}

	// 简单实现：查找支持最多工具的 agent
	agents := r.registry.ListAgents()
	if len(agents) == 0 {
		return "general-purpose", nil
	}

	var bestAgent string
	bestMatch := 0

	for _, agent := range agents {
		if agent.Tools == nil {
			continue
		}
		match := 0
		for _, reqTool := range requiredTools {
			for _, agentTool := range agent.Tools {
				if reqTool == agentTool {
					match++
				}
			}
		}
		if match > bestMatch {
			bestMatch = match
			bestAgent = agent.AgentType
		}
	}

	if bestAgent == "" {
		bestAgent = "general-purpose"
	}

	return bestAgent, nil
}

// routeByRoundRobin 轮询选择。
func (r *Router) routeByRoundRobin() (string, error) {
	agents := r.registry.ListAgents()
	if len(agents) == 0 {
		return "general-purpose", nil
	}

	idx := r.roundRobin % len(agents)
	r.roundRobin++

	return agents[idx].AgentType, nil
}
