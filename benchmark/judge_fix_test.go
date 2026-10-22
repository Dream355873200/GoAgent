package benchmark

import (
	"context"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// capturingProvider 记录收到的请求，按固定文本回复。
type capturingProvider struct {
	reply string
	reqs  []*provider.Request
}

func (p *capturingProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ModelID: "judge-test"}
}

func (p *capturingProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.reqs = append(p.reqs, req)
	return &provider.Response{Message: message.NewAssistantMessage(p.reply)}, nil
}

func (p *capturingProvider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.StreamEvent, error) {
	panic("judge 不应使用流式接口")
}

// 判分请求必须带上裁判 system prompt：此前 judgeSystem 定义了却从未发送，
// 裁判既不知道判分纪律，也不知道"首行 PASS/FAIL"的输出格式。
func TestFix_LLMJudgeSendsSystemPrompt(t *testing.T) {
	p := &capturingProvider{reply: "PASS\n满足准则。"}
	j := &LLMJudge{Provider: p}
	if _, err := j.Judge(context.Background(), JudgeInput{Input: "q", Output: "a", Criterion: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 1 {
		t.Fatalf("应调用一次 judge，实际 %d 次", len(p.reqs))
	}
	sys := p.reqs[0].SystemPrompt
	if sys != judgeSystem {
		t.Fatalf("判分请求未携带裁判 system prompt: %q", sys)
	}
	if !strings.Contains(sys, "PASS 或 FAIL") {
		t.Fatalf("裁判 system prompt 应约定输出格式: %q", sys)
	}
}
