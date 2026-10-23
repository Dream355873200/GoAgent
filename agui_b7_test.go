// agui_b7_test.go AG-UI 前端工具与改参审批（v0.14 B7）：前端工具以
// success + pendingToolCallIds 停车、tool 消息带回结果后接着输出；并行前端工具
// 分批作答；审批 resume 带 editedArgs 改参执行；v1 /approve 的 input 同样改参。
package goagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/protocol"
	"github.com/Dream355873200/GoAgent/protocol/agui"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/provider/scripted"
	"github.com/Dream355873200/GoAgent/session"
)

func toolResultsOf(r *provider.Request) map[string]string {
	out := map[string]string{}
	for _, msg := range r.Messages {
		for _, b := range msg.Content {
			if b.Type == "tool_result" {
				out[b.ForToolUseID] = b.Text
			}
		}
	}
	return out
}

var chartTool = agui.Tool{
	Name:        "show_chart",
	Description: "在页面上画图",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"]}`),
}

func toolMsg(id, callID, content string) agui.Message {
	return agui.Message{ID: id, Role: "tool", ToolCallID: callID, Content: content}
}

// 前端工具：运行以 success + pendingToolCallIds 结束；没带结果的新请求被拒；
// tool 消息带回结果后从停车处接着输出，不重发 TOOL_CALL_START、也不回显客户端
// 自己给出的结果。
func TestB7_AGUIFrontendTool(t *testing.T) {
	m := scripted.New(
		scripted.Tools(scripted.ToolCall{ID: "fc1", Name: "show_chart", Input: map[string]string{"kind": "bar"}}),
		scripted.Text("图表已显示").Expect(func(r *provider.Request) error {
			if got := toolResultsOf(r)["fc1"]; got != "rendered bar" {
				return errors.New("前端工具的结果应交给模型: " + got)
			}
			if len(r.Tools) == 0 || r.Tools[len(r.Tools)-1].Name != "show_chart" {
				return errors.New("前端工具应出现在请求的工具列表里")
			}
			return nil
		}),
	)
	app := New(WithProvider(m), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	base := aguiServer(t, app)
	in := agui.RunAgentInput{ThreadID: "b7-fe", RunID: "r1", Messages: []agui.Message{userMsg("u1", "画个柱状图")}, Tools: []agui.Tool{chartTool}}

	frames := postAGUI(t, base, in, false)
	start := findFrame(frames, agui.TypeToolCallStart)
	end := frames[len(frames)-1]
	if start == nil || start.ToolCallID != "fc1" || start.ToolCallName != "show_chart" || findFrame(frames, agui.TypeToolCallResult) != nil {
		t.Fatalf("前端工具调用应开始且没有结果: %s", aguiTypes(frames))
	}
	if end.Outcome == nil || end.Outcome.Type != "success" || strings.Join(end.Outcome.PendingToolCallIDs, ",") != "fc1" {
		t.Fatalf("应以 success + pendingToolCallIds 结束: %s", end.raw)
	}

	blocked := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b7-fe", RunID: "rx", Messages: []agui.Message{userMsg("u1", "画个柱状图")}, Tools: []agui.Tool{chartTool}}, false)
	if f := blocked[len(blocked)-1]; f.Type != agui.TypeRunError || f.Code != "pending_tool_calls" {
		t.Fatalf("没带结果的请求应 RUN_ERROR{pending_tool_calls}: %s", f.raw)
	}

	in.RunID = "r2"
	in.Messages = append(in.Messages, toolMsg("t1", "fc1", "rendered bar"))
	resumed := postAGUI(t, base, in, true)
	if findFrame(resumed, agui.TypeToolCallStart) != nil || findFrame(resumed, agui.TypeToolCallResult) != nil {
		t.Fatalf("接着输出时不重发 START，也不回显客户端给出的结果: %s", aguiTypes(resumed))
	}
	if f := resumed[len(resumed)-1]; f.Type != agui.TypeRunFinished || f.Outcome != nil {
		t.Fatalf("应成功结束: %s", f.raw)
	}
	snap := findFrame(resumed, agui.TypeMessagesSnapshot)
	var sawTool bool
	for _, msg := range snap.Messages {
		if msg.Role == "tool" && msg.ToolCallID == "fc1" && msg.Content == "rendered bar" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("快照应包含前端工具的结果")
	}
	m.AssertDone(t)
}

// 同一轮的两个前端工具：每次只回答第一个待处理的调用，未回答的立即再次停车，
// 直到全部带回结果后运行完成；工具名不合法时拒绝。
func TestB7_AGUIParallelFrontendTools(t *testing.T) {
	m := scripted.New(
		scripted.Tools(
			scripted.ToolCall{ID: "fa", Name: "show_chart", Input: map[string]string{"kind": "a"}},
			scripted.ToolCall{ID: "fb", Name: "show_chart", Input: map[string]string{"kind": "b"}},
		),
		scripted.Text("两张都好了").Expect(func(r *provider.Request) error {
			got := toolResultsOf(r)
			if got["fa"] != "ok-a" || got["fb"] != "ok-b" {
				return fmt.Errorf("两个结果都应交给模型: %v", got)
			}
			return nil
		}),
	)
	app := New(WithProvider(m), WithSessionManager(session.NewManager(session.NewMemoryStore())))
	base := aguiServer(t, app)

	bad := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b7-bad", RunID: "r0", Messages: []agui.Message{userMsg("u0", "x")},
		Tools: []agui.Tool{{Name: "坏 名字", Description: "x"}}}, false)
	if f := bad[len(bad)-1]; f.Type != agui.TypeRunError || f.Code != "invalid_tools" {
		t.Fatalf("不合法的工具名应 RUN_ERROR{invalid_tools}: %s", f.raw)
	}

	in := agui.RunAgentInput{ThreadID: "b7-par", RunID: "r1", Messages: []agui.Message{userMsg("u1", "画两张")}, Tools: []agui.Tool{chartTool}}
	frames := postAGUI(t, base, in, false)
	answered := map[string]bool{}
	for round := 0; ; round++ {
		end := frames[len(frames)-1]
		if end.Type != agui.TypeRunFinished || end.Outcome == nil || end.Outcome.Type != "success" {
			if end.Type == agui.TypeRunFinished && end.Outcome == nil {
				break
			}
			t.Fatalf("第 %d 轮应以 success 结束: %s", round, end.raw)
		}
		pending := end.Outcome.PendingToolCallIDs
		if len(pending) == 0 {
			break
		}
		t.Logf("第 %d 轮待作答: %v", round, pending)
		if round > 4 {
			t.Fatalf("轮次过多: %v", pending)
		}
		id := pending[0]
		if answered[id] {
			t.Fatalf("已回答的调用 %s 又出现在 pendingToolCallIds", id)
		}
		answered[id] = true
		in.RunID = fmt.Sprintf("r%d", round+2)
		in.Messages = append(in.Messages, toolMsg("t-"+id, id, "ok-"+strings.TrimPrefix(id, "f")))
		frames = postAGUI(t, base, in, true)
	}
	if !answered["fa"] || !answered["fb"] {
		t.Fatalf("两个前端工具都应被要求作答: %v", answered)
	}
	m.AssertDone(t)
}

