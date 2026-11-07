package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/schema"
)

// 「注解还是参数名」只有一个判断函数，且答案取决于位置。
//
// 2026-11-04 的同形 bug 就出在把位置丢了：同一个键名，在 schema 对象上是
// 注解（删），在 properties 映射里是参数名（留）。
func TestIsAnnotationKeyIsPositionAware(t *testing.T) {
	cases := []struct {
		key           string
		inPropertyMap bool
		want          bool
	}{
		{"description", false, true},   // schema 对象上的说明 → 注解
		{"description", true, false},   // properties 里的参数名 → 保留
		{"type", false, false},         // 结构关键字
		{"required", false, false},     // 结构关键字
		{"description ", false, false}, // 不做 trim：键名精确匹配
	}
	for _, c := range cases {
		if got := schema.IsAnnotationKey(c.key, c.inPropertyMap); got != c.want {
			t.Errorf("IsAnnotationKey(%q, %v) = %v，期望 %v", c.key, c.inPropertyMap, got, c.want)
		}
	}
}

// 「名字 → schema」的词汇表只有一份。少写一个（$defs/definitions）就会把
// 里面的参数名当注解删掉；多写一个就会把 items 的名字当关键字。
func TestIsPropertyMapKeyVocabulary(t *testing.T) {
	for _, k := range []string{"properties", "patternProperties", "$defs", "definitions"} {
		if !schema.IsPropertyMapKey(k) {
			t.Errorf("%q 应当是名字映射", k)
		}
	}
	for _, k := range []string{"items", "anyOf", "allOf", "oneOf", "additionalProperties", "required", "type", "description"} {
		if schema.IsPropertyMapKey(k) {
			t.Errorf("%q 不是名字映射：它的值是 schema 本身或字面量", k)
		}
	}
}

// 名叫 description 的参数不是注解，一个都不许删；注解必须删。
//
// 这条测试同时钉住两件事，因为这两件事是同一条规则的两面：只留参数名
// 不删注解，压表就没意义；只删注解不留参数名，就是 2026-11-04 那条 bug。
func TestCompactKeepsParamNamedDescription(t *testing.T) {
	in := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "description":{"type":"string","description":"给用户看的短描述"},
	    "command":{"type":"string","description":"要执行的命令"},
	    "options":{"type":"array","items":{
	      "type":"object",
	      "properties":{"description":{"type":"string","description":"选项说明"}},
	      "required":["description"]
	    }}
	  },
	  "required":["command","description"]
	}`)
	got := schema.Compact(in)

	var s struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items *struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("压完不是合法 JSON: %v（%s）", err, got)
	}
	if s.Properties["description"].Type != "string" {
		t.Fatalf("名叫 description 的参数被删了：模型只看到 required 里一个没类型的名字\n%s", got)
	}
	if it := s.Properties["options"].Items; it == nil || it.Properties["description"].Type != "string" {
		t.Fatalf("数组元素里名叫 description 的参数被删了\n%s", got)
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			t.Fatalf("required 点名了 %q，properties 里却没有——压表造出了悬空的必填项\n%s", r, got)
		}
	}
	for _, text := range []string{"给用户看的短描述", "要执行的命令", "选项说明"} {
		if strings.Contains(string(got), text) {
			t.Fatalf("注解 %q 还在：%s", text, got)
		}
	}
	// 固定点：再压一遍必须一字不差，否则说明还有该删的没删干净。
	if again := schema.Compact(got); string(again) != string(got) {
		t.Fatalf("压表不是不动点：\n一遍: %s\n二遍: %s", got, again)
	}
}

// `default` 是**数据**，不是 schema：它里面的键是默认值对象的字段。
//
// 2026-11-04 的 D3 行为表把这条记成「既有的过度删除，这次不改」。它和 D3 那条
// 是同一个形状：删不删由**键名**决定，而不是由**位置**决定。区别只是这次删掉的
// 是默认值里的一个字段——默认值本身就是"这个参数长什么样"的值，删一个键等于
// 把它改成了另一个默认值，而且两端都不报错。
//
// 红：压缩后 `default.description` 消失（模型/工具拿到的是一个少一个字段的默认值）。
// 绿：`default` 整棵子树原样保留；同一层上真正的注解照删。
func TestCompactKeepsDescriptionInsideDefault(t *testing.T) {
	in := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "config":{
	      "type":"object",
	      "description":"这一层的说明是注解，要删",
	      "default":{
	        "description":"默认值的字段，是数据",
	        "type":"ga",
	        "items":[{"description":"数组元素里的字段，也是数据"}],
	        "nested":{"properties":{"description":{"type":"string"}}}
	      }
	    }
	  }
	}`)
	got := schema.Compact(in)

	var s struct {
		Properties map[string]struct {
			Description string          `json:"description"`
			Default     json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("压完不是合法 JSON: %v（%s）", err, got)
	}
	if strings.Contains(string(got), "这一层的说明是注解") {
		t.Fatalf("schema 层上的注解没删：%s", got)
	}
	def := s.Properties["config"].Default
	if len(def) == 0 {
		t.Fatalf("default 整个没了：%s", got)
	}

	var d map[string]any
	if err := json.Unmarshal(def, &d); err != nil {
		t.Fatalf("default 不是合法 JSON: %v（%s）", err, def)
	}
	if d["description"] != "默认值的字段，是数据" {
		t.Fatalf("default 里的 description 字段被删了（那是数据，不是注解）：%s", def)
	}
	if d["type"] != "ga" {
		t.Fatalf("default 的其它字段被动了：%s", def)
	}
	items, _ := d["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("default.items 形状被改了：%s", def)
	}
	if first, _ := items[0].(map[string]any); first["description"] != "数组元素里的字段，也是数据" {
		t.Fatalf("default 的数组元素里的字段被删了：%s", def)
	}
	nested, _ := d["nested"].(map[string]any)
	props, _ := nested["properties"].(map[string]any)
	if p, _ := props["description"].(map[string]any); p["type"] != "string" {
		t.Fatalf("default 里嵌套对象的名字被当注解删了：%s", def)
	}

	// 固定点：数据不动，再压一遍就不能再变。
	if again := schema.Compact(got); string(again) != string(got) {
		t.Fatalf("带 default 的压表不是不动点：\n一遍: %s\n二遍: %s", got, again)
	}
}

