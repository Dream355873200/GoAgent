package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type treeNode struct {
	Name     string               `json:"name"`
	Children []treeNode           `json:"children,omitempty"`
	Parent   *treeNode            `json:"parent,omitempty"`
	Index    map[string]*treeNode `json:"index,omitempty"`
}

type mutualA struct {
	Name string   `json:"name"`
	B    *mutualB `json:"b,omitempty"`
}

type mutualB struct {
	A []mutualA `json:"a"`
}

type selfSlice []selfSlice

type selfPtr *selfPtr

type selfRefs struct {
	S selfSlice `json:"s"`
	P selfPtr   `json:"p"`
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("schema 无法序列化: %v", err)
	}
	return string(b)
}

func TestGenerateRecursiveTypes(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{treeNode{}, `{"properties":{"children":{"items":{"type":"object"},"type":"array"},"index":{"type":"object"},"name":{"type":"string"},"parent":{"type":"object"}},"required":["name"],"type":"object"}`},
		{&mutualA{}, `{"properties":{"b":{"properties":{"a":{"items":{"type":"object"},"type":"array"}},"required":["a"],"type":"object"},"name":{"type":"string"}},"required":["name"],"type":"object"}`},
		{selfRefs{}, `{"properties":{"p":{"type":"object"},"s":{"items":{"type":"array"},"type":"array"}},"required":["s","p"],"type":"object"}`},
	}
	for _, c := range cases {
		if got := mustJSON(t, Generate(c.v)); got != c.want {
			t.Errorf("Generate(%T)\n got: %s\nwant: %s", c.v, got, c.want)
		}
	}
}

type specialTypes struct {
	When  time.Time       `json:"when"`
	Until *time.Time      `json:"until,omitempty"`
	Times []time.Time     `json:"times,omitempty"`
	Raw   json.RawMessage `json:"raw" desc:"任意 JSON"`
	Any   any             `json:"any,omitempty"`
	Iface interface{}     `json:"iface,omitempty"`
	Dur   time.Duration   `json:"dur,omitempty"`
}

func TestGenerateSpecialTypes(t *testing.T) {
	props := Generate(specialTypes{})["properties"].(map[string]any)
	dateTime := map[string]any{"type": "string", "format": "date-time"}
	want := map[string]any{
		"when":  dateTime,
		"until": dateTime,
		"times": map[string]any{"type": "array", "items": dateTime},
		"raw":   map[string]any{"description": "任意 JSON"},
		"any":   map[string]any{},
		"iface": map[string]any{},
		"dur":   map[string]any{"type": "integer"},
	}
	for name, w := range want {
		if !reflect.DeepEqual(props[name], w) {
			t.Errorf("%s: got %v, want %v", name, props[name], w)
		}
	}
}

type snapInner struct {
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

type snapAll struct {
	Str      string         `json:"str" desc:"字符串"`
	Opt      string         `json:"opt,omitempty"`
	Forced   string         `json:"forced,omitempty" required:"true"`
	Optional int            `json:"optional" required:"false"`
	Mode     string         `json:"mode" enum:"a, b,c"`
	Int      int            `json:"int"`
	I8       int8           `json:"i8"`
	U64      uint64         `json:"u64"`
	F32      float32        `json:"f32"`
	F64      float64        `json:"f64"`
	Bool     bool           `json:"bool"`
	Strs     []string       `json:"strs"`
	Arr      [2]int         `json:"arr"`
	Bytes    []byte         `json:"bytes"`
	Map      map[string]int `json:"map"`
	Nested   snapInner      `json:"nested"`
	PNested  *snapInner     `json:"pnested,omitempty"`
	Items    []snapInner    `json:"items"`
	PBool    *bool          `json:"pbool,omitempty"`
	Dur      time.Duration  `json:"dur"`
	NoTag    string
	Skip     string `json:"-"`
	Dash     string `json:"-,"`
	//lint:ignore U1000 用于验证 schema 反射跳过非导出字段
	unexp   string
	Anon    struct{ X int }   `json:"anon"`
	MapAny  map[string]any    `json:"map_any"`
	Ch      chan int          `json:"ch"`
	Uptr    uintptr           `json:"uptr"`
	Iface2  fmt.Stringer      `json:"iface2"`
	Nested2 map[string][]bool `json:"nested2"`
}

// 期望值由修改前的 Generate 生成：除递归类型和特殊类型外，输出必须保持不变。
func TestGenerateUnchangedSnapshot(t *testing.T) {
	const want = `{"properties":{"-":{"type":"string"},"NoTag":{"type":"string"},"anon":{"properties":{"X":{"type":"integer"}},"required":["X"],"type":"object"},"arr":{"items":{"type":"integer"},"type":"array"},"bool":{"type":"boolean"},"bytes":{"items":{"type":"integer"},"type":"array"},"ch":{"type":"string"},"dur":{"type":"integer"},"f32":{"type":"number"},"f64":{"type":"number"},"forced":{"type":"string"},"i8":{"type":"integer"},"iface2":{"type":"string"},"int":{"type":"integer"},"items":{"items":{"properties":{"note":{"type":"string"},"value":{"type":"string"}},"required":["value"],"type":"object"},"type":"array"},"map":{"type":"object"},"map_any":{"type":"object"},"mode":{"enum":["a","b","c"],"type":"string"},"nested":{"properties":{"note":{"type":"string"},"value":{"type":"string"}},"required":["value"],"type":"object"},"nested2":{"type":"object"},"opt":{"type":"string"},"optional":{"type":"integer"},"pbool":{"type":"boolean"},"pnested":{"properties":{"note":{"type":"string"},"value":{"type":"string"}},"required":["value"],"type":"object"},"str":{"description":"字符串","type":"string"},"strs":{"items":{"type":"string"},"type":"array"},"u64":{"type":"integer"},"uptr":{"type":"string"}},"required":["str","forced","mode","int","i8","u64","f32","f64","bool","strs","arr","bytes","map","nested","items","dur","NoTag","-","anon","map_any","ch","uptr","iface2","nested2"],"type":"object"}`
	for _, v := range []any{snapAll{}, &snapAll{}} {
		if got := mustJSON(t, Generate(v)); got != want {
			t.Errorf("Generate(%T) 输出变化\n got: %s\nwant: %s", v, got, want)
		}
	}

	others := map[string]any{
		`{"properties":{},"type":"object"}`: struct{}{},
		`{"type":"object"}`:                 42,
	}
	for want, v := range others {
		if got := mustJSON(t, Generate(v)); got != want {
			t.Errorf("Generate(%T) = %s, want %s", v, got, want)
		}
	}
	if got := mustJSON(t, Generate(nil)); got != `{"type":"object"}` {
		t.Errorf("Generate(nil) = %s", got)
	}
}
