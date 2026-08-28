package goagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NoopSandbox 透传：ResolvePath 原样返回、Root 空、Close 无副作用。
func TestNoopSandboxPassthrough(t *testing.T) {
	sess, err := NoopSandbox.Enter(context.Background(), "s1", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Root() != "" {
		t.Fatalf("Noop Root 应为空: %q", sess.Root())
	}
	p, err := sess.ResolvePath("some/rel.txt", OpWrite)
	if err != nil || p != "some/rel.txt" {
		t.Fatalf("Noop 应原样返回: %q, %v", p, err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Noop Close 应无错: %v", err)
	}
}

// 策略最长前缀匹配：嵌套规则、无匹配拒绝、FSNone 显式拒绝。
func TestPolicyLongestPrefix(t *testing.T) {
	rules := []FSRule{
		{Path: `/base`, Access: FSReadWrite},
		{Path: `/base/ro`, Access: FSReadOnly},
		{Path: `/base/none`, Access: FSNone},
	}
	cases := []struct {
		path string
		want FSAccess
	}{
		{"/base", FSReadWrite},
		{"/base/a.txt", FSReadWrite},
		{"/base/ro/x.txt", FSReadOnly},
		{"/base/none/x.txt", FSNone},
		{"/other/x.txt", FSNone}, // 无匹配 = 拒绝（白名单语义）
	}
	for _, c := range cases {
		if got := matchFS(rules, filepath.Clean(c.path)); got != c.want {
			t.Errorf("matchFS(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// 兄弟目录不算包含
	if matchFS(rules, `/base2/x.txt`) != FSNone {
		t.Error("/base2 不应匹配 /base 前缀")
	}
}

// DirSandbox 收敛：根内读写放行、根外绝对路径拒绝、`..` 逃逸拒绝、
// 只读规则写拒绝、空策略默认根读写、Close 删除根。
func TestDirSandboxConfinement(t *testing.T) {
	base := t.TempDir()
	sb := NewDirSandbox(base)
	sess, err := sb.Enter(context.Background(), "user-test", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	root := sess.Root()
	if !strings.Contains(filepath.Base(root), "user-test") {
		t.Fatalf("沙箱根应含消毒后的 sessionID: %q", root)
	}

	// 根内相对路径 → 根内绝对路径
	p, err := sess.ResolvePath("a/b.txt", OpWrite)
	if err != nil {
		t.Fatalf("根内写应放行: %v", err)
	}
	if !pathContains(root, p) {
		t.Fatalf("解析结果不在根内: %q", p)
	}

	// 根外绝对路径拒绝
	if _, err := sess.ResolvePath(filepath.Join(base, "outside.txt"), OpWrite); err == nil {
		t.Fatal("根外绝对路径应拒绝")
	}

	// `..` 词法逃逸拒绝
	if _, err := sess.ResolvePath("../../../etc/passwd", OpWrite); err == nil {
		t.Fatal("`..` 逃逸应拒绝")
	}

	// 空路径返回根
	if p, _ := sess.ResolvePath("", OpRead); p != root {
		t.Fatalf("空路径应返回根: %q", p)
	}

	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("Close 后沙箱根应被删除")
	}
}

// DirSandbox 只读规则：写拒绝、读放行。
func TestDirSandboxReadOnly(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "allow-ro")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	sb := NewDirSandbox(base)
	sess, err := sb.Enter(context.Background(), "", Policy{
		FS: []FSRule{{Path: "", Access: FSReadWrite}, {Path: outside, Access: FSReadOnly}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if _, err := sess.ResolvePath(filepath.Join(outside, "x.txt"), OpRead); err != nil {
		t.Fatalf("只读路径读应放行: %v", err)
	}
	if _, err := sess.ResolvePath(filepath.Join(outside, "x.txt"), OpWrite); err == nil {
		t.Fatal("只读路径写应拒绝")
	}
	if !strings.Contains(func() string {
		_, err := sess.ResolvePath(filepath.Join(outside, "x.txt"), OpWrite)
		return err.Error()
	}(), "只读") {
		t.Fatal("写拒绝错误应是 LLM 可读中文")
	}
}

// Close 防御：根不在 base 下时拒绝删除。
func TestDirSandboxCloseDefense(t *testing.T) {
	base := t.TempDir()
	sess, err := NewDirSandbox(base).Enter(context.Background(), "", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*dirSandboxSession)
	fake := t.TempDir() // 另一个目录，不在 base 下
	s.root = fake
	if err := sess.Close(); err == nil {
		t.Fatal("根不在 base 下应拒绝删除")
	}
	s.root = filepath.Join(base, "goagent-sb-fake") // 不存在也允许（幂等）
	if err := sess.Close(); err != nil {
		t.Fatalf("不存在的根 Close 应幂等成功: %v", err)
	}
}

// WithSandboxSession / SandboxFromContext 注入提取契约（含 nil 安全）。
func TestSandboxContextInjection(t *testing.T) {
	if SandboxFromContext(nil) != nil {
		t.Fatal("nil context 应返回 nil")
	}
	if SandboxFromContext(context.Background()) != nil {
		t.Fatal("未注入应返回 nil")
	}
	ctx := WithSandboxSession(context.Background(), noopSession{})
	if SandboxFromContext(ctx) == nil {
		t.Fatal("注入后应可提取")
	}
	// newContextFromStd 提升到结构体字段
	got := newContextFromStd(ctx)
	if got.Sandbox == nil {
		t.Fatal("newContextFromStd 应提升 Sandbox 字段")
	}
}
