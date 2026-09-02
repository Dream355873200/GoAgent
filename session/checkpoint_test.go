package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// checkpoint 往返：写入 → 读取最近一条 → Restore 回填到 Session。
func TestCheckpoint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	storage := NewStorage(dir)
	sid := "sess-cp-1"

	// 无 checkpoint 时返回 nil, nil。
	cp, err := storage.ReadLastCheckpoint(sid)
	if err != nil {
		t.Fatalf("空会话读取不应报错: %v", err)
	}
	if cp != nil {
		t.Fatalf("无 checkpoint 应返回 nil, got %+v", cp)
	}

	// 写两条：恢复点应是最后一条。
	if err := storage.WriteCheckpoint(sid, Checkpoint{Reason: "approval", Turn: 2, PendingStep: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteCheckpoint(sid, Checkpoint{Reason: "user", Turn: 3, PendingStep: "confirm"}); err != nil {
		t.Fatal(err)
	}

	cp, err = storage.ReadLastCheckpoint(sid)
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.Reason != "user" || cp.Turn != 3 || cp.PendingStep != "confirm" {
		t.Fatalf("应取最后一条 checkpoint, got %+v", cp)
	}
	if cp.CreatedAt == 0 {
		t.Fatal("CreatedAt 应自动填充")
	}
}

// Restore 把 checkpoint 回填到 Session.LastCheckpoint。
func TestCheckpoint_RestoreBackfill(t *testing.T) {
	dir := t.TempDir()
	storage := NewStorage(dir)
	sid := "sess-cp-2"

	if err := storage.WriteCheckpoint(sid, Checkpoint{Reason: "approval", Turn: 5, PendingStep: "tool:deploy"}); err != nil {
		t.Fatal(err)
	}

	sess, err := Restore(storage, sid)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastCheckpoint == nil || sess.LastCheckpoint.Reason != "approval" || sess.LastCheckpoint.Turn != 5 {
		t.Fatalf("Restore 应回填 checkpoint, got %+v", sess.LastCheckpoint)
	}
}

// FileStore 实现 Checkpointer（Manager 能力探测路径）。
func TestCheckpoint_ManagerViaFileStore(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(NewFileStore(dir))
	ctx := context.Background()

	sid, err := mgr.Create(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.WriteCheckpoint(ctx, sid, Checkpoint{Reason: "approval", Turn: 1}); err != nil {
		t.Fatalf("FileStore 应支持 checkpoint: %v", err)
	}
	cp, err := mgr.ReadLastCheckpoint(ctx, sid)
	if err != nil || cp == nil || cp.Turn != 1 {
		t.Fatalf("读取失败: cp=%+v err=%v", cp, err)
	}
}

// MemoryStore 未实现 Checkpointer → ErrCheckpointUnsupported。
func TestCheckpoint_UnsupportedStore(t *testing.T) {
	mgr := NewManager(NewMemoryStore())
	ctx := context.Background()

	if err := mgr.WriteCheckpoint(ctx, "s", Checkpoint{}); err == nil {
		t.Fatal("MemoryStore 不支持 checkpoint 应报错")
	}
	if _, err := mgr.ReadLastCheckpoint(ctx, "s"); err == nil {
		t.Fatal("MemoryStore 读取也应报错")
	}
}

// checkpoint 记录与消息记录混排：ReadAll 不受影响。
func TestCheckpoint_MixedWithMessages(t *testing.T) {
	dir := t.TempDir()
	storage := NewStorage(dir)
	sid := "sess-cp-3"

	if err := storage.WriteState(sid, StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteCheckpoint(sid, Checkpoint{Reason: "user", Turn: 1}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteState(sid, StateSuspended); err != nil {
		t.Fatal(err)
	}

	records, err := storage.ReadAll(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("应有 3 条记录, got %d", len(records))
	}

	// 文件确实落盘。
	if _, err := os.Stat(filepath.Join(dir, sid+".jsonl")); err != nil {
		t.Fatalf("JSONL 文件应存在: %v", err)
	}
}
