package statedir

import (
	"path/filepath"
	"testing"
)

func TestDefaultAndJoin(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if Name() != Default {
		t.Fatalf("默认名应为 %q，实际 %q", Default, Name())
	}
	if got, want := Join("", "sessions"), filepath.Join(".yume", "sessions"); got != want {
		t.Fatalf("Join = %q, want %q", got, want)
	}
	if got, want := Join("proj", "commands", "a.md"), filepath.Join("proj", ".yume", "commands", "a.md"); got != want {
		t.Fatalf("Join = %q, want %q", got, want)
	}
}

func TestSet(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if err := Set(".myapp"); err != nil {
		t.Fatal(err)
	}
	if got, want := Join("proj", "plans"), filepath.Join("proj", ".myapp", "plans"); got != want {
		t.Fatalf("Join = %q, want %q", got, want)
	}
	for _, bad := range []string{".", "..", "a/b", `a\b`, "/abs", `C:\x`, "C:x"} {
		if err := Set(bad); err == nil {
			t.Errorf("Set(%q) 应被拒绝", bad)
		}
	}
	if Name() != ".myapp" {
		t.Fatalf("非法值不应改变当前设置，实际 %q", Name())
	}
	if err := Set(""); err != nil || Name() != Default {
		t.Fatalf("空字符串应恢复默认值，实际 %q, err=%v", Name(), err)
	}
}
