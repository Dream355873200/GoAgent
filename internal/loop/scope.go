package loop

import (
	"context"
	"log/slog"

	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
)

// Scope 是一次运行中所有工具调用共享的横切配置：检查链（权限门、plan 模式、
// hooks、中间件）、入参校验开关与观测出口。循环把自己的 Scope 随 ctx 交给
// 每一次工具执行；在工具里派生的子循环（NewChild）从 ctx 继承它——子运行
// 只能和父运行一样严格，观测数据也归属同一会话。
type Scope struct {
	Permission          *permission.Gate
	PlanChecker         PlanChecker
	Hooks               HooksRunner
	Middlewares         []Middleware
	SkipInputValidation bool

	// Provider / Fallback 是父运行的主 provider 与后备：子循环未指定 provider
	// 时沿用它们；指定的 provider 与父运行相同时一并沿用后备。
	Provider provider.Provider
	Fallback provider.Provider

	Observer  observer.Observer
	Logger    *slog.Logger
	Debug     bool
	SessionID string
}

type scopeKey struct{}

// WithScope 返回携带 s 的 ctx。
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom 返回 ctx 所属运行的作用域；ctx 不来自任何运行时为 nil。
func ScopeFrom(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// ApplyTo 把作用域套到子循环的配置上：检查链与入参校验开关一律取自作用域，
// 覆盖 cfg 中的同名字段；观测出口与会话 ID 在 cfg 未设置时沿用；cfg 未指定
// Provider 时沿用作用域的 provider，与之相同时一并沿用后备。子循环标记为
// Nested。
func (s *Scope) ApplyTo(cfg *Config) {
	cfg.Permission = s.Permission
	cfg.PlanChecker = s.PlanChecker
	cfg.Hooks = s.Hooks
	cfg.Middlewares = s.Middlewares
	cfg.SkipInputValidation = s.SkipInputValidation
	if cfg.Observer == nil {
		cfg.Observer = s.Observer
	}
	if cfg.Logger == nil {
		cfg.Logger = s.Logger
	}
	cfg.Debug = cfg.Debug || s.Debug
	if cfg.SessionID == "" {
		cfg.SessionID = s.SessionID
	}
	if cfg.Provider == nil {
		cfg.Provider = s.Provider
	}
	if cfg.Fallback == nil && sameProvider(cfg.Provider, s.Provider) {
		cfg.Fallback = s.Fallback
	}
	cfg.Nested = true
}

// NewChild 创建在 ctx 所属运行之内派生的子循环（pipeline 节点、团队成员、子
// agent 等）：配置先经 ctx 中的作用域处理（见 Scope.ApplyTo），再交给 New。
// ctx 不来自任何运行时等同于 New。
func NewChild(ctx context.Context, cfg Config) *Loop {
	if s := ScopeFrom(ctx); s != nil {
		s.ApplyTo(&cfg)
	}
	return New(cfg)
}

// scope 返回本循环交给工具执行的作用域。
func (l *Loop) scope() *Scope {
	return &Scope{
		Permission:          l.cfg.Permission,
		PlanChecker:         l.cfg.PlanChecker,
		Hooks:               l.cfg.Hooks,
		Middlewares:         l.cfg.Middlewares,
		SkipInputValidation: l.cfg.SkipInputValidation,
		Provider:            l.cfg.Provider,
		Fallback:            l.cfg.Fallback,
		Observer:            l.cfg.Observer,
		Logger:              l.cfg.Logger,
		Debug:               l.cfg.Debug,
		SessionID:           l.cfg.SessionID,
	}
}

// sameProvider 比较两个 provider 是否为同一个实例。动态类型不可比较时
// （例如以含切片的结构体值实现接口）视为不同，而不是 panic。
func sameProvider(a, b provider.Provider) (same bool) {
	if a == nil || b == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
