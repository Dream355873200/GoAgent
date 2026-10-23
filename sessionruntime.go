package goagent

import (
	"context"
	"sync"

	"github.com/Dream355873200/GoAgent/compaction"
	"github.com/Dream355873200/GoAgent/permission"
)

// sessionRuntime 是一个会话跨运行常驻的状态：工具的键值存储、压缩管理器
// （自动摘要的熔断状态）、"始终允许"授权与会话级的运行时权限模式。真实
// 会话的运行时登记在 App 上，直到 CloseSession；没有会话身份的运行（Run、
// RunWithHistory）每次使用一个新的运行时，彼此隔离。
type sessionRuntime struct {
	id string

	// kv 是工具 Context.Store / Load 的键值存储。
	kv sync.Map

	// grants 是会话内各次运行共享的"始终允许"授权。
	grants *permission.Grants

	// agents 是会话的子代理表（见 subagents.go）。
	agents *subagentRegistry

	compactorOnce sync.Once
	compactor     *compaction.Manager

	mu   sync.Mutex
	mode *PermissionModeOption // SetSessionPermissionMode 设置的模式
}

func newSessionRuntime(id string) *sessionRuntime {
	return &sessionRuntime{id: id, grants: permission.NewGrants(), agents: newSubagentRegistry(id)}
}

func ephemeralSession(id string) bool {
	return id == "" || id == "ephemeral"
}

// sessionRuntime 返回会话的运行时；sessionID 为空或为 "ephemeral" 时返回一个
// 不登记的新运行时。
func (a *App) sessionRuntime(sessionID string) *sessionRuntime {
	if ephemeralSession(sessionID) {
		return newSessionRuntime(sessionID)
	}
	if rt := a.existingRuntime(sessionID); rt != nil {
		return rt
	}
	v, _ := a.runtimes.LoadOrStore(sessionID, newSessionRuntime(sessionID))
	return v.(*sessionRuntime)
}

func (a *App) existingRuntime(sessionID string) *sessionRuntime {
	if v, ok := a.runtimes.Load(sessionID); ok {
		return v.(*sessionRuntime)
	}
	return nil
}

// CloseSession 释放会话在内存中的常驻状态：工具键值存储、压缩管理器、
// "始终允许"授权、会话级权限模式、子代理表（运行中的子代理被中断）与会话
// 事件日志（正在读取的游标随之结束）。
// 宿主删除会话时应同时调用；会话之后再次运行会从空状态开始（对话历史由会话
// 存储保存，不受影响）。
func (a *App) CloseSession(sessionID string) {
	if rt := a.existingRuntime(sessionID); rt != nil {
		rt.agents.cancelAll("会话已关闭")
	}
	a.runtimes.Delete(sessionID)
	if hub := a.eventHub(); hub != nil {
		hub.close(sessionID)
	}
	a.frontend.forget(sessionID) // 未被取走的前端工具结果随会话丢弃
}

// SessionGrants 返回会话的"始终允许"授权集合（会话还没有运行过时为 nil）。
// 宿主可以据此展示（Keys）或撤销（Clear）授权，对进行中的运行立即生效。
func (a *App) SessionGrants(sessionID string) *permission.Grants {
	if rt := a.existingRuntime(sessionID); rt != nil {
		return rt.grants
	}
	return nil
}

// compactionManager 返回会话的压缩管理器（同一会话同一时刻只有一个运行，
// 见 session.Manager.Acquire）；首次调用时按 cfg 创建。
func (rt *sessionRuntime) compactionManager(cfg compaction.Config) *compaction.Manager {
	rt.compactorOnce.Do(func() { rt.compactor = compaction.NewManager(cfg) })
	return rt.compactor
}

func (rt *sessionRuntime) permissionMode() (PermissionModeOption, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.mode == nil {
		return 0, false
	}
	return *rt.mode, true
}

func (rt *sessionRuntime) setPermissionMode(m *PermissionModeOption) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.mode = m
}

func withSessionRuntime(ctx context.Context, rt *sessionRuntime) context.Context {
	return context.WithValue(ctx, ctxKeySessionRuntime, rt)
}

