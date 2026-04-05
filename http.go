package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/anthropic-community/goagent/task"
)

// runHTTP 启动带 SSE 流式端点的 HTTP 服务器。
func runHTTP(app *App, addr string) error {
	// HTTP 模式下如果未配置审批者，使用自动审批。
	// 开发者可以传入 PermissionHandler 实现异步审批。
	if app.config.approver == nil {
		app.config.approver = AutoApprover()
	}

	mux := http.NewServeMux()

	// 活跃会话计数。
	var activeSessions sync.WaitGroup

	// 全局 Handler 注册表（按 session ID 管理）。
	permHandlers := &sync.Map{}

	// POST /chat — SSE 流式端点
	mux.HandleFunc("POST /chat", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		if req.Message == "" {
			http.Error(w, "message 字段必填", http.StatusBadRequest)
			return
		}

		// 设置 SSE 头。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // nginx 反代时禁用缓冲

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "不支持流式传输", http.StatusInternalServerError)
			return
		}

		activeSessions.Add(1)
		defer activeSessions.Done()

		// 为此会话创建 PermissionHandler（如果 App 使用了 PermissionHandler）。
		sessionID := req.SessionID
		if sessionID == "" {
			sessionID = fmt.Sprintf("sess-%d", time.Now().UnixNano())
		}

		// 检查 App 的 approver 是否为 PermissionHandler。
		permHandler, isAsyncPerm := app.config.approver.(*PermissionHandler)

		// 如果是异步权限模式，启动协程将权限请求推送到 SSE 流。
		if isAsyncPerm {
			permHandlers.Store(sessionID, permHandler)
			defer func() {
				permHandlers.Delete(sessionID)
			}()

			// 在 goroutine 中转发权限请求到 SSE 流。
			go func() {
				for req := range permHandler.Requests() {
					data, _ := json.Marshal(ssePermissionRequest{
						Type:       "permission_request",
						RequestID:  req.RequestID,
						SessionID:  sessionID,
						ToolName:   req.ToolName,
						ToolInput:  req.ToolInput,
						Permission: req.Permission,
					})
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}()
		}

		ctx := r.Context()
		startTime := time.Now()

		// 获取 App 级别的 handler 引用
		askUserHandler := app.askUserHandler
		planConfirmHandler := app.planConfirmHandler

		// 如果有 askUserHandler，启动 goroutine 转发请求到 SSE
		if askUserHandler != nil {
			go func() {
				for req := range askUserHandler.Requests() {
					data, _ := json.Marshal(sseAskUserRequest{
						Type:      "ask_user",
						RequestID: req.RequestID,
						SessionID: sessionID,
						Question:  req.Question,
					})
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}()
		}

		// 如果有 planConfirmHandler，启动 goroutine 转发请求到 SSE
		if planConfirmHandler != nil {
			go func() {
				for req := range planConfirmHandler.Requests() {
					data, _ := json.Marshal(ssePlanConfirmRequest{
						Type:        "plan_confirm",
						RequestID:   req.RequestID,
						SessionID:   sessionID,
						PlanContent: req.PlanContent,
					})
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}()
		}

		for ev := range app.Run(ctx, req.Message) {
			data, _ := json.Marshal(sseEvent{
				Type:       ev.Type.String(),
				Text:       ev.Text,
				Thinking:   ev.Thinking,
				ToolName:   ev.ToolName,
				ToolUseID:  ev.ToolUseID,
				ToolResult: ev.ToolResult,
				Error:      errString(ev.Error),
				Usage:      usageFromEvent(ev),
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// 发送完成元数据。
		elapsed := time.Since(startTime)
		meta, _ := json.Marshal(map[string]any{
			"type":       "metadata",
			"elapsed_ms": elapsed.Milliseconds(),
		})
		fmt.Fprintf(w, "data: %s\n\n", meta)
		flusher.Flush()
	})

	// POST /approve — 权限审批响应端点
	// 前端接收到 permission_request SSE 事件后，
	// 通过此端点返回用户的审批决定。
	mux.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		var req approveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}

		// 查找对应的 PermissionHandler。
		var resolved bool
		if req.SessionID != "" {
			if val, ok := permHandlers.Load(req.SessionID); ok {
				handler := val.(*PermissionHandler)
				resolved = handler.Resolve(req.RequestID, req.Allow, req.AlwaysAllow, req.Reason)
			}
		} else {
			// 无 session ID 时，尝试 App 级别的 handler。
			if handler, ok := app.config.approver.(*PermissionHandler); ok {
				resolved = handler.Resolve(req.RequestID, req.Allow, req.AlwaysAllow, req.Reason)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if resolved {
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "resolved": true})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "请求未找到或已过期"})
		}
	})

	// POST /askuser — AskUser 响应端点
	// 前端接收到 ask_user SSE 事件后，通过此端点返回用户回答。
	mux.HandleFunc("POST /askuser", func(w http.ResponseWriter, r *http.Request) {
		var req askUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}

		// 使用 AskUserHandler 处理响应
		var resolved bool
		if askUserHandler := app.askUserHandler; askUserHandler != nil {
			resolved = askUserHandler.Resolve(req.RequestID, req.Answer)
		}

		w.Header().Set("Content-Type", "application/json")
		if resolved {
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "resolved": true})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "请求未找到或已过期"})
		}
	})

	// POST /plan/confirm — 计划确认端点
	// 前端接收到 plan_confirm SSE 事件后，通过此端点返回用户确认。
	mux.HandleFunc("POST /plan/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req planConfirmRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}

		// 使用 PlanConfirmHandler 处理确认
		var resolved bool
		if planConfirmHandler := app.planConfirmHandler; planConfirmHandler != nil {
			resolved = planConfirmHandler.Resolve(req.RequestID, req.Confirm)
		}

		w.Header().Set("Content-Type", "application/json")
		if resolved {
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "resolved": true, "confirmed": req.Confirm})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "请求未找到或已过期"})
		}
	})

	// POST /interrupt — 中断执行端点
	// 前端可以通过此端点请求中断当前执行。
	mux.HandleFunc("POST /interrupt", func(w http.ResponseWriter, r *http.Request) {
		var req interruptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}

		// 使用 InterruptHandler 处理中断
		var err error
		if interruptHandler := app.interruptHandler; interruptHandler != nil {
			err = interruptHandler.Interrupt(req.SessionID, req.Reason)
		}

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": err.Error()})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	})

	// POST /execute — 同步端点
	mux.HandleFunc("POST /execute", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}

		activeSessions.Add(1)
		defer activeSessions.Done()

		result, err := app.Execute(r.Context(), req.Message)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(executeResponse{
			Text:         result.FinalText,
			InputTokens:  result.TotalUsage.InputTokens,
			OutputTokens: result.TotalUsage.OutputTokens,
		})
	})

	// GET /health — 健康检查
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(healthResponse{
			Status: "ok",
			Tools:  len(app.tools),
		})
	})

	// GET /tools — 列出已注册的工具
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		app.mu.RLock()
		tools := make([]toolInfoResponse, 0, len(app.toolOrder))
		for _, name := range app.toolOrder {
			rt := app.tools[name]
			tools = append(tools, toolInfoResponse{
				Name:        name,
				Description: rt.def.Description,
				Permission:  rt.def.Permission.String(),
				Concurrent:  rt.def.Concurrent,
			})
		}
		app.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tools)
	})

	// --- Task 端点 ---

	// GET /tasks — 列出所有任务
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		summaries := app.TaskSummaries()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summaries)
	})

	// POST /tasks — 创建新任务
	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Subject     string         `json:"subject"`
			Description string         `json:"description"`
			ActiveForm  string         `json:"active_form"`
			Metadata    map[string]any `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		if req.Subject == "" {
			http.Error(w, "subject 字段必填", http.StatusBadRequest)
			return
		}
		t := app.TaskStore().Create(req.Subject, req.Description, req.ActiveForm, req.Metadata)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(t)
	})

	// GET /tasks/{id} — 获取任务详情
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		t := app.TaskStore().Get(id)
		if t == nil {
			http.Error(w, "任务未找到", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	})

	// PUT /tasks/{id} — 更新任务
	mux.HandleFunc("PUT /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var patch struct {
			Subject     *string `json:"subject,omitempty"`
			Description *string `json:"description,omitempty"`
			ActiveForm  *string `json:"active_form,omitempty"`
			Status      *string `json:"status,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		taskPatch := task.UpdatePatch{}
		if patch.Subject != nil {
			taskPatch.Subject = *patch.Subject
		}
		if patch.Description != nil {
			taskPatch.Description = *patch.Description
		}
		if patch.ActiveForm != nil {
			taskPatch.ActiveForm = *patch.ActiveForm
		}
		if patch.Status != nil {
			taskPatch.Status = task.Status(*patch.Status)
		}
		updated, err := app.TaskStore().Update(id, taskPatch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if updated == nil {
			http.Error(w, "任务未找到", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated)
	})

	// DELETE /tasks/{id} — 删除任务
	mux.HandleFunc("DELETE /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := app.TaskStore().Delete(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- Plan 端点 ---

	// GET /plan — 获取计划状态
	mux.HandleFunc("GET /plan", func(w http.ResponseWriter, r *http.Request) {
		store := app.PlanStore()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active":    store.IsActive(),
			"state":     store.GetState().String(),
			"file_path": store.FilePath(),
			"content":   store.Content(),
		})
	})

	// POST /plan — 进入计划模式
	mux.HandleFunc("POST /plan", func(w http.ResponseWriter, r *http.Request) {
		filePath, err := app.PlanStore().Enter()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"file_path": filePath})
	})

	// DELETE /plan — 退出计划模式
	mux.HandleFunc("DELETE /plan", func(w http.ResponseWriter, r *http.Request) {
		content, err := app.PlanStore().Exit()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"content": content})
	})

	// --- BgTask 端点 ---

	// GET /bgtasks — 列出所有后台任务
	mux.HandleFunc("GET /bgtasks", func(w http.ResponseWriter, r *http.Request) {
		tasks := app.BgTaskStore().List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks)
	})

	// GET /bgtasks/{id} — 获取后台任务详情
	mux.HandleFunc("GET /bgtasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		task := app.BgTaskStore().Get(id)
		if task == nil {
			http.Error(w, "任务未找到", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(task)
	})

	// POST /bgtasks/{id}/stop — 停止后台任务
	mux.HandleFunc("POST /bgtasks/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := app.BgTaskStore().Kill(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	})

	// --- 可观测性端点 ---

	// GET /usage — 获取 token 使用成本统计
	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		summary := app.Usage()
		if summary == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "cost tracking not enabled"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	// GET /audit — 获取工具执行分析统计
	mux.HandleFunc("GET /audit", func(w http.ResponseWriter, r *http.Request) {
		summary := app.Analytics()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	fmt.Printf("GoAgent HTTP 服务器监听 %s\n", addr)
	fmt.Printf("  POST /chat      — SSE 流式传输\n")
	fmt.Printf("  POST /execute   — 同步执行\n")
	fmt.Printf("  POST /approve   — 权限审批响应\n")
	fmt.Printf("  GET  /health    — 健康检查\n")
	fmt.Printf("  GET  /tools     — 工具列表\n")
	fmt.Printf("  GET  /tasks     — 任务列表\n")
	fmt.Printf("  POST /tasks     — 创建任务\n")
	fmt.Printf("  GET  /tasks/{id} — 获取任务\n")
	fmt.Printf("  PUT  /tasks/{id} — 更新任务\n")
	fmt.Printf("  DELETE /tasks/{id} — 删除任务\n")
	fmt.Printf("  GET  /plan      — 计划状态\n")
	fmt.Printf("  POST /plan      — 进入计划模式\n")
	fmt.Printf("  DELETE /plan    — 退出计划模式\n")
	fmt.Printf("  GET  /bgtasks   — 后台任务列表\n")
	fmt.Printf("  GET  /bgtasks/{id} — 后台任务详情\n")
	fmt.Printf("  POST /bgtasks/{id}/stop — 停止后台任务\n")
	fmt.Printf("  GET  /usage     — Token 使用统计\n")
	fmt.Printf("  GET  /audit     — 工具执行分析\n")

	return http.ListenAndServe(addr, mux)
}

type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

type sseEvent struct {
	Type       string    `json:"type"`
	Text       string    `json:"text,omitempty"`
	Thinking   string    `json:"thinking,omitempty"`
	ToolName   string    `json:"tool_name,omitempty"`
	ToolUseID  string    `json:"tool_use_id,omitempty"`
	ToolResult string    `json:"tool_result,omitempty"`
	Error      string    `json:"error,omitempty"`
	Usage      *sseUsage `json:"usage,omitempty"`
}

type ssePermissionRequest struct {
	Type       string          `json:"type"`
	RequestID  string          `json:"request_id"`
	SessionID  string          `json:"session_id"`
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input"`
	Permission string          `json:"permission"`
}

type sseAskUserRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Question  string `json:"question"`
}

type ssePlanConfirmRequest struct {
	Type        string `json:"type"`
	RequestID   string `json:"request_id"`
	SessionID   string `json:"session_id"`
	PlanContent string `json:"plan_content"`
}

type approveRequest struct {
	RequestID   string `json:"request_id"`
	SessionID   string `json:"session_id,omitempty"`
	Allow       bool   `json:"allow"`
	AlwaysAllow bool   `json:"always_allow,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type askUserRequest struct {
	RequestID string `json:"request_id"`
	Answer    string `json:"answer"`
}

type planConfirmRequest struct {
	RequestID string `json:"request_id"`
	Confirm   bool   `json:"confirm"`
	Reason    string `json:"reason,omitempty"`
}

type interruptRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type sseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type executeResponse struct {
	Text         string `json:"text"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type healthResponse struct {
	Status string `json:"status"`
	Tools  int    `json:"tools"`
}

type toolInfoResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Permission  string `json:"permission"`
	Concurrent  bool   `json:"concurrent"`
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func usageFromEvent(ev Event) *sseUsage {
	if ev.Usage == nil {
		return nil
	}
	return &sseUsage{
		InputTokens:  ev.Usage.InputTokens,
		OutputTokens: ev.Usage.OutputTokens,
	}
}

// String 返回事件类型名称（用于 SSE 序列化）。
func (t EventType) String() string {
	switch t {
	case EventTextDelta:
		return "text_delta"
	case EventThinking:
		return "thinking"
	case EventToolStart:
		return "tool_start"
	case EventToolDone:
		return "tool_done"
	case EventNeedApproval:
		return "need_approval"
	case EventUsageUpdate:
		return "usage"
	case EventTurnComplete:
		return "turn_complete"
	case EventDone:
		return "done"
	case EventError:
		return "error"
	case EventProgress:
		return "progress"
	case EventCompaction:
		return "compaction"
	case EventAskUser:
		return "ask_user"
	case EventPlanConfirm:
		return "plan_confirm"
	case EventInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

// 确保 App 间接实现 http.Handler
var _ context.Context = context.Background()
