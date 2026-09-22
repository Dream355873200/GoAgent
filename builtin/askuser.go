package builtin

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/Dream355873200/GoAgent"
)

// AskUserInput 是 AskUser 工具的输入。
type AskUserInput struct {
	Question string `json:"question" desc:"要向用户提问的问题" required:"true"`
}

// askUserCallback 是可选的外部回调，TUI 模式下由 tui.TUIAsker.Ask 替换。
// 默认 nil 时 fallback 到 stdin 直接读取。
var askUserCallback func(question string) (string, error)

// askUserCallbackCtx 是 ctx 感知的提问回调（优先级高于 askUserCallback）。
// 多 App / 多会话宿主（HTTP 服务等）用它拿 Context.SessionID 做答案路由——
// 全局单槽回调无法区分是哪个会话在提问。
var askUserCallbackCtx func(goagent.Context, string) (string, error)

// SetAskUserCallback 设置 AskUser 工具的外部回调。
// TUI 模式下调用此函数注册通道桥接，替换默认的 stdin 读取。
func SetAskUserCallback(fn func(string) (string, error)) {
	askUserCallback = fn
}

// SetAskUserCallbackCtx 设置 ctx 感知的提问回调（优先于 SetAskUserCallback）。
// 第二个参数是用户对提问的回答文本。
func SetAskUserCallbackCtx(fn func(goagent.Context, string) (string, error)) {
	askUserCallbackCtx = fn
}

// AskUserTool 返回一个允许 LLM 主动向用户提问的工具。
// 当 agent 需要澄清、确认或用户输入时使用。
func AskUserTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "向用户提问并等待回答。当你需要澄清需求、确认操作或获取用户输入时使用。",
		Input:       AskUserInput{},
		Permission:  goagent.ReadOnly,
		Execute: func(ctx goagent.Context, in AskUserInput) (string, error) {
			return executeAskUser(ctx, in)
		},
	}
}

func executeAskUser(ctx goagent.Context, in AskUserInput) (string, error) {
	if in.Question == "" {
		return "", fmt.Errorf("question 不能为空")
	}

	// ctx 感知回调优先（多会话宿主按 SessionID 路由答案）。
	if askUserCallbackCtx != nil {
		return askUserCallbackCtx(ctx, in.Question)
	}

	// 其次使用外部回调（TUI 模式）。
	if askUserCallback != nil {
		return askUserCallback(in.Question)
	}

	// Fallback: 直接从 stdin 读取（SDK/headless 模式）。
	fmt.Printf("\n─── Agent 提问 ───\n%s\n> ", in.Question)

	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("读取用户输入失败: %w", err)
	}

	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "(用户未回答)", nil
	}
	return answer, nil
}
