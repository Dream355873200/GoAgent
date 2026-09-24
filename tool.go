package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/schema"
)

// 工具副作用分级（executor.Class 别名）——决定并行编排策略：
//
//	EffectReadOnly   只读：任何场景可并行（含独占工具运行期间）
//	EffectParallel   并行安全：可与并行/只读工具并行，不能与独占重叠
//	EffectExclusive  独占：必须独占执行（默认兜底）
const (
	EffectDefault   = executor.ClassUnset
	EffectExclusive = executor.ClassExclusive
	EffectParallel  = executor.ClassParallel
	EffectReadOnly  = executor.ClassReadOnly
)

// Permission 定义工具的安全级别。
// 框架根据此级别自动处理审批流程。
type Permission int

const (
	// ReadOnly 工具始终允许，无需询问用户。
	// 用于仅读取数据且无副作用的工具。
	ReadOnly Permission = iota

	// Normal 工具在首次调用时询问用户，之后用户
	// 可以选择"始终允许"来跳过后续提示。
	Normal

	// RequireApproval 工具每次都询问用户。
	// 用于有副作用但非破坏性的工具。
	RequireApproval

	// Dangerous 工具显示醒目警告并每次询问。
	// "始终允许"选项对危险工具不可用。
	// 用于不可逆或高影响操作。
	Dangerous
)

// String 返回权限级别名称。
func (p Permission) String() string {
	switch p {
	case ReadOnly:
		return "ReadOnly"
	case Normal:
		return "Normal"
	case RequireApproval:
		return "RequireApproval"
	case Dangerous:
		return "Dangerous"
	default:
		return fmt.Sprintf("Permission(%d)", int(p))
	}
}

// ToolDef 定义 agent 可以使用的工具。
//
// 用户通过提供以下内容创建工具：
//   - Description（展示给 LLM）
//   - Input 结构体（自动反射为 JSON Schema）
//   - Permission 级别
//   - Execute 函数
//
// 示例：
//
//	goagent.ToolDef{
//	    Description: "Deploy a service",
//	    Input:       DeployInput{},
//	    Permission:  goagent.Dangerous,
//	    InterruptMode: "block",
//	    Execute: func(ctx goagent.Context, in DeployInput) (string, error) {
//	        return deploy(in.Service, in.Env)
//	    },
//	}
type ToolDef struct {
	// Description 是 LLM 看到的工具描述，用于决定何时使用此工具。
	Description string

	// Input 是一个结构体，其类型被反射为 JSON Schema。
	// 使用结构体标签来丰富 schema：
	//   `json:"name"`                — 字段名
	//   `desc:"description"`         — 字段描述
	//   `enum:"a,b,c"`              — 允许的值
	//   `required:"true"`           — 标记为必填
	//   `json:"name,omitempty"`     — 标记为可选
	Input any

	// Schema 显式 JSON Schema（可空）：非空时直接作为工具入参 schema，
	// 不再反射 Input。用于 schema 来自外部的工具（如 MCP 服务器声明的
	// inputSchema）；此时 Execute 通常取 json.RawMessage 入参自行解析。
	Schema map[string]any
	// Permission 决定框架如何处理用户审批。
	Permission Permission

	// Concurrent 标识此工具是否可以与其他并发安全的工具并行运行。
	// 默认 false（串行执行）。
	// Deprecated: 使用 Effect 三级分级（只读/并行安全/独占）；
	// Effect 非 Unset 时本字段被忽略。
	Concurrent bool

	// Effect 副作用分级（元数据）：只读 / 并行安全 / 独占。
	// 决定执行器的并行编排——只读工具可与任何工具并行（含独占工具
	// 运行期间），并行安全工具互不冲突地并行，独占工具串行。
	// 零值 EffectDefault 时按 Concurrent 推导（true→并行安全，false→独占）。
	Effect executor.Class

	// Execute 是工具的实现函数。
	// 必须具有签名：func(Context, T) (string, error)
	// 其中 T 匹配 Input 的类型。
	Execute any

	// InterruptMode 工具被中断时的行为："cancel"（取消）或 "block"（等待完成）。
	// 默认 "cancel"。
	InterruptMode string

	// MaxResultSizeChars 是此工具结果的最大字符数。
	// 超过此限制的结果会被截断。0 表示使用全局默认值。
	MaxResultSizeChars int

	// Aliases 是此工具的别名列表。
	// LLM 可以用任一名称调用此工具。
	Aliases []string

	// SessionDescription 按会话动态生成描述（可空）：非空返回值覆盖该会话
	// run 看到的 Description。用于描述内嵌会话相关清单的工具（如各会话
	// 可用技能不同）——工具注册表仍是进程级一份，描述随会话变化。
	SessionDescription func(sessionID string) string
}

// inputSchemaOf 工具入参 JSON Schema：显式 Schema 优先，其次反射 Input，
// 都没有时为空 object schema。
func inputSchemaOf(def ToolDef) any {
	switch {
	case def.Schema != nil:
		return def.Schema
	case def.Input != nil:
		return schema.Generate(def.Input)
	default:
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
}

// call 使用给定的 JSON 输入调用 Execute 函数。
// 由框架内部调用。
func (d *ToolDef) call(ctx context.Context, rawInput json.RawMessage) (string, error) {
	execVal := reflect.ValueOf(d.Execute)
	execType := execVal.Type()

	if execType.Kind() != reflect.Func {
		return "", fmt.Errorf("goagent: Execute 必须是函数，得到 %T", d.Execute)
	}
	if execType.NumIn() != 2 || execType.NumOut() != 2 {
		return "", fmt.Errorf("goagent: Execute 必须具有签名 func(Context, T) (string, error)")
	}

	// 通过将 JSON 反序列化到输入类型来创建输入值。
	inputType := execType.In(1)
	inputPtr := reflect.New(inputType)
	if len(rawInput) > 0 {
		if err := json.Unmarshal(rawInput, inputPtr.Interface()); err != nil {
			return "", fmt.Errorf("goagent: 反序列化工具输入失败: %w", err)
		}
	}

	// 构建 Context 参数。
	agentCtx := newContextFromStd(ctx)

	// 调用函数。
	results := execVal.Call([]reflect.Value{
		reflect.ValueOf(agentCtx),
		inputPtr.Elem(),
	})

	// 提取结果：(string, error)
	resultStr := results[0].String()
	errIface := results[1].Interface()
	if errIface != nil {
		return resultStr, errIface.(error)
	}
	return resultStr, nil
}

// Result 是工具执行的结果。
type Result struct {
	Content string
	IsError bool
}
