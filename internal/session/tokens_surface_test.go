package session

import (
	"testing"

	"dshgo/internal/llm"
)

func TestHistoryToSurfaceKeepsContent(t *testing.T) {
	hist := []llm.Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "在", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
	}
	s := HistoryToSurface(hist)
	if len(s) != 2 {
		t.Fatalf("长度 = %d", len(s))
	}
	if s[0].Message.Content != "你好" || s[1].Message.Role != "assistant" {
		t.Fatalf("内容没带过来: %+v", s)
	}
	if s[0].Seq != -1 {
		t.Fatalf("历史派生出来的节点 seq 应为 -1（不是从日志来的），得到 %d", s[0].Seq)
	}
	var m Meter
	if est := m.Estimate(s); est <= 0 {
		t.Fatalf("估算应该大于 0，得到 %d", est)
	}
}
