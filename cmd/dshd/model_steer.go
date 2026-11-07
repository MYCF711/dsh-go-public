package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

type modelTurnInput struct {
	text    string
	content bool
	durable []session.ContentBlock
	message llm.Message
}

func modelInputFromText(text string) modelTurnInput {
	return modelTurnInput{text: text, message: llm.Message{Role: "user", Content: text}}
}

func modelInputFromBlocks(home string, blocks []transport.ContentBlock) (modelTurnInput, error) {
	durable, parts, err := promptPartsFromBlocks(home, blocks)
	if err != nil {
		return modelTurnInput{}, err
	}
	return modelTurnInput{content: true, durable: durable,
		message: llm.Message{Role: "user", Parts: parts}}, nil
}

type modelTurnEvent struct {
	input    *modelTurnInput
	response *llm.Response
	// typ 非空时是真实 step/start、assistant/text|reasoning、step/end。
	// 这些事件与 response 一起按原顺序持久化；live journal 在发生时立即发送。
	typ    string
	turn   int
	step   int
	index  int
	text   string
	model  string
	effort string
	// offloadImages / offloadIndexes 只挂在**本轮首条用户事件**上：它记录这一轮
	// 有几张图在请求失败后被降级成官方占位文本，以及它们在本条消息里的深度优先
	// 图片序号（从 0 起）。落盘时紧跟 user/message 写成一条 image/offload 决策，
	// durable 块一个字节都不改（append-only）。
	offloadImages  int
	offloadIndexes []int
}

func modelStreamEventData(event modelTurnEvent) map[string]any {
	if event.typ == "model/selection" {
		data := map[string]any{"model": event.model}
		if event.effort != "" {
			data["reasoningEffort"] = event.effort
		}
		return data
	}
	data := map[string]any{"turn": event.turn, "step": event.step}
	if event.typ == "assistant/text" || event.typ == "assistant/reasoning" {
		data["index"] = event.index
		data["text"] = event.text
		data["model"] = event.model
	}
	return data
}

func emitModelStreamEvent(journals *transport.Journals, pub func(string, transport.ServerFrame),
	sessionID string, event modelTurnEvent) {
	data, _ := json.Marshal(modelStreamEventData(event))
	emitFrame(journals, pub, sessionID, event.typ, data)
}

// modelTurnInbox 的最终 drain-or-close 与 Steer 共用一把锁：关闭后不能再
// accepted；有项时保持 open，下一模型调用之后还会再检查一次。
type modelTurnInbox struct {
	mu     sync.Mutex
	queue  []*modelTurnInput
	closed bool
}

func (b *modelTurnInbox) submit(input *modelTurnInput) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrSteerLate
	}
	b.queue = append(b.queue, input)
	return nil
}

func (b *modelTurnInbox) drain(closeIfEmpty bool) ([]*modelTurnInput, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queue) == 0 {
		if closeIfEmpty {
			b.closed = true
			return nil, true
		}
		return nil, false
	}
	out := append([]*modelTurnInput(nil), b.queue...)
	b.queue = nil
	return out, false
}

func (b *modelTurnInbox) close() {
	b.mu.Lock()
	b.closed = true
	b.queue = nil
	b.mu.Unlock()
}

func modelSteerKey(sessionID, turnID string) string { return sessionID + "\x00" + turnID }

func (r *modelRunner) signalModelSteerLocked() {
	if r.steerWake != nil {
		close(r.steerWake)
	}
	r.steerWake = make(chan struct{})
}

func (r *modelRunner) registerModelSteer(sessionID, turnID string) (*modelTurnInbox, error) {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	key := modelSteerKey(sessionID, turnID)
	if r.steers[key] != nil {
		return nil, fmt.Errorf("同一 turn 已有 model steer inbox: %s/%s", sessionID, turnID)
	}
	if r.steers == nil {
		r.steers = map[string]*modelTurnInbox{}
	}
	inbox := &modelTurnInbox{}
	r.steers[key] = inbox
	r.signalModelSteerLocked()
	return inbox, nil
}

func (r *modelRunner) unregisterModelSteer(sessionID, turnID string, inbox *modelTurnInbox) {
	inbox.close()
	r.steerMu.Lock()
	key := modelSteerKey(sessionID, turnID)
	if r.steers[key] == inbox {
		delete(r.steers, key)
	}
	if r.finished == nil {
		r.finished = map[string]string{}
	}
	r.finished[sessionID] = turnID
	r.signalModelSteerLocked()
	r.steerMu.Unlock()
}

