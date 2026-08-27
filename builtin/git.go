// git.go Git 版本快照工具：应用层通过工具让 agent 拥有「存档」能力。
// 提交粒度由应用层规范约束（如：每个功能完成、测试全绿后提交）。
package builtin

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent"
)

type gitCommitInput struct {
	Message string `json:"message" desc:"提交说明（祈使句，如：完成登录页）" required:"true"`
	// Add 可选：额外指定要暂存的路径（默认 -A 全部变更）
	Add string `json:"add,omitempty" desc:"额外暂存路径（可选，默认暂存全部变更）"`
}

// GitCommitTool 在会话工作目录执行 git add -A + commit。
// 没有变更时明确告知（非错误）——上层规范据此避免空提交。
func GitCommitTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "在当前项目目录创建一个 git 提交（git add -A + commit）。版本快照：完成一个功能、" +
			"修复一个缺陷、测试全绿后调用，让每个里程碑都有可回退的存档点。没有变更时返回「无变更」。",
		Input:      gitCommitInput{},
		Permission: goagent.Normal, // 版本历史是有分量的写操作
		Concurrent: false,
		Execute: func(ctx goagent.Context, in gitCommitInput) (string, error) {
			if strings.TrimSpace(in.Message) == "" {
				return "", errGitMsgEmpty
			}
			dir := workDirOrDot(ctx)
			parent := ctx.Context
			if parent == nil {
				parent = context.Background()
			}
			cctx, cancel := context.WithTimeout(parent, 30*time.Second)
			defer cancel()

			run := func(args ...string) (string, error) {
				cmd := exec.CommandContext(cctx, "git", args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				return strings.TrimRight(string(out), "\r\n "), err
			}

			// 仓库不存在（git init 未做）→ 明确报错而不是含糊失败
			if out, err := run("rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
				return "", errNoRepo
			}

			// 有无可变更（无变更直接返回，避免空提交）
			if out, _ := run("status", "--porcelain"); strings.TrimSpace(out) == "" {
				return "工作区无变更，无需提交", nil
			}

			if in.Add != "" {
				if _, err := run("add", in.Add); err != nil {
					return "", errGitRun("git add", err)
				}
			} else if _, err := run("add", "-A"); err != nil {
				return "", errGitRun("git add", err)
			}
			if _, err := run("commit", "-m", in.Message); err != nil {
				return "", errGitRun("git commit", err)
			}
			head, _ := run("rev-parse", "--short", "HEAD")
			return "已提交 " + strings.TrimSpace(head) + ": " + in.Message, nil
		},
	}
}

func workDirOrDot(ctx goagent.Context) string { return ctx.WorkDir }

type gitErr string

func (e gitErr) Error() string { return string(e) }

const (
	errGitMsgEmpty gitErr = "message 不能为空"
	errNoRepo      gitErr = "当前目录不是 git 仓库（项目初始化时应做 git init）"
)

func errGitRun(step string, err error) error {
	return gitErr(step + " 失败: " + err.Error())
}
