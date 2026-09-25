package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// streamTurn 以流式调用模型并汇总成一次完整响应。优先取 MessageComplete
// 携带的完整消息；provider 未给出时由增量文本/思考与工具调用拼装。
func streamTurn(ctx context.Context, prov provider.Provider, req *provider.Request) (*provider.Response, error) {
	ch, err := prov.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var (
		text, thinking strings.Builder
		calls          []message.ToolCall
		resp           provider.Response
		complete       bool
	)
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text.WriteString(ev.Text)
		case provider.EventThinkingDelta:
			thinking.WriteString(ev.Thinking)
		case provider.EventToolUseStart:
			if ev.ToolCall != nil {
				calls = append(calls, *ev.ToolCall)
			}
		case provider.EventUsage:
			if ev.Usage != nil {
				resp.Usage = *ev.Usage
			}
		case provider.EventMessageComplete:
			if ev.Message != nil {
				resp.Message = *ev.Message
				complete = true
			}
			resp.StopReason = ev.StopReason
		case provider.EventError:
			if ev.Error != nil {
				err = ev.Error
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !complete {
		if text.Len() == 0 && thinking.Len() == 0 && len(calls) == 0 {
			return nil, errors.New("模型流结束但没有任何输出")
		}
		msg := message.Message{Role: message.RoleAssistant}
		if thinking.Len() > 0 {
			msg.Content = append(msg.Content, message.ContentBlock{Type: "thinking", Thinking: thinking.String()})
		}
		if text.Len() > 0 {
			msg.Content = append(msg.Content, message.ContentBlock{Type: "text", Text: text.String()})
		}
		for _, c := range calls {
			msg.Content = append(msg.Content, message.ContentBlock{
				Type: "tool_use", ToolUseID: c.ID, ToolName: c.Name, Input: c.Input,
			})
		}
		resp.Message = msg
	}
	return &resp, nil
}

// activityKeys 工具入参中最能概括「在做什么」的字段，按优先级。
var activityKeys = []string{"description", "file_path", "path", "pattern", "command", "url", "query", "task", "prompt"}

// ToolActivity 把一次工具调用概括成一行活动描述（如 "Read main.go"）。
func ToolActivity(name string, input json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(input, &args) == nil {
		for _, k := range activityKeys {
			if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
				return name + " " + clip(firstLine(s), 80)
			}
		}
	}
	return name
}

// firstLine 取首个非空行。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return clip(line, 120)
		}
	}
	return ""
}

// clip 按 rune 截断，超长加省略号。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
