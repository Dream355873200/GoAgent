package tui

// 本文件实现 TUI 与 agent goroutine 之间的通道桥接。
//
// 两种桥接模式：
//   - TUIApprover: 权限审批（agent 调 Approve → TUI 渲染审批框 → 用户按键回复）
//   - TUIAsker: AskUser 工具（agent 调 Ask → TUI 渲染提问框 → 用户输入回答）
//
// 注意：tui 包不能 import goagent（循环依赖），所以使用 int 表示权限级别。
// 适配层在 goagent/cli.go 中完成。

// PermLevel 是权限级别（与 goagent.Permission 对应）。
// 0=ReadOnly, 1=Normal, 2=RequireApproval, 3=Dangerous。
type PermLevel int

const (
	PermReadOnly        PermLevel = 0
	PermNormal          PermLevel = 1
	PermRequireApproval PermLevel = 2
	PermDangerous       PermLevel = 3
)

// ── 权限审批桥 ──────────────────────────────────────────────────────

// ApprovalRequest 是从 agent goroutine 发送到 TUI 的审批请求。
type ApprovalRequest struct {
	ToolName string
	Input    string
	Perm     PermLevel
}

// ApprovalResponse 是 TUI 返回给 agent goroutine 的审批结果。
type ApprovalResponse struct {
	Allowed bool
	Always  bool
}

// TUIApprover 通过通道桥接审批请求到 Bubble Tea。
// agent goroutine 调用 Approve() 时阻塞，TUI 主线程渲染审批 UI 后通过 Respond() 回复。
type TUIApprover struct {
	reqCh  chan ApprovalRequest
	respCh chan ApprovalResponse
}

// NewApprover 创建一个新的 TUI 审批者。
func NewApprover() *TUIApprover {
	return &TUIApprover{
		reqCh:  make(chan ApprovalRequest, 1),
		respCh: make(chan ApprovalResponse, 1),
	}
}

// Approve 发送审批请求并阻塞等待回复。
// 由 goagent/cli.go 中的适配器调用（将 goagent.Permission 转为 PermLevel）。
func (a *TUIApprover) Approve(toolName string, input string, perm PermLevel) (bool, bool) {
	a.reqCh <- ApprovalRequest{
		ToolName: toolName,
		Input:    input,
		Perm:     perm,
	}
	resp := <-a.respCh
	return resp.Allowed, resp.Always
}

// RequestCh 返回审批请求通道，供 Bubble Tea 监听。
func (a *TUIApprover) RequestCh() <-chan ApprovalRequest {
	return a.reqCh
}

// Respond 由 TUI 主线程调用，向 agent goroutine 发送审批结果。
func (a *TUIApprover) Respond(allowed, always bool) {
	a.respCh <- ApprovalResponse{Allowed: allowed, Always: always}
}

// ── AskUser 桥 ──────────────────────────────────────────────────────

// AskUserRequest 是从 agent goroutine 发送到 TUI 的提问请求。
type AskUserRequest struct {
	Question string
}

// AskUserResponse 是 TUI 返回给 agent goroutine 的回答。
type AskUserResponse struct {
	Answer string
}

// TUIAsker 桥接 AskUser 工具到 TUI 界面。
// agent goroutine 调用 Ask() 时阻塞，TUI 主线程渲染提问 UI 后通过 Respond() 回复。
type TUIAsker struct {
	reqCh  chan AskUserRequest
	respCh chan AskUserResponse
}

// NewAsker 创建一个新的 TUI 提问者。
func NewAsker() *TUIAsker {
	return &TUIAsker{
		reqCh:  make(chan AskUserRequest, 1),
		respCh: make(chan AskUserResponse, 1),
	}
}

// Ask 在 agent goroutine 中调用，阻塞直到 TUI 侧回复。
// 此方法作为回调注册到 builtin.SetAskUserCallback()。
func (a *TUIAsker) Ask(question string) (string, error) {
	a.reqCh <- AskUserRequest{Question: question}
	resp := <-a.respCh
	if resp.Answer == "" {
		return "(用户未回答)", nil
	}
	return resp.Answer, nil
}

// RequestCh 返回提问请求通道，供 Bubble Tea 监听。
func (a *TUIAsker) RequestCh() <-chan AskUserRequest {
	return a.reqCh
}

// Respond 由 TUI 主线程调用，向 agent goroutine 发送回答。
func (a *TUIAsker) Respond(answer string) {
	a.respCh <- AskUserResponse{Answer: answer}
}
