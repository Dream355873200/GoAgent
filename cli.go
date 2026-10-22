package goagent

import (
	"fmt"
	"os"
)

// cliRunner 是 cli 包在 init 中注册的终端界面实现。根包不依赖终端界面库，
// 只有导入了 cli 包的程序才会链接它们。
var cliRunner func(*App)

// RegisterCLIRunner 由 cli 包调用，注册 RunCLI 使用的终端界面实现。
func RegisterCLIRunner(fn func(*App)) {
	cliRunner = fn
}

// RunCLI 启动交互式终端界面（REPL），阻塞直到用户退出。终端界面位于
// github.com/Dream355873200/GoAgent/cli 包：导入该包（或直接调用
// cli.Run(app)）后可用；未导入时在标准错误输出提示并返回。
func (a *App) RunCLI() {
	if cliRunner == nil {
		fmt.Fprintln(os.Stderr, `goagent: RunCLI 需要终端界面包：import _ "github.com/Dream355873200/GoAgent/cli"（或直接调用 cli.Run(app)）`)
		return
	}
	cliRunner(a)
}

// setAskUserCallbackFn 是由 builtin 包注入的 AskUser 回调设置函数。
// 通过延迟注入避免 goagent→builtin→goagent 的循环依赖。
var setAskUserCallbackFn func(fn func(string) (string, error))

// RegisterSetAskUserCallback 由 builtin 包调用以注册 AskUser 回调设置函数。
func RegisterSetAskUserCallback(fn func(func(string) (string, error))) {
	setAskUserCallbackFn = fn
}

// SetAskUserCallback 设置内置 AskUser 工具在没有 AskUserHandler 时使用的提问
// 回调（例如终端界面的输入框）。没有导入 builtin 包时返回 false。
func SetAskUserCallback(fn func(question string) (string, error)) bool {
	if setAskUserCallbackFn == nil {
		return false
	}
	setAskUserCallbackFn(fn)
	return true
}
