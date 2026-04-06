package goagent

import (
	"context"
	"encoding/json"
)

// ToolOption 是 InferTool 的可选配置。
type ToolOption func(*ToolDef)

// WithConcurrent 设置工具允许并发执行。
func WithConcurrent() ToolOption {
	return func(d *ToolDef) {
		d.Concurrent = true
	}
}

// WithInterruptMode 设置工具的中断行为："cancel" 或 "block"。
func WithInterruptMode(mode string) ToolOption {
	return func(d *ToolDef) {
		d.InterruptMode = mode
	}
}

// WithMaxResultSize 设置工具结果的最大字符数。
func WithMaxResultSize(n int) ToolOption {
	return func(d *ToolDef) {
		d.MaxResultSizeChars = n
	}
}

// InferTool 从函数签名自动推断 Input Schema，生成 NamedTool。
//
// 函数签名：func(ctx context.Context, input T) (output D, error)
//
//   - T 的 struct tags（json, desc）生成 JSON Schema
//   - D 自动 json.Marshal 为 string
//   - context.Context 自动适配为 goagent.Context
//
// 用法：
//
//	app.UseTools(goagent.InferTool("deploy", "部署服务", deployService))
//	app.UseTools(goagent.InferTool("deploy", "部署服务", deployService, goagent.Normal))
//	app.UseTools(goagent.InferTool("bash", "执行命令", bashFn, goagent.Normal, goagent.WithInterruptMode("block")))
func InferTool[T, D any](name, description string, fn func(context.Context, T) (D, error), opts ...any) NamedTool {
	var zero T

	perm := ReadOnly
	var toolOpts []ToolOption

	for _, o := range opts {
		switch v := o.(type) {
		case Permission:
			perm = v
		case ToolOption:
			toolOpts = append(toolOpts, v)
		}
	}

	def := ToolDef{
		Description: description,
		Input:       zero,
		Permission:  perm,
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
	}

	for _, o := range toolOpts {
		o(&def)
	}

	return NamedTool{
		Name: name,
		Def:  def,
	}
}
