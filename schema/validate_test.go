package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var toolSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path":  map[string]any{"type": "string"},
		"mode":  map[string]any{"type": "string", "enum": []any{"a", "b", "c"}},
		"count": map[string]any{"type": "integer"},
		"ratio": map[string]any{"type": "number"},
		"force": map[string]any{"type": "boolean"},
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"spec": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"members": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":       "object",
						"properties": map[string]any{"role": map[string]any{"type": "string"}},
						"required":   []string{"role"},
					},
				},
			},
			"required": []string{"name"},
		},
	},
	"required": []string{"path"},
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string // 空串表示应通过
	}{
		{"合法", `{"path":"a.txt"}`, ""},
		{"缺必填", `{}`, `缺少必填参数 "path"`},
		{"空输入", ``, `缺少必填参数 "path"`},
		{"null 输入", `null`, `缺少必填参数 "path"`},
		{"空白输入", " \n\t ", `缺少必填参数 "path"`},
		{"enum 不符", `{"path":"x","mode":"zzz"}`, `参数 "mode" 的值 "zzz" 不在允许范围 [a b c] 内`},
		{"enum 符合", `{"path":"x","mode":"b"}`, ""},
		{"整数给字符串", `{"path":"x","count":"5"}`, `参数 "count" 应为整数，实际为字符串 "5"`},
		{"整数给小数", `{"path":"x","count":1.5}`, `参数 "count" 应为整数，实际为数字 1.5`},
		{"整数接受 3.0", `{"path":"x","count":3.0}`, ""},
		{"整数接受指数", `{"path":"x","count":1e3}`, ""},
		{"整数接受大整数", `{"path":"x","count":123456789012345678901234567890}`, ""},
		{"number 接受整数", `{"path":"x","ratio":2}`, ""},
		{"布尔给字符串", `{"path":"x","force":"true"}`, `参数 "force" 应为布尔值，实际为字符串 "true"`},
		{"字符串给数字", `{"path":123}`, `参数 "path" 应为字符串，实际为数字 123`},
		{"数组元素类型", `{"path":"x","tags":["a",2]}`, `参数 "tags[1]" 应为字符串，实际为数字 2`},
		{"数组给字符串", `{"path":"x","tags":"a"}`, `参数 "tags" 应为数组，实际为字符串 "a"`},
		{"嵌套缺必填", `{"path":"x","spec":{}}`, `缺少必填参数 "spec.name"`},
		{"数组内对象缺必填", `{"path":"x","spec":{"name":"n","members":[{"role":"r"},{}]}}`, `缺少必填参数 "spec.members[1].role"`},
		{"嵌套对象类型", `{"path":"x","spec":"oops"}`, `参数 "spec" 应为对象，实际为字符串 "oops"`},
		{"多余字段放行", `{"path":"x","extra":1,"spec":{"name":"n","more":true}}`, ""},
		{"可选字段为 null", `{"path":"x","count":null,"mode":null,"spec":null}`, ""},
		{"必填字段为 null", `{"path":null}`, `参数 "path" 应为字符串，实际为null`},
		{"根不是对象", `[1,2]`, `输入参数应为对象，实际为数组`},
		{"多处错误合并", `{"mode":"zzz","count":"x"}`, "缺少必填参数 \"path\"\n参数 \"count\" 应为整数，实际为字符串 \"x\"\n参数 \"mode\" 的值 \"zzz\" 不在允许范围 [a b c] 内"},
	}
	for _, c := range cases {
		err := Validate(toolSchema, json.RawMessage(c.input))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: 应通过，得到 %v", c.name, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("%s:\n got: %v\nwant: %s", c.name, err, c.want)
		}
	}
}

