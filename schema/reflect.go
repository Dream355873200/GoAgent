// Package schema generates JSON Schema from Go struct types using reflection.
// It reads struct tags (json, desc, enum, required) to produce schemas
// suitable for LLM tool input definitions.
package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

var (
	timeType       = reflect.TypeOf(time.Time{})
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
)

// Generate creates a JSON Schema from a Go struct value.
// The value should be a zero-value instance of the struct (e.g., MyInput{}).
//
// Supported struct tags:
//
//	`json:"name"`           — field name in the schema
//	`json:"name,omitempty"` — marks the field as optional
//	`desc:"description"`    — field description
//	`enum:"a,b,c"`          — allowed values
//	`required:"true"`       — explicitly mark as required (overrides omitempty)
//
// time.Time 生成 {"type":"string","format":"date-time"}；json.RawMessage 与
// interface{}/any 生成不带类型约束的 {}。递归类型（如 Children []Node）
// 在递归引用处生成 {"type":"object"}（数组类型为 {"type":"array"}），不再展开。
func Generate(v any) map[string]any {
	t := reflect.TypeOf(v)
	if t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return map[string]any{"type": "object"}
	}
	g := &generator{expanding: make(map[reflect.Type]bool)}
	return g.structSchema(t)
}

// generator 记录正在展开的命名类型。Go 的递归类型必然经过命名类型，
// 只跟踪命名类型即可保证终止。
type generator struct {
	expanding map[reflect.Type]bool
}

// enter 标记 t 开始展开；t 已在展开栈中（递归引用）时返回 false。
func (g *generator) enter(t reflect.Type) bool {
	if t.Name() == "" {
		return true
	}
	if g.expanding[t] {
		return false
	}
	g.expanding[t] = true
	return true
}

func (g *generator) leave(t reflect.Type) {
	delete(g.expanding, t)
}

func (g *generator) structSchema(t reflect.Type) map[string]any {
	if !g.enter(t) {
		return map[string]any{"type": "object"}
	}
	defer g.leave(t)

	properties := make(map[string]any)
	var required []string

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		// Parse json tag for name and omitempty.
		jsonTag := field.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		name, opts := parseJSONTag(jsonTag)
		if name == "" {
			name = field.Name
		}
		omitempty := strings.Contains(opts, "omitempty")

		// Build property schema.
		prop := g.typeSchema(field.Type)

		// Description from desc tag.
		if desc := field.Tag.Get("desc"); desc != "" {
			prop["description"] = desc
		}

		// Enum from enum tag.
		if enum := field.Tag.Get("enum"); enum != "" {
			parts := strings.Split(enum, ",")
			enumVals := make([]any, len(parts))
			for j, p := range parts {
				enumVals[j] = strings.TrimSpace(p)
			}
			prop["enum"] = enumVals
		}

		properties[name] = prop

		// Required logic: explicit tag wins, otherwise !omitempty means required.
		reqTag := field.Tag.Get("required")
		if reqTag == "true" || (!omitempty && reqTag != "false") {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// typeSchema converts a Go type to a basic JSON Schema type descriptor.
// 每次返回新 map，调用方可以直接追加 description/enum。
func (g *generator) typeSchema(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawMessageType, t.Kind() == reflect.Interface && t.NumMethod() == 0:
		return map[string]any{}
	}

	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if !g.enter(t) {
			return map[string]any{"type": "array"}
		}
		defer g.leave(t)
		return map[string]any{"type": "array", "items": g.typeSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Struct:
		return g.structSchema(t)
	case reflect.Ptr:
		if !g.enter(t) {
			return map[string]any{"type": "object"}
		}
		defer g.leave(t)
		return g.typeSchema(t.Elem())
	default:
		return map[string]any{"type": "string"}
	}
}

// parseJSONTag splits a json tag into name and options.
func parseJSONTag(tag string) (string, string) {
	if idx := strings.Index(tag, ","); idx != -1 {
		return tag[:idx], tag[idx+1:]
	}
	return tag, ""
}
