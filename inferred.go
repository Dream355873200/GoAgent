package goagent

import (
	"context"
	"encoding/json"
)

// InferTool 从函数签名自动推断 Input Schema，生成 NamedTool。
//
// 函数签名：func(ctx context.Context, input T) (output D, error)
//
//   - T 的 struct tags（json, desc）生成 JSON Schema
//   - D 自动 json.Marshal 为 string
//   - context.Context 自动适配为 goagent.Context
func InferTool[T, D any](name, description string, fn func(context.Context, T) (D, error)) NamedTool {
	var zero T

	return NamedTool{
		Name: name,
		Def: ToolDef{
			Description: description,
			Input:       zero,
			Permission:  ReadOnly,
			Execute: func(ctx Context, in T) (string, error) {
				result, err := fn(ctx.Context, in)
				if err != nil {
					return "", err
				}
				data, err := json.Marshal(result)
				if err != nil {
					return "", err
				}
				return string(data), nil
			},
		},
	}
}
