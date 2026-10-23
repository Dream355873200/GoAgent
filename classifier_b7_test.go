// classifier_b7_test.go 可插拔分类器（v0.14 B7）：WithPermissionClassifier 替换
// YOLO，auto 模式下裁决工具调用，拒绝原因进入交给模型的工具结果。
package goagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/permission"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
)

func TestB7_PermissionClassifier(t *testing.T) {
	m := scripted.New(
		scripted.Tools(
			scripted.ToolCall{ID: "ok", Name: "deploy", Input: map[string]string{"env": "staging"}},
			scripted.ToolCall{ID: "no", Name: "deploy", Input: map[string]string{"env": "prod"}},
		),
		scripted.Text("好的").Expect(func(r *provider.Request) error {
			results := map[string]string{}
			for _, msg := range r.Messages {
				for _, b := range msg.Content {
					if b.Type == "tool_result" {
						results[b.ForToolUseID] = b.Text
					}
				}
			}
			if results["ok"] != "deployed staging" {
				return errors.New("放行的调用应执行: " + results["ok"])
			}
			if !strings.Contains(results["no"], "禁止操作生产环境") {
				return errors.New("拒绝原因应交给模型: " + results["no"])
			}
			return nil
		}),
	)
	var asked []string
	app := New(
		WithProvider(m),
		WithPermissionMode(PermissionAuto),
		WithPermissionClassifier(permission.ClassifierFunc(func(ctx context.Context, req permission.ClassifyRequest) (permission.Classification, error) {
			asked = append(asked, req.Input)
			if len(req.Transcript) == 0 || message.ExtractText(req.Transcript[0]) != "部署" {
				return permission.Classification{}, errors.New("分类请求应带对话记录")
			}
			if strings.Contains(req.Input, "prod") {
				return permission.Classification{Behavior: permission.BehaviorDeny, Reason: "禁止操作生产环境"}, nil
			}
			return permission.Classification{Behavior: permission.BehaviorAllow}, nil
		})),
	)
	app.Tool("deploy", ToolDef{Description: "deploy", Input: struct {
		Env string `json:"env"`
	}{}, Permission: RequireApproval,
		Execute: func(_ Context, in struct {
			Env string `json:"env"`
		}) (string, error) {
			return "deployed " + in.Env, nil
		}})
	for ev := range app.Run(context.Background(), "部署") {
		if ev.Type == EventError {
			t.Fatalf("运行出错: %v", ev.Error)
		}
	}
	m.AssertDone(t)
	if len(asked) != 2 {
		t.Fatalf("两次调用都应经分类器: %v", asked)
	}
}
