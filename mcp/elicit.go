package mcp

import (
	"context"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Elicitation 是 MCP 服务器在工具调用中向用户索取的输入（elicitation/create）。
type Elicitation struct {
	// Server 是发起的服务器（ServerConfig.Name）。
	Server string
	// Mode 为 "form"（按 Schema 填写字段）或 "url"（打开 URL，在外部完成，
	// 例如 OAuth 授权或付款）。
	Mode    string
	Message string
	// Schema 是 form 模式要填写的字段：只有一层属性的对象 JSON Schema。
	Schema map[string]any
	// URL 与 ID 是 url 模式要打开的地址与 elicitationId。
	URL string
	ID  string
}

// elicitation 的三种回应。
const (
	ElicitAccept  = "accept"
	ElicitDecline = "decline"
	ElicitCancel  = "cancel"
)

// ElicitResponse 是用户的回应：Action 为 ElicitAccept / ElicitDecline /
// ElicitCancel；form 模式接受时 Content 是填写的字段。
type ElicitResponse struct {
	Action  string
	Content map[string]any
}

// ElicitHandler 处理 elicitation。ctx 带着触发它的工具调用的值（会话、调用 ID 等），
// 在该调用被取消或服务器撤回请求时取消。无法确定是哪一次调用触发的（没有进行中的
// 调用，或几个进行中的调用不在同一个 WithElicitScope 范围内）时，ctx 不带这些值，
// 处理函数通常应拒绝。返回错误时服务器收到 JSON-RPC 错误。
type ElicitHandler func(ctx context.Context, e Elicitation) (ElicitResponse, error)

type elicitScopeKey struct{}

// WithElicitScope 标记工具调用所属的范围（例如会话 ID）。MCP 的 elicitation 请求
// 不携带它属于哪一次工具调用；同一个客户端上有几个调用同时进行时，只有它们都在
// 同一个范围内，elicitation 才会交给其中最新开始的那次调用（goagent.MCPToolDef
// 以会话 ID 为范围）。
func WithElicitScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, elicitScopeKey{}, scope)
}

func elicitScope(ctx context.Context) string {
	s, _ := ctx.Value(elicitScopeKey{}).(string)
	return s
}

// inflight 记录进行中的工具调用（elicitation 据此找到触发它的调用）。
type inflight struct {
	mu    sync.Mutex
	seq   uint64
	calls map[uint64]context.Context
}

func (f *inflight) add(ctx context.Context) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[uint64]context.Context{}
	}
	f.seq++
	id := f.seq
	f.calls[id] = ctx
	return func() {
		f.mu.Lock()
		delete(f.calls, id)
		f.mu.Unlock()
	}
}

// owner 返回 elicitation 所属调用的 ctx：只有一个进行中的调用时是它；几个调用
// 都在同一个非空范围内时是其中最新的；否则为 nil。
func (f *inflight) owner() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	var newest uint64
	scope, same := "", true
	for id, ctx := range f.calls {
		if id > newest {
			newest = id
		}
		s := elicitScope(ctx)
		if scope == "" {
			scope = s
		}
		if s == "" || s != scope {
			same = false
		}
	}
	switch {
	case len(f.calls) == 1:
		return f.calls[newest]
	case len(f.calls) > 1 && same:
		return f.calls[newest]
	}
	return nil
}

// handleElicit 把 SDK 的 elicitation 请求交给 handler：ctx 取所属调用的值，
// 所属调用结束或服务器撤回请求（sctx 取消）时取消。
func (c *Client) handleElicit(sctx context.Context, handler ElicitHandler, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
	base := c.calls.owner()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	stop := context.AfterFunc(sctx, cancel)
	defer stop()

	p := req.Params
	e := Elicitation{Server: c.ServerName(), Mode: p.Mode, Message: p.Message, URL: p.URL, ID: p.ElicitationID}
	if e.Mode == "" {
		e.Mode = "form"
		if p.URL != "" {
			e.Mode = "url"
		}
	}
	if e.Mode == "form" {
		e.Schema = schemaMap(p.RequestedSchema)
	}
	resp, err := handler(ctx, e)
	if err != nil {
		return nil, err
	}
	switch resp.Action {
	case ElicitAccept, ElicitDecline, ElicitCancel:
	default:
		resp.Action = ElicitCancel
	}
	res := &sdk.ElicitResult{Action: resp.Action}
	if resp.Action == ElicitAccept && e.Mode == "form" {
		res.Content = resp.Content
		if res.Content == nil {
			res.Content = map[string]any{}
		}
	}
	return res, nil
}
