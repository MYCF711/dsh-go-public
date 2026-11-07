package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dshgo/internal/llm"
)

type sideQueryChatFunc func(context.Context, llm.Request, func(llm.Event) error) (*llm.Response, error)

func (f sideQueryChatFunc) Stream(ctx context.Context, req llm.Request, emit func(llm.Event) error) (*llm.Response, error) {
	return f(ctx, req, emit)
}

func TestSideQueryRunnerStreamsOfficialPromptWithoutToolsOrHistoryAlias(t *testing.T) {
	history := []llm.Message{{Role: "system", Content: "source-system"}}
	var deltas []string
	runner := &SideQueryRunner{Chat: sideQueryChatFunc(func(_ context.Context, req llm.Request, emit func(llm.Event) error) (*llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Fatalf("side query 暴露工具: %+v", req.Tools)
		}
		if len(req.Messages) != 2 || req.Messages[1].Content != "official-shared-prompt" {
			t.Fatalf("messages=%+v", req.Messages)
		}
		req.Messages[0].Content = "mutated-by-child"
		_ = emit(llm.Event{Kind: llm.EventContent, Text: "side "})
		_ = emit(llm.Event{Kind: llm.EventContent, Text: "answer"})
		return &llm.Response{Content: "side answer", FinishReason: "stop"}, nil
	}), Model: "model-a"}
	answer, err := runner.Run(context.Background(), history, "official-shared-prompt", func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil || answer != "side answer" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if history[0].Content != "source-system" || len(deltas) != 2 || deltas[0] != "side " || deltas[1] != "answer" {
		t.Fatalf("history=%+v deltas=%q", history, deltas)
	}
}

func TestSideQueryRunnerNonStreamingFallsBackToWholeAnswer(t *testing.T) {
	var deltas []string
	runner := &SideQueryRunner{Chat: sideQueryChatFunc(func(context.Context, llm.Request, func(llm.Event) error) (*llm.Response, error) {
		return &llm.Response{Content: "  complete answer  ", FinishReason: "stop"}, nil
	})}
	answer, err := runner.Run(context.Background(), nil, "recap", func(delta string) error { deltas = append(deltas, delta); return nil })
	if err != nil || answer != "complete answer" || len(deltas) != 1 || deltas[0] != "  complete answer  " {
		t.Fatalf("answer=%q deltas=%q err=%v", answer, deltas, err)
	}
}

func TestSideQueryRunnerFailureBranchesAreReal(t *testing.T) {
	for name, chat := range map[string]sideQueryChatFunc{
		"empty": func(context.Context, llm.Request, func(llm.Event) error) (*llm.Response, error) {
			return &llm.Response{}, nil
		},
		"tool": func(context.Context, llm.Request, func(llm.Event) error) (*llm.Response, error) {
			return &llm.Response{Content: "claim", ToolCalls: []llm.ToolCall{{ID: "call-1"}}}, nil
		},
		"model": func(context.Context, llm.Request, func(llm.Event) error) (*llm.Response, error) {
			return nil, errors.New("gateway failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &SideQueryRunner{Chat: chat}
			if answer, err := runner.Run(context.Background(), nil, "question", nil); err == nil || answer != "" {
				t.Fatalf("失败分支变成成功: answer=%q err=%v", answer, err)
			}
		})
	}
}

func TestSideQueryRunnerCancelStopsLateTextAndABStayIndependent(t *testing.T) {
	started := make(chan string, 2)
	var mu sync.Mutex
	seen := map[string]string{}
	chat := sideQueryChatFunc(func(ctx context.Context, req llm.Request, emit func(llm.Event) error) (*llm.Response, error) {
		prompt := req.Messages[len(req.Messages)-1].Content
		started <- prompt
		mu.Lock()
		seen[prompt] = req.Messages[0].Content
		mu.Unlock()
		if prompt == "cancel-me" {
			<-ctx.Done()
			if err := emit(llm.Event{Kind: llm.EventContent, Text: "late"}); !errors.Is(err, context.Canceled) {
				t.Errorf("取消后迟到 text 未被挡住: %v", err)
			}
			return nil, ctx.Err()
		}
		_ = emit(llm.Event{Kind: llm.EventContent, Text: prompt + "-answer"})
		return &llm.Response{Content: prompt + "-answer", FinishReason: "stop"}, nil
	})
	runner := &SideQueryRunner{Chat: chat}
	ctx, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan error, 1)
	var late []string
	go func() {
		_, err := runner.Run(ctx, []llm.Message{{Role: "system", Content: "A"}}, "cancel-me", func(delta string) error { late = append(late, delta); return nil })
		cancelDone <- err
	}()
	if got := <-started; got != "cancel-me" {
		t.Fatalf("started=%q", got)
	}
	otherDone := make(chan string, 1)
	go func() {
		answer, _ := runner.Run(context.Background(), []llm.Message{{Role: "system", Content: "B"}}, "other", nil)
		otherDone <- answer
	}()
	if got := <-started; got != "other" {
		t.Fatalf("started=%q", got)
	}
	cancel()
	select {
	case err := <-cancelDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消没有停止模型调用")
	}
	if answer := <-otherDone; answer != "other-answer" {
		t.Fatalf("B answer=%q", answer)
	}
	if len(late) != 0 || seen["cancel-me"] != "A" || seen["other"] != "B" {
		t.Fatalf("late=%q seen=%+v", late, seen)
	}
}
