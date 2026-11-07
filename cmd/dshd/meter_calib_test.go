package main

import (
	"context"
	"strings"
	"testing"

	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// countingChat 数一数压缩器到底有没有去要摘要。
type countingChat struct {
	stubChat
	called int
}

func (c *countingChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	c.called++
	return &llm.Response{Content: "摘要正文", FinishReason: "stop"}, nil
}

// run 的压缩判断必须用**带实测锚点**的估算器。
//
// 以前每步新建一个空 meter：永远按启发式比例猜。校准过的 meter 认为同一份
// 历史大得多，压缩就应当被触发——这条钉的是"compactor 真的用了传进来的 meter"。
func TestRunCompactorHonoursCalibratedMeter(t *testing.T) {
	l := buildLongSession(t)
	defer l.Close()

	chat := &countingChat{}

	// 有锚点：实测比估算大 50 倍 → 同一份历史估算爆表 → 必须压。
	cal := &session.Meter{}
	cal.Observe(50000, 1000)
	c := runCompactor(chat, l, 500, "m", cal)
	if c == nil {
		t.Fatal("budget=500 时不该返回 nil")
	}
	if _, err := c(context.Background(), 2, nil); err != nil {
		t.Fatal(err)
	}
	if chat.called == 0 {
		t.Fatal("校准过的 meter 说这份历史爆表，压缩却没被触发——compactor 没在用传进来的 meter")
	}
	_ = spi.Block{}
	_ = strings.TrimSpace
}
