package permission

import (
	"context"
	"encoding/json"
	"sync"
)

// 审批改参：审批者批准一次工具调用时可以给出替换后的参数（AG-UI 审批的
// editedArgs、v1 /approve 的 input），这次调用改用它执行。循环在检查权限前用
// WithInputEdit 准备收集点；审批者批准时调用 SetEditedInput 写入。

type inputEditKey struct{}

// InputEdit 收集一次权限检查中审批者给出的替换参数。
type InputEdit struct {
	mu    sync.Mutex
	input json.RawMessage
}

// WithInputEdit 返回带收集点的 ctx：在它之下的权限检查中，审批者可以替换
// 这次调用的参数。
func WithInputEdit(ctx context.Context) (context.Context, *InputEdit) {
	e := &InputEdit{}
	return context.WithValue(ctx, inputEditKey{}, e), e
}

// Input 返回审批者给出的替换参数（没有修改时为 nil）。
func (e *InputEdit) Input() json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.input
}

// SetEditedInput 由审批者在批准时调用：这次工具调用改用 input（完整替换）执行。
// ctx 不支持改参（调用方没有准备收集点）时返回 false，审批者应当拒绝而不是按
// 原参数放行。
func SetEditedInput(ctx context.Context, input json.RawMessage) bool {
	e, ok := ctx.Value(inputEditKey{}).(*InputEdit)
	if !ok || e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.input = append(json.RawMessage(nil), input...)
	return true
}
