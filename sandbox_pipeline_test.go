package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// 沙箱 pipeline 接线测试：run 级沙箱注入、节点级策略覆盖、
// PipelineConfig.SessionID 会话上下文缺口修复。
//
// probeProvider 按节点名记录轮次：第 1 轮返回 probe 工具调用，第 2 轮
// 文本收尾——不依赖真实 LLM 即可驱动「agent 调工具」全链路。
type probeProvider struct {
	mu    sync.Mutex
	calls map[string]int
}

func (p *probeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{}
}

func (p *probeProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return &provider.Response{Message: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "ok"}}}}, nil
}

func (p *probeProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	key := "main"
	if v, ok := ctx.Value(PipelineNodeNameKey).(string); ok && v != "" {
		key = v
	}
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[key]++
	turn := p.calls[key]
	p.mu.Unlock()

	ch := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(ch)
		if turn == 1 {
			id := fmt.Sprintf("call-%s-1", key)
			ch <- provider.StreamEvent{
				Type:     provider.EventToolUseStart,
				ToolCall: &message.ToolCall{ID: id, Name: "probe", Input: json.RawMessage(`{}`)},
			}
			ch <- provider.StreamEvent{
				Type:       provider.EventMessageComplete,
				StopReason: provider.StopToolUse,
				Message: &message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{
					Type: "tool_use", ToolUseID: id, ToolName: "probe", Input: json.RawMessage(`{}`),
				}}},
			}
			return
		}
		ch <- provider.StreamEvent{Type: provider.EventTextDelta, Text: "done"}
		ch <- provider.StreamEvent{
			Type:       provider.EventMessageComplete,
			StopReason: provider.StopEndTurn,
			Message:    &message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: "text", Text: "done"}}},
		}
	}()
	return ch, nil
}

// probeCtx 工具收到调用时记录 Context 的关键字段。
type probeResult struct {
	SessionID string
	WorkDir   string
	SbRoot    string
}

// newProbePipeline 构造单节点 pipeline：节点 agent 带 probe 工具，
// provider 第 1 轮调 probe、第 2 轮收尾。
func newProbePipeline(t *testing.T, cfg PipelineConfig) (*App, chan probeResult) {
	t.Helper()
	results := make(chan probeResult, 8)
	type probeInput struct{}
	probe := InferTool("probe", "探测上下文", func(ctx context.Context, in probeInput) (string, error) {
		root := ""
		if sb := SandboxFromContext(ctx); sb != nil {
			root = sb.Root()
		}
		results <- probeResult{SessionID: SessionIDFromContext(ctx), WorkDir: WorkDirFromContext(ctx), SbRoot: root}
		return "probed", nil
	})
	cfg.Nodes = []PipelineNode{{
		Name:    "worker",
		Message: "probe now",
		Agent:   &PipelineAgentDef{Name: "worker", Instruction: "test", Tools: []NamedTool{probe}},
	}}
	app := New(WithProvider(&probeProvider{}), WithMaxTurns(4))
	app.UsePipeline(cfg)
	return app, results
}

// 缺口修复：PipelineConfig.SessionID + WithSessionWorkDir →
// 节点工具看到 SessionID 与 WorkDir（此前恒为空）。
func TestPipelineSessionContextGapFix(t *testing.T) {
	workRoot := t.TempDir()
	app, results := newProbePipeline(t, PipelineConfig{SessionID: "sess-42"})
	app.config.sessionWorkDirFn = func(id string) string { return workRoot }

	if err := app.RunPipeline(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.SessionID != "sess-42" {
			t.Errorf("节点工具 SessionID = %q, want sess-42", r.SessionID)
		}
		if r.WorkDir != workRoot {
			t.Errorf("节点工具 WorkDir = %q, want %q", r.WorkDir, workRoot)
		}
	default:
		t.Fatal("probe 工具未被调用")
	}
}

// run 级沙箱：WithSandbox → 节点工具拿到沙箱会话，沙箱根覆盖 WorkDir。
func TestPipelineRunLevelSandbox(t *testing.T) {
	base := t.TempDir()
	app, results := newProbePipeline(t, PipelineConfig{SessionID: "sess-1"})
	app.config.sandbox = NewDirSandbox(base)
	app.config.sandboxPolicy = Policy{}

	if err := app.RunPipeline(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.SbRoot == "" {
			t.Fatal("节点工具应拿到沙箱会话")
		}
		if r.WorkDir != r.SbRoot {
			t.Errorf("WorkDir 应被沙箱根覆盖: workdir=%q root=%q", r.WorkDir, r.SbRoot)
		}
	default:
		t.Fatal("probe 工具未被调用")
	}
}

// 节点级覆盖：node.Sandbox 策略生效（工具拿到独立会话，与 run 级不同根）。
func TestPipelineNodeLevelSandboxOverride(t *testing.T) {
	runBase := t.TempDir()
	nodeBase := t.TempDir()
	app, results := newProbePipeline(t, PipelineConfig{SessionID: "sess-2"})
	app.config.sandbox = NewDirSandbox(runBase)
	app.config.sandboxPolicy = Policy{}
	app.pipeline.cfg.Nodes[0].Sandbox = &Policy{}
	// 让节点沙箱可区分：节点级用另一个 base。通过自定义工厂实现。
	app.config.sandbox = multiBaseSandbox{run: NewDirSandbox(runBase), node: NewDirSandbox(nodeBase)}

	if err := app.RunPipeline(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.SbRoot == "" {
			t.Fatal("节点工具应拿到沙箱会话")
		}
		if r.WorkDir != r.SbRoot {
			t.Errorf("WorkDir 应被节点沙箱根覆盖: workdir=%q root=%q", r.WorkDir, r.SbRoot)
		}
	default:
		t.Fatal("probe 工具未被调用")
	}
}

// multiBaseSandbox 测试辅助：run 级与节点级分别用不同 base 的沙箱工厂，
// 验证节点级覆盖确实产生了独立会话。
type multiBaseSandbox struct {
	run  *DirSandbox
	node *DirSandbox
}

func (m multiBaseSandbox) Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error) {
	// 节点级覆盖的 sessionID = 节点名（runNode 以 nodeName 调 Enter）。
	if sessionID == "worker" {
		return m.node.Enter(ctx, sessionID, policy)
	}
	return m.run.Enter(ctx, sessionID, policy)
}

// run 级沙箱 Enter 失败 → RunPipeline 返回错误（不 panic）。
func TestPipelineSandboxEnterError(t *testing.T) {
	app, _ := newProbePipeline(t, PipelineConfig{})
	app.config.sandbox = errSandbox{}
	err := app.RunPipeline(context.Background())
	if err == nil || err.Error() == "" {
		t.Fatalf("沙箱 Enter 失败应返回错误: %v", err)
	}
}

type errSandbox struct{}

func (errSandbox) Enter(ctx context.Context, sessionID string, policy Policy) (SandboxSession, error) {
	return nil, fmt.Errorf("boom")
}

var _ Sandbox = errSandbox{}
