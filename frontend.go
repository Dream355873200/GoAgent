package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Dream355873200/GoAgent/executor"
	"github.com/Dream355873200/GoAgent/internal/loop"
	"github.com/Dream355873200/GoAgent/observer"
	"github.com/Dream355873200/GoAgent/permission"
)

// 前端工具（剖面无关的实现）：模型调用一个由"前端"执行的工具——服务端
// 没有这个能力，执行者是请求方（浏览器/桌面端）。工具执行体阻塞等待结果，
// 结果由"剖面适配层"投递：AG-UI 剖面经 resume 的 tool 消息（见 agui.go），
// v1 剖面经 POST /chat/tool-result（见 http.go）。

// FrontendToolDecl 是一次运行的前端工具声明（AG-UI RunAgentInput.tools 与
// v1 /chat 的 tools 字段共用）。
type FrontendToolDecl struct {
	// Name 工具名（provider 接受的形态：字母数字与 _-，最长 64）。
	Name string `json:"name"`
	// Description 给模型看的"何时找我"。
	Description string `json:"description"`
	// Parameters 是入参 JSON Schema 对象；省略 = 无参数。
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// frontendToolWaitTimeout 是前端工具等待结果的上限：前端一直不回结果时
// 工具按失败收尾，不无限占用会话。
const frontendToolWaitTimeout = 30 * time.Minute

// frontendResult 是前端返回的工具结果（err 非空表示失败）。
type frontendResult struct {
	content string
	err     string
}

// frontendWait 一次等待中的调用。
type frontendWait struct {
	ch   chan frontendResult
	name string
}

// frontendCalls 登记等待前端返回结果的工具调用。结果可能先于调用开始
// 等待到达（同一轮并行的前端工具，后一个还没开始执行），先存下来。
type frontendCalls struct {
	mu      sync.Mutex
	waiting map[string]*frontendWait // thread|toolCallId
	early   map[string]frontendResult
}

func (f *frontendCalls) wait(ctx context.Context, thread, id string) (frontendResult, error) {
	key := thread + "|" + id
	f.mu.Lock()
	if r, ok := f.early[key]; ok {
		delete(f.early, key)
		f.mu.Unlock()
		return r, nil
	}
	if f.waiting == nil {
		f.waiting = map[string]*frontendWait{}
	}
	w := &frontendWait{ch: make(chan frontendResult, 1)}
	f.waiting[key] = w
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		if f.waiting[key] == w {
			delete(f.waiting, key)
		}
		f.mu.Unlock()
	}()
	select {
	case r := <-w.ch:
		return r, nil
	case <-ctx.Done():
		return frontendResult{}, ctx.Err()
	}
}

// deliver 交付一个前端工具结果（调用还没开始等待时先存下）。
func (f *frontendCalls) deliver(thread, id string, r frontendResult) {
	key := thread + "|" + id
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.waiting[key]; ok {
		delete(f.waiting, key)
		w.ch <- r
		return
	}
	if f.early == nil {
		f.early = map[string]frontendResult{}
	}
	f.early[key] = r
}

// forget 丢弃会话里没有被取走的结果（内核运行结束或会话关闭时）。
func (f *frontendCalls) forget(thread string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.early {
		if strings.HasPrefix(key, thread+"|") {
			delete(f.early, key)
		}
	}
}

// pending 返回会话里正在等待前端执行结果的调用（断线重连后据此恢复）。
func (f *frontendCalls) pending(thread string) []FrontendToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FrontendToolCall
	prefix := thread + "|"
	for key, w := range f.waiting {
		if strings.HasPrefix(key, prefix) {
			out = append(out, FrontendToolCall{ToolCallID: strings.TrimPrefix(key, prefix), Name: w.name})
		}
	}
	return out
}

// FrontendToolCall 是正在等待前端执行结果的工具调用。
type FrontendToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
}

// deliverFrontendToolResult 把前端执行的结果交给等待中的工具（v1 的
// POST /chat/tool-result 与 AG-UI 的 tool 消息都走到这里）。返回是否找到
// 了等待中的调用；errMessage 非空表示前端执行失败。
func (a *App) deliverFrontendToolResult(sessionID, toolCallID, content, errMessage string) bool {
	r := frontendResult{content: content}
	if errMessage != "" {
		r.err = errMessage
		if content != "" {
			r.err += "\n" + content
		}
	}
	a.frontend.deliver(sessionID, toolCallID, r)
	return true
}

// PendingFrontendTools 返回会话里等待前端执行结果的调用（未决时为空）。
func (a *App) PendingFrontendTools(sessionID string) []FrontendToolCall {
	return a.frontend.pending(sessionID)
}

// frontendToolNamePattern 是 provider 接受的工具名。
var frontendToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// frontendTools 把前端工具声明转为本次运行的工具：执行体向前端等待结果
// （onCall 非空时先通知剖面——AG-UI 剖面据此停车，v1 不需要）。等待上限
// frontendToolWaitTimeout，到点按失败收尾。
func (a *App) frontendTools(thread string, decls []FrontendToolDecl, onCall func(ctx context.Context, toolCallID, name string)) ([]loop.ToolEntry, error) {
	var out []loop.ToolEntry
	for _, t := range decls {
		if !frontendToolNamePattern.MatchString(t.Name) {
			return nil, fmt.Errorf("前端工具名 %q 不合法（只能含字母、数字、_ 与 -，最长 64）", t.Name)
		}
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
			params = nil
			if err := json.Unmarshal(t.Parameters, &params); err != nil || params == nil {
				return nil, fmt.Errorf("前端工具 %s 的 parameters 不是 JSON Schema 对象", t.Name)
			}
		}
		name := t.Name
		out = append(out, loop.ToolEntry{
			Name:        name,
			Description: t.Description,
			InputSchema: params,
			Permission:  permission.LevelReadOnly,
			Class:       executor.ClassReadOnly,
			ExecuteFn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				id := observer.ToolCallIDFromContext(ctx)
				if onCall != nil {
					onCall(ctx, id, name)
				}
				waitCtx, cancel := context.WithTimeout(ctx, frontendToolWaitTimeout)
				defer cancel()
				r, err := a.frontend.wait(waitCtx, thread, id)
				if err != nil {
					return "", fmt.Errorf("前端工具 %s 没有返回结果: %w", name, err)
				}
				if r.err != "" {
					return "", errors.New(r.err)
				}
				return r.content, nil
			},
		})
	}
	return out, nil
}

type runToolsKey struct{}

// withRunTools 让本次运行额外带上 tools（与已注册工具重名的不加入）。
func withRunTools(ctx context.Context, tools []loop.ToolEntry) context.Context {
	if len(tools) == 0 {
		return ctx
	}
	return context.WithValue(ctx, runToolsKey{}, tools)
}

func runToolsFrom(ctx context.Context) []loop.ToolEntry {
	tools, _ := ctx.Value(runToolsKey{}).([]loop.ToolEntry)
	return tools
}
