package team

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/internal/spi"
)

// 发信这条路的错误必须能被定位：先钉住"正常情况能发出去"。
func TestSendWritesMailbox(t *testing.T) {
	dir := t.TempDir()
	tool := &Tool{box: &box{dir: dir}, kind: kindSend}
	ctx := spi.WithSession(context.Background(), "acp-sender")
	res, err := tool.Exec(ctx, json.RawMessage(`{"agent_id":"acp-target","message":"hello"}`))
	if err != nil {
		t.Fatalf("发信失败: %v / hint=%q", err, res.Hint)
	}
	if res.Summary == "" {
		t.Fatal("Summary 不能为空")
	}
}

// 没有会话上下文时必须明确拒绝，而不是写到一个空 id 的信箱里。
func TestSendWithoutSessionRejected(t *testing.T) {
	tool := &Tool{box: &box{dir: t.TempDir()}, kind: kindSend}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"agent_id":"a","message":"x"}`)); err == nil {
		t.Fatal("没有会话 id 时必须拒绝")
	}
}
