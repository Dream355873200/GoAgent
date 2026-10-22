package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// within 在 d 内等待 fn 返回，超时视为死锁。
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s 在 %v 内未返回，疑似死锁", what, d)
	}
}

func TestSaveMainNoDeadlock(t *testing.T) {
	am := NewAutoMemory(t.TempDir())
	var err error
	within(t, 2*time.Second, "SaveMain", func() { err = am.SaveMain("主记忆") })
	if err != nil {
		t.Fatalf("SaveMain: %v", err)
	}
	if got := am.LoadMain(); got != "主记忆" {
		t.Fatalf("LoadMain = %q", got)
	}
}

// 读者持读锁期间若再次 RLock，而写者恰好在两次 RLock 之间排队，就会互相等待。
// 写者循环写入时这种交错几乎必然出现（原实现在此测试下稳定死锁）。
// 这里只用 Save 写入，SaveMain 的重入死锁由 TestSaveMainNoDeadlock 单独覆盖。
func TestNestedReadLockNoDeadlock(t *testing.T) {
	am := NewAutoMemory(t.TempDir())
	if err := am.Save("MEMORY", "keyword main"); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("topic%d", i)
		loop(func() {
			_ = am.Save(name, "keyword topic")
			_ = am.Save("MEMORY", "keyword main")
			_ = am.Delete(name)
		})
	}
	for i := 0; i < 4; i++ {
		loop(func() {
			_ = am.FormatForInjection()
			_, _ = am.Search([]string{"keyword"})
		})
		loop(func() {
			_ = am.LoadMain()
			_, _ = am.LoadAll()
			_ = am.Exists("topic0")
		})
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	within(t, 10*time.Second, "并发读写", wg.Wait)
}

func TestSaveRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	am := NewAutoMemory(filepath.Join(root, "mem"))

	bad := []string{"../escaped", `..\escaped`, "../../escaped", "sub/escaped", `sub\escaped`,
		"..", ".", "", "/abs/escaped", `C:\escaped`, "C:escaped", "con", "a..b", "trail."}
	for _, name := range bad {
		if err := am.Save(name, "x"); err == nil {
			t.Errorf("Save(%q) 应被拒绝", name)
		}
	}

	// 非法名称在落盘前被拒绝：根目录下不应出现任何文件（包括记忆目录本身）。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("记忆目录之外出现了 %s", e.Name())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.md")); !os.IsNotExist(err) {
		t.Errorf("escaped.md 被写到了记忆目录之外: %v", err)
	}
}

func TestExistsDeleteRejectInvalidName(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim.md")
	if err := os.WriteFile(victim, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	am := NewAutoMemory(filepath.Join(root, "mem"))
	if err := am.Save("ok", "x"); err != nil {
		t.Fatal(err)
	}

	if am.Exists("../victim") {
		t.Error("Exists(\"../victim\") 应返回 false")
	}
	if err := am.Delete("../victim"); err == nil {
		t.Error("Delete(\"../victim\") 应返回错误")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("记忆目录之外的文件被删除: %v", err)
	}
}

func TestMainFileStillUsable(t *testing.T) {
	am := NewAutoMemory(t.TempDir())

	if err := am.SaveMain("第一版"); err != nil {
		t.Fatal(err)
	}
	if !am.Exists("MEMORY") || am.LoadMain() != "第一版" {
		t.Fatalf("SaveMain 后 Exists=%v LoadMain=%q", am.Exists("MEMORY"), am.LoadMain())
	}
	if err := am.Save("MEMORY", "第二版"); err != nil {
		t.Fatalf("Save(\"MEMORY\"): %v", err)
	}
	if got := am.FormatForInjection(); got != "# auto memory\n\n第二版" {
		t.Fatalf("FormatForInjection = %q", got)
	}

	if err := am.Save("项目约定", "中文记忆名"); err != nil {
		t.Fatalf("中文记忆名应合法: %v", err)
	}
	found, err := am.Search([]string{"中文"})
	if err != nil || len(found) != 1 || found[0].Name != "项目约定" {
		t.Fatalf("Search = %+v, %v", found, err)
	}
	if err := am.Delete("项目约定"); err != nil || am.Exists("项目约定") {
		t.Fatalf("Delete: %v, Exists=%v", err, am.Exists("项目约定"))
	}
}

func TestDisabledAutoMemory(t *testing.T) {
	am := NewAutoMemory("")
	if err := am.Save("ok", "x"); err != nil {
		t.Errorf("未配置目录时 Save 应为空操作: %v", err)
	}
	if err := am.Save("../x", "x"); err == nil {
		t.Error("非法名称无论是否配置目录都应报错")
	}
	if am.Exists("ok") || am.LoadMain() != "" || am.FormatForInjection() != "" {
		t.Error("未配置目录时应读不到任何记忆")
	}
}
