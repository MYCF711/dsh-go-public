package agent

import (
	"context"
	"strings"
	"testing"

	"dshgo/internal/llm"
)

type fakeChat struct {
	calls int
	reply string
}

func (f *fakeChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	f.calls++
	return &llm.Response{Content: f.reply, FinishReason: "stop"}, nil
}

// Compact 钩子必须在每一步之前被调用，并且**替换掉**历史。
func TestLoopCallsCompactEachStep(t *testing.T) {
	chat := &fakeChat{reply: "好"}
	steps := []int{}
	l := &Loop{
		Chat:     chat,
		Model:    "m",
		System:   "sys",
		MaxSteps: 3,
		Compact: func(ctx context.Context, step int, hist []llm.Message) ([]llm.Message, error) {
			steps = append(steps, step)
			// 模拟压缩：只留 system + 一条摘要
			return []llm.Message{
				{Role: "system", Content: "sys"},
				{Role: "user", Content: "（摘要）"},
			}, nil
		},
	}
	_, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "原始"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("Compact 一次都没被调用")
	}
	if steps[0] != 1 {
		t.Fatalf("第一次调用应该在 step 1，得到 %d", steps[0])
	}
}

// Compact 返回 nil 表示"这一轮不用改"，Loop 必须保留原历史。
func TestLoopCompactNilKeepsHistory(t *testing.T) {
	chat := &fakeChat{reply: "好"}
	var seen []string
	l := &Loop{
		Chat:     chat,
		Model:    "m",
		MaxSteps: 1,
		Compact: func(ctx context.Context, step int, hist []llm.Message) ([]llm.Message, error) {
			for _, m := range hist {
				seen = append(seen, m.Content)
			}
			return nil, nil
		},
	}
	if _, err := l.Run(context.Background(), 1, []llm.Message{{Role: "user", Content: "原始"}}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || !strings.Contains(strings.Join(seen, ","), "原始") {
		t.Fatalf("钩子应该看到原始历史，实际 %v", seen)
	}
}
