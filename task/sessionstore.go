// sessionstore.go 按会话隔离的 Task 存储（含落盘与闲置回收）。
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionRouter 由「按会话隔离」的 Task 存储实现（如 SessionStore）。
// Task 工具在执行时从 ctx.SessionID 路由到对应分区；HTTP /tasks 端点
// 按 query 参数路由。未实现此接口的存储保持全局共享（旧行为）。
type SessionRouter interface {
	ForSession(sessionID string) StoreInterface
}

// SessionStoreConfig 配置 SessionStore。
type SessionStoreConfig struct {
	// DirFn 返回某会话的落盘目录（如 <project>/.yume/tasks/<sid>.json 所在目录）。
	// nil = 纯内存，不落盘。
	DirFn func(sessionID string) string

	// IdleTTL 分区闲置多久后从内存回收（RAM 释放，落盘数据保留）。
	// 0 = 不回收（默认）。回收是惰性的：下次 ForSession/Sweep 时触发。
	IdleTTL time.Duration
}

// sessionPartition 一个会话的任务分区：内存 Store + 闲置时间戳。
type sessionPartition struct {
	store   *Store
	lastUse time.Time
}

// SessionStore 按会话隔离的 Task 存储。
//
// 语义对齐 Claude Code：task 跟随 session——不同会话各自一套任务列表，
// 互不可见。每个分区：
//   - 内存中是独立的 task.Store（读写走内存，快）；
//   - 若配置了 DirFn，每次变更后整分区 JSON 落盘，进程重启后
//     首次访问自动加载（磁盘是持久真源，内存是缓存）；
//   - 若配置了 IdleTTL，闲置超时的分区被逐出内存（G3：长时间无对话
//     的会话收回 RAM），下次访问从磁盘恢复。
type SessionStore struct {
	cfg   SessionStoreConfig
	mu    sync.Mutex
	parts map[string]*sessionPartition
}

// NewSessionStore 创建会话隔离存储。
func NewSessionStore(cfg SessionStoreConfig) *SessionStore {
	if cfg.IdleTTL < 0 {
		cfg.IdleTTL = 0
	}
	return &SessionStore{
		cfg:   cfg,
		parts: make(map[string]*sessionPartition),
	}
}

// ForSession 返回指定会话的任务分区（不存在则加载/创建）。
// 访问即刷新该分区的闲置计时；顺带惰性回收其他超时分区。
func (ss *SessionStore) ForSession(sessionID string) StoreInterface {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.sweepLocked()

	p, ok := ss.parts[sessionID]
	if !ok {
		p = &sessionPartition{store: ss.loadFromDisk(sessionID)}
		ss.parts[sessionID] = p
	}
	p.lastUse = time.Now()
	return &persistingStore{inner: p.store, save: func() { ss.saveToDisk(sessionID, p.store) }}
}

// Sweep 显式回收所有闲置超时的分区（供外部定时器调用）。
// 返回回收的分区数。
func (ss *SessionStore) Sweep() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.sweepLocked()
}

// StartSweeper 启动后台定时回收，返回停止函数。
// interval 为扫描周期；IdleTTL 才是回收阈值。
func (ss *SessionStore) StartSweeper(ctx context.Context, interval time.Duration) (stop func()) {
	if ss.cfg.IdleTTL <= 0 || interval <= 0 {
		return func() {}
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		select {
		case <-ctx.Done():
		case <-done:
		}
	}()
	return func() {
		close(done)
		ticker.Stop()
	}
}

// sweepLocked 回收闲置超时的分区；调用方需持有 ss.mu。
// 落盘数据在每次变更时已写入，逐出内存不丢数据。
func (ss *SessionStore) sweepLocked() int {
	if ss.cfg.IdleTTL <= 0 {
		return 0
	}
	now := time.Now()
	evicted := 0
	for sid, p := range ss.parts {
		if now.Sub(p.lastUse) >= ss.cfg.IdleTTL {
			delete(ss.parts, sid)
			evicted++
		}
	}
	return evicted
}

