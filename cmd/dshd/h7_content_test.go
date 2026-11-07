package main

import (
	"encoding/json"
	"testing"
)

// H7：助手内容的两种落法都必须读得到。
//
// 真实语料用的是 data.message.content；但历史上也见过 data.content。
// 只认一种 → 那批会话在界面上"一条助手消息都没有"，
// 而那看起来像会话坏了，不像读错了键。
func TestTextOfReadsBothContentShapes(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		nested bool
		want   string
	}{
		{
			name:   "助手：内容在 message.content（真实语料的形状）",
			raw:    `{"message":{"role":"assistant","content":[{"type":"text","text":"答案在嵌套里"}]}}`,
			nested: true,
			want:   "答案在嵌套里",
		},
		{
			name:   "助手：内容在顶层 content（老形状）",
			raw:    `{"content":[{"type":"text","text":"答案在顶层"}]}`,
			nested: true,
			want:   "答案在顶层",
		},
		{
			name:   "用户：内容在顶层 content",
			raw:    `{"content":[{"type":"text","text":"用户说的话"}]}`,
			nested: false,
			want:   "用户说的话",
		},
		{
			name:   "用户：内容在 message.content（少见但要认）",
			raw:    `{"message":{"content":[{"type":"text","text":"也在嵌套里"}]}}`,
			nested: false,
			want:   "也在嵌套里",
		},
	}
	for _, c := range cases {
		got, ok := textOf(json.RawMessage(c.raw), c.nested)
		if !ok || got != c.want {
			t.Fatalf("%s：得到 %q ok=%v，期望 %q", c.name, got, ok, c.want)
		}
	}
}
