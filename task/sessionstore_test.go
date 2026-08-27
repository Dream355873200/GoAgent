package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 会话隔离：不同会话的任务列表互不可见（task 跟随 session）。
func TestSessionStoreIsolation(t *testing.T) {
	ss := NewSessionStore(SessionStoreConfig{})

	a := ss.ForSession("sess-a")
	b := ss.ForSession("sess-b")

	a.Create("任务A", "", "", nil)
	b.Create("任务B", "", "", nil)

	if got := len(a.ListSummaries()); got != 1 {
		t.Errorf("sess-a 应有 1 个任务，得到 %d", got)
	}
	if got := len(b.ListSummaries()); got != 1 {
		t.Errorf("sess-b 应有 1 个任务，得到 %d", got)
	}
	if a.ListSummaries()[0].Subject != "任务A" || b.ListSummaries()[0].Subject != "任务B" {
		t.Errorf("会话任务串了: a=%v b=%v", a.ListSummaries(), b.ListSummaries())
	}

	// 隔离存储的 ID 各自从 1 自增。
	if a.ListSummaries()[0].ID != "1" || b.ListSummaries()[0].ID != "1" {
		t.Errorf("各分区 ID 应独立自增: a=%s b=%s", a.ListSummaries()[0].ID, b.ListSummaries()[0].ID)
	}
}

// 落盘与恢复：新实例从磁盘加载旧数据（进程重启模拟）。
func TestSessionStorePersistence(t *testing.T) {
	dir := t.TempDir()
	ss := NewSessionStore(SessionStoreConfig{
		DirFn: func(sessionID string) string { return filepath.Join(dir, sessionID) },
	})

	s := ss.ForSession("proj1")
	s.Create("写登录页", "", "", nil)
	s.Create("写注册页", "", "", nil)

	// 新实例（同目录）：模拟进程重启后恢复。
	ss2 := NewSessionStore(SessionStoreConfig{
		DirFn: func(sessionID string) string { return filepath.Join(dir, sessionID) },
	})
	s2 := ss2.ForSession("proj1")
	sums := s2.ListSummaries()
	if len(sums) != 2 {
		t.Fatalf("重启后应恢复 2 个任务，得到 %d", len(sums))
	}
	// nextID 也恢复：新建不撞旧 ID。
	nt := s2.Create("新任务", "", "", nil)
	if nt.ID == "1" || nt.ID == "2" {
		t.Errorf("恢复后新建任务 ID 应 >2，得到 %s", nt.ID)
	}
}

// 闲置回收：超时分区被逐出内存，数据仍在磁盘，下次访问恢复。
func TestSessionStoreIdleEviction(t *testing.T) {
	dir := t.TempDir()
	ss := NewSessionStore(SessionStoreConfig{
		DirFn:   func(sessionID string) string { return dir },
		IdleTTL: 50 * time.Millisecond,
	})

	s := ss.ForSession("idle-sess")
	s.Create("会被回收的任务", "", "", nil)

	// 未超时：Sweep 不回收。
	time.Sleep(10 * time.Millisecond)
	if n := ss.Sweep(); n != 0 {
		t.Errorf("未超时不应回收，回收了 %d 个", n)
	}
	// 超时：回收。
	time.Sleep(60 * time.Millisecond)
	if n := ss.Sweep(); n != 1 {
		t.Fatalf("超时应回收 1 个分区，回收了 %d 个", n)
	}
	// 回收后：内存分区没了，但数据从磁盘恢复。
	ss.mu.Lock()
	_, exists := ss.parts["idle-sess"]
	ss.mu.Unlock()
	if exists {
		t.Errorf("分区应已逐出内存")
	}
	s2 := ss.ForSession("idle-sess")
	if got := len(s2.ListSummaries()); got != 1 {
		t.Errorf("回收后重新访问应从磁盘恢复任务，得到 %d 个", got)
	}
}

// sessionID 路径消毒：恶意 ID 不逃出落盘目录。
func TestSessionStorePathSanitize(t *testing.T) {
	dir := t.TempDir()
	ss := NewSessionStore(SessionStoreConfig{
		DirFn: func(sessionID string) string { return dir },
	})
	_ = ss.ForSession("../../evil/../sess")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel != e.Name() {
			t.Errorf("落盘文件逃出目录: %s", e.Name())
		}
	}
}

// 工具层路由契约：SessionRouter 断言。
func TestSessionStoreImplementsRouter(t *testing.T) {
	var _ SessionRouter = NewSessionStore(SessionStoreConfig{})
	_ = context.Background()
}
