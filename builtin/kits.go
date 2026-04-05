package builtin

import "github.com/anthropic-community/goagent"

// FileKit 返回文件操作工具包：Read、Write、Edit。
// 适用于需要读写文件但不需要搜索和命令执行的场景。
func FileKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "FileKit",
		Description: "文件操作工具包（Read/Write/Edit）",
	}.WithTools(
		goagent.NamedTool{Name: "Read", Def: ReadTool()},
		goagent.NamedTool{Name: "Write", Def: WriteTool()},
		goagent.NamedTool{Name: "Edit", Def: EditTool()},
	)
}

// SearchKit 返回搜索工具包：Glob、Grep。
// 适用于代码搜索和文件查找场景。
func SearchKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "SearchKit",
		Description: "搜索工具包（Glob/Grep）",
	}.WithTools(
		goagent.NamedTool{Name: "Glob", Def: GlobTool()},
		goagent.NamedTool{Name: "Grep", Def: GrepTool()},
	)
}

// ShellKit 返回 Shell 执行工具包：Bash。
// 适用于需要执行系统命令的场景。
func ShellKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "ShellKit",
		Description: "Shell 执行工具包（Bash）",
	}.WithTools(
		goagent.NamedTool{Name: "Bash", Def: BashTool()},
	)
}

// InteractKit 返回用户交互工具包：AskUser。
// 适用于需要与用户对话确认的场景。
func InteractKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "InteractKit",
		Description: "用户交互工具包（AskUser）",
	}.WithTools(
		goagent.NamedTool{Name: "AskUser", Def: AskUserTool()},
	)
}

// CodeKit 返回代码开发全套工具包：FileKit + SearchKit + ShellKit。
// 适用于编码 Agent 场景，包含文件读写、代码搜索和命令执行。
func CodeKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "CodeKit",
		Description: "代码开发全套（Read/Write/Edit/Glob/Grep/Bash）",
	}.WithTools(
		append(
			append(FileKit().Tools(), SearchKit().Tools()...),
			ShellKit().Tools()...,
		)...,
	)
}

// AllKit 返回全部内置工具包。
// 等价于 AllTools() 的 ToolKit 形式。
func AllKit() goagent.ToolKit {
	return goagent.ToolKit{
		Name:        "AllKit",
		Description: "全部内置工具（Read/Write/Edit/Glob/Grep/Bash/AskUser）",
	}.WithTools(AllTools()...)
}
