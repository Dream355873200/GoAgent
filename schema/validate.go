package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	// maxReportedErrors 是 Validate 合并错误时最多列出的条数。
	maxReportedErrors = 10
	// maxBriefRunes 是错误信息中回显取值的最大字符数。
	maxBriefRunes = 60
)

// Validate 按 JSON Schema 子集校验工具入参 input，失败时返回面向模型的中文
// 错误（含字段路径）；多处错误合并为一条，每行一处，最多列出前 10 处。
//
// 支持的关键字：
//   - type：object/string/number/integer/boolean/array/null 或其数组；
//     integer 接受小数部分为 0 的数字（如 3.0）
//   - properties、required、enum、items（单个 schema 或按位置的 schema 数组）
//   - additionalProperties：仅显式为 false 时才检查多余字段
//   - nullable（OpenAPI 风格）：为 true 时额外允许 null
//
// 其余关键字（$ref、anyOf、format、minimum、pattern 等）一律忽略。为与
// Generate 及 encoding/json 的解码行为一致，另有以下宽松规则：
//   - input 为空或 "null" 时按空对象处理
//   - 非必填字段的值为 null 时视同未提供（encoding/json 对 null 不赋值）
//   - 字符串 enum 成员与数字/布尔值按字面比较（enum 标签总是生成字符串）
//
// s 为空时只检查 input 是否为合法 JSON。
func Validate(s map[string]any, input json.RawMessage) error {
	value, err := decodeInput(input)
	if err != nil {
		return fmt.Errorf("参数不是合法的 JSON：%v", err)
	}
	if len(s) == 0 {
		return nil
	}
	root, err := normalizeSchema(s)
	if err != nil {
		// 无法序列化的 schema 也发不到模型侧，不因此阻断工具调用。
		return nil
	}
	var v validator
	v.check(root, value, "")
	return v.result()
}

