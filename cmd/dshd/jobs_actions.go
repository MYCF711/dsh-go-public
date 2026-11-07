package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/internal/transport"
)

type jobActionRequest struct {
	RequestID string `json:"requestId"`
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Cursor    int    `json:"cursor"`
	Limit     int    `json:"limit"`
	WaitMs    int    `json:"waitMs"`
	Stop      bool   `json:"stop"`
}

type jobActionResponse struct {
	RequestID string               `json:"requestId"`
	SessionID string               `json:"sessionId"`
	ID        string               `json:"id"`
	Output    *transport.JobOutput `json:"output,omitempty"`
	Stopped   bool                 `json:"stopped,omitempty"`
	Code      string               `json:"code,omitempty"`
	Error     string               `json:"error,omitempty"`
}

type jobActionRun struct {
	sessionID string
	turnID    string
	pid       int
	image     string
	stdin     io.WriteCloser
	writeMu   sync.Mutex
	done      bool
	// exited 在 cmd.Wait 且 stderr 分发收尾后关闭。steer 等待除了 request
	// response 还必须监听它，否则过晚提交会等到调用方超时，而不是立刻明确失败。
	exited   chan struct{}
	exitOnce sync.Once
}

type jobActionBroker struct {
	mu           sync.Mutex
	home         string
	runs         map[string]*jobActionRun
	pending      map[string]chan jobActionResponse
	steerPending map[string]chan steerIPCResponse
	// runChanged 叫醒 prompt ACK 后、子进程尚未 register 的早到 steer。
	// close+重建在 b.mu 内完成，等待者不会丢注册/退出代次。
	runChanged chan struct{}
}

func newJobActionBroker(home string) *jobActionBroker {
	return &jobActionBroker{home: home, runs: map[string]*jobActionRun{}, pending: map[string]chan jobActionResponse{}, steerPending: map[string]chan steerIPCResponse{}, runChanged: make(chan struct{})}
}

func (b *jobActionBroker) signalRunChangeLocked() {
	if b.runChanged != nil {
		close(b.runChanged)
	}
	b.runChanged = make(chan struct{})
}

// registerRun 保留给旧任务动作调用方；生产 steer 使用 registerTurnRun，
// 把宿主 turnID 与具体 PID/stdin 代次绑定，A/B 会话不会落到共享 stdin。
func (b *jobActionBroker) registerRun(sessionID string, pid int, image string, stdin io.WriteCloser) (*jobActionRun, error) {
	return b.registerTurnRun(sessionID, "", pid, image, stdin)
}