// 改参审批：resume 带 editedArgs 时工具以修改后的参数执行，结果注明参数被修改；
// 修改后的参数不符合 schema 时不执行。能力描述声明 approveWithEdits 与 clientProvided。
func TestB7_AGUIEditedArgs(t *testing.T) {
	for _, tc := range []struct {
		name, edited, want string
		ran                bool
	}{
		{"合法改参", `{"path":"b.txt"}`, "wrote b.txt", true},
		{"不合法改参", `{"path":1}`, "不符合工具", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := scripted.New(
				scripted.Tools(scripted.ToolCall{ID: "w1", Name: "write", Input: map[string]string{"path": "a.txt"}}),
				scripted.Text("好"),
			)
			var ran bool
			app := New(WithProvider(m), WithSessionManager(session.NewManager(session.NewMemoryStore())),
				WithApprover(NewPermissionHandler()), WithYoloClassifier(false))
			app.Tool("write", ToolDef{Description: "写文件", Input: struct {
				Path string `json:"path"`
			}{}, Permission: Normal,
				Execute: func(_ Context, in struct {
					Path string `json:"path"`
				}) (string, error) {
					ran = true
					return "wrote " + in.Path, nil
				}})
			base := aguiServer(t, app)
			frames := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b7-ed", RunID: "r1", Messages: []agui.Message{userMsg("u1", "写")}}, false)
			end := frames[len(frames)-1]
			if end.Outcome == nil || end.Outcome.Type != "interrupt" {
				t.Fatalf("应停在审批上: %s", end.raw)
			}
			it := end.Outcome.Interrupts[0]
			props, _ := it.ResponseSchema["properties"].(map[string]any)
			if props["editedArgs"] == nil {
				t.Fatalf("审批中断的 responseSchema 应声明 editedArgs: %v", it.ResponseSchema)
			}
			payload := json.RawMessage(`{"approved":true,"editedArgs":` + tc.edited + `}`)
			resumed := postAGUI(t, base, agui.RunAgentInput{ThreadID: "b7-ed", RunID: "r2", Messages: []agui.Message{userMsg("u1", "写")},
				Resume: []agui.ResumeEntry{{InterruptID: it.ID, Status: "resolved", Payload: payload}}}, true)
			res := findFrame(resumed, agui.TypeToolCallResult)
			content, _ := res.Content.(string)
			if !strings.Contains(content, tc.want) || ran != tc.ran || (tc.ran && !strings.Contains(content, "用户在审批时修改了参数")) {
				t.Fatalf("改参结果: %q ran=%v", content, ran)
			}
			m.AssertDone(t)
		})
	}

	app := New(WithProvider(&agentScript{}), WithApprover(NewPermissionHandler()))
	var caps agui.AgentCapabilities
	getJSON(t, aguiServer(t, app)+"/agui/capabilities", &caps)
	if !caps.HumanInTheLoop.ApproveWithEdits || !caps.Tools.ClientProvided {
		t.Fatalf("能力描述应声明改参审批与前端工具: %+v %+v", caps.HumanInTheLoop, caps.Tools)
	}
}