// 同一类数据位置不止 `default`：`const` / `enum` / `examples` 的值同样是实例数据，
// 里面叫 description 的键是值的一部分。删它和删 default 里的同名字段是同一个错误，
// 只是更难遇到——所以更容易在下次重构时被"统一按键名删"顺手改回去。
func TestCompactKeepsDescriptionInsideDataKeys(t *testing.T) {
	in := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "mode":{
	      "type":"string",
	      "const":{"description":"常量里的字段","value":"a"},
	      "enum":[{"description":"选项一的字段"},{"description":"选项二的字段"}],
	      "examples":[{"description":"示例里的字段"}]
	    }
	  }
	}`)
	got := schema.Compact(in)

	var s struct {
		Properties map[string]struct {
			Const    json.RawMessage   `json:"const"`
			Enum     []json.RawMessage `json:"enum"`
			Examples []json.RawMessage `json:"examples"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("压完不是合法 JSON: %v（%s）", err, got)
	}
	mode := s.Properties["mode"]
	var c map[string]any
	if err := json.Unmarshal(mode.Const, &c); err != nil || c["description"] != "常量里的字段" {
		t.Fatalf("const 里的 description 字段被删了：%s（err=%v）", mode.Const, err)
	}
	if len(mode.Enum) != 2 {
		t.Fatalf("enum 形状被改了：%s", mode.Enum)
	}
	for i, want := range []string{"选项一的字段", "选项二的字段"} {
		var e map[string]any
		if err := json.Unmarshal(mode.Enum[i], &e); err != nil || e["description"] != want {
			t.Fatalf("enum[%d] 里的 description 字段被删了：%s（err=%v）", i, mode.Enum[i], err)
		}
	}
	var ex map[string]any
	if err := json.Unmarshal(mode.Examples[0], &ex); err != nil || ex["description"] != "示例里的字段" {
		t.Fatalf("examples 里的 description 字段被删了：%s（err=%v）", mode.Examples[0], err)
	}
}

// 反方向：schema 位置上的注解一个都不能留。
//
// `items` / `anyOf` / `$defs` 的值是 schema（不是数据、也不是名字映射），
// 里面的 description 是注解，必须删；`$defs` 这一层本身是「名字 → schema」，
// 所以 `$defs` 下面一个**名叫 description 的定义**要保留。
func TestCompactStripsAnnotationsInSchemaPositions(t *testing.T) {
	in := json.RawMessage(`{
	  "$defs":{
	    "description":{"type":"object","description":"定义上的注解",
	      "properties":{"items":{"type":"array","description":"参数上的注解",
	        "items":{"type":"string","description":"元素上的注解"}}}}
	  },
	  "type":"object",
	  "properties":{"x":{"anyOf":[
	    {"type":"string","description":"anyOf 分支上的注解"},
	    {"type":"number","description":"另一个分支上的注解"}
	  ]}}
	}`)
	got := schema.Compact(in)
	for _, text := range []string{"定义上的注解", "参数上的注解", "元素上的注解", "anyOf 分支上的注解", "另一个分支上的注解"} {
		if strings.Contains(string(got), text) {
			t.Fatalf("schema 位置上的注解 %q 还在：%s", text, got)
		}
	}
	var s struct {
		Defs map[string]struct {
			Type string `json:"type"`
		} `json:"$defs"`
		Properties map[string]struct {
			AnyOf []struct {
				Type string `json:"type"`
			} `json:"anyOf"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("压完不是合法 JSON: %v（%s）", err, got)
	}
	// $defs 这一层是名字映射：名叫 description 的定义必须还在。
	if s.Defs["description"].Type != "object" {
		t.Fatalf("$defs 里名叫 description 的定义被当成注解删了：%s", got)
	}
	if len(s.Properties["x"].AnyOf) != 2 {
		t.Fatalf("anyOf 的形状被改了：%s", got)
	}
}

// 不是合法 JSON 就原样返回：投影宁可少压一点，也不能把看不懂的东西弄坏。
func TestCompactBadInputPassthrough(t *testing.T) {
	bad := json.RawMessage(`{not json`)
	if got := string(schema.Compact(bad)); got != string(bad) {
		t.Fatalf("坏输入应当原样返回，得到 %s", got)
	}
}

// 压完必须更小：这是这个函数存在的全部理由。
func TestCompactShrinks(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"一句话"}}}`)
	if len(schema.Compact(in)) >= len(in) {
		t.Fatal("压完没有变小，这个函数就没有意义")
	}
}