func (b *jobActionBroker) registerTurnRun(sessionID, turnID string, pid int, image string, stdin io.WriteCloser) (*jobActionRun, error) {
	if !validJobActionPart(sessionID) || pid <= 0 || stdin == nil {
		return nil, fmt.Errorf("job run 登记缺 sessionId/PID/stdin")
	}
	canonical, err := canonicalProcessImage(image)
	if err != nil {
		return nil, err
	}
	if err := verifyProcessImage(pid, canonical); err != nil {
		return nil, fmt.Errorf("宿主刚启动的 PID %d 映像归属校验失败: %w", pid, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.runs[sessionID]; old != nil && !old.done {
		return nil, fmt.Errorf("会话 %s 已有宿主任务进程 PID %d", sessionID, old.pid)
	}
	run := &jobActionRun{sessionID: sessionID, turnID: turnID, pid: pid, image: canonical, stdin: stdin, exited: make(chan struct{})}
	b.runs[sessionID] = run
	b.signalRunChangeLocked()
	return run, nil
}

func (b *jobActionBroker) markExited(run *jobActionRun) {
	if run != nil {
		run.exitOnce.Do(func() { close(run.exited) })
	}
}

func (b *jobActionBroker) unregisterRun(run *jobActionRun) {
	b.markExited(run)
	b.mu.Lock()
	if b.runs[run.sessionID] == run {
		run.done = true
		b.signalRunChangeLocked()
	}
	b.mu.Unlock()
}

func (b *jobActionBroker) deliver(line string) bool {
	if strings.HasPrefix(line, steerIPCResponseTag) {
		return b.deliverSteer(line)
	}
	if !strings.HasPrefix(line, jobsOutRespTag) {
		return false
	}
	var resp jobActionResponse
	if json.Unmarshal([]byte(strings.TrimPrefix(line, jobsOutRespTag)), &resp) != nil || resp.RequestID == "" {
		return false
	}
	b.mu.Lock()
	ch := b.pending[resp.RequestID]
	delete(b.pending, resp.RequestID)
	b.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
	return true
}

func (b *jobActionBroker) Output(ctx context.Context, sessionID, id string, cursor, limit int, wait time.Duration) (transport.JobOutput, error) {
	if !validJobActionPart(sessionID) || !validJobActionPart(id) || cursor < 0 || limit <= 0 || limit > transport.MaxJobOutputLimit {
		return transport.JobOutput{}, fmt.Errorf("jobs.output 参数或游标越界")
	}
	run := b.liveRun(sessionID)
	if run == nil {
		return b.readRetained(sessionID, id, cursor, limit)
	}
	resp, err := b.ask(ctx, run, jobActionRequest{SessionID: sessionID, ID: id, Cursor: cursor, Limit: limit, WaitMs: int(wait / time.Millisecond)})
	if err != nil {
		// cmdRun 可能刚退出而 unregister 还没执行；优先读它退出前保留的真值。
		if retained, retainedErr := b.readRetained(sessionID, id, cursor, limit); retainedErr == nil {
			return retained, nil
		} else {
			return transport.JobOutput{}, errors.Join(err, retainedErr)
		}
	}
	if resp.Output == nil {
		return transport.JobOutput{}, fmt.Errorf("任务输出应答缺 output")
	}
	return *resp.Output, nil
}

func (b *jobActionBroker) Stop(ctx context.Context, sessionID, id string) (transport.JobStopResult, error) {
	if !validJobActionPart(sessionID) || !validJobActionPart(id) {
		return transport.JobStopResult{}, fmt.Errorf("jobs.stop 参数含非法 sessionId/id")
	}
	run := b.liveRun(sessionID)
	if run == nil {
		out, err := b.readRetained(sessionID, id, 0, 1)
		if err == nil && out.Done {
			return transport.JobStopResult{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrJobFinished, sessionID, id)
		}
		b.mu.Lock()
		otherRuns := false
		for _, candidate := range b.runs {
			if !candidate.done {
				otherRuns = true
				break
			}
		}
		b.mu.Unlock()
		if otherRuns {
			return transport.JobStopResult{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrCrossSession, sessionID, id)
		}
		if err != nil {
			return transport.JobStopResult{}, err
		}
		return transport.JobStopResult{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrJobFinished, sessionID, id)
	}
	// PID 可被 OS 复用，所以每个破坏性动作前重新打开 PID 并核对完整映像路径。
	if err := verifyProcessImage(run.pid, run.image); err != nil {
		if out, readErr := b.readRetained(sessionID, id, 0, 1); readErr == nil && out.Done {
			return transport.JobStopResult{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrJobFinished, sessionID, id)
		}
		return transport.JobStopResult{}, fmt.Errorf("%w: PID %d 映像复核失败: %v", transport.ErrCrossSession, run.pid, err)
	}
	resp, err := b.ask(ctx, run, jobActionRequest{SessionID: sessionID, ID: id, Limit: 1, Stop: true})
	if err != nil {
		if out, readErr := b.readRetained(sessionID, id, 0, 1); readErr == nil && out.Done {
			return transport.JobStopResult{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrJobFinished, sessionID, id)
		}
		return transport.JobStopResult{}, err
	}
	if resp.Output == nil || !resp.Stopped || !resp.Output.Done {
		return transport.JobStopResult{}, fmt.Errorf("子进程未确认任务停止完成")
	}
	return transport.JobStopResult{ID: id, SessionID: sessionID, Stopped: true, Done: resp.Output.Done, ExitCode: resp.Output.ExitCode}, nil
}

func (b *jobActionBroker) liveRun(sessionID string) *jobActionRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.runs[sessionID]
	if run == nil || run.done {
		return nil
	}
	return run
}

func (b *jobActionBroker) ask(ctx context.Context, run *jobActionRun, req jobActionRequest) (jobActionResponse, error) {
	requestID, err := newJobRequestID()
	if err != nil {
		return jobActionResponse{}, err
	}
	req.RequestID = requestID
	ch := make(chan jobActionResponse, 1)
	b.mu.Lock()
	if run.done {
		b.mu.Unlock()
		return jobActionResponse{}, fmt.Errorf("%w: 会话 %s 的宿主进程已结束", transport.ErrJobFinished, run.sessionID)
	}
	b.pending[requestID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, requestID)
		b.mu.Unlock()
	}()
	raw, _ := json.Marshal(req)
	run.writeMu.Lock()
	_, err = fmt.Fprintf(run.stdin, "%s%s\n", jobsOutReqTag, raw)
	run.writeMu.Unlock()
	if err != nil {
		return jobActionResponse{}, fmt.Errorf("写任务动作到宿主子进程失败: %w", err)
	}
	select {
	case resp := <-ch:
		if resp.SessionID != req.SessionID || resp.ID != req.ID {
			return jobActionResponse{}, fmt.Errorf("%w: 应答会话或任务 id 不匹配", transport.ErrCrossSession)
		}
		if resp.Error != "" {
			switch resp.Code {
			case "not_found":
				return jobActionResponse{}, fmt.Errorf("%w: %s", transport.ErrNoSuchJob, resp.Error)
			case "job_finished":
				return jobActionResponse{}, fmt.Errorf("%w: %s", transport.ErrJobFinished, resp.Error)
			case "cross_session":
				return jobActionResponse{}, fmt.Errorf("%w: %s", transport.ErrCrossSession, resp.Error)
			case "bad_cursor":
				return jobActionResponse{}, fmt.Errorf("%w: %s", transport.ErrBadJobCursor, resp.Error)
			default:
				return jobActionResponse{}, errors.New(resp.Error)
			}
		}
		return resp, nil
	case <-ctx.Done():
		return jobActionResponse{}, fmt.Errorf("等任务动作应答失败: %w", ctx.Err())
	}
}

