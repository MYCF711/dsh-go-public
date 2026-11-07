package main

import (
	"encoding/json"
	"testing"
)

// 两张 schema 只差一个**名叫 description 的参数**：不是同构。
//
// 这是 agent.CompactSchema 那条 bug 的姊妹版：`sameShape` 用 `stripDescriptions`
// 先把说明文字删掉再比，而它原来也是"见到 description 键就删"——于是
// 「多一个 description 参数」和「没有这个参数」会被判成同构。
// 合并层据此把两个并不一样的工具并成一族，模型拿到的参数契约就少了一条，
// 而且合并"成功"、没有任何报错。
func TestSameShapeKeepsDescriptionParam(t *testing.T) {
	with := json.RawMessage(`{"type":"object","properties":{"description":{"type":"string"}}}`)
	without := json.RawMessage(`{"type":"object","properties":{}}`)
	if sameShape(with, without) {
		t.Fatal("多一个 description 参数的两张 schema 不是同构")
	}

	// 只有说明文字不同 → 同构。这是这个函数本来的意思，不许误伤。
	a := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"说明一"}}}`)
	b := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"说明二"}}}`)
	if !sameShape(a, b) {
		t.Fatal("只差说明文字应当判同构")
	}

	// 嵌套数组元素里的同名参数同理。
	c := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"description":{"type":"string"}}}}}}`)
	d := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{}}}}}`)
	if sameShape(c, d) {
		t.Fatal("嵌套元素里多一个 description 参数也不是同构")
	}
}
