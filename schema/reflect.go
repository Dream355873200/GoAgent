// Package schema generates JSON Schema from Go struct types using reflection.
// It reads struct tags (json, desc, enum, required) to produce schemas
// suitable for LLM tool input definitions.
package schema

import (
	"reflect"
	"strings"
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
func Generate(v any) map[string]any {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return map[string]any{"type": "object"}
	}

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
		prop := typeToSchema(field.Type)

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

// typeToSchema converts a Go type to a basic JSON Schema type descriptor.
func typeToSchema(t reflect.Type) map[string]any {
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
		items := typeToSchema(t.Elem())
		return map[string]any{"type": "array", "items": items}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Struct:
		return Generate(reflect.New(t).Elem().Interface())
	case reflect.Ptr:
		return typeToSchema(t.Elem())
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