func (b *jobActionBroker) readRetained(sessionID, id string, cursor, limit int) (transport.JobOutput, error) {
	if !validJobActionPart(sessionID) || !validJobActionPart(id) {
		return transport.JobOutput{}, fmt.Errorf("%w: 非法会话/任务 id", transport.ErrCrossSession)
	}
	path := filepath.Join(b.home, "state", "jobs", sessionID, id+".json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return transport.JobOutput{}, fmt.Errorf("%w: session=%s id=%s", transport.ErrNoSuchJob, sessionID, id)
	}
	if err != nil {
		return transport.JobOutput{}, err
	}
	var out transport.JobOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return transport.JobOutput{}, fmt.Errorf("保留任务输出 %s 损坏: %w", path, err)
	}
	if out.SessionID != sessionID || out.ID != id {
		return transport.JobOutput{}, fmt.Errorf("%w: 保留输出归属不匹配", transport.ErrCrossSession)
	}
	return transport.PageJobOutput(out, cursor, limit), nil
}

func validJobActionPart(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, `/\\:*?"<>|`) && !strings.ContainsAny(v, "\r\n\t")
}

func newJobRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "jobreq-" + hex.EncodeToString(raw[:]), nil
}

func canonicalProcessImage(image string) (string, error) {
	abs, err := filepath.Abs(image)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}
