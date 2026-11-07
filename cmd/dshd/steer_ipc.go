package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dshgo/internal/agent"
	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

// ErrSteerLate 表示当前轮没有可接管的下一 step。调用方不得把它退成
// followup/新 turn；HTTP 层会按普通 prompt 冲突返回明确的 409。
var ErrSteerLate = errors.New("steer 过晚：当前轮已收工或尚未接管")

const (
	steerIPCRequestTag  = "@@STEER@@ "
	steerIPCResponseTag = "@@STEER-RESP@@ "
	steerIPCWait        = 5 * time.Second
)

type steerIPCRequest struct {
	RequestID string                   `json:"requestId"`
	SessionID string                   `json:"sessionId"`
	TurnID    string                   `json:"turnId"`
	Text      string                   `json:"text,omitempty"`
	Blocks    []transport.ContentBlock `json:"blocks,omitempty"`
}

type steerIPCResponse struct {
	RequestID string `json:"requestId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	Accepted  bool   `json:"accepted"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Steer/SteerContent 都进入同一个子进程 inbox；文本只是单 text 块的旧入口，
// 不另建一条排队路径。应答只在 agent.SteerInbox.Submit 真正接管后 accepted。
func (r *loopRunner) Steer(ctx context.Context, sessionID, turnID, text string) error {
	if r.jobActions == nil {
		return fmt.Errorf("%w: 没有当前子进程 IPC", ErrSteerLate)
	}
	return r.jobActions.submitSteer(ctx, steerIPCRequest{SessionID: sessionID, TurnID: turnID, Text: text})
}

func (r *loopRunner) SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	if r.jobActions == nil {
		return fmt.Errorf("%w: 没有当前子进程 IPC", ErrSteerLate)
	}
	return r.jobActions.submitSteer(ctx, steerIPCRequest{SessionID: sessionID, TurnID: turnID, Blocks: blocks})
}

// modelOverrideLoopRunner 每轮复制的是 argv 配置，不是运行代次；steer 必须
// 回到共享 base 的 jobActionBroker，绝不能写到另一个会话/子进程的 stdin。
func (r *modelOverrideLoopRunner) Steer(ctx context.Context, sessionID, turnID, text string) error {
	return r.base.Steer(ctx, sessionID, turnID, text)
}

func (r *modelOverrideLoopRunner) SteerContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	return r.base.SteerContent(ctx, sessionID, turnID, blocks)
}

func (b *jobActionBroker) deliverSteer(line string) bool {
	var resp steerIPCResponse
	if json.Unmarshal([]byte(strings.TrimPrefix(line, steerIPCResponseTag)), &resp) != nil || resp.RequestID == "" {
		return false
	}
	b.mu.Lock()
	ch := b.steerPending[resp.RequestID]
	delete(b.steerPending, resp.RequestID)
	b.mu.Unlock()
	if ch != nil {
		select {
		case ch <- resp:
		default:
		}
	}
	return true
}

func (b *jobActionBroker) submitSteer(ctx context.Context, req steerIPCRequest) error {
	waitCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		waitCtx, cancel = context.WithTimeout(ctx, steerIPCWait)
	}
	defer cancel()
	run, err := b.waitForSteerRun(waitCtx, req.SessionID, req.TurnID)
	if err != nil {
		return err
	}
	requestID, err := newSteerRequestID()
	if err != nil {
		return err
	}
	req.RequestID = requestID
	ch := make(chan steerIPCResponse, 1)
	b.mu.Lock()
	if run.done {
		b.mu.Unlock()
		return fmt.Errorf("%w: 子进程已收工", ErrSteerLate)
	}
	if b.steerPending == nil {
		b.steerPending = map[string]chan steerIPCResponse{}
	}
	b.steerPending[requestID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.steerPending, requestID)
		b.mu.Unlock()
	}()

	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	run.writeMu.Lock()
	_, err = fmt.Fprintf(run.stdin, "%s%s\n", steerIPCRequestTag, raw)
	run.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("写 steer 到当前子进程失败: %w", err)
	}

	select {
	case resp := <-ch:
		return steerResponseError(req, resp)
	case <-run.exited:
		// Wait+stderr 分发结束后才 close(exited)。若 ACK 已在队列，真实接管结果
		// 优先于退出通知，避免正常收工把已 accepted 随机报成 late。
		select {
		case resp := <-ch:
			return steerResponseError(req, resp)
		default:
			return fmt.Errorf("%w: 当前 turn 子进程已退出", ErrSteerLate)
		}
	case <-waitCtx.Done():
		return fmt.Errorf("等当前 turn 接管 steer 失败: %w", waitCtx.Err())
	}
}

