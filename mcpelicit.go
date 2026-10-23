package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Dream355873200/GoAgent/mcp"
	"github.com/Dream355873200/GoAgent/schema"
)

// maxElicitAttempts 是回答不符合表单时重新提问的总次数上限。
const maxElicitAttempts = 3

// ElicitViaAskUser 是 mcp.ElicitHandler：把 MCP 服务器在工具调用中发起的
// elicitation 转成触发它的会话里的一次提问（ask_user 帧 / AskUserRequest），
// WithMCP 连接的服务器默认使用它；自行 mcp.Connect 的宿主可以填进
// ServerConfig.Elicit（工具经 MCPToolDef 注册时才能定位会话）。
//
// 提问的 Payload：
//
//	{"kind":"mcp_elicitation", "server":..., "mode":"form"|"url", "message":...,
//	 "schema":{...}（form）, "url":...、"elicitation_id":...（url）, "error":...（上次回答无效的原因）}
//
// 回答的写法：
//   - 表单内容的 JSON 对象，如 {"name":"Ada","age":36}——接受；
//   - {"action":"decline"} 或 {"action":"cancel"}——拒绝 / 取消；
//     {"action":"accept","content":{...}} 同第一种；
//   - 表单只有一个字段时可以直接回答文本（按字段类型转换）；url 模式回答任意
//     非空文本即表示同意打开链接；
//   - 空回答——取消。
//
// 回答不符合表单时附上原因重新提问（共 3 次）。没有提问处理器、不在会话里、
// 或无法确定是哪个会话的调用时拒绝。
func ElicitViaAskUser(ctx context.Context, e mcp.Elicitation) (mcp.ElicitResponse, error) {
	h := AskUserHandlerFromContext(ctx)
	sessionID := SessionIDFromContext(ctx)
	if h == nil || sessionID == "" {
		return mcp.ElicitResponse{Action: mcp.ElicitDecline}, nil
	}
	question := fmt.Sprintf("MCP 服务器「%s」请求：%s", e.Server, e.Message)
	payload := map[string]any{"kind": "mcp_elicitation", "server": e.Server, "mode": e.Mode, "message": e.Message}
	if e.Mode == "url" {
		question += "\n请打开：" + e.URL
		payload["url"] = e.URL
		payload["elicitation_id"] = e.ID
	} else if e.Schema != nil {
		payload["schema"] = e.Schema
		if fields := elicitFieldNames(e.Schema); fields != "" {
			question += "\n需要填写：" + fields
		}
	}
	q := question
	for range maxElicitAttempts {
		answer, err := h.AskSessionCtx(ctx, sessionID, q, payload)
		if err != nil {
			return mcp.ElicitResponse{Action: mcp.ElicitCancel}, nil
		}
		resp, problem := parseElicitAnswer(e, answer)
		if problem == "" {
			return resp, nil
		}
		q = question + "\n（上次的回答无效：" + problem + "，请重新填写）"
		payload = cloneMap(payload)
		payload["error"] = problem
	}
	return mcp.ElicitResponse{Action: mcp.ElicitCancel}, nil
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// elicitFieldNames 列出表单字段（带 title / description 时附上）。
func elicitFieldNames(s map[string]any) string {
	props, _ := s["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		prop, _ := props[name].(map[string]any)
		for _, k := range []string{"title", "description"} {
			if v, ok := prop[k].(string); ok && v != "" {
				names[i] = name + "（" + v + "）"
				break
			}
		}
	}
	return strings.Join(names, "、")
}

// parseElicitAnswer 把提问的回答转为 elicitation 回应；problem 非空表示回答
// 不符合表单，应附上原因重新提问。
func parseElicitAnswer(e mcp.Elicitation, answer string) (resp mcp.ElicitResponse, problem string) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return mcp.ElicitResponse{Action: mcp.ElicitCancel}, ""
	}
	var content map[string]any
	var obj map[string]any
	isObject := json.Unmarshal([]byte(answer), &obj) == nil && obj != nil
	if isObject {
		content = obj
		if action, ok := obj["action"].(string); ok {
			switch action {
			case mcp.ElicitDecline, mcp.ElicitCancel:
				return mcp.ElicitResponse{Action: action}, ""
			case mcp.ElicitAccept:
				content, _ = obj["content"].(map[string]any)
				if content == nil {
					content = map[string]any{}
				}
			}
		}
	}
	if e.Mode == "url" {
		return mcp.ElicitResponse{Action: mcp.ElicitAccept}, ""
	}
	if !isObject {
		name, prop, ok := singleField(e.Schema)
		if !ok {
			return mcp.ElicitResponse{}, "请按表单字段以 JSON 对象回答"
		}
		v, err := coerceField(prop, answer)
		if err != nil {
			return mcp.ElicitResponse{}, err.Error()
		}
		content = map[string]any{name: v}
	}
	if e.Schema != nil {
		raw, _ := json.Marshal(content)
		if err := schema.Validate(e.Schema, raw); err != nil {
			return mcp.ElicitResponse{}, err.Error()
		}
	}
	return mcp.ElicitResponse{Action: mcp.ElicitAccept, Content: content}, ""
}

// singleField 返回只有一个字段的表单的字段名与定义。
func singleField(s map[string]any) (string, map[string]any, bool) {
	props, _ := s["properties"].(map[string]any)
	if len(props) != 1 {
		return "", nil, false
	}
	for name, p := range props {
		prop, _ := p.(map[string]any)
		return name, prop, true
	}
	return "", nil, false
}

// coerceField 把文本回答按字段类型转换。
func coerceField(prop map[string]any, text string) (any, error) {
	switch prop["type"] {
	case "number":
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("需要一个数字")
		}
		return v, nil
	case "integer":
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("需要一个整数")
		}
		return v, nil
	case "boolean":
		switch strings.ToLower(text) {
		case "true", "yes", "y", "1", "是", "好", "确认", "同意":
			return true, nil
		case "false", "no", "n", "0", "否", "不", "不同意":
			return false, nil
		}
		return nil, fmt.Errorf("需要回答是或否")
	}
	return text, nil
}