// v1：/approve 带 input 时以修改后的参数执行（不记为始终允许）。
func TestB7_V1ApproveWithInput(t *testing.T) {
	m := scripted.New(
		scripted.Tools(scripted.ToolCall{ID: "w1", Name: "write", Input: map[string]string{"path": "a.txt"}}),
		scripted.Text("好").Expect(func(r *provider.Request) error {
			if got := toolResultsOf(r)["w1"]; !strings.HasPrefix(got, "wrote c.txt") {
				return errors.New("应以修改后的参数执行: " + got)
			}
			return nil
		}),
	)
	app := New(WithProvider(m), WithSessionManager(session.NewManager(session.NewMemoryStore())),
		WithApprover(NewPermissionHandler()), WithYoloClassifier(false))
	app.Tool("write", ToolDef{Description: "写文件", Input: struct {
		Path string `json:"path"`
	}{}, Permission: Normal,
		Execute: func(_ Context, in struct {
			Path string `json:"path"`
		}) (string, error) {
			return "wrote " + in.Path, nil
		}})
	base := aguiServer(t, app)
	st := openSSEStream(t, "POST", base+"/chat", `{"session_id":"b7-v1","message":"写"}`, nil)
	defer st.close()
	frames := st.until(t, func(e protocol.Envelope) bool { return e.Type == protocol.TypeNeedApproval })
	req := frames[len(frames)-1]
	resp := postJSON(t, base+"/approve", fmt.Sprintf(`{"request_id":%q,"session_id":"b7-v1","allow":true,"input":{"path":"c.txt"}}`, req.RequestID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/approve status %d", resp.StatusCode)
	}
	st.rest(t)
	m.AssertDone(t)
	if keys := app.SessionGrants("b7-v1").Keys(); len(keys) != 0 {
		t.Fatalf("改参批准不应记为始终允许: %v", keys)
	}
}
