package goagent

// ToolKit 是按领域分组的工具集合。
// 开发者可通过 WithToolKits() 快速注册一组相关工具，
// 或通过 ToolKit.Tools() 获取工具列表后自定义注册。
//
// 框架内置以下 ToolKit：
//   - FileKit()    — 文件操作：Read/Write/Edit
//   - SearchKit()  — 搜索：Glob/Grep
//   - ShellKit()   — Shell 执行：Bash
//   - InteractKit() — 用户交互：AskUser
//   - CodeKit()    — 代码开发全套：FileKit + SearchKit + ShellKit
//   - AllKit()     — 全部内置工具
type ToolKit struct {
	// Name 是工具包名称。
	Name string
	// Description 是工具包描述。
	Description string
	// tools 是此工具包包含的工具列表。
	tools []NamedTool
}

// Tools 返回此工具包中的所有工具。
func (k ToolKit) Tools() []NamedTool {
	return k.tools
}

// WithTools 返回添加了工具的新 ToolKit（用于链式构建）。
func (k ToolKit) WithTools(tools ...NamedTool) ToolKit {
	k.tools = append(k.tools, tools...)
	return k
}

// QuickTool 是简化版工具定义辅助函数。
// 减少定义简单工具时的样板代码。
//
// 示例：
//
//	app.UseTools(
//	    goagent.QuickTool("deploy", "部署服务", goagent.Normal,
//	        func(ctx goagent.Context, in DeployInput) (string, error) {
//	            return kubectl.Deploy(in.Service, in.Env)
//	        },
//	        DeployInput{},
//	    ),
//	)
func QuickTool[T any](name, description string, perm Permission, execute func(Context, T) (string, error), input T) NamedTool {
	return NamedTool{
		Name: name,
		Def: ToolDef{
			Description: description,
			Input:       input,
			Permission:  perm,
			Execute:     execute,
		},
	}
}

// QuickReadOnlyTool 是只读工具的简化定义。
// 自动设置 Permission=ReadOnly 和 Concurrent=true。
//
// 示例：
//
//	app.UseTools(
//	    goagent.QuickReadOnlyTool("status", "查看系统状态",
//	        func(ctx goagent.Context, in StatusInput) (string, error) {
//	            return getSystemStatus()
//	        },
//	        StatusInput{},
//	    ),
//	)
func QuickReadOnlyTool[T any](name, description string, execute func(Context, T) (string, error), input T) NamedTool {
	return NamedTool{
		Name: name,
		Def: ToolDef{
			Description: description,
			Input:       input,
			Permission:  ReadOnly,
			Concurrent:  true,
			Execute:     execute,
		},
	}
}

// QuickDangerousTool 是危险工具的简化定义。
// 自动设置 Permission=Dangerous 和 Concurrent=false。
func QuickDangerousTool[T any](name, description string, execute func(Context, T) (string, error), input T) NamedTool {
	return NamedTool{
		Name: name,
		Def: ToolDef{
			Description: description,
			Input:       input,
			Permission:  Dangerous,
			Concurrent:  false,
			Execute:     execute,
		},
	}
}
