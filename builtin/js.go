package builtin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent"
	"github.com/dop251/goja"
)

// ---------------------------------------------------------------------------
// L4-α：goja 执行器（run_js 工具）。
//
// 给 agent 真正的代码执行能力，但不开放宿主进程——LLM 生成的 JS 源码
// 进 goja 解释器运行，宿主不可穿透：
//
//   - 纯计算形态（无沙箱）：零能力注入。通往 os/fs/net 的调用路径在
//     运行时里物理不存在（区别于 Tier 1 的「检查后放行」）——JS 只能
//     解析/变换/聚合/生成数据，对 benchmark trial 最常用。
//   - 能力注入形态（沙箱在场）：注入 4 个文件 host 函数，全部经沙箱
//     ResolvePath 双锁——能力与射程同时受限（解释器万一有逃逸 bug，
//     拿到的也只是被 Tier 1 Policy 约束的环境）。
//
// 资源限额（进程内模式的唯一真实弱点，必做）：
//   - 超时：vm.Interrupt 强杀死循环；上限受沙箱 Policy.Timeout 约束
//   - 输出大小：MaxResultSizeChars 截断
//
// 失败也是产出：编译/运行错误带 JS 栈结构化回传，喂回
// 「生成 → 执行 → 报错 → 修正」内循环（benchmark 自迭代的地基）。
// ---------------------------------------------------------------------------

// RunJSInput 是 run_js 工具的输入。
type RunJSInput struct {
	Code      string `json:"code" desc:"要执行的 JavaScript 源码（ES5.1+ 大部分 ES6）。顶层 return 或最后一条语句的值作为结果返回" required:"true"`
	TimeoutMs int    `json:"timeout_ms,omitempty" desc:"执行超时毫秒数，超时被强制终止。默认 10000；沙箱策略有更小上限时以策略为准"`
}

// runJSTimeoutDefault 默认执行超时（防 DoS：沙箱不在场时没有策略兜底）。
const runJSTimeoutDefault = 10_000

// runJSMaxTimeout 单次执行允许的最大超时（输入值再大也被压到这）。
const runJSMaxTimeout = 300_000

// RunJSTool 返回 JS 沙箱执行工具定义。
func RunJSTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "在 JS 沙箱（goja 解释器）里执行一段 JavaScript 代码并返回结果。" +
			"适用于数据处理/解析/变换/聚合/生成文本——比用 Bash 管道更快更可控，错误带行号和调用栈可自我修正。" +
			"支持 console.log（输出附在结果后）。用顶层 return 返回结果（没有 return 时结果是 undefined）。" +
			"默认没有任何文件/网络能力（纯计算）；" +
			"沙箱会话在场时自动注入 readFile/writeFile/listDir/stat 四个函数，路径被沙箱策略约束。" +
			"死循环/超时会被强制终止。",
		Input:              RunJSInput{},
		Permission:         goagent.Normal,
		Concurrent:         true,
		MaxResultSizeChars: 100_000,
		Execute: func(ctx goagent.Context, in RunJSInput) (string, error) {
			return executeRunJS(ctx, in)
		},
	}
}

