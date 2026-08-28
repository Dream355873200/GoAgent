package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent"
)

// 内置工具的沙箱接线测试：Write/Read/Edit 经 ctx.Sandbox 做路径
// 重写与策略检查；Bash 的 cmd.Dir 落沙箱根 + 策略 Timeout 上限。
// 手工构造 goagent.Context{Sandbox: ...}（sandboxOf 双路提取的字段路径）。

func sbCtx(sess goagent.SandboxSession) goagent.Context {
	return goagent.Context{Context: context.Background(), SessionID: "sb-test", Sandbox: sess}
}

func newSandboxSession(t *testing.T) (goagent.SandboxSession, string) {
	t.Helper()
	sess, err := goagent.NewDirSandbox(t.TempDir()).Enter(context.Background(), "test", goagent.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, sess.Root()
}

// Write：根外路径被沙箱拒绝；根内成功落盘。
func TestSandboxedWrite(t *testing.T) {
	sess, root := newSandboxSession(t)
	w := writeExec(t)

	// 根外（绝对路径）拒绝
	if _, err := w(sbCtx(sess), WriteInput{FilePath: filepath.Join(t.TempDir(), "x.txt"), Content: "x"}); err == nil {
		t.Fatal("根外写应被沙箱拒绝")
	}

	// `..` 逃逸拒绝
	if _, err := w(sbCtx(sess), WriteInput{FilePath: "../escape.txt", Content: "x"}); err == nil {
		t.Fatal("`..` 逃逸应被沙箱拒绝")
	}

	// 根内相对路径成功（新文件无需先 Read）
	out, err := w(sbCtx(sess), WriteInput{FilePath: "ok.txt", Content: "hello"})
	if err != nil {
		t.Fatalf("根内写应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("文件应落在沙箱根: %v", err)
	}
	if !strings.Contains(out, "已写入") {
		t.Logf("write output: %s", out)
	}
}

// Read + Edit：根内读写链路完整；只读策略下 Edit 被拒。
func TestSandboxedReadEdit(t *testing.T) {
	sess, root := newSandboxSession(t)
	ctx := sbCtx(sess)

	// 先写一个文件再读
	if _, err := writeExec(t)(ctx, WriteInput{FilePath: "note.txt", Content: "line1\nline2\n"}); err != nil {
		t.Fatal(err)
	}
	// 先写一个文件再读：写后读会短路返回「未修改」——短路前需要真实
	// 读取文件比对指纹，返回该消息即证明 Read 解析到了正确的沙箱路径。
	out, err := readExec(t)(ctx, ReadInput{FilePath: "note.txt"})
	if err != nil || !strings.Contains(out, "未修改") {
		t.Fatalf("根内读应成功（或短路）: %v / %.100s", err, out)
	}

	// Edit（已读过的文件）成功
	if _, err := editExec(t)(ctx, EditInput{FilePath: "note.txt", OldString: "line1", NewString: "LINE1"}); err != nil {
		t.Fatalf("根内 Edit 应成功: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "note.txt"))
	if !strings.Contains(string(raw), "LINE1") {
		t.Fatal("Edit 结果应落盘")
	}

	// 只读策略：Edit 拒绝、Read 放行
	roSess, err := goagent.NewDirSandbox(t.TempDir()).Enter(context.Background(), "ro",
		goagent.Policy{FS: []goagent.FSRule{{Path: "", Access: goagent.FSReadOnly}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = roSess.Close() })
	roCtx := sbCtx(roSess)
	roRoot := roSess.Root()
	os.WriteFile(filepath.Join(roRoot, "a.txt"), []byte("x"), 0o644)
	if _, err := readExec(t)(roCtx, ReadInput{FilePath: "a.txt"}); err != nil {
		t.Fatalf("只读策略下 Read 应放行: %v", err)
	}
	if _, err := editExec(t)(roCtx, EditInput{FilePath: "a.txt", OldString: "x", NewString: "y"}); err == nil {
		t.Fatal("只读策略下 Edit 应拒绝")
	}
}

// Bash：cmd.Dir 落沙箱根；策略 Timeout 生效（min 语义）。
func TestSandboxedBash(t *testing.T) {
	sess, root := newSandboxSession(t)
	ctx := sbCtx(sess)

	b := BashTool().Execute.(func(goagent.Context, BashInput) (string, error))

	// Windows: cmd /c cd 输出当前目录；Unix: pwd。
	cmd := "pwd"
	if goos := runtimeGOOS(); goos == "windows" {
		cmd = "cd"
	}
	out, err := b(ctx, BashInput{Command: cmd})
	if err != nil {
		t.Fatalf("Bash 应成功: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(out), root) {
		t.Fatalf("Bash 工作目录应为沙箱根 %q, got %q", root, out)
	}

	// 策略 Timeout：输入 10s 但上限 300ms → 按 300ms 超时。
	// （Windows 上杀掉 cmd 后子进程仍持有输出句柄，CombinedOutput 会等
	// 子进程自然退出——断言超时消息里的 300ms 而非墙钟。）
	slowSess, err := goagent.NewDirSandbox(t.TempDir()).Enter(context.Background(), "slow",
		goagent.Policy{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slowSess.Close() })
	out2, err := b(sbCtx(slowSess), BashInput{Command: sleepCmd(3), Timeout: 10000})
	combined := out2 + errString(err)
	if !strings.Contains(combined, "300ms") {
		t.Fatalf("策略 Timeout 未生效（应按 300ms 上限超时）: out=%.100s err=%v", out, err)
	}
}

func runtimeGOOS() string { return runtime.GOOS }

// errString nil 安全的错误字符串。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sleepCmd 平台对应的 sleep 命令。
func sleepCmd(seconds int) string {
	if runtime.GOOS == "windows" {
		return "ping -n " + itoa(seconds+1) + " 127.0.0.1 > nul"
	}
	return "sleep " + itoa(seconds)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
