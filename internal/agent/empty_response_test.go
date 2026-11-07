package agent

import (
	"strings"
	"testing"

	"dshgo/internal/llm"
)

// "200 + 空流"必须判成失败，而不是"模型说了句空话"。
//
// 真机踩过：网关列着一个模型 id，上游是坏的——有的网关回 502，
// 有的回 **200 但一条内容都没有**。后一种会被记成
// "assistant 空消息 + turn completed + 退出码 0"，用户看到的是"模型没说话"，
// 而真相是"这个模型不能用"。两种事的下一步完全不同。
func TestEmptyResponseIsAnError(t *testing.T) {
	// 什么都没有 → 错，且要指路
	err := emptyResponseErr(&llm.Response{FinishReason: "completed"})
	if err == nil {
		t.Fatal("空响应必须报错")
	}
	if !strings.Contains(err.Error(), "空响应") || !strings.Contains(err.Error(), "dshd models") {
		t.Fatalf("要说清是什么并指路：%v", err)
	}

	// 被 max_tokens 截断 → 另一种话（不是换模型，是调大上限）
	err = emptyResponseErr(&llm.Response{FinishReason: "length"})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("截断要给不同的下一步：%v", err)
	}

	// 三者任一非空都算正常，不许误伤
	for _, r := range []*llm.Response{
		{Content: "hi"},
		{Reasoning: "想"},
		{ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.Func{Name: "now_line"}}}},
	} {
		if err := emptyResponseErr(r); err != nil {
			t.Fatalf("有产出就不该报错：%+v → %v", r, err)
		}
	}
}