func TestValidateInvalidJSON(t *testing.T) {
	for _, in := range []string{`{"path":`, `{"path":"x"} trailing`, `{"path":"x"}{}`, `{path:"x"}`} {
		err := Validate(toolSchema, json.RawMessage(in))
		if err == nil || !strings.HasPrefix(err.Error(), "参数不是合法的 JSON") {
			t.Errorf("Validate(%s) = %v，应报告非法 JSON", in, err)
		}
	}
	if err := Validate(nil, json.RawMessage(`{bad`)); err == nil {
		t.Error("schema 为空时也应检查 JSON 合法性")
	}
	if err := Validate(nil, json.RawMessage(`[1,"x"]`)); err != nil {
		t.Errorf("schema 为空时不应约束取值: %v", err)
	}
}

func TestValidateErrorLimit(t *testing.T) {
	err := Validate(toolSchema, json.RawMessage(`{"path":"x","tags":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15]}`))
	if err == nil {
		t.Fatal("应报错")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != maxReportedErrors+1 {
		t.Fatalf("应列出 %d 条再加一行汇总，得到 %d 行:\n%v", maxReportedErrors, len(lines), err)
	}
	if lines[0] != `参数 "tags[0]" 应为字符串，实际为数字 1` || lines[maxReportedErrors] != "…（另有 5 处错误未列出）" {
		t.Fatalf("错误信息异常:\n%v", err)
	}
}

func TestValidateKeywords(t *testing.T) {
	obj := func(props map[string]any, extra ...any) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		for i := 0; i+1 < len(extra); i += 2 {
			s[extra[i].(string)] = extra[i+1]
		}
		return s
	}
	str := map[string]any{"type": "string"}

	cases := []struct {
		name   string
		schema map[string]any
		input  string
		want   string
	}{
		{"additionalProperties=false 拒绝多余字段", obj(map[string]any{"a": str, "b": str}, "additionalProperties", false),
			`{"a":"x","zzz":1}`, `不支持参数 "zzz"，允许的参数为 [a b]`},
		{"additionalProperties 为 schema 时忽略", obj(map[string]any{"a": str}, "additionalProperties", map[string]any{"type": "string"}),
			`{"zzz":1}`, ""},
		{"类型数组接受 null", obj(map[string]any{"v": map[string]any{"type": []any{"string", "null"}}}, "required", []any{"v"}),
			`{"v":null}`, ""},
		{"类型数组拒绝其他类型", obj(map[string]any{"v": map[string]any{"type": []string{"string", "null"}}}),
			`{"v":1}`, `参数 "v" 应为字符串或null，实际为数字 1`},
		{"nullable 允许 null", obj(map[string]any{"v": map[string]any{"type": "string", "nullable": true}}, "required", []string{"v"}),
			`{"v":null}`, ""},
		{"未识别关键字忽略", obj(map[string]any{"v": map[string]any{
			"type": "string", "format": "email", "pattern": "^x$", "maxLength": 1, "minimum": 10,
			"anyOf": []any{map[string]any{"type": "integer"}}, "$ref": "#/defs/x",
		}}), `{"v":"not-an-email"}`, ""},
		{"未识别类型名不约束", obj(map[string]any{"v": map[string]any{"type": "any"}}), `{"v":[1]}`, ""},
		{"按位置的 items", obj(map[string]any{"pair": map[string]any{"type": "array", "items": []any{str, map[string]any{"type": "integer"}}}}),
			`{"pair":["a","b","extra"]}`, `参数 "pair[1]" 应为整数，实际为字符串 "b"`},
		{"required 重复只报一次", map[string]any{"type": "object", "required": []string{"a", "a"}},
			`{}`, `缺少必填参数 "a"`},
		{"根数组", map[string]any{"type": "array", "items": str}, `["a",1]`, `参数 "[1]" 应为字符串，实际为数字 1`},
	}
	for _, c := range cases {
		err := Validate(c.schema, json.RawMessage(c.input))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: 应通过，得到 %v", c.name, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("%s:\n got: %v\nwant: %s", c.name, err, c.want)
		}
	}
}

