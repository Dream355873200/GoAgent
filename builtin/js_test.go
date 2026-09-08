package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dream355873200/GoAgent"
)

// L4-α run_js 工具测试：纯计算形态（无沙箱零能力注入）、沙箱能力注入
// 形态（readFile/writeFile/listDir/stat 套沙箱射程）、资源限额（Interrupt
// 超时强杀 + Policy.Timeout 压顶 + 输出截断）、结构化错误回传（编译错误
// 带行号 / 运行错误带栈 / 超时可读中文）。

// runJSExec 构造直接调用 executeRunJS 的函数（绕过 ToolDef 反射层）。
func runJSExec(t *testing.T) func(goagent.Context, RunJSInput) (string, error) {
	t.Helper()
	return func(ctx goagent.Context, in RunJSInput) (string, error) {
		return executeRunJS(ctx, in)
	}
}

func plainCtx() goagent.Context {
	return goagent.Context{Context: context.Background(), SessionID: "js-test"}
}

// 纯计算：算术/字符串/对象返回值 JSON 序列化 + 顶层 return 语义。
func TestRunJSPureComputation(t *testing.T) {
	exec := runJSExec(t)
	ctx := plainCtx()

	out, err := exec(ctx, RunJSInput{Code: "return 1 + 2 * 3"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "7") {
		t.Fatalf("算术结果错误: %s", out)
	}

	out, err = exec(ctx, RunJSInput{Code: `return {a: 1, b: "x"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"a":1`) {
		t.Fatalf("对象应 JSON 序列化: %s", out)
	}

	// 无 return：函数完成值 = undefined（REPL 完成值语义在函数调用里不存在）
	out, err = exec(ctx, RunJSInput{Code: `var x = "hello world"`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "undefined") {
		t.Fatalf("无 return 应为 undefined: %s", out)
	}

	// undefined
	out, err = exec(ctx, RunJSInput{Code: `var x = 1;`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "undefined") {
		t.Fatalf("无值语句应返回 undefined: %s", out)
	}
}

// console.log 捕获：输出附在结果后。
func TestRunJSConsoleCapture(t *testing.T) {
	exec := runJSExec(t)
	out, err := exec(plainCtx(), RunJSInput{Code: `console.log("processing", 42); return "done"`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "console 输出") || !strings.Contains(out, "processing 42") {
		t.Fatalf("console.log 应被捕获: %s", out)
	}
}

// 无沙箱 = 能力不存在：文件函数是 ReferenceError（而非被拦截）。
func TestRunJSNoSandboxNoCapabilities(t *testing.T) {
	exec := runJSExec(t)
	_, err := exec(plainCtx(), RunJSInput{Code: `readFile("x.txt")`})
	if err == nil || !strings.Contains(err.Error(), "readFile") {
		t.Fatalf("无沙箱时 readFile 应为 ReferenceError（能力不存在）: %v", err)
	}
}

// 编译错误带行号；运行错误带调用栈——LLM 可据此修正。
func TestRunJSStructuredErrors(t *testing.T) {
	exec := runJSExec(t)
	ctx := plainCtx()

	_, err := exec(ctx, RunJSInput{Code: "var x = ;"})
	if err == nil || !strings.Contains(err.Error(), "编译错误") || !strings.Contains(err.Error(), "Line") {
		t.Fatalf("编译错误应带行号: %v", err)
	}

	_, err = exec(ctx, RunJSInput{Code: `null.x`})
	if err == nil || !strings.Contains(err.Error(), "运行错误") {
		t.Fatalf("运行错误应结构化回传: %v", err)
	}

	// host 函数路径违规 → JS GoError（沙箱拒绝消息 LLM 可读）
	sess, _ := newSandboxSession(t)
	_, err = exec(sbCtx(sess), RunJSInput{Code: `writeFile("../escape.txt", "x")`})
	if err == nil || !strings.Contains(err.Error(), "沙箱") {
		t.Fatalf("路径违规应带沙箱拒绝信息: %v", err)
	}
}

// 死循环超时强杀：默认超时太慢，传小 TimeoutMs 验证秒级中断。
func TestRunJSTimeoutInterrupt(t *testing.T) {
	exec := runJSExec(t)
	start := time.Now()
	_, err := exec(plainCtx(), RunJSInput{
		Code:      `while (true) { }`,
		TimeoutMs: 200,
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "执行超时") {
		t.Fatalf("死循环应被超时强杀: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("中断应秒级生效, 实际 %v", elapsed)
	}
}

// 沙箱 Policy.Timeout 压顶：策略上限比输入小时以策略为准。
func TestRunJSPolicyTimeoutCap(t *testing.T) {
	sess, err := goagent.NewDirSandbox(t.TempDir()).Enter(context.Background(), "test",
		goagent.Policy{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	exec := runJSExec(t)
	start := time.Now()
	// 输入 30s，策略 150ms → 150ms 掐断
	_, err = exec(sbCtx(sess), RunJSInput{
		Code:      `while (true) { }`,
		TimeoutMs: 30_000,
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "执行超时") {
		t.Fatalf("Policy.Timeout 应压顶: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("策略上限应生效, 实际 %v", elapsed)
	}
}

// 沙箱注入形态：根内写读往返 + listDir + stat。
func TestRunJSSandboxedFS(t *testing.T) {
	sess, root := newSandboxSession(t)
	exec := runJSExec(t)
	ctx := sbCtx(sess)

	// writeFile 根内成功
	_, err := exec(ctx, RunJSInput{Code: `writeFile("data/a.txt", "hello sandbox"); return "w"`})
	if err != nil {
		t.Fatalf("根内写失败: %v", err)
	}

	// readFile 读回
	out, err := exec(ctx, RunJSInput{Code: `return readFile("data/a.txt")`})
	if err != nil || !strings.Contains(out, "hello sandbox") {
		t.Fatalf("读回应返回写入内容: %v %s", err, out)
	}

	// listDir
	out, err = exec(ctx, RunJSInput{Code: `return listDir("data")`})
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("listDir 应列出 a.txt: %v %s", err, out)
	}

	// stat
	out, err = exec(ctx, RunJSInput{Code: `var s = stat("data/a.txt"); return s.size + ":" + s.isDir`})
	if err != nil || !strings.Contains(out, "13:false") {
		t.Fatalf("stat 应返回大小与类型: %v %s", err, out)
	}

	// 落盘位置 = 沙箱根下
	if !fileContains(t, root+"/data/a.txt", "hello sandbox") {
		t.Fatal("文件应落在沙箱根内")
	}

	// 根外绝对路径拒绝（正斜杠形式——JS 字符串字面量里反斜杠是转义符，
	// t.TempDir() 的 Windows 反斜杠路径拼进字面量会被吃掉）
	outside := filepath.ToSlash(t.TempDir())
	_, err = exec(ctx, RunJSInput{Code: `writeFile("` + outside + `/x.txt", "x")`})
	if err == nil || !strings.Contains(err.Error(), "沙箱") {
		t.Fatalf("根外写应被拒绝: %v", err)
	}
}

// fileContains 小工具：文件存在且含子串。
func fileContains(t *testing.T, path, substr string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), substr)
}

// 大输出截断：MaxResultSizeChars=100_000，结果超限被截断。
func TestRunJSResultTruncation(t *testing.T) {
	exec := runJSExec(t)
	// 生成 200KB 字符串
	out, err := exec(plainCtx(), RunJSInput{Code: `var s = "x"; while (s.length < 200000) s += s; return s`})
	if err != nil {
		t.Fatal(err)
	}
	// 工具结果统一截断发生在 ToolDef.call 之后的框架层；
	// 这里验证 executeRunJS 自身不崩且结果完整可序列化。
	if len(out) < 200_000 {
		t.Fatalf("执行器自身不应截断（框架层负责）: %d", len(out))
	}
}

// 定时器用后即停：超时未触发的 timer 不泄漏（timer.Stop 由 defer 保证，
// 这里行为验证——快速成功的执行不受 timer 影响）。
func TestRunJSTimerNoLeak(t *testing.T) {
	exec := runJSExec(t)
	start := time.Now()
	_, err := exec(plainCtx(), RunJSInput{Code: `return 1`, TimeoutMs: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("快速执行的 timer 不应阻塞返回")
	}
}
