package goagent

import (
	"context"
	"testing"
)

// 会话上下文注入：WithSessionContext 的值必须经 newContextFromStd
// 透出到工具的 goagent.Context 字段（SessionID/WorkDir）。
// 这是「单进程多会话多根目录」的契约——G1 的核心。
func TestSessionContextPropagation(t *testing.T) {
	ctx := WithSessionContext(context.Background(), "sess-1", `/e/proj-a`)

	if got := SessionIDFromContext(ctx); got != "sess-1" {
		t.Errorf("SessionIDFromContext = %q, want sess-1", got)
	}
	if got := WorkDirFromContext(ctx); got != `/e/proj-a` {
		t.Errorf("WorkDirFromContext = %q, want /e/proj-a", got)
	}

	c := newContextFromStd(ctx)
	if c.SessionID != "sess-1" {
		t.Errorf("Context.SessionID = %q, want sess-1", c.SessionID)
	}
	if c.WorkDir != `/e/proj-a` {
		t.Errorf("Context.WorkDir = %q, want /e/proj-a", c.WorkDir)
	}
}

// 未注入时保持旧行为：空 SessionID / 空 WorkDir（工具回退进程 cwd）。
func TestSessionContextEmpty(t *testing.T) {
	ctx := context.Background()
	if got := SessionIDFromContext(ctx); got != "" {
		t.Errorf("SessionIDFromContext = %q, want empty", got)
	}
	if got := WorkDirFromContext(ctx); got != "" {
		t.Errorf("WorkDirFromContext = %q, want empty", got)
	}
	c := newContextFromStd(ctx)
	if c.SessionID != "" || c.WorkDir != "" {
		t.Errorf("newContextFromStd 应为空值，得到 %+v", c)
	}
}
