package loop

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/provider"
)

// debugModeCapture 用内存 buffer 收集 slog 输出的测试 handler。
func debugModeCapture() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &buf, slog.New(h)
}

// probeStreamProvider 按脚本回放流事件的假 provider（不依赖真实 LLM）。
type probeStreamProvider struct {
	model string
}

func (p *probeStreamProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ModelID:         p.model,
		SupportsTools:   false,
		SupportsCaching: false,
		ContextWindow:   100000,
	}
}

func (p *probeStreamProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "done"}
		ch <- provider.StreamEvent{Type: provider.EventMessageComplete, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

func (p *probeStreamProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: provider.StopEndTurn}, nil
}

// 关键事件日志（启动/完成/错误）不受 debug 开关控制，默认输出。
func TestLogging_KeyEventsWithoutDebug(t *testing.T) {
	buf, lg := debugModeCapture()

	l := New(Config{
		Provider:  &probeStreamProvider{model: "probe-1"},
		MaxTurns:  3,
		SessionID: "sess-log-1",
		Logger:    lg,
		Debug:     false,
	})
	for range l.Run(context.Background(), "hi") {
	}

	out := buf.String()
	if !strings.Contains(out, "loop: 启动") || !strings.Contains(out, "model=probe-1") {
		t.Fatalf("启动日志缺失: %q", out)
	}
	if !strings.Contains(out, "loop: 完成") {
		t.Fatalf("完成日志缺失: %q", out)
	}
	if strings.Contains(out, "turn=1 请求") {
		t.Fatalf("debug 关闭时不应输出细节日志: %q", out)
	}
}

// 测试模式（Debug=true）：逐轮请求参数等细节日志额外输出。
func TestLogging_DebugModeDetails(t *testing.T) {
	buf, lg := debugModeCapture()

	l := New(Config{
		Provider:  &probeStreamProvider{model: "probe-2"},
		MaxTurns:  3,
		SessionID: "sess-log-2",
		Logger:    lg,
		Debug:     true,
	})
	for range l.Run(context.Background(), "hi") {
	}

	out := buf.String()
	if !strings.Contains(out, "turn=1 请求: msgs=") {
		t.Fatalf("debug 开启时应输出逐轮请求明细: %q", out)
	}
	if !strings.Contains(out, "debug=true") {
		t.Fatalf("启动日志应标识 debug 状态: %q", out)
	}
}

// Logger 未注入（nil）时所有日志调用都应静默跳过，不 panic。
func TestLogging_NilLoggerSafe(t *testing.T) {
	l := New(Config{
		Provider: &probeStreamProvider{model: "probe-3"},
		MaxTurns: 2,
	})
	for range l.Run(context.Background(), "hi") {
	}
}
