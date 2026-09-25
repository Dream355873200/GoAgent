package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/session"
	"github.com/Dream355873200/GoAgent/task"
)

// sseWriter 提供并发安全的 SSE 写入。
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	closed  bool // handler 已返回：此后的写一律丢弃（心跳等异步写者）
}

func (s *sseWriter) writeEvent(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.w.Write(data)
	s.flusher.Flush()
}

func (s *sseWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// sseHeartbeatInterval SSE 注释心跳间隔。任务长时间无帧（模型长思考、
// 长工具）时连接仍有字节流动，客户端/代理的读空闲超时不会误断流。
const sseHeartbeatInterval = 15 * time.Second

// runHTTP 启动带 SSE 流式端点的 HTTP 服务器。
func runHTTP(app *App, addr string) error {
	// HTTP 模式下如果未配置审批者，使用自动审批。
	// 开发者可以传入 PermissionHandler 实现异步审批。
	if app.config.approver == nil {
		app.config.approver = AutoApprover()
	}

	// HTTP 模式下如果未配置会话管理器，自动创建基于文件系统的持久化
	// （对齐 RunCLI 的行为；会话存储在 .yume/sessions/ 目录下）。
	// 没有它，/chat 只能走无状态的 App.Run —— 每条消息都是全新会话，
	// 前端传的 session_id 形同虚设，模型每轮都失忆从头探索。
	if app.config.sessionManager == nil {
		sessDir := filepath.Join(".yume", "sessions")
		store := session.NewFileStore(sessDir)
		app.config.sessionManager = session.NewManager(store)
	}

	mux := newHTTPMux(app)

	fmt.Printf("GoAgent HTTP 服务器监听 %s\n", addr)
	fmt.Printf("  POST /chat      — SSE 流式传输\n")
	fmt.Printf("  GET  /sessions  — 会话列表\n")
	fmt.Printf("  GET  /sessions/{id}/messages — 会话历史消息\n")
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
	fmt.Printf("  GET  /protocol  — 协议自描述（版本/帧类型/端点表）\n")

	// 宿主扩展路由（WithHTTPRoutes）：领域端点注册进同一 mux，
	// 与内置路由共用 App/会话上下文。仅 RunHTTP 模式生效。
	for pattern, handler := range app.config.httpRoutes {
		mux.HandleFunc(pattern, handler)
	}

	return http.ListenAndServe(addr, mux)
}

// newHTTPMux 构建 HTTP API 的路由表（/chat /approve /tasks /plan ...）。
// 与监听逻辑解耦：测试可用 httptest.NewServer(newHTTPMux(app)) 直接挂载，
// 无需占用真实端口。所有 handler 按会话（session_id）隔离——不同会话的
// /chat 并发执行互不阻塞（同一会话仍互斥，见 session.Manager.Acquire）。
func newHTTPMux(app *App) *http.ServeMux {

	mux := http.NewServeMux()

	// 活跃会话计数。
	var activeSessions sync.WaitGroup

	// 全局 Handler 注册表（按 session ID 管理）。
	permHandlers := &sync.Map{}

	// POST /chat — SSE 流式端点
	mux.HandleFunc("POST /chat", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		resume := req.ResumeQueue && req.Message == ""
		if req.Message == "" && !(resume && req.SessionID != "") {
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

		sw := &sseWriter{w: w, flusher: flusher}
		defer sw.close()
		heartbeatDone := make(chan struct{})
		defer close(heartbeatDone)
		go func() {
			t := time.NewTicker(sseHeartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-heartbeatDone:
					return
				case <-t.C:
					sw.writeEvent([]byte(": ping\n\n"))
				}
			}
		}()
		// 协议信封写出口：统一盖章 seq（连接内单调）与 v（协议版本）。
		seq := &atomic.Int64{}
		writeFrame := func(env protocol.Envelope) {
			env.Seq = seq.Add(1)
			env.Version = protocol.Version
			data, _ := json.Marshal(env)
			sw.writeEvent([]byte(fmt.Sprintf("data: %s\n\n", data)))
		}
		// 活跃会话计数。
		activeSessions.Add(1)
		defer activeSessions.Done()

		// 为此会话创建 PermissionHandler（如果 App 使用了 PermissionHandler）。
		sessionID := req.SessionID
		if sessionID == "" {
			sessionID = fmt.Sprintf("sess-%d", time.Now().UnixNano())
		}

		// 检查 App 的 approver 是否为 PermissionHandler。
		permHandler, isAsyncPerm := app.config.approver.(*PermissionHandler)

		// 如果是异步权限模式，按会话绑定本连接的审批下发通道并起协程转发。
		// 绑定生命周期与请求一致：handler 退出注销并关闭通道，转发协程
		// 随 range 退出——不会遗留 stale 消费者与后续连接抢帧。
		if isAsyncPerm {
			permHandlers.Store(sessionID, permHandler)
			defer func() {
				permHandlers.Delete(sessionID)
			}()

			permCh, unbindPerm := permHandler.Subscribe(sessionID)
			defer unbindPerm()
			go func() {
				for req := range permCh {
					writeFrame(protocol.Envelope{
						Type:       protocol.TypeNeedApproval,
						SessionID:  sessionID,
						RequestID:  req.RequestID,
						ToolName:   req.ToolName,
						ToolInput:  req.ToolInput,
						Permission: req.Permission,
					})
				}
			}()
		}

		ctx := r.Context()
		startTime := time.Now()

		// 任务生命周期与 HTTP 连接解耦：后台 ctx 取消仅停止 SSE 转发
		// （前端刷新/离开项目页），agent 任务本身继续跑完并落盘 —— 消息
		// 逐条即时持久化，前端重新进入时从 /sessions/{id}/messages 回放。
		// 中断只能经 POST /interrupt（用户点终止按钮），此时用后台 ctx 的
		// cancel 精确终止任务。
		bgCtx, bgCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer bgCancel() // 请求 handler 退出不杀任务：仅在无人消费时兜底回收
		go func() {
			<-ctx.Done()
			// 前端连接断开：不 cancel bgCtx（任务继续）。
			// 仅当任务尚未开始（RunSession 尚未 Acquire）时由 bgCancel 兜底。
		}()

		// 注册中断链路：/interrupt 按会话路由到本任务的 cancel。每个请求
		// 只注销自己的登记——忙时撞车的请求不会抹掉在跑任务的中断入口。
		if app.interruptHandler != nil {
			defer app.interruptHandler.Register(sessionID, bgCancel)()
		}

		// 获取 App 级别的 handler 引用
		askUserHandler := app.askUserHandler
		planConfirmHandler := app.planConfirmHandler

		// 如果有 askUserHandler，按会话绑定本连接的提问下发通道并起协程
		// 转发（AskSession 命中本会话 → 帧写进本连接）。绑定生命周期与
		// 请求一致，不与其它连接共享消费——多轮之后旧连接的转发器不会
		// 残留抢帧（抢走 = 当前流收不到提问帧，工具在引擎侧无限阻塞）。
		if askUserHandler != nil {
			askCh, unbindAsk := askUserHandler.Subscribe(sessionID)
			defer unbindAsk()
			go func() {
				for req := range askCh {
					writeFrame(protocol.Envelope{
						Type:      protocol.TypeAskUser,
						SessionID: sessionID,
						RequestID: req.RequestID,
						Question:  req.Question,
						Payload:   req.Payload, // 结构化交互载荷（确认卡选项等），客户端按 kind 分发
					})
				}
			}()
		}

		// 如果有 planConfirmHandler，启动 goroutine 转发请求到 SSE
		if planConfirmHandler != nil {
			go func() {
				for req := range planConfirmHandler.Requests() {
					writeFrame(protocol.Envelope{
						Type:        protocol.TypePlanConfirm,
						SessionID:   sessionID,
						RequestID:   req.RequestID,
						PlanContent: req.PlanContent,
					})
				}
			}()
		}

		// 多轮对话：session_id 命中已有会话则自动加载历史并在结束后持久化；
		// 未传 session_id 或未配置会话管理器时退化为无状态单轮（旧行为）。
		// 用 bgCtx（与连接解耦）：SSE 断开不取消任务，/interrupt 才取消。
		// 准入分流：会话忙且启用插话通道时，消息改走 guide 车道——任务在
		// 最近的工具批结束边界收到，前端立即收到确认（无需排队 SSE）。
		// Steer 竞态失败（检查后任务恰好结束）则落回普通新 run 路径。
		var events <-chan Event
		if resume && (app.config.sessionManager == nil || app.config.sessionManager.IsBusy(req.SessionID)) {
			// 唤醒撞上在跑的 run：队列会在该 run 结束后自行消费，无需再起一轮。
			writeFrame(protocol.Envelope{Type: protocol.TypeMetadata, SessionID: req.SessionID})
			return
		}
		if req.SessionID != "" && app.config.sessionManager != nil {
			if !resume && app.config.steering != nil && app.config.sessionManager.IsBusy(req.SessionID) {
				if err := app.config.steering.Steer(req.SessionID, req.Message); err == nil {
					writeFrame(protocol.Envelope{
						Type:      protocol.TypeSteer,
						SessionID: req.SessionID,
						Text:      req.Message,
					})
					writeFrame(protocol.Envelope{
						Type:      protocol.TypeMetadata,
						SessionID: req.SessionID,
						Steered:   true,
					})
					return
				}
				// 会话忙但此刻不可插话（run 尚在准备/已在收尾）：改入排队
				// 车道。直接开新 run 只会撞上会话锁报「正在运行中」，消息丢失。
				if app.config.sessionManager.IsBusy(req.SessionID) {
					item := app.config.steering.Enqueue(req.SessionID, req.Message)
					writeFrame(protocol.Envelope{
						Type:      protocol.TypeMetadata,
						SessionID: req.SessionID,
						Queued:    true,
						Text:      item.ID,
					})
					return
				}
			}
			events = app.RunSession(bgCtx, req.SessionID, req.Message)
		} else {
			events = app.Run(bgCtx, req.Message)
		}

		// 生命周期帧：run 开始（流内第一个数据帧，客户端据此绑定会话并
		// 把 UI 切到「运行中」；seq 从本帧起计数）。
		writeFrame(protocol.Envelope{
			Type:      protocol.TypeRunStart,
			SessionID: sessionID,
		})

		for ev := range events {
			// 连接已断（前端离开）则停止转发，任务在后台继续；
			// 但必须排空事件 channel，否则 channel 缓冲填满后 agent 循环
			// 会阻塞在 out <- 上，任务永远跑不完。
			if ctx.Err() != nil {
				for range events {
				}
				break
			}
			writeFrame(envelopeFromEvent(ev, sessionID))
		}

		// 生命周期帧：run 结束元数据（流内最后一个帧）。
		writeFrame(protocol.Envelope{
			Type:      protocol.TypeMetadata,
			SessionID: sessionID,
			ElapsedMs: time.Since(startTime).Milliseconds(),
		})
	})

	// POST /approve — 权限审批响应端点
	// 前端接收到 permission_request SSE 事件后，
	// 通过此端点返回用户的审批决定。
	mux.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ApproveRequest
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
		var req protocol.AskUserAnswer
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

	// GET /pending-ask — 会话未决提问查询（前端重载/重连后恢复提问卡）。
	// 提问未答时 run 仍阻塞在引擎内存里，回答通道（request_id）也只在
	// 内存中——历史消息里没有这些。run 已结束 / 引擎重启过时无未决，
	// 前端按历史渲染、不复活提问卡。
	mux.HandleFunc("GET /pending-ask", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessionID := r.URL.Query().Get("session_id")
		var pending *AskUserRequest
		if sessionID != "" && app.askUserHandler != nil {
			pending = app.askUserHandler.PendingBySession(sessionID)
		}
		if pending == nil {
			json.NewEncoder(w).Encode(map[string]any{"pending": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"pending":    true,
			"request_id": pending.RequestID,
			"question":   pending.Question,
			"payload":    pending.Payload,
		})
	})

	// POST /plan/confirm — 计划确认端点
	// 前端接收到 plan_confirm SSE 事件后，通过此端点返回用户确认。
	mux.HandleFunc("POST /plan/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.PlanConfirmAnswer
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
		var req protocol.InterruptRequest
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

	// POST /queue — 消息队列端点（WithSteering）。
	// 消息进入 queue 车道，当前 run 结束后自动作为下一条输入续跑。
	// 与 /chat 的区别：/chat 忙时注入当前轮（guide），/queue 永远排队
	// （任务结束后才独立成轮执行）。未启用插话通道返回 501。
	mux.HandleFunc("POST /queue", func(w http.ResponseWriter, r *http.Request) {
		if app.config.steering == nil {
			http.Error(w, "未启用插话通道（WithSteering）", http.StatusNotImplemented)
			return
		}
		var req protocol.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
			http.Error(w, "message 字段必填", http.StatusBadRequest)
			return
		}
		sessionID := req.SessionID
		if sessionID == "" {
			http.Error(w, "session_id 字段必填", http.StatusBadRequest)
			return
		}
		item := app.config.steering.Enqueue(sessionID, req.Message)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"id":      item.ID,
			"pending": app.config.steering.PendingQueued(sessionID),
		})
	})

	// GET /queue — 查看排队消息（WithSteering，队头在前，含条目 ID）。
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		if app.config.steering == nil {
			http.Error(w, "未启用插话通道（WithSteering）", http.StatusNotImplemented)
			return
		}
		sessionID := r.URL.Query().Get("session_id")
		items := app.config.steering.ListQueued(sessionID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"pending": len(items),
			"items":   items,
		})
	})

	// POST /queue/remove — 按 ID 取走一条排队消息（WithSteering）。
	// 返回原文，供宿主「撤回输入框编辑」或「立即发送」复用；ID 不存在
	// 返回 404（可能已被 drain 消费或别的端撤回）。
	mux.HandleFunc("POST /queue/remove", func(w http.ResponseWriter, r *http.Request) {
		if app.config.steering == nil {
			http.Error(w, "未启用插话通道（WithSteering）", http.StatusNotImplemented)
			return
		}
		var req struct {
			SessionID string `json:"session_id"`
			ItemID    string `json:"item_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.ItemID == "" {
			http.Error(w, "session_id 与 item_id 字段必填", http.StatusBadRequest)
			return
		}
		text, ok := app.config.steering.RemoveQueued(req.SessionID, req.ItemID)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "队列项不存在或已消费"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "text": text})
	})

	// GET /mode — 当前权限模式（运行时覆写优先，否则构造配置，缺省 default）。
	mux.HandleFunc("GET /mode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"mode": permissionModeString(app.CurrentPermissionMode())})
	})

	// POST /mode — 运行时切换权限模式（SetPermissionMode）。
	// 立即生效：进行中的 run 直接改写权限门，后续 run 沿用覆写值。
	// 合法值：default / accept_edits / plan / bypass / deny_all。
	mux.HandleFunc("POST /mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		mode, ok := permissionModeFromString(req.Mode)
		if !ok {
			http.Error(w, "mode 必须是 default/accept_edits/plan/bypass/deny_all", http.StatusBadRequest)
			return
		}
		app.SetPermissionMode(mode)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "mode": permissionModeString(mode)})
	})

	// GET /thinking — 当前思考强度档位（off/low/medium/high；空 = 不干预）。
	mux.HandleFunc("GET /thinking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"effort": app.CurrentThinkingEffort()})
	})

	// POST /thinking — 运行时切换思考强度档位（自下一次 run 起生效）。
	mux.HandleFunc("POST /thinking", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Effort string `json:"effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "无效的请求体", http.StatusBadRequest)
			return
		}
		switch req.Effort {
		case "off", "low", "medium", "high", "":
			app.SetThinkingEffort(req.Effort)
		default:
			http.Error(w, "effort 必须是 off/low/medium/high 或空", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "effort": req.Effort})
	})

	// GET /model — 当前模型 ID（Capabilities.ModelID，随运行时切换而变）。
	mux.HandleFunc("GET /model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": app.ModelID()})
	})

	// POST /model — 运行时切换模型（provider 实现 ModelSwitcher 时生效，
	// 下一次模型请求即用新模型；不需要重启引擎）。
	mux.HandleFunc("POST /model", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
			http.Error(w, "model 字段必填", http.StatusBadRequest)
			return
		}
		if !app.SetModel(req.Model) {
			http.Error(w, "当前 Provider 不支持运行时切换模型", http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model": req.Model})
	})

	// POST /execute — 同步端点
	mux.HandleFunc("POST /execute", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ChatRequest
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

	// GET /tasks — 列出所有任务。
	// ?session_id= 路由到该会话的任务分区（存储为 SessionStore 时）；
	// 不带参数且存储为会话隔离时返回空（各会话互不可见）。
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		store := app.TaskStore()
		if store == nil {
			http.Error(w, "task system not enabled", http.StatusServiceUnavailable)
			return
		}
		store = routeTaskStore(store, r.URL.Query().Get("session_id"))
		summaries := store.ListSummaries()
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
		store := app.TaskStore()
		if store == nil {
			http.Error(w, "task system not enabled", http.StatusServiceUnavailable)
			return
		}
		store = routeTaskStore(store, r.URL.Query().Get("session_id"))
		t := store.Create(req.Subject, req.Description, req.ActiveForm, req.Metadata)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(t)
	})

	// GET /tasks/{id} — 获取任务详情
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		store := app.TaskStore()
		if store == nil {
			http.Error(w, "task system not enabled", http.StatusServiceUnavailable)
			return
		}
		store = routeTaskStore(store, r.URL.Query().Get("session_id"))
		t := store.Get(id)
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
		store := app.TaskStore()
		if store == nil {
			http.Error(w, "task system not enabled", http.StatusServiceUnavailable)
			return
		}
		store = routeTaskStore(store, r.URL.Query().Get("session_id"))
		updated, err := store.Update(id, taskPatch)
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
		store := app.TaskStore()
		if store == nil {
			http.Error(w, "task system not enabled", http.StatusServiceUnavailable)
			return
		}
		if err := store.Delete(id); err != nil {
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
		if store == nil {
			// Plan 系统未启用：返回未激活状态而非 panic（与 POST /plan 的 nil 检查对齐）
			json.NewEncoder(w).Encode(map[string]any{
				"active":    false,
				"state":     "disabled",
				"file_path": "",
				"content":   "",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"active":    store.IsActive(),
			"state":     store.GetState().String(),
			"file_path": store.FilePath(),
			"content":   store.Content(),
		})
	})

	// POST /plan — 进入计划模式
	mux.HandleFunc("POST /plan", func(w http.ResponseWriter, r *http.Request) {
		planStore := app.PlanStore()
		if planStore == nil {
			http.Error(w, "plan system not enabled", http.StatusServiceUnavailable)
			return
		}
		filePath, err := planStore.Enter()
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
		planStore := app.PlanStore()
		if planStore == nil {
			http.Error(w, "plan system not enabled", http.StatusServiceUnavailable)
			return
		}
		content, err := planStore.Exit()
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
		bgStore := app.BgTaskStore()
		if bgStore == nil {
			http.Error(w, "bgtask system not enabled", http.StatusServiceUnavailable)
			return
		}
		tasks := bgStore.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks)
	})

	// GET /bgtasks/{id} — 获取后台任务详情
	mux.HandleFunc("GET /bgtasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		bgStore := app.BgTaskStore()
		if bgStore == nil {
			http.Error(w, "bgtask system not enabled", http.StatusServiceUnavailable)
			return
		}
		task := bgStore.Get(id)
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
		bgStore := app.BgTaskStore()
		if bgStore == nil {
			http.Error(w, "bgtask system not enabled", http.StatusServiceUnavailable)
			return
		}
		if err := bgStore.Kill(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	})

	// --- 可观测性端点 ---

	// --- Session 端点 ---
	// GET /sessions — 列出所有会话摘要（不含消息体）。
	// 前端据此恢复「最近对话」；session_id 由前端在 /chat 时自行指定。
	// 磁盘上遗留 running 状态的会话（进程被杀时未及收尾）修正为 interrupted；
	// 本进程内正在跑的会话（IsBusy）保持 running——客户端据此在断流后转轮询跟踪。
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		mgr := app.Sessions()
		if mgr == nil {
			http.Error(w, "session system not enabled", http.StatusServiceUnavailable)
			return
		}
		summaries, err := mgr.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, s := range summaries {
			if s.State == "running" && !mgr.IsBusy(s.ID) {
				s.State = "interrupted"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summaries)
	})

	// GET /sessions/{id}/messages — 返回会话的完整消息历史。
	// 用于前端重开项目时回放对话流：只含 user/assistant 两种角色的
	// 消息（按时间序），thinking 块保留在 assistant 消息内供前端折叠展示。
	mux.HandleFunc("GET /sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		mgr := app.Sessions()
		if mgr == nil {
			http.Error(w, "session system not enabled", http.StatusServiceUnavailable)
			return
		}
		sess, err := mgr.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if sess == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sess.Messages)
	})

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

	// GET /protocol — 协议自描述：版本、帧类型、端点表。客户端启动时
	// 拉取一次做兼容性校验（version 不匹配即警告），无需硬编码事件清单。
	mux.HandleFunc("GET /protocol", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.Describe())
	})

	return mux
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

