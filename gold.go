// gold.go GoldReplayer（B2）：期望终态用参考轨迹回放派生，不手写。
//
// SABER 教训（research.md §2.2）：τ-bench 被系统性批出 50+ 处手写
// 期望态标注错误（修复后 airline 涨 14-20 分——原版长期把「正确拒绝」
// 的 agent 判错）。手写期望态是标注错误重灾区；回放派生让期望终态
// 和真实工具语义永远一致（工具改了行为，重放一次就重新对齐）。
//
// 回放是「跑工具不跑模型」：直接把参考 ToolCall 序列逐条喂给工具
// 的 Execute（走 ToolDef.call 同一咽喉点——WorkDir/沙箱解析与真实
// agent 运行完全一致），跑完快照工作区。不走 LLM——gold 轨迹本身
// 就是「应该发生什么」的记录，再让模型自由发挥就引入了要测的噪声。
//
// 只比终态不比路径（等价路径全通过）：派生物是 State 快照，配合
// "state-equals" 断言使用——agent 用不同顺序/不同工具组合达到同一
// 终态照样过。
package goagent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dream355873200/GoAgent/benchmark"
)

// DeriveState 在 dir（应为空目录——隔离纪律：期望终态派生环境必须
// 与 trial 环境同构起步，复用脏目录会把脏文件拍进期望快照）回放
// 参考工具调用序列，返回工作区终态快照。
//
// calls 通常来自一次成功 run 的 TargetOutput.ToolCalls（人工审过
// 的 gold 轨迹）；tools 必须覆盖 calls 里出现的全部工具名。
// 回放中任一工具报错即返回 error——gold 轨迹跑不通说明轨迹或工具
// 装配坏了，派生的期望终态不可信。
func DeriveState(ctx context.Context, dir string, tools []NamedTool, calls []benchmark.ToolCall) (map[string]string, error) {
	idx := make(map[string]*ToolDef, len(tools))
	for i := range tools {
		idx[tools[i].Name] = &tools[i].Def
	}
	// WorkDir 注入走 WithSessionContext（与 App.run 同一机制）——
	// 工具的相对路径解析/沙箱行为与真实 agent 运行一致。
	ctx = WithSessionContext(ctx, "gold-replay", dir)
	for i, c := range calls {
		def, ok := idx[c.Name]
		if !ok {
			return nil, fmt.Errorf("goagent: gold 轨迹[%d] 用到工具 %q，但 tools 里没有", i, c.Name)
		}
		if _, err := def.call(ctx, c.Input); err != nil {
			return nil, fmt.Errorf("goagent: gold 轨迹[%d] 工具 %q 回放失败: %w", i, c.Name, err)
		}
	}
	return SnapshotDir(dir)
}

// SnapshotDir 把目录拍成终态快照：相对路径（'/' 分隔）→ 文本内容。
// 跳过子目录本身（只留文件）；非文本文件（含 NUL 字节）跳过——
// 文本快照不承载二进制，宿主有需求走 RegisterAssert 自定义断言。
// 结果按路径排序，遍历顺序确定。
func SnapshotDir(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.ContainsRune(string(b), 0) {
			return nil // 二进制跳过
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("goagent: 快照目录 %s 失败: %w", dir, err)
	}
	return out, nil
}

// SortedPaths 快照的排序路径视图（日志/diff 展示用，确定性顺序）。
func SortedPaths(state map[string]string) []string {
	paths := make([]string, 0, len(state))
	for p := range state {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
