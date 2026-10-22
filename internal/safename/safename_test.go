package safename

import (
	"path/filepath"
	"testing"
)

func TestCheck(t *testing.T) {
	good := []string{"5e45706de307daa69cf8b901da220c74", "sess-123", "项目约定", "a.b_c-d", "ephemeral", "MEMORY"}
	for _, s := range good {
		if err := Check("name", s); err != nil {
			t.Errorf("Check(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"", ".", "..", "../x", "../../escaped", `..\x`, "a/b", `a\b`, "a:b", "con", "NUL.txt", "x\x00y", " lead", "trail ", "dot.", "a..b"}
	for _, s := range bad {
		if err := Check("name", s); err == nil {
			t.Errorf("Check(%q) = nil, want error", s)
		}
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"../../../escaped": "_________escaped",
		"call_01/abc":      "call_01_abc",
		"toolu_01ABC":      "toolu_01ABC",
		"":                 "_",
		"..":               "_",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
		if err := Check("name", Sanitize(in)); err != nil {
			t.Errorf("Sanitize(%q) 结果仍不合法: %v", in, err)
		}
	}
}

func TestJoin(t *testing.T) {
	dir := t.TempDir()
	p, err := Join("会话 ID", dir, "abc", ".jsonl")
	if err != nil || p != filepath.Join(dir, "abc.jsonl") {
		t.Fatalf("Join = %q, %v", p, err)
	}
	if _, err := Join("会话 ID", dir, "../../victim", ".jsonl"); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
}