// routeTaskStore 按 session_id 路由任务存储：存储实现了 SessionRouter
// （如 task.SessionStore）且请求带 session_id 时返回该会话的分区；
// 否则原样返回（全局共享存储，旧行为）。
func routeTaskStore(store task.StoreInterface, sessionID string) task.StoreInterface {
	if sessionID == "" {
		return store
	}
	if r, ok := store.(task.SessionRouter); ok {
		return r.ForSession(sessionID)
	}
	return store
}

// envelopeFromEvent 把 agent loop 事件映射为协议信封（字段语义见
// protocol.Envelope 注释）。事件类型名沿用历史线上格式（向后兼容）。
func envelopeFromEvent(ev Event, sessionID string) protocol.Envelope {
	env := protocol.Envelope{
		Type:          ev.Type.String(),
		SessionID:     sessionID,
		Text:          ev.Text,
		StatusKey:     ev.StatusKey, // 非空=状态行原地更新（429 重试倒计时等）
		Thinking:      ev.Thinking,
		ToolName:      ev.ToolName,
		ToolUseID:     ev.ToolUseID,
		ToolInput:     ev.ToolInput,
		ToolResult:    ev.ToolResult,
		Error:         errString(ev.Error),
		AgentID:       ev.AgentID,
		AgentDesc:     ev.AgentDesc,
		AgentStatus:   ev.AgentStatus,
		AgentActivity: ev.AgentActivity,
		AgentToolUses: ev.AgentToolUses,
		AgentTokens:   ev.AgentTokens,
	}
	if ev.Usage != nil {
		env.Usage = &protocol.Usage{
			InputTokens:  ev.Usage.InputTokens,
			OutputTokens: ev.Usage.OutputTokens,
		}
	}
	return env
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
	case EventInterrupted:
		// 用户主动终止：前端渲染「已停止」而非报错样式。
		return "interrupted"
	case EventRetrieval:
		// RAG 前置检索摘要（信息性事件，观测检索开销）。
		return "retrieval"
	case EventSteer:
		// guide 车道注入进模型上下文（工具批边界；宿主通知/环境提醒）。
		return protocol.TypeSteer
	case EventQueueRun:
		// queue 车道消费：排队消息作为新一轮输入开跑。
		return protocol.TypeQueueRun
	case EventSubAgentProgress:
		// 子 agent 运行进度（并发子 agent 的树形状态）。
		return protocol.TypeSubAgentProgress
	default:
		return "unknown"
	}
}

// permissionModeString / permissionModeFromString — 权限模式与 HTTP 字符串的
// 双向映射（GET/POST /mode 端点用）。
func permissionModeString(m PermissionModeOption) string {
	switch m {
	case PermissionBypass:
		return "bypass"
	case PermissionAcceptEdits:
		return "accept_edits"
	case PermissionPlanOnly:
		return "plan"
	case PermissionDenyAll:
		return "deny_all"
	default:
		return "default"
	}
}

func permissionModeFromString(s string) (PermissionModeOption, bool) {
	switch s {
	case "default":
		return PermissionDefault, true
	case "accept_edits":
		return PermissionAcceptEdits, true
	case "plan":
		return PermissionPlanOnly, true
	case "bypass":
		return PermissionBypass, true
	case "deny_all":
		return PermissionDenyAll, true
	default:
		return PermissionDefault, false
	}
}

// 确保 App 间接实现 http.Handler
var _ context.Context = context.Background()
