package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// 压表只许掉 description，结构（type/enum/required/items）一个不许动——
// 少一个 required 模型就会漏参数，少一个 enum 就会传非法值。
func TestCompactSchemaKeepsStructure(t *testing.T) {
	in := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "action":{"type":"string","enum":["a","b"],"description":"很长很长的说明"},
	    "list":{"type":"array","items":{"type":"string","description":"元素说明"},"description":"列表说明"}
	  },
	  "required":["action"]
	}`)
	got := string(CompactSchema(in))
	if strings.Contains(got, "description") {
		t.Fatalf("description 没被去掉: %s", got)
	}
	for _, want := range []string{`"enum":["a","b"]`, `"required":["action"]`, `"type":"array"`, `"items"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("结构被改坏了，缺 %s: %s", want, got)
		}
	}
}

// 不是合法 JSON 就原样返回，不许 panic 也不许把表弄成空。
func TestCompactSchemaBadInputPassthrough(t *testing.T) {
	bad := json.RawMessage(`{not json`)
	if got := string(CompactSchema(bad)); got != string(bad) {
		t.Fatalf("坏输入应当原样返回，得到 %s", got)
	}
}

// 名叫 description 的**参数**不是注解，一个都不许删。
//
// 真机踩过：agent.CompactSchema 见 description 键就删，本仓有 8 个工具真有一个
// 叫 description 的参数（ask_user_question/bash/present/spawn_teammate/subagent/
// subagent_fork/team_task_create/team_task_update）。删完之后 required 里还点着它，
// 模型看到的是「必填、但没有类型」——凭空少一条契约，而且**没有任何一处报错**。
//
// 判据不是「还留着 description 这个词」，而是**压完的 schema 自己还自洽**：
// required 里的每个名字，properties 里都真的有一个带类型的定义。
func TestCompactSchemaKeepsParamNamedDescription(t *testing.T) {
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
	  "required":["command","description"],
	  "additionalProperties":false
	}`)
	got := CompactSchema(in)

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

	// 顶层：参数名 description 必须还在，且还是一个有类型的 schema。
	if s.Properties["description"].Type != "string" {
		t.Fatalf("名叫 description 的参数被删了：模型只看到 required 里一个没类型的名字\n%s", got)
	}
	// 嵌套数组里的同名参数同理。
	if it := s.Properties["options"].Items; it == nil || it.Properties["description"].Type != "string" {
		t.Fatalf("数组元素里名叫 description 的参数被删了\n%s", got)
	}
	// 自洽性：这是这条 bug 唯一要守的契约。
	if len(s.Required) != 2 {
		t.Fatalf("required 被动了：%v", s.Required)
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			t.Fatalf("required 点名了 %q，properties 里却没有\n%s", r, got)
		}
	}
	// 注解（不是参数名）仍然要删掉，否则这条压表就没意义了。
	if strings.Contains(string(got), "给用户看的短描述") || strings.Contains(string(got), "要执行的命令") {
		t.Fatalf("参数的说明文字还在：%s", got)
	}
}

// 压完必须更小：这是它的全部理由。
func TestCompactSchemaShrinks(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"一句话"}}}`)
	if len(CompactSchema(in)) >= len(in) {
		t.Fatal("压完没有变小，这个函数就没有意义")
	}
}