// AsMap 把任意形式的 schema（map、结构体、json.RawMessage 等）转成 Validate
// 接受的 map 形式；nil 或无法转换时返回 nil，表示不做校验。
func AsMap(s any) map[string]any {
	switch v := s.(type) {
	case nil:
		return nil
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(v, &m) != nil {
			return nil
		}
		return m
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// decodeInput 解析 input；数字保留为 json.Number，以便精确判断整数。
func decodeInput(input json.RawMessage) (any, error) {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || string(input) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("JSON 值之后还有多余内容")
	}
	return v, nil
}

// normalizeSchema 经 JSON 往返把 s 规整为 map[string]any、[]any、json.Number
// 等通用形态，兼容手写 schema 中 []string、map[string]map[string]any 等具体类型。
func normalizeSchema(s map[string]any) (map[string]any, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

type validator struct {
	errs  []string
	total int
}

func (v *validator) addf(format string, args ...any) {
	v.total++
	if len(v.errs) < maxReportedErrors {
		v.errs = append(v.errs, fmt.Sprintf(format, args...))
	}
}

func (v *validator) result() error {
	if v.total == 0 {
		return nil
	}
	msg := strings.Join(v.errs, "\n")
	if more := v.total - len(v.errs); more > 0 {
		msg += fmt.Sprintf("\n…（另有 %d 处错误未列出）", more)
	}
	return errors.New(msg)
}

// check 校验 value 是否满足 schema s；path 为字段路径，根为空串。
// 类型或 enum 不符时不再深入检查该值。
func (v *validator) check(s map[string]any, value any, path string) {
	if types := schemaTypes(s); len(types) > 0 && !matchesType(types, value) &&
		!(value == nil && s["nullable"] == true) {
		v.addf("%s应为%s，实际为%s", subject(path), typeNames(types), describe(value))
		return
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 && !inEnum(enum, value) {
		v.addf("%s的值 %s 不在允许范围 %s 内", subject(path), brief(value), enumList(enum))
		return
	}
	switch val := value.(type) {
	case map[string]any:
		v.checkObject(s, val, path)
	case []any:
		v.checkArray(s, val, path)
	}
}

func (v *validator) checkObject(s map[string]any, obj map[string]any, path string) {
	required := make(map[string]bool)
	if list, ok := s["required"].([]any); ok {
		for _, r := range list {
			name, ok := r.(string)
			if !ok || required[name] {
				continue
			}
			required[name] = true
			if _, present := obj[name]; !present {
				v.addf("缺少必填参数 %q", joinPath(path, name))
			}
		}
	}

	props, _ := s["properties"].(map[string]any)
	strict := s["additionalProperties"] == false
	for _, name := range sortedKeys(obj) {
		sub, declared := props[name]
		if !declared {
			if strict {
				v.addf("不支持参数 %q，允许的参数为 %s", joinPath(path, name), keyList(props))
			}
			continue
		}
		val := obj[name]
		if val == nil && !required[name] {
			continue
		}
		if subSchema, ok := sub.(map[string]any); ok {
			v.check(subSchema, val, joinPath(path, name))
		}
	}
}

func (v *validator) checkArray(s map[string]any, arr []any, path string) {
	switch items := s["items"].(type) {
	case map[string]any:
		for i, el := range arr {
			v.check(items, el, fmt.Sprintf("%s[%d]", path, i))
		}
	case []any:
		for i := 0; i < len(arr) && i < len(items); i++ {
			if sub, ok := items[i].(map[string]any); ok {
				v.check(sub, arr[i], fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
}

// typeLabels 是可识别的类型名及其在错误信息中的中文名。
var typeLabels = map[string]string{
	"object":  "对象",
	"array":   "数组",
	"string":  "字符串",
	"number":  "数字",
	"integer": "整数",
	"boolean": "布尔值",
	"null":    "null",
}

// schemaTypes 返回 s 中可识别的 type 取值；未声明或都不可识别时为空，即不约束类型。
func schemaTypes(s map[string]any) []string {
	var raw []any
	switch t := s["type"].(type) {
	case string:
		raw = []any{t}
	case []any:
		raw = t
	}
	var types []string
	for _, x := range raw {
		if name, ok := x.(string); ok && typeLabels[name] != "" {
			types = append(types, name)
		}
	}
	return types
}

func matchesType(types []string, value any) bool {
	for _, t := range types {
		if isType(t, value) {
			return true
		}
	}
	return false
}

func isType(t string, value any) bool {
	switch t {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		return ok && isInteger(n)
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return false
}

// isInteger 判断数字是否为整数值：不含小数点和指数的字面量直接成立（兼容超出
// float64 精度的大整数），否则要求小数部分为 0。
func isInteger(n json.Number) bool {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		return true
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == math.Trunc(f)
}

func inEnum(enum []any, value any) bool {
	for _, member := range enum {
		if jsonEqual(member, value) || literalEqual(member, value) {
			return true
		}
	}
	return false
}

// literalEqual 让字符串 enum 成员匹配字面相同的数字/布尔值：Generate 对任何
// 字段类型都把 enum 标签生成为字符串（如 int 字段的 enum:"1,2,3"）。
func literalEqual(member, value any) bool {
	s, ok := member.(string)
	if !ok {
		return false
	}
	switch x := value.(type) {
	case json.Number:
		return numberEqual(json.Number(s), x)
	case bool:
		return s == strconv.FormatBool(x)
	}
	return false
}

// jsonEqual 按 JSON 语义比较两个规整后的值，数字按数值比较（1 等于 1.0）。
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numberEqual(x, y)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

func numberEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	fa, errA := strconv.ParseFloat(string(a), 64)
	fb, errB := strconv.ParseFloat(string(b), 64)
	return errA == nil && errB == nil && fa == fb
}

// subject 返回错误信息的主语。
func subject(path string) string {
	if path == "" {
		return "输入参数"
	}
	return fmt.Sprintf("参数 %q ", path)
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func typeNames(types []string) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = typeLabels[t]
	}
	return strings.Join(names, "或")
}

// describe 描述实际值的类型，标量附带简短取值。
func describe(value any) string {
	switch x := value.(type) {
	case nil:
		return "null"
	case bool:
		return "布尔值 " + strconv.FormatBool(x)
	case json.Number:
		return "数字 " + brief(x)
	case string:
		return "字符串 " + brief(x)
	case []any:
		return "数组"
	case map[string]any:
		return "对象"
	}
	return fmt.Sprintf("%T", value)
}

// brief 返回值的 JSON 文本，过长时截断，避免把大段内容回显给模型。
func brief(value any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	n := 0
	for i := range s {
		if n == maxBriefRunes {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// enumList 把 enum 成员格式化为 [a b c]：字符串原样输出，其余输出 JSON 文本。
func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, member := range enum {
		if s, ok := member.(string); ok {
			parts[i] = s
		} else {
			parts[i] = brief(member)
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func keyList(props map[string]any) string {
	return "[" + strings.Join(sortedKeys(props), " ") + "]"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
