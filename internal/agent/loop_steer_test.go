package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"dshgo/internal/llm"
	"dshgo/internal/spi"
)

// steerTestChat 的 onCall 在 Stream 返回前运行，因此可以模拟模型在飞时提交 steer。
type steerTestChat struct {
	responses []*llm.Response
	errs      []error
	onCall    func(call int)
	reqs      []llm.Request
}

func (c *steerTestChat) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event) error) (*llm.Response, error) {
	c.reqs = append(c.reqs, req)
	call := len(c.reqs)
	if c.onCall != nil {
		c.onCall(call)
	}
	i := call - 1
	if i >= len(c.responses) {
		return nil, fmt.Errorf("steer 测试脚本在第 %d 次调用用完了", call)
	}
	if i < len(c.errs) && c.errs[i] != nil {
		return nil, c.errs[i]
	}
	resp := c.responses[i]
	if onEvent != nil {
		if resp.Reasoning != "" {
			if err := onEvent(llm.Event{Kind: llm.EventReasoning, Text: resp.Reasoning}); err != nil {
				return nil, err
			}
		}
		if resp.Content != "" {
			if err := onEvent(llm.Event{Kind: llm.EventContent, Text: resp.Content}); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

func mustSubmitSteer(t *testing.T, inbox *SteerInbox, content string, data any) {
	t.Helper()
	if err := inbox.Submit(UserMessage{
		Message: llm.Message{Role: "user", Content: content},
		Data:    data,
	}); err != nil {
		t.Fatalf("提交 steer %q 失败: %v", content, err)
	}
}

func TestSteerInjectedBeforeFirstStep(t *testing.T) {
	inbox := NewSteerInbox()
	mustSubmitSteer(t, inbox, "首步前 steer", "first-data")
	chat := &steerTestChat{responses: []*llm.Response{{Content: "完成", FinishReason: "stop"}}}
	var events []Event
	var requestCounts []int
	loop := &Loop{
		Chat:   chat,
		Model:  "test-model",
		Steers: inbox,
		Emit: func(e Event) error {
			events = append(events, e)
			if e.Kind == EventUserMsg {
				requestCounts = append(requestCounts, len(chat.reqs))
			}
			return nil
		},
	}

	hist, err := loop.Run(context.Background(), 4, []llm.Message{{Role: "user", Content: "原始问题"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.reqs) != 1 {
		t.Fatalf("模型调用数 = %d，期望 1", len(chat.reqs))
	}
	if got := chat.reqs[0].Messages; len(got) != 2 || got[0].Content != "原始问题" || got[1].Content != "首步前 steer" {
		t.Fatalf("首步请求未包含 steer: %+v", got)
	}
	if !reflect.DeepEqual(requestCounts, []int{0}) {
		t.Fatalf("user/message 必须在首步模型调用前，事件发生时请求数 = %v", requestCounts)
	}
	var userEvents []Event
	for _, e := range events {
		if e.Kind == EventUserMsg {
			userEvents = append(userEvents, e)
		}
	}
	if len(userEvents) != 1 || userEvents[0].Turn != 4 || userEvents[0].Step != 1 ||
		userEvents[0].User == nil || userEvents[0].User.Data != "first-data" {
		t.Fatalf("首步 user/message 事件不对: %+v", userEvents)
	}
	if len(hist) != 3 || hist[1].Content != "首步前 steer" || hist[2].Content != "完成" {
		t.Fatalf("首步 steer 返回历史不对: %+v", hist)
	}
}

func TestSteersSubmittedDuringFirstLLMContinueSameTurnFIFO(t *testing.T) {
	inbox := NewSteerInbox()
	data1 := map[string]any{"id": 1}
	data2 := map[string]any{"id": 2}
	chat := &steerTestChat{responses: []*llm.Response{
		{Content: "assistant1", FinishReason: "stop"},
		{Content: "assistant2", FinishReason: "stop"},
	}}
	chat.onCall = func(call int) {
		if call == 1 {
			mustSubmitSteer(t, inbox, "steer1", data1)
			mustSubmitSteer(t, inbox, "steer2", data2)
		}
	}
	var events []Event
	var userRequestCounts []int
	loop := &Loop{
		Chat:   chat,
		Model:  "test-model",
		Steers: inbox,
		Emit: func(e Event) error {
			events = append(events, e)
			if e.Kind == EventUserMsg {
				userRequestCounts = append(userRequestCounts, len(chat.reqs))
			}
			return nil
		},
	}

	hist, err := loop.Run(context.Background(), 9, []llm.Message{{Role: "user", Content: "原始问题"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.reqs) != 2 {
		t.Fatalf("模型调用数 = %d，期望 2；事件=%+v", len(chat.reqs), events)
	}
	wantKinds := []string{
		"step/start", "assistant/text", "assistant/message", "step/end",
		"user/message", "user/message",
		"step/start", "assistant/text", "assistant/message", "step/end", "turn/end",
	}
	if got := kinds(events); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("steer 中间步必须在 user 前闭合、第二步在 user 后开始：got=%v want=%v", got, wantKinds)
	}
	second := chat.reqs[1].Messages
	if len(second) != 4 || second[0].Content != "原始问题" || second[1].Content != "assistant1" ||
		second[2].Content != "steer1" || second[3].Content != "steer2" {
		t.Fatalf("第二请求应含 assistant1+steer1+steer2，实际: %+v", second)
	}
	if !reflect.DeepEqual(userRequestCounts, []int{1, 1}) {
		t.Fatalf("两条 user/message 都必须在第二模型调用前，事件发生时请求数 = %v", userRequestCounts)
	}
	var userEvents []Event
	for _, e := range events {
		if e.Turn != 9 {
			t.Fatalf("steer 后续事件不得换 turn: %+v", e)
		}
		if e.Kind == EventUserMsg {
			userEvents = append(userEvents, e)
		}
	}
	if len(userEvents) != 2 {
		t.Fatalf("user/message 事件数 = %d，期望 2；全部事件=%+v", len(userEvents), events)
	}
	for i, e := range userEvents {
		if e.Step != 2 || e.User == nil {
			t.Fatalf("第 %d 条 steer 事件必须在 step2 且带 User: %+v", i+1, e)
		}
	}
	if userEvents[0].User.Message.Content != "steer1" || userEvents[1].User.Message.Content != "steer2" ||
		!reflect.DeepEqual(userEvents[0].User.Data, data1) || !reflect.DeepEqual(userEvents[1].User.Data, data2) {
		t.Fatalf("steer FIFO 或 Data 透传不对: %+v", userEvents)
	}
	wantHistory := []string{"原始问题", "assistant1", "steer1", "steer2", "assistant2"}
	if len(hist) != len(wantHistory) {
		t.Fatalf("返回历史 = %+v", hist)
	}
	for i, want := range wantHistory {
		if hist[i].Content != want {
			t.Fatalf("历史第 %d 条 = %q，期望 %q；全部=%+v", i, hist[i].Content, want, hist)
		}
	}
	if err := inbox.Submit(UserMessage{Message: llm.Message{Role: "user", Content: "too late"}}); !errors.Is(err, ErrSteerClosed) {
		t.Fatalf("Run 完成后 Submit 必须返回 ErrSteerClosed，实际 %v", err)
	}
}

type steerSubmitTool struct {
	inbox *SteerInbox
	item  UserMessage
	stop  bool
}

func (t *steerSubmitTool) Name() string            { return "submit_steer" }
func (t *steerSubmitTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *steerSubmitTool) Description() string     { return "在工具执行中提交 steer" }
func (t *steerSubmitTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *steerSubmitTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	if err := t.inbox.Submit(t.item); err != nil {
		return spi.Result{}, err
	}
	return spi.Result{Summary: "工具结果", Stop: t.stop}, nil
}

func toolSteerLoop(t *testing.T, stop bool) (*Loop, *steerTestChat, *[]Event, *[]int) {
	t.Helper()
	inbox := NewSteerInbox()
	tool := &steerSubmitTool{
		inbox: inbox,
		item: UserMessage{
			Message: llm.Message{Role: "user", Content: "工具期间 steer"},
			Data:    "tool-data",
		},
		stop: stop,
	}
	chat := &steerTestChat{responses: []*llm.Response{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function", Function: llm.Func{Name: "submit_steer", Arguments: `{}`},
		}}},
		{Content: "第二回答", FinishReason: "stop"},
	}}
	loop, events := newLoop(t, chat, tool)
	loop.Steers = inbox
	requestCounts := &[]int{}
	loop.Emit = func(e Event) error {
		*events = append(*events, e)
		if e.Kind == EventUserMsg {
			*requestCounts = append(*requestCounts, len(chat.reqs))
		}
		return nil
	}
	return loop, chat, events, requestCounts
}

func TestSteerAtToolNextStepBoundary(t *testing.T) {
	loop, chat, events, requestCounts := toolSteerLoop(t, false)
	hist, err := loop.Run(context.Background(), 3, []llm.Message{{Role: "user", Content: "调用工具"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.reqs) != 2 {
		t.Fatalf("工具路径模型调用数 = %d，期望 2", len(chat.reqs))
	}
	second := chat.reqs[1].Messages
	if len(second) != 4 || second[1].Role != "assistant" || second[2].Role != "tool" ||
		second[2].ToolCallID != "call_1" || second[3].Content != "工具期间 steer" {
		t.Fatalf("工具结果后的下一边界请求不对: %+v", second)
	}
	if !reflect.DeepEqual(*requestCounts, []int{1}) {
		t.Fatalf("工具路径 user/message 必须在第二模型调用前，请求数 = %v", *requestCounts)
	}
	toolResultIndex, userIndex := -1, -1
	for i, e := range *events {
		if e.Turn != 3 {
			t.Fatalf("工具路径 steer 不得换 turn: %+v", e)
		}
		if e.Kind == EventToolResult {
			toolResultIndex = i
		}
		if e.Kind == EventUserMsg {
			userIndex = i
			if e.Step != 2 || e.User == nil || e.User.Data != "tool-data" {
				t.Fatalf("工具路径 steer 事件不对: %+v", e)
			}
		}
	}
	if toolResultIndex < 0 || userIndex <= toolResultIndex {
		t.Fatalf("user/message 必须在工具结果之后: %+v", *events)
	}
	if len(hist) != 5 || hist[3].Content != "工具期间 steer" || hist[4].Content != "第二回答" {
		t.Fatalf("工具路径返回历史不对: %+v", hist)
	}
}

func TestSteerContinuesAfterAwaitingUser(t *testing.T) {
	loop, chat, events, _ := toolSteerLoop(t, true)
	_, err := loop.Run(context.Background(), 6, []llm.Message{{Role: "user", Content: "调用工具"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.reqs) != 2 {
		t.Fatalf("awaiting_user 有 steer 时必须继续第二模型步，实际调用 %d 次", len(chat.reqs))
	}
	var turnEnds []Event
	var users []Event
	for _, e := range *events {
		switch e.Kind {
		case EventTurnEnd:
			turnEnds = append(turnEnds, e)
		case EventUserMsg:
			users = append(users, e)
		}
	}
	if len(users) != 1 || users[0].Step != 2 || users[0].Turn != 6 {
		t.Fatalf("awaiting_user 的 steer 事件不对: %+v", users)
	}
	if len(turnEnds) != 1 || turnEnds[0].Step != 2 || turnEnds[0].Finish != "stop" {
		t.Fatalf("有 steer 时不得先以 awaiting_user 收工: %+v", turnEnds)
	}
}

func TestSteerNotEmittedOnCancellationOrModelError(t *testing.T) {
	modelErr := errors.New("模型在飞失败")
	for _, tc := range []struct {
		name    string
		err     error
		cancel  bool
		wantErr error
	}{
		{name: "取消", err: context.Canceled, cancel: true, wantErr: context.Canceled},
		{name: "模型错误", err: modelErr, wantErr: modelErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := NewSteerInbox()
			chat := &steerTestChat{
				responses: []*llm.Response{{Content: "不会成功"}},
				errs:      []error{tc.err},
			}
			ctx := context.Background()
			cancel := func() {}
			if tc.cancel {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			chat.onCall = func(call int) {
				mustSubmitSteer(t, inbox, "未消费 steer", nil)
				cancel()
			}
			loop, events := newLoop(t, chat)
			loop.Steers = inbox
			initial := []llm.Message{{Role: "user", Content: "原始问题"}}
			hist, err := loop.Run(ctx, 1, initial)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run 错误 = %v，期望包含 %v", err, tc.wantErr)
			}
			for _, e := range *events {
				if e.Kind == EventUserMsg {
					t.Fatalf("取消/模型错误不得写未消费 steer: %+v", e)
				}
			}
			if !reflect.DeepEqual(hist, initial) {
				t.Fatalf("取消/模型错误返回历史不得含 steer: %+v", hist)
			}
			if len(chat.reqs) != 1 {
				t.Fatalf("失败后不得再请求模型，实际 %d 次", len(chat.reqs))
			}
		})
	}
}

func TestCancelledRunDoesNotEmitPreloadedSteer(t *testing.T) {
	inbox := NewSteerInbox()
	mustSubmitSteer(t, inbox, "首步前但未消费", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chat := &steerTestChat{responses: []*llm.Response{{Content: "不应调用"}}}
	loop, events := newLoop(t, chat)
	loop.Steers = inbox
	initial := []llm.Message{{Role: "user", Content: "原始问题"}}
	hist, err := loop.Run(ctx, 1, initial)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 Run 的错误 = %v", err)
	}
	if !reflect.DeepEqual(hist, initial) || len(chat.reqs) != 0 {
		t.Fatalf("已取消 Run 不应写 steer 或调用模型: hist=%+v calls=%d", hist, len(chat.reqs))
	}
	for _, e := range *events {
		if e.Kind == EventUserMsg {
			t.Fatalf("已取消 Run 不得 emit steer: %+v", e)
		}
	}
}

func TestSteerEmitFailureDoesNotAppendFailedMessage(t *testing.T) {
	inbox := NewSteerInbox()
	mustSubmitSteer(t, inbox, "已 emit steer", nil)
	mustSubmitSteer(t, inbox, "emit 失败 steer", nil)
	emitErr := errors.New("user/message 持久化失败")
	chat := &steerTestChat{responses: []*llm.Response{{Content: "不应调用"}}}
	attempts := 0
	loop := &Loop{
		Chat:   chat,
		Model:  "test-model",
		Steers: inbox,
		Emit: func(e Event) error {
			if e.Kind == EventUserMsg {
				attempts++
				if attempts == 2 {
					return emitErr
				}
			}
			return nil
		},
	}
	initial := []llm.Message{{Role: "user", Content: "原始问题"}}
	hist, err := loop.Run(context.Background(), 1, initial)
	if !errors.Is(err, emitErr) {
		t.Fatalf("Emit 错误必须原样冒泡，实际 %v", err)
	}
	if attempts != 2 {
		t.Fatalf("user/message emit 次数 = %d，期望 2", attempts)
	}
	if len(hist) != 2 || hist[0].Content != "原始问题" || hist[1].Content != "已 emit steer" {
		t.Fatalf("只应 append emit 成功的第一条 steer: %+v", hist)
	}
	for _, msg := range hist {
		if msg.Content == "emit 失败 steer" {
			t.Fatalf("Emit 失败的 steer 不得 append: %+v", hist)
		}
	}
	if len(chat.reqs) != 0 {
		t.Fatalf("user/message Emit 失败后不得调用模型，实际 %d 次", len(chat.reqs))
	}
}

func TestSteerCloseIsIdempotentAndRunClosesWithoutSteer(t *testing.T) {
	inbox := NewSteerInbox()
	mustSubmitSteer(t, inbox, "未消费", nil)
	inbox.Close()
	inbox.Close()
	if items := inbox.drain(); len(items) != 0 {
		t.Fatalf("Close 必须清空未消费项: %+v", items)
	}
	if err := inbox.Submit(UserMessage{}); !errors.Is(err, ErrSteerClosed) {
		t.Fatalf("Close 后 Submit = %v，期望 ErrSteerClosed", err)
	}

	runInbox := NewSteerInbox()
	chat := &steerTestChat{responses: []*llm.Response{{Content: "完成", FinishReason: "stop"}}}
	loop, _ := newLoop(t, chat)
	loop.Steers = runInbox
	if _, err := loop.Run(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := runInbox.Submit(UserMessage{Message: llm.Message{Role: "user", Content: "迟到"}}); !errors.Is(err, ErrSteerClosed) {
		t.Fatalf("无 steer 的 Run 完成后 Submit = %v，期望 ErrSteerClosed", err)
	}
}