func sessionRuntimeFrom(ctx context.Context) *sessionRuntime {
	if ctx == nil {
		return nil
	}
	rt, _ := ctx.Value(ctxKeySessionRuntime).(*sessionRuntime)
	return rt
}

// liveGate 是一个进行中运行的权限门的登记信息。
type liveGate struct {
	sessionID string
	// sessionMode 表示模式来自会话级设置（SetSessionPermissionMode 或
	// WithSessionPermissionMode 的解析结果），全局切换不覆盖它。
	sessionMode bool
}

// acquireGate 为一次运行（或模型编排的子运行）构造权限门并登记为进行中，
// 运行结束时调用 release 注销。登记中的权限门即时接收模式切换与 Guard 增删。
func (a *App) acquireGate(rt *sessionRuntime) (gate *permission.Gate, release func()) {
	gate, sessionMode := a.newGate(rt)
	a.gatesMu.Lock()
	if a.gates == nil {
		a.gates = make(map[*permission.Gate]*liveGate)
	}
	a.gates[gate] = &liveGate{sessionID: rt.id, sessionMode: sessionMode}
	a.gatesMu.Unlock()
	return gate, func() {
		a.gatesMu.Lock()
		delete(a.gates, gate)
		a.gatesMu.Unlock()
	}
}

// eachLiveGate 对每个进行中的权限门调用 fn（持有登记锁）。
func (a *App) eachLiveGate(fn func(*permission.Gate, *liveGate)) {
	a.gatesMu.Lock()
	defer a.gatesMu.Unlock()
	for g, lg := range a.gates {
		fn(g, lg)
	}
}

// sessionModeFor 返回会话级的权限模式：SetSessionPermissionMode 的设置优先，
// 其次是 WithSessionPermissionMode 的解析结果；都没有时 ok 为 false。
func (a *App) sessionModeFor(rt *sessionRuntime) (PermissionModeOption, bool) {
	if m, ok := rt.permissionMode(); ok {
		return m, true
	}
	a.mu.RLock()
	fn := a.config.sessionPermModeFn
	a.mu.RUnlock()
	if fn != nil && !ephemeralSession(rt.id) {
		return fn(rt.id)
	}
	return 0, false
}

// SetSessionPermissionMode 运行时切换某个会话的权限模式：立即作用于该会话
// 进行中的运行，并在之后的运行中优先于全局模式与 WithSessionPermissionMode
// 的解析结果，直到 ClearSessionPermissionMode 或 CloseSession。没有会话身份
// （空或 "ephemeral"）时不做任何事。
func (a *App) SetSessionPermissionMode(sessionID string, mode PermissionModeOption) {
	if ephemeralSession(sessionID) {
		return
	}
	a.sessionRuntime(sessionID).setPermissionMode(&mode)
	a.eachLiveGate(func(g *permission.Gate, lg *liveGate) {
		if lg.sessionID == sessionID {
			g.SetMode(permission.PermissionMode(mode))
			lg.sessionMode = true
		}
	})
}

// ClearSessionPermissionMode 撤销会话级的运行时权限模式：该会话回到
// WithSessionPermissionMode 的解析结果或全局模式，进行中的运行同样生效。
func (a *App) ClearSessionPermissionMode(sessionID string) {
	rt := a.existingRuntime(sessionID)
	if rt == nil {
		return
	}
	rt.setPermissionMode(nil)
	mode, sessionMode := a.sessionModeFor(rt)
	if !sessionMode {
		mode = a.CurrentPermissionMode()
	}
	a.eachLiveGate(func(g *permission.Gate, lg *liveGate) {
		if lg.sessionID == sessionID {
			g.SetMode(permission.PermissionMode(mode))
			lg.sessionMode = sessionMode
		}
	})
}

// SessionPermissionMode 返回会话当前生效的权限模式：会话级设置优先，否则为
// 全局模式（见 CurrentPermissionMode）。
func (a *App) SessionPermissionMode(sessionID string) PermissionModeOption {
	rt := a.existingRuntime(sessionID)
	if rt == nil {
		rt = newSessionRuntime(sessionID)
	}
	if m, ok := a.sessionModeFor(rt); ok {
		return m
	}
	return a.CurrentPermissionMode()
}
