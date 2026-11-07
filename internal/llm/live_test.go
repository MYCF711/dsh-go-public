package llm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveMagpie 打真机：验证「能流式收到 reasoning」这条 P0 判据的前半。
//
// 默认跳过（CI 上没有网关）。本机跑：
//
//	DSHD_LIVE_LLM=1 go test ./internal/llm -run TestLiveMagpie -v
func TestLiveMagpie(t *testing.T) {
	if os.Getenv("DSHD_LIVE_LLM") != "1" {
		t.Skip("设置 DSHD_LIVE_LLM=1 才跑真机")
	}
	base := os.Getenv("DSHD_LLM_BASE")
	if base == "" {
		base = "http://127.0.0.1:3425/v1"
	}
	model := os.Getenv("DSHD_LLM_MODEL")
	if model == "" {
		model = "group/deepseek-v4.1-flash-1017"
	}

	c := New(base, os.Getenv("DSHD_LLM_KEY"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var reasoningBytes, contentBytes, chunks int
	start := time.Now()
	resp, err := c.Stream(ctx, Request{
		Model:     model,
		Messages:  []Message{{Role: "user", Content: "用一句话说明为什么 1+1=2。"}},
		MaxTokens: 128,
	}, func(e Event) error {
		chunks++
		switch e.Kind {
		case EventReasoning:
			reasoningBytes += len(e.Text)
		case EventContent:
			contentBytes += len(e.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("真机调用失败: %v", err)
	}
	t.Logf("模型=%s 时长=%s 分片=%d reasoning=%d 字节 content=%d 字节 finish=%q",
		model, time.Since(start).Round(time.Millisecond), chunks, reasoningBytes, len(resp.Content), resp.FinishReason)
	t.Logf("usage: %+v", resp.Usage)

	if contentBytes == 0 {
		t.Fatal("没有收到任何 content 增量")
	}
	if reasoningBytes == 0 {
		t.Error("没有收到 reasoning 增量（模型/网关可能没开思维链；不算失败，但要记下来）")
	}
}
