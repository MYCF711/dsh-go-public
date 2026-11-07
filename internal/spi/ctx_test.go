package spi

import (
	"context"
	"testing"
)

// 工具靠 ctx 知道"我是哪个会话"——这条链路断了，信箱、目标这类工具只能拒绝。
func TestSessionRoundTrip(t *testing.T) {
	ctx := WithSession(context.Background(), "acp-123")
	if got := SessionFrom(ctx); got != "acp-123" {
		t.Fatalf("SessionFrom=%q，期望 acp-123", got)
	}
}

func TestSessionAbsentIsEmpty(t *testing.T) {
	if got := SessionFrom(context.Background()); got != "" {
		t.Fatalf("没绑会话时应当返回空串，得到 %q", got)
	}
	// nil ctx 也不许 panic：工具在异常路径下照样会被调用。
	//lint:ignore SA1012 这里就是要在 nil ctx 上验"不 panic"，传 context.TODO 就测不到
	if got := SessionFrom(nil); got != "" {
		t.Fatalf("nil ctx 应当返回空串，得到 %q", got)
	}
}

func TestEmptySessionIgnored(t *testing.T) {
	ctx := WithSession(context.Background(), "")
	if got := SessionFrom(ctx); got != "" {
		t.Fatalf("空 id 不该被绑上，得到 %q", got)
	}
}

// run id 与 session id 各存各的，别互相覆盖。
func TestRunIDSeparate(t *testing.T) {
	ctx := WithRunID(WithSession(context.Background(), "s1"), "r1")
	if SessionFrom(ctx) != "s1" || RunIDFrom(ctx) != "r1" {
		t.Fatalf("session=%q run=%q", SessionFrom(ctx), RunIDFrom(ctx))
	}
}