func steerResponseError(req steerIPCRequest, resp steerIPCResponse) error {
	if resp.SessionID != req.SessionID || resp.TurnID != req.TurnID || resp.RequestID != req.RequestID {
		return fmt.Errorf("steer IPC 应答身份不匹配: %+v", resp)
	}
	if resp.Accepted {
		return nil
	}
	if resp.Code == "late" {
		return fmt.Errorf("%w: %s", ErrSteerLate, resp.Error)
	}
	if resp.Error == "" {
		resp.Error = "steer 被子进程拒绝"
	}
	return errors.New(resp.Error)
}

func (b *jobActionBroker) waitForSteerRun(ctx context.Context, sessionID, turnID string) (*jobActionRun, error) {
	for {
		b.mu.Lock()
		run := b.runs[sessionID]
		if run != nil {
			stale := run.turnID == "" || run.turnID != turnID || run.done
			b.mu.Unlock()
			if stale {
				return nil, fmt.Errorf("%w: session=%s turn=%s 已收工或代次不匹配", ErrSteerLate, sessionID, turnID)
			}
			return run, nil
		}
		if b.runChanged == nil {
			b.runChanged = make(chan struct{})
		}
		changed := b.runChanged
		b.mu.Unlock()
		// promptService 已登记 live，但 go execute/子进程 Start 可能还没跑；
		// 这里等真实注册，不能把这段初始化空窗误报成“过晚”。
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: 等当前 turn 子进程注册失败: %v", ErrSteerLate, ctx.Err())
		}
	}
}

func newSteerRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "steerreq-" + hex.EncodeToString(raw[:]), nil
}

// serveSteerIPC 是 run 子进程唯一的 steer 请求消费者。stdinDispatcher 保持
// 线上字节顺序，本 goroutine 串行解析/转换/Submit，因此混合 text/content 也 FIFO。
func serveSteerIPC(ctx context.Context, requests <-chan string, responses io.Writer, inbox *agent.SteerInbox, home, sessionID, turnID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-requests:
			if !ok {
				return
			}
			var req steerIPCRequest
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, steerIPCRequestTag)), &req); err != nil {
				continue // 无法取得 requestId，父进程会由退出/超时得到失败，不能伪造 ACK。
			}
			resp := steerIPCResponse{RequestID: req.RequestID, SessionID: req.SessionID, TurnID: req.TurnID}
			item, err := steerUserMessage(home, req)
			if err == nil && (req.SessionID != sessionID || req.TurnID != turnID) {
				err = fmt.Errorf("%w: 请求身份不属于这个 run 子进程", ErrSteerLate)
			}
			if err == nil {
				err = inbox.Submit(item)
			}
			if err != nil {
				resp.Code = "error"
				if errors.Is(err, ErrSteerLate) || errors.Is(err, agent.ErrSteerClosed) {
					resp.Code = "late"
				}
				resp.Error = err.Error()
			} else {
				resp.Accepted = true
			}
			raw, _ := json.Marshal(resp)
			fmt.Fprintf(responses, "%s%s\n", steerIPCResponseTag, raw)
		}
	}
}

func steerUserMessage(home string, req steerIPCRequest) (agent.UserMessage, error) {
	if len(req.Blocks) == 0 {
		return agent.UserMessage{
			Message: llm.Message{Role: "user", Content: req.Text},
			Data:    session.UserMessage([]spi.Block{{Type: "text", Text: req.Text}}),
		}, nil
	}
	durable, parts, err := promptPartsFromBlocks(home, req.Blocks)
	if err != nil {
		return agent.UserMessage{}, err
	}
	return agent.UserMessage{
		Message: llm.Message{Role: "user", Parts: parts},
		Data:    session.UserContentMessage(durable),
	}, nil
}