// MCP 服务器的 inputSchema 来自 JSON 解码：数字为 float64，数组为 []any。
func TestValidateMCPSchema(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"level":{"type":"integer","enum":[1,2,3]},"opts":{"type":"object","properties":{"deep":{"type":"boolean"}},"required":["deep"]}},"required":["level"]}`), &s); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"level":2}`:                   "",
		`{"level":2.0}`:                 "",
		`{"level":4}`:                   `参数 "level" 的值 4 不在允许范围 [1 2 3] 内`,
		`{"level":"2"}`:                 `参数 "level" 应为整数，实际为字符串 "2"`,
		`{"level":1,"opts":{}}`:         `缺少必填参数 "opts.deep"`,
		`{"level":1,"opts":{"deep":0}}`: `参数 "opts.deep" 应为布尔值，实际为数字 0`,
	}
	for in, want := range cases {
		err := Validate(s, json.RawMessage(in))
		if (want == "" && err != nil) || (want != "" && (err == nil || err.Error() != want)) {
			t.Errorf("Validate(%s)\n got: %v\nwant: %q", in, err, want)
		}
	}
}

type genInput struct {
	Path   string          `json:"path"`
	Note   string          `json:"note,omitempty"`
	Forced string          `json:"forced,omitempty" required:"true"`
	Loose  string          `json:"loose" required:"false"`
	Mode   string          `json:"mode,omitempty" enum:"fast,slow"`
	Level  int             `json:"level,omitempty" enum:"1,2,3"`
	When   time.Time       `json:"when,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	Tree   *treeNode       `json:"tree,omitempty"`
}

// Validate 与 Generate 的 required 推断（omitempty / required 标签）及 enum 标签一致，
// 且通过校验的入参能被 encoding/json 解码到原结构体。
func TestValidateAgreesWithGenerate(t *testing.T) {
	s := Generate(genInput{})
	cases := []struct {
		input string
		want  string
	}{
		{`{}`, "缺少必填参数 \"path\"\n缺少必填参数 \"forced\""},
		{`{"path":"p","forced":"f"}`, ""},
		{`{"path":"p","forced":"f","mode":"medium"}`, `参数 "mode" 的值 "medium" 不在允许范围 [fast slow] 内`},
		{`{"path":"p","forced":"f","level":2}`, ""},
		{`{"path":"p","forced":"f","level":5}`, `参数 "level" 的值 5 不在允许范围 [1 2 3] 内`},
		{`{"path":"p","forced":"f","when":"2026-10-22T10:00:00Z","raw":{"any":[1,"x"]},"tree":{"name":"root","children":[{"name":"c","children":[{"name":"gc"}]}]}}`, ""},
		{`{"path":"p","forced":"f","tree":{"children":[{}]}}`, `缺少必填参数 "tree.name"`},
	}
	for _, c := range cases {
		err := Validate(s, json.RawMessage(c.input))
		if (c.want == "" && err != nil) || (c.want != "" && (err == nil || err.Error() != c.want)) {
			t.Errorf("Validate(%s)\n got: %v\nwant: %q", c.input, err, c.want)
			continue
		}
		if c.want == "" {
			var v genInput
			if err := json.Unmarshal([]byte(c.input), &v); err != nil {
				t.Errorf("通过校验的入参无法解码: %s: %v", c.input, err)
			}
		}
	}
}

func TestValidateRobustness(t *testing.T) {
	long := strings.Repeat("长", 1000)
	err := Validate(toolSchema, json.RawMessage(`{"path":"x","mode":"`+long+`"}`))
	if err == nil || len([]rune(err.Error())) > 120 || !strings.Contains(err.Error(), "…") {
		t.Errorf("过长的取值应在错误信息中截断: %v", err)
	}

	unmarshalable := map[string]any{"type": "object", "x": func() {}}
	if err := Validate(unmarshalable, json.RawMessage(`{}`)); err != nil {
		t.Errorf("无法序列化的 schema 不应阻断调用: %v", err)
	}

	recursive := Generate(treeNode{})
	if err := Validate(recursive, json.RawMessage(`{"name":"a","children":[{"name":"b","children":[{"children":[]}]}],"parent":{"x":1}}`)); err != nil {
		t.Errorf("递归类型的 schema 校验失败: %v", err)
	}
}
