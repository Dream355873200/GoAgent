package goagent

import (
	"context"
	"errors"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
	"github.com/Dream355873200/GoAgent/session"
)

// WithSessionModel：会话级模型覆盖必须落到该会话请求的 req.Model；
// 无覆盖的会话 req.Model 为空（跟随 provider 当前模型），互不影响。
func TestSessionModelOverride(t *testing.T) {
	m := scripted.New(
		scripted.Text("a").Expect(func(r *provider.Request) error {
			if r.Model != "model-b" {
				return errors.New("覆盖会话 req.Model = " + r.Model + ", want model-b")
			}
			return nil
		}),
		scripted.Text("b").Expect(func(r *provider.Request) error {
			if r.Model != "" {
				return errors.New("无覆盖会话 req.Model 应为空，实际 " + r.Model)
			}
			return nil
		}),
	)
	app := New(
		WithProvider(m),
		WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithSessionModel(func(sessionID string) string {
			if sessionID == "sa" {
				return "model-b"
			}
			return ""
		}),
	)
	for range app.RunSession(context.Background(), "sa", "hi") {
	}
	for range app.RunSession(context.Background(), "sb", "hi") {
	}
	if m.Remaining() != 0 {
		t.Fatalf("两个请求都应发生（脚本未走完）")
	}
}
