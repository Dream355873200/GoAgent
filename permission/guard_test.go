package permission

import "testing"

// TestGuardChain 单调 Guard 链：可拒不可放。
func TestGuardChain(t *testing.T) {
	g := NewGate(nil)
	g.AddGuard(Guard{Name: "block-rm", Fn: func(toolName, input string, level Level) *PermissionResult {
		if toolName == "Bash" && contains(input, "rm ") {
			return &PermissionResult{Behavior: BehaviorDeny, Reason: "红线命令"}
		}
		return nil
	}})

	// 命中 Guard → 拒绝，Source 标注策略名。
	r := g.CheckResult("Bash", "rm -rf /", LevelNormal)
	if r.Behavior != BehaviorDeny || r.Source != "guard:block-rm" {
		t.Fatalf("guard deny 未生效: %+v", r)
	}

	// 未命中 → 交还后续策略链（默认级别 → ask）。
	r = g.CheckResult("Bash", "ls", LevelNormal)
	if r.Behavior != BehaviorAsk {
		t.Fatalf("guard 不应影响未命中的调用: %+v", r)
	}

	// 单调性：bypass 模式下 Guard 拒绝依然生效；Guard 返回 Allow 不放行 ask。
	g.SetMode(ModeBypassPermissions)
	if r := g.CheckResult("Bash", "rm -rf /", LevelNormal); r.Behavior != BehaviorDeny {
		t.Fatalf("bypass 模式下 guard deny 失效: %+v", r)
	}
	g.AddGuard(Guard{Name: "loose", Fn: func(toolName, input string, level Level) *PermissionResult {
		return &PermissionResult{Behavior: BehaviorAllow}
	}})
	if r := g.CheckResult("Bash", "anything", LevelNormal); r.Behavior == BehaviorAllow && r.Source == "guard:loose" {
		t.Fatalf("guard 不得强行放行: %+v", r)
	}

	// 同名覆盖与注销。
	g.AddGuard(Guard{Name: "block-rm", Fn: func(toolName, input string, level Level) *PermissionResult { return nil }})
	if r := g.CheckResult("Bash", "rm -rf /", LevelNormal); r.Behavior == BehaviorDeny && r.Source == "guard:block-rm" {
		t.Fatalf("同名覆盖失败: %+v", r)
	}
	g.RemoveGuard("block-rm")
	g.RemoveGuard("loose")
	if r := g.CheckResult("Bash", "rm -rf /", LevelNormal); r.Behavior == BehaviorDeny && r.Source == "guard:block-rm" {
		t.Fatalf("注销失败: %+v", r)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
