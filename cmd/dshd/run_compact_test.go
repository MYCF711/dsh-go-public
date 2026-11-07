package main

import (
	"context"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

type stubChat struct{ reply string }

func (s *stubChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	return &llm.Response{Content: s.reply, FinishReason: "stop"}, nil
}

func buildLongSession(t *testing.T) *session.Log {
	t.Helper()
	l, err := session.CreateSession(t.TempDir(), "/tmp/run-c", "run-c", "default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := l.Append("user/message", session.UserMessage([]spi.Block{
			{Type: "text", Text: "第几轮就第几轮的消息，用来把上下文撑大"},
		})); err != nil {
			t.Fatal(err)
		}
		if err := l.Append("assistant/message", session.AssistantMessageEvent(1, 1,
			[]map[string]any{session.TextBlock("收到")}, "stop", nil)); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// run 的自动压缩必须真的落事件、真的瘦身历史。
func TestRunCompactorCompacts(t *testing.T) {
	l := buildLongSession(t)
	defer l.Close()

	before, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}

	c := runCompactor(&stubChat{reply: "（摘要）"}, l, 1, "m", nil)
	if c == nil {
		t.Fatal("budget=1 时不该返回 nil")
	}
	hist, err := c(context.Background(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events)+3 {
		t.Fatalf("事件数 %d → %d，期望 +3（压缩三条事件）", len(before.Events), len(after.Events))
	}
	if len(session.Surface(after)) >= len(session.Surface(before)) {
		t.Fatal("surface 没变少")
	}
	if len(hist) == 0 {
		t.Fatal("压缩后应该给出新的历史")
	}
}

// 没超预算时必须返回 nil，让 Loop 保留原历史（不能平白瘦身）。
func TestRunCompactorNoopWhenUnderBudget(t *testing.T) {
	l := buildLongSession(t)
	defer l.Close()
	c := runCompactor(&stubChat{reply: "x"}, l, 1_000_000, "m", nil)
	if c == nil {
		t.Fatal("预算很大时也应有函数（只是不压）")
	}
	hist, err := c(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hist != nil {
		t.Fatal("没超预算时应返回 nil")
	}
}

func TestRunCompactorDisabled(t *testing.T) {
	if runCompactor(&stubChat{}, nil, 0, "m", nil) != nil {
		t.Fatal("budget=0 应关闭（返回 nil）")
	}
}
