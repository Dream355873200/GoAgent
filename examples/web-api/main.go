// Example: Web API — 展示如何集成到 Gin/Hertz/Fiber 等 Web 框架
//
// 此示例展示 GoAgent 作为纯 SDK 使用，不依赖内置 HTTP 服务器。
// 开发者使用自己的 Web 框架，GoAgent 只提供 Agent 能力。
//
// 核心概念：
//   - App.Run() 返回事件 channel — 开发者自己做 SSE/WebSocket 推送
//   - PermissionHandler — 异步权限审批，前端参与决策
//   - ToolKit — 按需加载工具能力
//
// 前端交互流程：
//
//	┌──────┐    POST /chat     ┌──────┐   Run()    ┌─────────┐
//	│ 前端 │ ──────────────── →│ 后端 │ ────────→  │ GoAgent │
//	│      │ ← SSE events ──  │ (Gin)│ ← events   │   App   │
//	│      │                   │      │             │         │
//	│      │  permission_req   │      │  Approve()  │         │
//	│      │ ← ─ ─ ─ ─ ─ ─ ─  │      │             │         │
//	│      │ POST /approve  →  │      │  Resolve()  │         │
//	│      │ ── ── ── ── ── → │      │ ── ── ── → │         │
//	└──────┘                   └──────┘             └─────────┘
//
// 使用方式（伪代码，实际需要 gin 依赖）：
//
//	go run main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/Dream355873200/GoAgent"
	_ "github.com/Dream355873200/GoAgent/builtin" // 注册内置工具
)

func main() {
	// ─── 1. 创建 PermissionHandler（异步权限审批）───
	permHandler := goagent.NewPermissionHandler()

	// ─── 2. 创建 GoAgent App（纯 SDK 模式）───
	app := goagent.New(
		goagent.ProviderConfig{
			Type:    "openai",
			Model:   envOr("OPENAI_MODEL", "qwen2.5:7b"),
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			BaseURL: envOr("OPENAI_BASE_URL", "http://localhost:11434/v1"),
		},
		goagent.WithSystemPrompt("你是一个 AI 助手。你可以读写文件和执行命令。"),
		goagent.WithBuiltinTools(),

		// 关键：使用异步权限处理器，不用 stdin 阻塞
		goagent.WithApprover(permHandler),

		// 可选：配置权限规则
		goagent.WithPermissionRules(
			goagent.NewPermissionRules().
				Allow("Read", "").        // Read 工具无需审批
				Allow("Glob", "").        // Glob 工具无需审批
				Allow("Grep", "").        // Grep 工具无需审批
				Deny("Bash", "rm -rf *"). // 禁止危险删除
				Ask("Bash", "").          // 其他 Bash 命令需审批
				Ask("Write", ""),         // Write 需审批
		),
	)

	// ─── 3. 在你自己的 HTTP 框架中集成 ───
	// 这里用标准库 net/http 演示，实际可用 Gin/Hertz/Fiber

	mux := http.NewServeMux()

	// 存储活跃的 SSE 连接（用于推送权限请求）
	var (
		sseClients   = make(map[string]chan []byte)
		sseClientsMu sync.RWMutex
	)

	// 后台：把权限请求推送到对应的 SSE 连接
	go func() {
		for req := range permHandler.Requests() {
			data, _ := json.Marshal(map[string]any{
				"type":       "permission_request",
				"request_id": req.RequestID,
				"tool_name":  req.ToolName,
				"tool_input": req.ToolInput,
				"permission": req.Permission,
			})

			// 广播给所有活跃连接
			sseClientsMu.RLock()
			for _, ch := range sseClients {
				select {
				case ch <- data:
				default:
					// 连接满了，跳过
				}
			}
			sseClientsMu.RUnlock()
		}
	}()

	// POST /chat — 流式对话（SSE）
	mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Message   string `json:"message"`
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		// SSE 头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher := w.(http.Flusher)

		sessionID := req.SessionID
		if sessionID == "" {
			sessionID = fmt.Sprintf("sess-%d", r.Context().Value(http.LocalAddrContextKey))
		}

		// 注册此连接到 SSE 广播
		eventCh := make(chan []byte, 32)
		sseClientsMu.Lock()
		sseClients[sessionID] = eventCh
		sseClientsMu.Unlock()
		defer func() {
			sseClientsMu.Lock()
			delete(sseClients, sessionID)
			sseClientsMu.Unlock()
		}()

		// 启动 goroutine 转发权限请求事件
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		go func() {
			for {
				select {
				case data, ok := <-eventCh:
					if !ok {
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				case <-ctx.Done():
					return
				}
			}
		}()

		// 运行 Agent 并将事件推送到 SSE
		for ev := range app.Run(ctx, req.Message) {
			data, _ := json.Marshal(map[string]any{
				"type":        ev.Type.String(),
				"text":        ev.Text,
				"thinking":    ev.Thinking,
				"tool_name":   ev.ToolName,
				"tool_result": ev.ToolResult,
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			// 业务层持久化：在 EventDone 时获取完整消息列表。
			// ev.Messages 包含本轮所有消息（含 thinking 内容），
			// 业务层可按需存储到 DB/Redis。
			if ev.Type == goagent.EventDone && ev.Messages != nil {
				// 示例：保存到数据库（伪代码）
				// db.SaveConversation(userID, sessionID, ev.Messages)
				_ = ev.Messages // 业务层自行处理
			}
		}

		// 发送结束事件
		fmt.Fprintf(w, "data: {\"type\":\"done\"}\n\n")
		flusher.Flush()
	})

	// POST /approve — 前端返回权限审批结果
	mux.HandleFunc("/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			RequestID   string `json:"request_id"`
			Allow       bool   `json:"allow"`
			AlwaysAllow bool   `json:"always_allow"`
			Reason      string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		// 解决权限请求
		resolved := permHandler.Resolve(req.RequestID, req.Allow, req.AlwaysAllow, req.Reason)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"resolved": resolved,
		})
	})

	fmt.Println("Web API Example")
	fmt.Println("  POST /chat    — 流式对话 (SSE)")
	fmt.Println("  POST /approve — 权限审批响应")
	fmt.Println()
	fmt.Println("前端集成流程:")
	fmt.Println("  1. POST /chat 发送消息，接收 SSE 事件流")
	fmt.Println("  2. 收到 permission_request 事件时展示审批 UI")
	fmt.Println("  3. 用户决定后 POST /approve 返回结果")
	fmt.Println("  4. Agent 自动继续或停止")
	fmt.Println()
	fmt.Println("监听 :8080 ...")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		fmt.Fprintf(os.Stderr, "服务器错误: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
