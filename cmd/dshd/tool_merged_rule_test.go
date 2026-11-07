package main

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/agent"
)

// 反漂移：compact 与 tighten 是两套**变换**，但「这个 description 是注解
// 还是参数名」只有一个判断。下面这张表同时喂给两处实现；任何一处退回
// 「见到 description 键就删」，断言就会红——2026-11-04 那天两个同形 bug
// 就是两份实现各写了一遍这个判断、各错一次。
//
// 为什么不是「两份输出必须逐字节相等」：它们本来就不等。tightenSchema 还要
// 删冗余骨架（顶层 type:object / 空 properties / 空 required /
// additionalProperties:true），且用 json.RawMessage 逐字节保留数字与 enum；
// CompactSchema 只删注解。这里只钉两者**必须一致**的那部分：
// 参数名留、注解删、required 自洽、压完是固定点。
//
// 红→绿证据（2026-11-04）：在 62ac2ca（修前）的隔离副本里跑同一条测试，
// agent.CompactSchema 子测试与下面的 sameShape 断言同时失败：
//
//	名叫 description 的参数被删了：模型只看到 required 里一个没类型的名字
//
// 修好后同一条测试全绿；docs/silent-success-audit.md §4 D3 记了这次收敛。
func TestDescriptionRuleIsOneJudge(t *testing.T) {
	src := json.RawMessage(`{
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

	entries := []struct {
		name    string
		compact func(json.RawMessage) json.RawMessage
	}{
		{"agent.CompactSchema", agent.CompactSchema},
		{"tightenSchema", tightenSchema},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			got := e.compact(src)
			if !json.Valid(got) {
				t.Fatalf("压完不是合法 JSON: %s", got)
			}

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
				t.Fatalf("压完解析失败: %v（%s）", err, got)
			}

			// 1) 名叫 description 的参数（顶层与数组元素里）必须还在，且有类型。
			if s.Properties["description"].Type != "string" {
				t.Fatalf("名叫 description 的参数被删了：模型只看到 required 里一个没类型的名字\n%s", got)
			}
			if it := s.Properties["options"].Items; it == nil || it.Properties["description"].Type != "string" {
				t.Fatalf("数组元素里名叫 description 的参数被删了\n%s", got)
			}

			// 2) required 里的每个名字都必须真在 properties 里（压表不许造悬空必填）。
			for _, r := range s.Required {
				if _, ok := s.Properties[r]; !ok {
					t.Fatalf("required 点名了 %q，properties 里却没有\n%s", r, got)
				}
			}

			// 3) 注解（两处）必须删掉，否则压表没有意义。
			for _, text := range []string{"给用户看的短描述", "要执行的命令", "选项说明"} {
				if strings.Contains(string(got), text) {
					t.Fatalf("注解 %q 还在：%s", text, got)
				}
			}

			// 4) 固定点：再压一遍必须一字不差（还有该删的没删就会在第二遍掉）。
			if again := e.compact(got); string(again) != string(got) {
				t.Fatalf("压表不是不动点：\n一遍: %s\n二遍: %s", got, again)
			}
		})
	}

	// sameShape 是同一个判断的第三个出口：它用来判断两个工具对同一个参数的
	// 定义是否同构。少一个名叫 description 的参数，就不是同构。
	with := json.RawMessage(`{"type":"object","properties":{"description":{"type":"string"}},"required":["description"]}`)
	without := json.RawMessage(`{"type":"object","properties":{}}`)
	if sameShape(with, without) {
		t.Fatal("多一个 description 参数的两张 schema 不是同构（sameShape 把参数名当注解删了）")
	}
	// 只差注解文字 → 同构。这是 sameShape 本来的意思，不许误伤。
	a := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"说明一"}}}`)
	b := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"说明二"}}}`)
	if !sameShape(a, b) {
		t.Fatal("只差注解文字应当判同构")
	}
	// 嵌套数组元素里的同名参数同理。
	c := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"description":{"type":"string"}}}}}}`)
	d := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{}}}}}`)
	if sameShape(c, d) {
		t.Fatal("嵌套元素里多一个 description 参数也不是同构")
	}
}
