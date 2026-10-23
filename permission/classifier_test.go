package permission

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
)

// funcClassifier 记录收到的请求并按 decide 裁决。
type funcClassifier struct {
	mu     sync.Mutex
	reqs   []ClassifyRequest
	decide func(ClassifyRequest) (Classification, error)
}

func (c *funcClassifier) ClassifyCall(ctx context.Context, req ClassifyRequest) (Classification, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		return Classification{}, errors.New("应带超时")
	}
	return c.decide(req)
}

// 自定义分类器在 auto 模式下裁决每个非只读调用：放行、拒绝（带原因）、改为
// 询问；请求带工具名、参数、级别、原因与 ctx 里的对话记录；Source 为 classifier。
func TestCustomClassifierAutoMode(t *testing.T) {
	clf := &funcClassifier{decide: func(r ClassifyRequest) (Classification, error) {
		switch {
		case strings.Contains(r.Input, "prod"):
			return Classification{Behavior: BehaviorDeny, Reason: "禁止操作生产环境"}, nil
		case strings.Contains(r.Input, "maybe"):
			return Classification{Behavior: BehaviorAsk, Reason: "拿不准"}, nil
		}
		return Classification{Behavior: BehaviorAllow, Reason: "规则允许"}, nil
	}}
	g := NewGate(nil)
	g.SetMode(ModeAuto)
	g.SetClassifier(clf)

	transcript := []message.Message{message.NewUserMessage("部署到测试环境")}
	ctx := WithTranscript(context.Background(), transcript)
	if ok, reason := g.CheckContext(ctx, "Deploy", `{"env":"staging"}`, LevelNormal); !ok || reason != "规则允许" {
		t.Fatalf("应放行: %v %q", ok, reason)
	}
	if ok, reason := g.CheckContext(ctx, "Deploy", `{"env":"prod"}`, LevelNormal); ok || !strings.Contains(reason, "禁止操作生产环境") {
		t.Fatalf("应拒绝并带原因: %v %q", ok, reason)
	}
	r := g.CheckResult("Deploy", `{"env":"maybe"}`, LevelNormal)
	if r.Behavior != BehaviorAsk || r.Source != "classifier" || r.Reason != "拿不准" {
		t.Fatalf("Ask 裁决应改为询问: %+v", r)
	}
	if ok, _ := g.Check("Deploy", `{"env":"maybe"}`, LevelNormal); ok {
		t.Fatal("没有审批者时询问即拒绝")
	}
	if r := g.CheckResult("Read", `{}`, LevelReadOnly); r.Behavior != BehaviorAllow {
		t.Fatalf("只读调用不经分类器: %+v", r)
	}

	clf.mu.Lock()
	first := clf.reqs[0]
	n := len(clf.reqs)
	clf.mu.Unlock()
	if n != 4 {
		t.Fatalf("只读调用不应调用分类器: %d 次", n)
	}
	if first.ToolName != "Deploy" || first.Input != `{"env":"staging"}` || first.Level != LevelNormal ||
		first.Why != "auto 模式下的操作" || len(first.Transcript) != 1 {
		t.Fatalf("分类请求: %+v", first)
	}
}

// bypass 模式只把高风险调用交给分类器，Why 说明原因；分类器出错或返回无效
// 裁决时改为询问；typed nil 的 YOLO 分类器等于没有分类器。
func TestCustomClassifierBypassAndFailures(t *testing.T) {
	clf := &funcClassifier{decide: func(r ClassifyRequest) (Classification, error) {
		return Classification{Behavior: BehaviorDeny, Reason: "不行"}, nil
	}}
	g := NewGate(nil)
	g.SetMode(ModeBypassPermissions)
	g.SetClassifier(clf)
	if ok, _ := g.Check("Bash", bashInput("go test ./..."), LevelNormal); !ok {
		t.Fatal("普通命令应直接放行")
	}
	if ok, reason := g.Check("Bash", bashInput("rm -rf /tmp/x"), LevelNormal); ok || reason != "不行" {
		t.Fatalf("高风险命令应经分类器拒绝: %v %q", ok, reason)
	}
	clf.mu.Lock()
	why := clf.reqs[0].Why
	clf.mu.Unlock()
	if len(clf.reqs) != 1 || !strings.HasPrefix(why, "高风险操作（") {
		t.Fatalf("只有高风险调用经分类器，Why=%q n=%d", why, len(clf.reqs))
	}

	for name, decide := range map[string]func(ClassifyRequest) (Classification, error){
		"错误": func(ClassifyRequest) (Classification, error) {
			return Classification{}, errors.New("策略服务不可达")
		},
		"无效裁决": func(ClassifyRequest) (Classification, error) { return Classification{Behavior: 42}, nil },
	} {
		g.SetClassifier(&funcClassifier{decide: decide})
		if r := g.CheckResult("Bash", bashInput("rm -rf /tmp/x"), LevelNormal); r.Behavior != BehaviorAsk || r.Source != "classifier_unavailable" {
			t.Fatalf("%s：应改为询问: %+v", name, r)
		}
	}

	var nilYolo *YoloClassifier
	g.SetClassifier(nilYolo)
	if r := g.CheckResult("Bash", bashInput("rm -rf /tmp/x"), LevelNormal); r.Behavior != BehaviorAsk || r.Source != "classifier_unavailable" {
		t.Fatalf("typed nil 应等于没有分类器（而不是空指针 panic）: %+v", r)
	}
}

// ClassifierFunc 与 YOLO 分类器都实现 Classifier。
func TestClassifierImplementations(t *testing.T) {
	var _ Classifier = ClassifierFunc(nil)
	var _ Classifier = (*YoloClassifier)(nil)
	g, _ := newClassifiedGate(ModeAuto, "yes", false)
	if r := g.CheckResult("Deploy", `{}`, LevelNormal); r.Behavior != BehaviorDeny || r.Source != "yolo_classifier" || !strings.HasPrefix(r.Reason, "YOLO 分类器阻止") {
		t.Fatalf("YOLO 经 Classifier 接口的裁决: %+v", r)
	}
}
