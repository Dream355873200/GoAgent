package goagent

import "testing"

// 会话级提示词目录优先于全局；解析为空 / 无会话时回退全局。
func TestEffectivePromptDir(t *testing.T) {
	cfg := appConfig{promptDir: "global", sessionPromptDirFn: func(sid string) string {
		if sid == "s1" {
			return "mode-a"
		}
		return ""
	}}
	for sid, want := range map[string]string{"s1": "mode-a", "s2": "global", "": "global"} {
		if got := effectivePromptDir(cfg, sid); got != want {
			t.Fatalf("session %q: got %q want %q", sid, got, want)
		}
	}
}
