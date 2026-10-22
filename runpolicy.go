package goagent

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/prompts"
)

// MaxSubRunDepth 是子运行（pipeline、团队、子 agent）允许的最大嵌套深度。
// 主对话为第 0 层；超过此深度的 create_pipeline / run_team / 子 agent 调用
// 直接返回错误，防止模型无限递归地派生子任务。
const MaxSubRunDepth = 3

// 一次运行的安全策略（权限门、plan 模式、hooks、中间件）与观测出口由
// internal/loop 的 Scope 承载：循环把它随 ctx 交给每一次工具执行，在工具里
// 派生的子运行（pipeline 节点、supervisor、团队成员、子 agent）经
// loop.NewChild 继承——子运行只能和父运行一样严格。

// subRunDepth 返回 ctx 所处的子运行嵌套深度（主对话为 0）。
func subRunDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	d, _ := ctx.Value(ctxKeyDepth).(int)
	return d
}

// enterSubRun 进入一层子运行：超过 MaxSubRunDepth 时返回错误。
func enterSubRun(ctx context.Context, what string) (context.Context, error) {
	d := subRunDepth(ctx) + 1
	if d > MaxSubRunDepth {
		return ctx, fmt.Errorf("%s 嵌套过深（已达 %d 层上限），请直接完成当前任务而不是继续派生子任务", what, MaxSubRunDepth)
	}
	return context.WithValue(ctx, ctxKeyDepth, d), nil
}

// inheritedScope 返回 ctx 所在运行的作用域；不在任何带权限门的运行之内时
// 为 nil。
func inheritedScope(ctx context.Context) *loop.Scope {
	if s := loop.ScopeFrom(ctx); s != nil && s.Permission != nil {
		return s
	}
	return nil
}

// subRunScope 返回由模型编排的子运行（create_pipeline、run_team）应使用的
// 作用域：在父运行之内派生时继承父运行的作用域；不在任何带权限门的运行之内
// （宿主代码直接发起，或位于宿主信任的 pipeline 节点里）时按 App 配置新建
// 一套——模型写出的编排无论从哪里启动都要过权限门。子运行结束时调用 release。
func (a *App) subRunScope(ctx context.Context) (scope *loop.Scope, release func()) {
	if s := inheritedScope(ctx); s != nil {
		return s, func() {}
	}
	return a.newScope(ctx)
}

// newScope 按 App 配置构造一套完整的作用域；权限门登记为进行中（见
// acquireGate），结束时调用 release。
func (a *App) newScope(ctx context.Context) (*loop.Scope, func()) {
	sessionID := SessionIDFromContext(ctx)
	rt := sessionRuntimeFrom(ctx)
	if rt == nil {
		rt = a.sessionRuntime(sessionID)
	}
	gate, release := a.acquireGate(rt)
	a.mu.RLock()
	cfg := a.config
	prov, fallback := a.provider, a.fallback
	mws := make([]MiddlewareWithFilter, len(a.middlewares))
	copy(mws, a.middlewares)
	a.mu.RUnlock()
	s := &loop.Scope{
		Permission:          gate,
		Middlewares:         toLoopMiddlewares(mws),
		SkipInputValidation: cfg.skipInputValidation,
		Provider:            prov,
		Fallback:            fallback,
		Observer:            observer.ResolveObserver(ctx, a.obsRegistry.Observer()),
		Logger:              a.logger,
		Debug:               cfg.debug,
		SessionID:           sessionID,
	}
	if a.hooksMgr != nil {
		s.Hooks = &hooksRunnerAdapter{mgr: a.hooksMgr}
	}
	if a.planMgr != nil {
		s.PlanChecker = &planCheckerAdapter{mgr: a.planMgr}
	}
	return s, release
}

// newGate 按 App 配置构造会话 rt 的权限门：模式（会话级设置 > 全局运行时
// 覆写 > 构造配置）、会话共享的"始终允许"授权、规则、Guard、编辑类工具与
// 可选的 YOLO 分类器。sessionMode 表示模式来自会话级设置。
func (a *App) newGate(rt *sessionRuntime) (gate *permission.Gate, sessionMode bool) {
	sessionID := rt.id
	mode, sessionMode := a.sessionModeFor(rt)
	if !sessionMode {
		mode = a.CurrentPermissionMode()
	}
	a.mu.RLock()
	cfg := a.config
	prov := a.provider
	editTools := a.editToolNamesLocked()
	a.mu.RUnlock()

	gate = permission.NewGate(wrapApprover(cfg.approver))
	gate.SetMode(permission.PermissionMode(mode))
	gate.SetGrants(rt.grants)
	if cfg.permissionRules != nil {
		gate.SetRules(cfg.permissionRules)
	}
	for _, gd := range cfg.permissionGuards {
		gate.AddGuard(gd)
	}
	gate.SetEditTools(editTools)
	// YOLO 分类器（默认装配）：auto 模式判定每个非只读调用，bypass 模式只
	// 判定高风险调用，其他模式不调用。提示词优先级：WithYoloPromptFile >
	// 提示词目录中的同名文件 > 嵌入的默认值。
	if prov != nil && !cfg.yoloDisabled {
		yoloCfg := permission.YoloClassifierConfig{Mode: "both", Model: cfg.yoloModel}
		if cfg.yoloPromptFile != "" {
			yoloCfg.PromptFile = cfg.yoloPromptFile
		} else if dir := effectivePromptDir(cfg, sessionID); dir != "" {
			yoloCfg.PromptFile = filepath.Join(dir, prompts.YoloClassifier)
		}
		gate.SetClassifier(permission.NewYoloClassifier(prov, yoloCfg))
	}
	return gate, sessionMode
}

// editToolNamesLocked 返回标记为文件编辑类（ToolDef.FileEdit）的工具名。
// 调用方须持有 a.mu。
func (a *App) editToolNamesLocked() []string {
	var names []string
	for _, name := range a.toolOrder {
		if rt := a.tools[name]; rt != nil && rt.def.FileEdit {
			names = append(names, name)
		}
	}
	return names
}
