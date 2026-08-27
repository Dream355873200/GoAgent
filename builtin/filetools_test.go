package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
)

// 文件工具的 Claude Code 对标行为测试。
// 覆盖：Read 截断/二进制/短路，Edit 前置校验/诊断报错，Write 覆盖保护。

func setupFile(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func readExec(t *testing.T) func(goagent.Context, ReadInput) (string, error) {
	t.Helper()
	return ReadTool().Execute.(func(goagent.Context, ReadInput) (string, error))
}

func editExec(t *testing.T) func(goagent.Context, EditInput) (string, error) {
	t.Helper()
	return EditTool().Execute.(func(goagent.Context, EditInput) (string, error))
}

func writeExec(t *testing.T) func(goagent.Context, WriteInput) (string, error) {
	t.Helper()
	return WriteTool().Execute.(func(goagent.Context, WriteInput) (string, error))
}

// Read 超长行截断：行中间被挖空，首尾保留。
func TestReadLineTruncation(t *testing.T) {
	_, p := setupFile(t, "short\n"+strings.Repeat("x", 5000)+"\nend\n")
	out, err := readExec(t)(goagent.Context{Context: context.Background()}, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("超长行未截断:\n%.200s", out)
	}
	if len(out) > 3000 {
		t.Errorf("截断后仍过长: %d 字符", len(out))
	}
	// 首尾行完整保留
	if !strings.Contains(out, "short") || !strings.Contains(out, "end") {
		t.Errorf("正常行被误截:\n%s", out)
	}
}

// Read 二进制检测：PNG/不可打印内容返回类型提示而非乱码。
func TestReadBinaryDetection(t *testing.T) {
	_, p := setupFile(t, "\x00\x01\x02\x00text\x00more\x00binary\x00stuff\x00")
	out, err := readExec(t)(goagent.Context{Context: context.Background()}, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "二进制文件") {
		t.Errorf("二进制未被识别: %q", out)
	}
}

// Read 未修改短路：同会话二次读返回「未修改」。
func TestReadUnchangedShortcircuit(t *testing.T) {
	_, p := setupFile(t, "hello\nworld\n")
	ctx := goagent.Context{Context: context.Background(), SessionID: "s1"}
	r := readExec(t)
	if out, _ := r(ctx, ReadInput{FilePath: p}); strings.Contains(out, "未修改") {
		t.Fatalf("首次读取不应短路: %q", out)
	}
	out, err := r(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "未修改") {
		t.Errorf("未修改文件应短路: %q", out)
	}
	// 文件变了 → 正常重读
	if err := os.WriteFile(p, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ := r(ctx, ReadInput{FilePath: p}); !strings.Contains(out, "changed") {
		t.Errorf("文件修改后应能重读: %q", out)
	}
}

// Edit 前置校验：未读过的文件拒绝编辑。
func TestEditRequiresRead(t *testing.T) {
	_, p := setupFile(t, "aaa\nbbb\nccc\n")
	_, err := editExec(t)(goagent.Context{Context: context.Background(), SessionID: "s2"}, EditInput{
		FilePath: p, OldString: "bbb", NewString: "xxx",
	})
	if err == nil || !strings.Contains(err.Error(), "尚未读取") {
		t.Fatalf("未读文件 Edit 应被拒绝: %v", err)
	}
	// Read 后 Edit 成功
	r := readExec(t)
	if _, rerr := r(goagent.Context{Context: context.Background(), SessionID: "s2"}, ReadInput{FilePath: p}); rerr != nil {
		t.Fatal(rerr)
	}
	out, err := editExec(t)(goagent.Context{Context: context.Background(), SessionID: "s2"}, EditInput{
		FilePath: p, OldString: "bbb", NewString: "xxx",
	})
	if err != nil || !strings.Contains(out, "已替换") {
		t.Fatalf("读后 Edit 应成功: %v %q", err, out)
	}
	// Edit 使指纹失效 → 下次 Read 不短路
	if out2, _ := r(goagent.Context{Context: context.Background(), SessionID: "s2"}, ReadInput{FilePath: p}); strings.Contains(out2, "未修改") {
		t.Errorf("Edit 后 Read 不应短路: %q", out2)
	}
}

// Edit 失配诊断：空白差异时给出具体原因。
func TestEditMissDiagnostic(t *testing.T) {
	_, p := setupFile(t, "    const apiKey = 'prod';\nnormal\n")
	ctx := goagent.Context{Context: context.Background(), SessionID: "s3"}
	r := readExec(t)
	_, _ = r(ctx, ReadInput{FilePath: p})
	// 模型常见错误：把缩进敲成 tab（内容语义相同但逐字符不同）
	_, err := editExec(t)(ctx, EditInput{
		FilePath: p, OldString: "\tconst apiKey = 'prod';", NewString: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "空白") {
		t.Fatalf("空白差异应给出诊断: %v", err)
	}
	// 完全不存在的文本 → 提示重读
	_, err = editExec(t)(ctx, EditInput{
		FilePath: p, OldString: "this text never exists", NewString: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "重新 Read") {
		t.Fatalf("未命中应提示重读: %v", err)
	}
}

// Write 覆盖保护：未读过的已存在文件拒绝覆盖；新文件直接写。
func TestWriteOverwriteProtection(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := writeExec(t)
	ctx := goagent.Context{Context: context.Background(), SessionID: "s4"}
	// 未读 → 拒绝
	_, err := w(ctx, WriteInput{FilePath: existing, Content: "new"})
	if err == nil || !strings.Contains(err.Error(), "未读取过") {
		t.Fatalf("未读覆盖应被拒绝: %v", err)
	}
	// 读后 → 允许
	r := readExec(t)
	_, _ = r(ctx, ReadInput{FilePath: existing})
	if _, err := w(ctx, WriteInput{FilePath: existing, Content: "new"}); err != nil {
		t.Fatalf("读后覆盖应成功: %v", err)
	}
	// 新文件 → 直接写
	fresh := filepath.Join(dir, "fresh.txt")
	if _, err := w(ctx, WriteInput{FilePath: fresh, Content: "first"}); err != nil {
		t.Fatalf("新文件写入不应要求先读: %v", err)
	}
}

// 会话隔离：A 会话读过不影响 B 会话的校验状态。
func TestReadStateSessionIsolation(t *testing.T) {
	_, p := setupFile(t, "content\n")
	r := readExec(t)
	_, _ = r(goagent.Context{Context: context.Background(), SessionID: "sa"}, ReadInput{FilePath: p})
	// B 会话没读过 → Edit 被拒
	if _, err := editExec(t)(goagent.Context{Context: context.Background(), SessionID: "sb"}, EditInput{
		FilePath: p, OldString: "content", NewString: "x",
	}); err == nil {
		t.Errorf("会话 B 不应继承会话 A 的读取状态")
	}
}