// sessionTaskFile 会话分区落盘文件路径（sessionID 消毒防路径穿越）。
func (ss *SessionStore) sessionTaskFile(sessionID string) string {
	if ss.cfg.DirFn == nil {
		return ""
	}
	dir := ss.cfg.DirFn(sessionID)
	if dir == "" {
		return ""
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, sessionID)
	if safe == "" || safe == "." || safe == ".." {
		safe = "_default"
	}
	return filepath.Join(dir, "tasks-"+safe+".json")
}

// sessionFileState 落盘格式：nextID + 任务数组。
type sessionFileState struct {
	NextID int64   `json:"next_id"`
	Tasks  []*Task `json:"tasks"`
}

// saveToDisk 整分区落盘（每次变更后调用；失败静默——落盘是缓存持久层，
// 失败只影响重启恢复，不影响当前会话）。
func (ss *SessionStore) saveToDisk(sessionID string, s *Store) {
	path := ss.sessionTaskFile(sessionID)
	if path == "" {
		return
	}
	s.mu.RLock()
	state := sessionFileState{Tasks: make([]*Task, 0, len(s.tasks))}
	for _, t := range s.tasks {
		state.Tasks = append(state.Tasks, t)
	}
	state.NextID = s.nextID.Load()
	s.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

// loadFromDisk 从磁盘恢复分区（文件缺失/损坏时返回空 Store）。
func (ss *SessionStore) loadFromDisk(sessionID string) *Store {
	s := NewStore()
	path := ss.sessionTaskFile(sessionID)
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var state sessionFileState
	if err := json.Unmarshal(b, &state); err != nil {
		return s // 损坏：从空开始，旧文件被下次落盘覆盖
	}
	for _, t := range state.Tasks {
		s.tasks[t.ID] = t
		if v, err := strconvParse(t.ID); err == nil && v > s.nextID.Load() {
			s.nextID.Store(v)
		}
	}
	if state.NextID > s.nextID.Load() {
		s.nextID.Store(state.NextID)
	}
	return s
}

// persistingStore 包装分区 Store：每次变更后回调保存。
type persistingStore struct {
	inner *Store
	save  func()
}

func (p *persistingStore) Create(subject, description, activeForm string, metadata map[string]any) *Task {
	t := p.inner.Create(subject, description, activeForm, metadata)
	p.save()
	return t
}

func (p *persistingStore) Get(id string) *Task { return p.inner.Get(id) }

func (p *persistingStore) Update(id string, patch UpdatePatch) (*Task, error) {
	t, err := p.inner.Update(id, patch)
	if err == nil {
		p.save()
	}
	return t, err
}

func (p *persistingStore) List() []*Task { return p.inner.List() }

func (p *persistingStore) Delete(id string) error {
	err := p.inner.Delete(id)
	if err == nil {
		p.save()
	}
	return err
}

func (p *persistingStore) ListSummaries() []ListSummary { return p.inner.ListSummaries() }

// strconvParse 解析十进制字符串为 int64。
func strconvParse(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

// StoreInterface 兼容：无会话上下文的调用（HTTP 不带 session_id、
// 工具拿不到 SessionID 的场景）落到 "" 兜底分区，与会话分区同样隔离。
func (ss *SessionStore) Create(subject, description, activeForm string, metadata map[string]any) *Task {
	return ss.ForSession("").Create(subject, description, activeForm, metadata)
}

func (ss *SessionStore) Get(id string) *Task { return ss.ForSession("").Get(id) }

func (ss *SessionStore) Update(id string, patch UpdatePatch) (*Task, error) {
	return ss.ForSession("").Update(id, patch)
}

func (ss *SessionStore) List() []*Task { return ss.ForSession("").List() }

func (ss *SessionStore) Delete(id string) error { return ss.ForSession("").Delete(id) }

func (ss *SessionStore) ListSummaries() []ListSummary { return ss.ForSession("").ListSummaries() }

var _ StoreInterface = (*SessionStore)(nil)

var _ StoreInterface = (*persistingStore)(nil)
var _ SessionRouter = (*SessionStore)(nil)
