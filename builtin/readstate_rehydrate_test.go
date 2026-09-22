package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
)

// 重水合：最近读的小文件全文重注（带行号），大文件退化为提醒；
// 最新读的优先，MaxFiles 限流；未注入会话 ID 时不产出任何提醒。
func TestReadStateRehydrater(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.md")
	big := filepath.Join(dir, "big.txt")
	deleted := filepath.Join(dir, "gone.md")

	if err := os.WriteFile(small, []byte("第一行\n第二行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bigContent := strings.Repeat("很长的内容", 5000) // 20000 字符 > 默认 12000 上限
	if err := os.WriteFile(big, []byte(bigContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// deleted：先真实存在并读取，随后删除（重水合时的已删除分支）。
	if err := os.WriteFile(deleted, []byte("旧内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 读取顺序：small → big → deleted（随后删除文件），最新优先。
	markRead("sess-rehydrate", small, "第一行\n第二行\n")
	markRead("sess-rehydrate", big, bigContent)
	markRead("sess-rehydrate", deleted, "旧内容")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	r := NewReadStateRehydrater()
	out := r.Reminders(goagent.WithSessionContext(context.Background(), "sess-rehydrate", ""))
	if len(out) != 3 {
		t.Fatalf("应产出 3 条提醒, got %d: %v", len(out), out)
	}
	// 最新优先：deleted 在前，big 次之，small 最后。
	if !strings.Contains(out[0], "可能已删除") {
		t.Fatalf("第 1 条应为已删除提示: %q", out[0])
	}
	if !strings.Contains(out[1], big) || !strings.Contains(out[1], "必须重新 Read") {
		t.Fatalf("第 2 条应为大文件提醒: %q", out[1][:80])
	}
	if !strings.Contains(out[2], "small.md") || !strings.Contains(out[2], "2\t第二行") {
		t.Fatalf("第 3 条应为小文件全文重注（带行号）: %q", out[2][:120])
	}

	// 未注入会话 ID：无提醒。
	if out := r.Reminders(context.Background()); out != nil {
		t.Fatalf("无会话 ID 应返回 nil, got %v", out)
	}
	// 无关会话：无提醒。
	if out := r.Reminders(goagent.WithSessionContext(context.Background(), "other", "")); out != nil {
		t.Fatalf("无关会话应返回 nil, got %v", out)
	}
}
