package goagent

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 沙箱层（Sandbox）：工具执行边界的隔离抽象。
//
// Agent 对世界的全部作用力都经过工具调用——LLM 本身只会生成 token。
// 把路径解析与子进程派生这一个执行面拦住，单 agent、pipeline 节点、
// benchmark trial 三种模式自动全覆盖（都汇入 ToolDef.call 同一咽喉点，
// 沙箱会话经 context value 流到工具的 Context.Sandbox 字段）。
//
// 强度阶梯（Tier）：
//   Tier 0  无沙箱（默认，零开销）        —— 不配置即此，行为与历史版本一致
//   Tier 1  DirSandbox / WorktreeSandbox  —— 进程内路径前缀强制 + 工作副本
//   Tier 2  Docker / OS 级（未实现）      —— ExecSession 接口已预留
//   Tier 3  WASM / wazero（未实现）       —— 能力模型，L4 代码执行的地基
//
// Tier 1 防意外不防恶意：进程内前缀检查挡不住工具代码里的一句 os.Remove。
// 进程级隔离是宿主部署者的责任（容器/VM 跑不受信 workload）。
//
// 优先级规则：沙箱会话在场且 Root() 非空时，有效工作目录 = 沙箱根，
// 覆盖 WithSessionWorkDir。仅在传入 WithSandbox 时激活。
// ---------------------------------------------------------------------------

// FSOp 工具想要执行的文件操作类别。
type FSOp int

const (
	OpRead FSOp = iota
	OpWrite
)

// FSAccess 单个路径前缀的访问级别。
type FSAccess int

const (
	FSNone      FSAccess = iota // 显式拒绝
	FSReadOnly                  // 只读（写拒绝）
	FSReadWrite                 // 读写
)

// FSRule 一条路径规则。Path 为绝对路径；相对路径由沙箱按根归一化。
// 空 Path 在归一化后表示「沙箱根本身」。
type FSRule struct {
	Path   string
	Access FSAccess
}

// NetPolicy 网络策略。Tier 1 只携带、不强制（文档化行为）。
type NetPolicy int

const (
	NetDenyAll NetPolicy = iota // 默认：拒绝（Tier 1 不强制）
	NetAllowAll
)

// Policy 沙箱策略。
//
// FS 规则匹配：最长路径前缀优先；无匹配 = 拒绝（白名单语义）。
// 空 FS 由各沙箱实现归一化为「沙箱根读写」。
type Policy struct {
	// FS 路径白名单规则（最长前缀优先，无匹配拒绝）。
	FS []FSRule
	// Net 网络策略。Tier 1 携带不强制。
	Net NetPolicy
	// Env 环境变量白名单。Tier 1 携带不强制。
	Env []string
	// Timeout 单工具执行超时上限；0 = 不限。Bash 取 min(输入值, 此值)。
	Timeout time.Duration
}

// Sandbox 沙箱工厂。每次受约束的运行（一轮 App.Run / 一次 RunPipeline /
// 一个 benchmark trial / 一个节点级覆盖）调用一次 Enter 创建独立会话。
// 实现必须并发安全。
type Sandbox interface {
	// Enter 创建沙箱会话。sessionID 可为空（如匿名 run）。
	// policy 为空时由实现归一化（通常是「沙箱根读写」）。
	Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error)
}

// SandboxSession 一次受沙箱约束的运行期。生命周期由调用方管理
// （App.run / RunPipeline defer Close）。
type SandboxSession interface {
	// Root 沙箱根目录；空串 = 不重定向（Noop 语义）。
	Root() string

	// ResolvePath 把工具输入里的路径解析为沙箱内绝对路径并做策略检查。
	// 相对路径按沙箱根解析；绝对路径直接检查。op 区分读/写。
	// 违规返回面向 LLM 的可读错误（LLM 可据此改用合法路径重试）。
	ResolvePath(p string, op FSOp) (string, error)

	// CommandDir Bash/git 等子进程的工作目录（通常 = Root）。
	CommandDir() string

	// Policy 当前会话策略（供 Bash 超时上限等读取）。
	Policy() Policy

	// Close 释放会话资源（删临时目录/worktree）。幂等友好。
	Close() error
}

// ExecSession 可选扩展接口（Tier 2+ 预留）：沙箱内执行命令。
// Bash 工具 type-assert 此接口；沙箱未实现时回退本地 exec + CommandDir()。
// Docker/容器沙箱实现此接口后，Bash 命令自动路由进沙箱。
type ExecSession interface {
	Exec(ctx context.Context, name string, args []string, dir string) ([]byte, error)
}

// NoopSandbox 显式选用的透传沙箱（效果等价于不配置）。
var NoopSandbox Sandbox = noopSandbox{}

type noopSandbox struct{}

func (noopSandbox) Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error) {
	return noopSession{}, nil
}

type noopSession struct{}

func (noopSession) Root() string                            { return "" }
func (noopSession) CommandDir() string                      { return "" }
func (noopSession) Policy() Policy                          { return Policy{} }
func (noopSession) Close() error                            { return nil }
func (noopSession) ResolvePath(p string, op FSOp) (string, error) { return p, nil }

// ---------------------------------------------------------------------------
// 策略匹配
// ---------------------------------------------------------------------------

// matchFS 返回路径 p 在规则集下的访问级别。最长路径前缀优先，
// 无匹配返回 FSNone（白名单语义：没写到的就是拒绝的）。
// Windows 路径大小写不敏感（按系统卷语义近似为全路径折叠）。
func matchFS(rules []FSRule, p string) FSAccess {
	best := FSNone
	bestLen := -1
	for _, r := range rules {
		if pathContains(r.Path, p) && len(r.Path) > bestLen {
			best = r.Access
			bestLen = len(r.Path)
		}
	}
	return best
}

// pathContains 报告子路径 sub 是否位于目录 dir 之下（含相等）。
// 词法判断：Clean 后用 filepath.Rel；`..` 逃逸 = 不包含。
func pathContains(dir, sub string) bool {
	dir, sub = filepath.Clean(dir), filepath.Clean(sub)
	if runtime.GOOS == "windows" {
		dir, sub = strings.ToLower(dir), strings.ToLower(sub)
	}
	rel, err := filepath.Rel(dir, sub)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkPath 按策略检查单个操作，违规返回 LLM 可读错误。
func checkPath(rules []FSRule, p string, op FSOp) error {
	acc := matchFS(rules, p)
	switch {
	case acc == FSNone:
		return fmt.Errorf("sandbox: 拒绝访问 %s（路径不在沙箱允许范围内，可用根目录见 FS 规则）", p)
	case acc == FSReadOnly && op == OpWrite:
		return fmt.Errorf("sandbox: %s 是只读的，不允许写入", p)
	}
	return nil
}

// sanitizeName 把 sessionID 消毒成可用的目录名成分（保留字母数字-_.）。
func sanitizeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}
