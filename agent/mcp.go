package agent

// MCPServerConfig MCP 服务器配置。
type MCPServerConfig struct {
	// Name 服务器名称。
	Name string
	// Command 启动命令（stdio 模式使用）。
	Command string
	// Args 启动参数（stdio 模式使用）。
	Args []string
	// Env 环境变量（stdio 模式使用，追加/覆盖继承的进程环境）。
	Env map[string]string
	// Dir 子进程工作目录（stdio 模式使用，可空）。
	Dir string
	// Transport 传输方式："stdio" 或 "http"（空时按 Command/URL 推断）。
	Transport string
	// URL Streamable HTTP 端点（http 模式使用）。
	URL string
	// Auth 认证信息（http 模式使用）：作为 Authorization 请求头原样发送，
	// 如 "Bearer xxx"。
	Auth string
	// Headers 附加 HTTP 请求头（http 模式使用）。
	Headers map[string]string
}
