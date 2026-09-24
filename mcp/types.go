// Package mcp — MCP 协议类型定义。
//
// 定义 JSON-RPC 消息格式和 MCP 特有的数据类型。
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request 是 JSON-RPC 请求（id 由传输层分配；通知走 Transport.Notify，不经此类型）。
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response 是 JSON-RPC 响应。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError 是 JSON-RPC 错误。
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("%s (code: %d)", e.Message, e.Code)
}

// wireMessage 线路上的任意 JSON-RPC 消息：响应（有 id 无 method）、
// 通知（有 method 无 id）或服务端发起的请求（两者都有）。
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// isResponse 是否为某个请求的响应。
func (m *wireMessage) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// isServerRequest 是否为服务端发起、需要客户端回复的请求（如 ping）。
func (m *wireMessage) isServerRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// responseID 响应 id（本客户端只发整数 id；非整数 id 不属于本客户端）。
func (m *wireMessage) responseID() (int64, bool) {
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return 0, false
	}
	return id, true
}

// toResponse 转为 Response。
func (m *wireMessage) toResponse(id int64) *Response {
	return &Response{JSONRPC: m.JSONRPC, ID: id, Result: m.Result, Error: m.Error}
}

// replyTo 服务端请求的回复：ping 回空结果，其余方法回「未实现」。
func replyTo(m *wireMessage) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if m.Method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = map[string]any{"code": -32601, "message": "method not found: " + m.Method}
	}
	return reply
}

// notification 通知报文（无 id）。
func notification(method string, params any) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return msg
}

// ToolInfo 描述 MCP 服务器提供的一个工具。
type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// ToolCallResult 是 MCP 工具调用的结果。
type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem 是 MCP 工具结果中的内容项。
type ContentItem struct {
	Type     string           `json:"type"` // "text", "image", "audio", "resource", "resource_link"
	Text     string           `json:"text,omitempty"`
	MimeType string           `json:"mimeType,omitempty"`
	Data     string           `json:"data,omitempty"`
	URI      string           `json:"uri,omitempty"`
	Resource *ResourceContent `json:"resource,omitempty"`
}

// ResourceContent 内嵌资源（type=resource）。
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ExtractText 把工具结果转为文本：文本项原样拼接，内嵌文本资源展开，
// 二进制内容（图片/音频/二进制资源）以占位说明代替（模型至少知道产出了什么）。
func (r *ToolCallResult) ExtractText() string {
	var parts []string
	for _, item := range r.Content {
		switch item.Type {
		case "text":
			parts = append(parts, item.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s %s，%d 字节 base64]", item.Type, item.MimeType, len(item.Data)))
		case "resource":
			if res := item.Resource; res != nil {
				if res.Text != "" {
					parts = append(parts, res.Text)
				} else {
					parts = append(parts, fmt.Sprintf("[resource %s %s]", res.URI, res.MimeType))
				}
			}
		case "resource_link":
			parts = append(parts, fmt.Sprintf("[resource_link %s]", item.URI))
		}
	}
	return strings.Join(parts, "\n")
}

// ServerCapabilities 描述 MCP 服务器的能力。
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}

// ToolsCapability 描述工具能力。
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability 描述资源能力。
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability 描述提示能力。
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}
