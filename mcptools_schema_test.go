package goagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// GLM 等兼容端点不解 $ref 引用式 schema（带同级键直接 400 invalid_argument）：
// sanitizeMCPSchema 必须解内联引用、保留兄弟键、删 $schema。
func TestSanitizeMCPSchemaDerefsLocalRefs(t *testing.T) {
	raw := []byte(`{
	 "type": "object",
	 "properties": {
	  "objectName": {"type": "string", "description": "Object name."},
	  "location": {"type": "array", "minItems": 3, "maxItems": 3, "description": "New location."},
	  "rotation": {"$ref": "#/properties/location", "description": "New Euler rotation (radians)."},
	  "scale": {"$ref": "#/properties/location", "description": "New scale."}
	 },
	 "required": ["objectName"],
	 "additionalProperties": false,
	 "$schema": "http://json-schema.org/draft-07/schema#"
	}`)
	m := sanitizeMCPSchema(json.RawMessage(raw))
	if m == nil {
		t.Fatal("应能归一化 raw JSON schema")
	}
	if _, has := m["$schema"]; has {
		t.Error("$schema 应被移除")
	}
	rot, _ := m["properties"].(map[string]any)["rotation"].(map[string]any)
	if rot == nil {
		t.Fatal("rotation 应存在")
	}
	if _, has := rot["$ref"]; has {
		t.Error("rotation 的 $ref 应已展开")
	}
	if rot["type"] != "array" {
		t.Errorf("rotation 应继承 location 的 type=array，实际 %v", rot["type"])
	}
	if rot["description"] != "New Euler rotation (radians)." {
		t.Errorf("兄弟键 description 应保留，实际 %v", rot["description"])
	}
	if _, has := rot["minItems"]; !has {
		t.Error("目标子树的约束（minItems）应随展开带入")
	}
	out, _ := json.Marshal(m)
	if strings.Contains(string(out), "$ref") || strings.Contains(string(out), "$schema") {
		t.Error("全 schema 不应再含 $ref/$schema")
	}
}

func TestSanitizeMCPSchemaUnresolvableRefDegrades(t *testing.T) {
	m := sanitizeMCPSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"$ref": "#/definitions/NotThere", "description": "d"},
		},
	})
	x := m["properties"].(map[string]any)["x"].(map[string]any)
	if _, has := x["$ref"]; has {
		t.Error("解不开的引用应删除 $ref")
	}
	if x["description"] != "d" {
		t.Errorf("兄弟键应保留，实际 %v", x["description"])
	}
}

func TestSanitizeMCPSchemaPassThrough(t *testing.T) {
	if v := sanitizeMCPSchema(nil); v != nil {
		t.Error("nil 应返回 nil")
	}
	in := map[string]any{"type": "object"}
	if v := sanitizeMCPSchema(in); v == nil || v["type"] != "object" {
		t.Error("普通 map 应原样透传")
	}
}

func TestSanitizeMCPSchemaCollapsesTupleItems(t *testing.T) {
	m := sanitizeMCPSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{
				"type":     "array",
				"items":    []any{map[string]any{"type": "number"}, map[string]any{"type": "number"}, map[string]any{"type": "number"}},
				"minItems": 3, "maxItems": 3, "description": "New location.",
			},
		},
	})
	loc := m["properties"].(map[string]any)["location"].(map[string]any)
	items, ok := loc["items"].(map[string]any)
	if !ok {
		t.Fatalf("元组 items 应折叠为单 schema，实际 %v", loc["items"])
	}
	if items["type"] != "number" {
		t.Errorf("折叠后应取唯一元素脸，实际 %v", items)
	}
	if fmt.Sprint(loc["minItems"]) != "3" || fmt.Sprint(loc["maxItems"]) != "3" {
		t.Errorf("定长约束 minItems/maxItems 应保留，实际 %v/%v", loc["minItems"], loc["maxItems"])
	}
	if loc["description"] != "New location." {
		t.Error("description 应保留")
	}
}
