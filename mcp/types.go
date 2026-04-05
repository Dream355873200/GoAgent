// Package mcp — MCP 协议类型定义。
//
// 定义 JSON-RPC 消息格式和 MCP 特有的数据类型。
package mcp

import "encoding/json"

// Request 是 JSON-RPC 请求。
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
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
	Type     string `json:"type"` // "text", "image", "resource"
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// ExtractText 从工具调用结果中提取所有文本内容。
func (r *ToolCallResult) ExtractText() string {
	var text string
	for _, item := range r.Content {
		if item.Type == "text" {
			if text != "" {
				text += "\n"
			}
			text += item.Text
		}
	}
	return text
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
