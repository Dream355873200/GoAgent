// validate.go case 入库前双端验证（B2）：把「判分器本身有 bug」
// 从运行时污染提前到入库拦截。
//
// SWE-bench validation 阶段 + τ-bench 调研第 8 条（research.md §2.3）：
// 每条 case 入库前跑双端验证——gold（参考）输出必须全过，bad（故意
// 错误）输出必须至少挂一条。不满足的 case 是坏 case，根本不该进
// 套件：gold 都过不了的断言无法判定任何 agent 成功；bad 都拦不住的
// 断言对错误输出免疫（恒真断言，只贡献虚假安全感）。
package benchmark

import (
	"fmt"
)

// ValidateCase case 入库验证。gold 是参考输出（人工审过的成功 run
// 或 GoldReplayer 派生口径），bad 是故意错误的输出（写错内容/调错
// 工具/空输出都行——只要它「明显不对」）。
//
// 返回 nil = case 可入库；返回 error 描述哪一端失败。库只提供验证
// 原语，「入库流水线」（CI 拦截、错误上报）由宿主组装。
//
// 注意 gold/bad 都要带 State：含终态断言的 case，双端输出缺 State
// 会让终态断言在两端都挂——gold 端先报错，能发现但仍建议双端都
// 采集（bad 的 State 也要真实——「故意留错文件」也是有效 bad 形态）。
func ValidateCase(c Case, gold, bad TargetOutput) error {
	goldPass, _, _, err := EvaluateCase(nil, c, gold)
	if err != nil {
		return fmt.Errorf("case %q gold 端断言无法评估（case 损坏）: %w", c.ID, err)
	}
	if !goldPass {
		return fmt.Errorf("case %q gold 端未全过：gold 都过不了的断言无法判定成功", c.ID)
	}
	badPass, _, _, err := EvaluateCase(nil, c, bad)
	if err != nil {
		return fmt.Errorf("case %q bad 端断言无法评估（case 损坏）: %w", c.ID, err)
	}
	if badPass {
		return fmt.Errorf("case %q bad 端全过：断言对错误输出免疫（恒真），拦不住任何回归", c.ID)
	}
	return nil
}