// executeRunJS run_js 的执行体（测试直接调用）。
func executeRunJS(ctx goagent.Context, in RunJSInput) (string, error) {
	if strings.TrimSpace(in.Code) == "" {
		return "", fmt.Errorf("code 不能为空")
	}

	timeout := in.TimeoutMs
	if timeout <= 0 {
		timeout = runJSTimeoutDefault
	}
	if timeout > runJSMaxTimeout {
		timeout = runJSMaxTimeout
	}
	// 沙箱策略上限压顶：Policy.Timeout > 0 且更小时以策略为准（对齐 Bash 工具）。
	if sb := sandboxOf(ctx); sb != nil {
		if pt := sb.Policy().Timeout; pt > 0 && pt < time.Duration(timeout)*time.Millisecond {
			timeout = int(pt / time.Millisecond)
		}
	}

	vm := goja.New()

	// console 捕获：收集到 slice，随结果一起返回（替代 stdout——沙箱里没有标准流概念）。
	var consoleLogs []string
	_ = vm.Set("console", map[string]func(goja.FunctionCall) goja.Value{
		"log":   consoleSink(&consoleLogs),
		"warn":  consoleSink(&consoleLogs),
		"error": consoleSink(&consoleLogs),
		"info":  consoleSink(&consoleLogs),
		"debug": consoleSink(&consoleLogs),
	})

	// 能力注入：仅当沙箱会话在场。注入的 host 函数全部经 sb.ResolvePath
	// 双锁（能力与射程），沙箱不在场 = 一个能力都不注入（能力不存在而非被拦截）。
	if sb := sandboxOf(ctx); sb != nil {
		injectFSCapabilities(vm, sb)
	}

	// 超时强杀：到点 vm.Interrupt，RunProgram 返回可识别的 interrupt 错误。
	interruptReason := fmt.Sprintf("执行超时（%dms），代码被强制终止——检查死循环或改小算法复杂度", timeout)
	timer := time.AfterFunc(time.Duration(timeout)*time.Millisecond, func() {
		vm.Interrupt(interruptReason)
	})
	defer timer.Stop()

	// 包一层函数让顶层 return 合法（对齐 eval 语义：code 里可以直接写 return）。
	// 注意：JS 函数没有 return 语句时值恒为 undefined——「最后一条语句的值」
	// 是 REPL 完成值语义，函数调用没有，文档明确要求显式 return。
	prog, err := goja.Compile("", "(function(){"+in.Code+"\n})", false)
	if err != nil {
		// 编译错误（*goja.CompilerException）自带行号，直接回传给 LLM 修正。
		return "", fmt.Errorf("JS 编译错误: %w", err)
	}

	v, err := vm.RunProgram(prog)
	if err != nil {
		return "", formatJSError(err, interruptReason)
	}

	// RunProgram 跑的是 program body——函数体 program 返回函数对象本身，
	// 再显式调用取返回值（goja 标准用法：AssertFunction + call）。
	fn, ok := goja.AssertFunction(v)
	if !ok {
		return "", fmt.Errorf("JS 执行失败: 包装函数断言失败（不应发生）")
	}
	v, err = fn(goja.Undefined())
	if err != nil {
		return "", formatJSError(err, interruptReason)
	}

	// 结果序列化：undefined/null → 字面量；其余走 JSON（对象/数组/基本类型统一）。
	var resultStr string
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		resultStr = "undefined"
	} else if s, ok := v.Export().(string); ok {
		resultStr = s
	} else {
		b, mErr := json.Marshal(v.Export())
		if mErr != nil {
			resultStr = fmt.Sprintf("%v", v.Export())
		} else {
			resultStr = string(b)
		}
	}

	var b strings.Builder
	b.WriteString("结果: ")
	b.WriteString(resultStr)
	if len(consoleLogs) > 0 {
		b.WriteString("\n\nconsole 输出:\n")
		b.WriteString(strings.Join(consoleLogs, "\n"))
	}
	return b.String(), nil
}

// formatJSError 把 goja 的错误类型翻译成 LLM 可读的中文 + JS 栈，
// 供「生成 → 执行 → 报错 → 修正」内循环消费。
func formatJSError(err error, interruptReason string) error {
	// 超时强杀：vm.Interrupt 的 reason 原样包在 InterruptedError 里。
	if ie, ok := err.(*goja.InterruptedError); ok {
		if v, _ := ie.Value().(string); strings.Contains(v, "执行超时") {
			return fmt.Errorf("JS %s", v)
		}
		return fmt.Errorf("JS 被强制中断: %v", ie.Value())
	}
	if ce, ok := err.(*goja.CompilerSyntaxError); ok {
		return fmt.Errorf("JS 编译错误（含行号，可直接据此修正）: %s", ce.Error())
	}
	if ge, ok := err.(*goja.Exception); ok {
		return fmt.Errorf("JS 运行错误（含调用栈）: %s", ge.String())
	}
	return fmt.Errorf("JS 执行失败: %w", err)
}

// consoleSink 构造捕获到 slice 的 console.* 函数。
func consoleSink(sink *[]string) func(goja.FunctionCall) goja.Value {
	return func(fc goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(fc.Arguments))
		for _, arg := range fc.Arguments {
			parts = append(parts, jsValueToString(arg))
		}
		*sink = append(*sink, strings.Join(parts, " "))
		return goja.Undefined()
	}
}