func (r *modelRunner) modelSteer(ctx context.Context, sessionID, turnID string, input *modelTurnInput) error {
	waitCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		waitCtx, cancel = context.WithTimeout(ctx, steerIPCWait)
	}
	defer cancel()
	key := modelSteerKey(sessionID, turnID)
	for {
		r.steerMu.Lock()
		inbox := r.steers[key]
		finished := r.finished[sessionID] == turnID
		if r.steerWake == nil {
			r.steerWake = make(chan struct{})
		}
		wake := r.steerWake
		r.steerMu.Unlock()
		if inbox != nil {
			return inbox.submit(input)
		}
		if finished {
			return fmt.Errorf("%w: session=%s turn=%s", ErrSteerLate, sessionID, turnID)
		}
		select {
		case <-wake:
		case <-waitCtx.Done():
			return fmt.Errorf("%w: 等当前 model turn 注册失败: %v", ErrSteerLate, waitCtx.Err())
		}
	}
}

func (r *modelRunner) Steer(ctx context.Context, sessionID, turnID, text string) error {
	input := modelInputFromText(text)
	return r.modelSteer(ctx, sessionID, turnID, &input)
}

func (r *modelRunner) SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	input, err := modelInputFromBlocks(r.home, blocks)
	if err != nil {
		return err
	}
	return r.modelSteer(ctx, sessionID, turnID, &input)
}

func (r *modelRunner) runModelTurn(ctx context.Context, sessionID, turnID string, initial modelTurnInput) error {
	inbox, err := r.registerModelSteer(sessionID, turnID)
	if err != nil {
		emitTurnFrame(r.journals, r.pub, sessionID, turnID, err)
		return err
	}
	defer r.unregisterModelSteer(sessionID, turnID, inbox)
	fail := func(err error) error {
		// 先拒绝新接管，再让客户端看到终局；反过来会留下“error 后又 accepted”的窗口。
		inbox.close()
		emitTurnFrame(r.journals, r.pub, sessionID, turnID, err)
		return err
	}

	history, err := r.history(sessionID)
	if err != nil {
		return fail(err)
	}
	model, err := resolveServeModel(r.home, sessionID, r.model)
	if err != nil {
		return fail(err)
	}
	provider, routed, err := resolveServeProvider(r.home, model)
	if err != nil {
		return fail(err)
	}
	base, credential := r.base, os.Getenv("DSHD_LLM_KEY")
	if routed {
		base, credential = provider.BaseURL, provider.Credential()
	}

	events := []modelTurnEvent{{input: &initial}}
	history = append(history, initial.message)
	for {
		// 首步前也是一个 step 边界；首步在飞时收到的项则在下面的响应后边界接管。
		queued, _ := inbox.drain(false)
		for _, input := range queued {
			events = append(events, modelTurnEvent{input: input})
			history = append(history, input.message)
		}
		resp, streamErr := llm.New(base, credential).Stream(ctx,
			llm.Request{Model: model, Messages: history, Stream: true}, nil)
		if streamErr != nil {
			streamErr = redactServeProviderError(streamErr, credential)
			return fail(streamErr)
		}
		events = append(events, modelTurnEvent{response: resp})
		emitModelAssistant(r.journals, r.pub, sessionID, resp)
		if resp != nil {
			history = append(history, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		}

		queued, closed := inbox.drain(true)
		for _, input := range queued {
			events = append(events, modelTurnEvent{input: input})
			history = append(history, input.message)
		}
		if closed {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			break
		}
	}

	if err := r.persistModelEvents(sessionID, events); err != nil {
		return fail(err)
	}
	emitTurnFrame(r.journals, r.pub, sessionID, turnID, nil)
	return nil
}

func emitModelAssistant(journals *transport.Journals, pub func(string, transport.ServerFrame), sessionID string, resp *llm.Response) {
	if resp == nil || !assistantHasPayload(resp.Content, resp.Reasoning, len(resp.ToolCalls)) {
		return
	}
	payload := map[string]any{"text": resp.Content}
	if strings.TrimSpace(resp.Reasoning) != "" && strings.TrimSpace(resp.Content) == "" {
		payload["reasoning"] = resp.Reasoning
	}
	data, _ := json.Marshal(payload)
	emitFrame(journals, pub, sessionID, "assistant/message", data)
}
