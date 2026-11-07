package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dshgo/internal/llm"
)

// SideQueryRunner 是一次独立、无工具、无持久化的旁路模型调用。
// history 由宿主从源会话的持久化边界只读派生；本 runner 不认识源 session id，
// 因而无法把旁路 prompt 写回父会话、journal、queue、goal 或 title。
type SideQueryRunner struct {
	Chat      Chat
	Model     string
	Reasoning string
	MaxTokens int
}

// Run 消费官方 shared prompt 原文。Tools 始终缺省；每次调用都有独立消息切片，
// 并发 A/B 不会共享 history  backing array 或累计文本。
func (r *SideQueryRunner) Run(ctx context.Context, history []llm.Message, prompt string, onText func(string) error) (string, error) {
	if r == nil || r.Chat == nil {
		return "", errors.New("agent: side query 需要真实 Chat")
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("agent: side query prompt 不能为空")
	}
	if onText == nil {
		onText = func(string) error { return nil }
	}
	messages := cloneSideQueryHistory(history)
	messages = append(messages, llm.Message{Role: "user", Content: prompt})
	var streamed strings.Builder
	response, err := r.Chat.Stream(ctx, llm.Request{
		Model: r.Model, Messages: messages, MaxTokens: r.MaxTokens, Reasoning: r.Reasoning,
	}, func(event llm.Event) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if event.Kind != llm.EventContent || event.Text == "" {
			return nil
		}
		if err := onText(event.Text); err != nil {
			return err
		}
		streamed.WriteString(event.Text)
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if response == nil {
		return "", errors.New("agent: side query 模型返回 nil response")
	}
	if len(response.ToolCalls) != 0 {
		return "", errors.New("agent: side query 不允许工具调用")
	}
	answer := streamed.String()
	if answer == "" {
		answer = response.Content
		if answer != "" {
			if err := onText(answer); err != nil {
				return "", err
			}
		}
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", errors.New("agent: side query 模型没有给出回答")
	}
	return answer, nil
}

func cloneSideQueryHistory(history []llm.Message) []llm.Message {
	out := make([]llm.Message, len(history))
	for i, message := range history {
		out[i] = message
		out[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
		out[i].Images = append([]llm.MessageImage(nil), message.Images...)
		out[i].Parts = append([]llm.MessagePart(nil), message.Parts...)
	}
	return out
}

func (r *SideQueryRunner) String() string {
	if r == nil {
		return "independent-runner<nil>"
	}
	return fmt.Sprintf("independent-runner(model=%s)", r.Model)
}