// jsValueToString console 参数的可读化（对齐浏览器行为）。
func jsValueToString(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "undefined"
	}
	if s, ok := v.Export().(string); ok {
		return s
	}
	if b, err := json.Marshal(v.Export()); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v.Export())
}

// injectFSCapabilities 注入文件 host 函数（全部套沙箱射程）。
// 函数签名对齐 Node 风格命名但同步执行；路径违规返回 JS Error（LLM 可读）。
func injectFSCapabilities(vm *goja.Runtime, sb goagent.SandboxSession) {
	_ = vm.Set("readFile", func(call goja.FunctionCall) goja.Value {
		return jsWrapFS(vm, call, func(p string) (any, error) {
			abs, err := sb.ResolvePath(p, goagent.OpRead)
			if err != nil {
				return nil, err
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("无法读取 %s: %w", p, err)
			}
			return string(raw), nil
		})
	})

	_ = vm.Set("writeFile", func(call goja.FunctionCall) goja.Value {
		return jsWrapFS(vm, call, func(p string) (any, error) {
			if len(call.Arguments) < 2 {
				return nil, fmt.Errorf("writeFile(path, content) 需要 2 个参数")
			}
			content := jsValueToString(call.Arguments[1])
			abs, err := sb.ResolvePath(p, goagent.OpWrite)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return nil, fmt.Errorf("无法创建目录: %w", err)
			}
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				return nil, fmt.Errorf("无法写入 %s: %w", p, err)
			}
			return nil, nil // undefined = 成功（对齐 Node fs.writeFileSync）
		})
	})

	_ = vm.Set("listDir", func(call goja.FunctionCall) goja.Value {
		return jsWrapFS(vm, call, func(p string) (any, error) {
			abs, err := sb.ResolvePath(p, goagent.OpRead)
			if err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(abs)
			if err != nil {
				return nil, fmt.Errorf("无法列出 %s: %w", p, err)
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			return names, nil
		})
	})

	_ = vm.Set("stat", func(call goja.FunctionCall) goja.Value {
		return jsWrapFS(vm, call, func(p string) (any, error) {
			abs, err := sb.ResolvePath(p, goagent.OpRead)
			if err != nil {
				return nil, err
			}
			fi, err := os.Stat(abs)
			if err != nil {
				return nil, nil // 不存在 = null（对齐 Node fs.statSync 会 throw，这里温和一点）
			}
			return map[string]any{
				"name":  filepath.Base(abs),
				"size":  fi.Size(),
				"isDir": fi.IsDir(),
				"mode":  fi.Mode().String(),
			}, nil
		})
	})
}

// jsWrapFS host 函数的统一包装：首参取路径 → 执行 → 错误 panic 成 JS
// GoError（goja 约定：host 函数 panic Value = JS 异常，RunProgram 以
// *goja.Exception 返回，栈带 JS 调用点，LLM 可定位）；返回值透传给 JS。
func jsWrapFS(vm *goja.Runtime, call goja.FunctionCall, fn func(p string) (any, error)) goja.Value {
	if len(call.Arguments) < 1 {
		panic(vm.NewTypeError("需要至少 1 个参数（路径）"))
	}
	p, ok := call.Arguments[0].Export().(string)
	if !ok {
		panic(vm.NewTypeError("第一个参数必须是字符串路径"))
	}
	out, err := fn(p)
	if err != nil {
		panic(vm.NewGoError(err))
	}
	if out == nil {
		return goja.Undefined()
	}
	return vm.ToValue(out)
}

// JSCapabilityKit 返回 JS 沙箱执行工具包（run_js）。
// 需要文件能力时与沙箱层组合使用（WithSandbox）；无沙箱 = 纯计算形态。
func JSCapabilityKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "JSCapabilityKit",
		Description: "JS 沙箱执行工具包（run_js：goja 解释器，默认纯计算，沙箱在场时注入文件能力）",
	}.WithTools(
		goagent.NamedTool{Name: "run_js", Def: RunJSTool()},
	)
}
