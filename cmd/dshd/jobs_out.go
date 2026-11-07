package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

var jobPersistMu sync.Mutex
var errJobPagerSnapshotChanged = errors.New("Pager 保留窗口变化")

const (
	// W69 每个任务最多保留 1 MiB。适配器拒绝为其他 JobPager 实现无界分配内存。
	maxJobPagerSnapshotBytes = 1024 * 1024
	maxJobPagerSnapshotTries = 4
)

// serveJobsOut 是子进程侧的有界 jobs.output/jobs.stop 通道。它只接受本 cmdRun
// 的 sessionId；父进程先验 PID/映像，子进程再按本 Scope 内的 job id 执行。
func serveJobsOut(root *kernel.Scope, home, sessionID string) {
	job, err := kernel.Inject(root, spi.JobKey)
	if err != nil || job == nil {
		return
	}
	for line := range jobsOutCh {
		var req jobActionRequest
		if json.Unmarshal([]byte(strings.TrimPrefix(line, jobsOutReqTag)), &req) != nil || req.ID == "" {
			continue
		}
		// 兼容旧父进程只有 id 的 output 请求；新动作必须有 requestId/sessionId。
		if req.RequestID == "" {
			serveLegacyJobOutput(job, req.ID)
			continue
		}
		resp := jobActionResponse{RequestID: req.RequestID, SessionID: req.SessionID, ID: req.ID}
		if req.SessionID != sessionID || !validJobActionPart(req.SessionID) || !validJobActionPart(req.ID) {
			resp.Code, resp.Error = "cross_session", "任务请求不属于当前 cmdRun 会话"
			writeJobActionResponse(resp)
			continue
		}
		wait := time.Duration(req.WaitMs) * time.Millisecond
		if wait < 0 {
			wait = 0
		}
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		out, oerr := readJobOutputSnapshot(job, sessionID, req.ID, wait)
		if oerr != nil {
			resp.Code, resp.Error = jobActionError(oerr, false)
			writeJobActionResponse(resp)
			continue
		}
		if req.Stop {
			if out.Done {
				resp.Code, resp.Error = "job_finished", fmt.Sprintf("任务 %s 已经结束，不能重复停止", req.ID)
				writeJobActionResponse(resp)
				continue
			}
			var stopErr error
			if controller, ok := job.(spi.JobController); ok {
				// 新执行层只在核对实例、PID 创建代次和映像后停止整棵归属进程树。
				stopErr = controller.Stop(req.ID)
			} else {
				// 老模块没有 Controller 时才保留旧 Kill 兼容路径。
				stopErr = job.Kill(req.ID)
			}
			if stopErr != nil {
				resp.Code, resp.Error = jobActionError(stopErr, true)
				writeJobActionResponse(resp)
				continue
			}
			out, oerr = readJobOutputSnapshot(job, sessionID, req.ID, 2*time.Second)
			if oerr != nil {
				resp.Code, resp.Error = jobActionError(oerr, false)
				writeJobActionResponse(resp)
				continue
			}
			resp.Stopped = out.Done
		}
		if req.Stop {
			if err := persistTransportJobOutput(home, sessionID, out); err != nil {
				resp.Error = fmt.Sprintf("任务已停止，但保留输出写盘失败: %v", err)
				resp.Stopped = false
				writeJobActionResponse(resp)
				continue
			}
		}
		if req.Cursor > len([]rune(out.Output)) {
			resp.Code, resp.Error = "bad_cursor", fmt.Sprintf("cursor=%d 超过当前输出长度 %d", req.Cursor, len([]rune(out.Output)))
			writeJobActionResponse(resp)
			continue
		}
		resp.Output = scopedTransportJobOutput(out, req.Cursor, req.Limit)
		writeJobActionResponse(resp)
	}
}

func serveLegacyJobOutput(job spi.Job, id string) {
	resp := map[string]any{"id": id}
	out, err := job.Output(id, 0)
	if err != nil {
		resp["error"] = err.Error()
	} else {
		resp["output"], resp["done"], resp["exit"] = out.Output, out.Done, out.ExitCode
		if out.Cwd != "" {
			resp["cwd"] = out.Cwd
		}
	}
	raw, err := json.Marshal(resp)
	if err == nil {
		fmt.Fprintf(os.Stderr, "%s%s\n", jobsOutRespTag, raw)
	}
}

// readJobOutputSnapshot 优先消费 JobPager，但绝不让 SPI 字节游标与 HTTP rune
// 游标互传。Pager 只用于取当前保留窗口；窗口拼成完整字节快照后才按 rune 解码。
// 没有 Pager 的旧模块仍走原 Output 兼容路径。
func readJobOutputSnapshot(job spi.Job, sessionID, id string, wait time.Duration) (transport.JobOutput, error) {
	if pager, ok := job.(spi.JobPager); ok {
		return readJobPagerSnapshot(pager, sessionID, id, wait)
	}
	out, err := job.Output(id, wait)
	if err != nil {
		return transport.JobOutput{}, err
	}
	return transport.JobOutput{ID: out.ID, SessionID: sessionID, Output: out.Output, Done: out.Done, ExitCode: out.ExitCode, Cwd: out.Cwd, Total: len([]rune(out.Output))}, nil
}

func readJobPagerSnapshot(pager spi.JobPager, sessionID, id string, wait time.Duration) (transport.JobOutput, error) {
	// 旧 Output(wait) 会等 ended；OutputPage 只在“当前游标有新输出或 ended”时等待。
	// 先移到当前 total 等待，才能保持 HTTP waitMs 与 stop 后 readback 的旧语义。
	if wait > 0 {
		if err := waitJobPagerDone(pager, id, wait); err != nil {
			return transport.JobOutput{}, err
		}
	}
	var lastErr error
	for attempt := 0; attempt < maxJobPagerSnapshotTries; attempt++ {
		out, err := readJobPagerSnapshotOnce(pager, sessionID, id, 0)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, errJobPagerSnapshotChanged) {
			return transport.JobOutput{}, err
		}
		lastErr = err
	}
	return transport.JobOutput{}, fmt.Errorf("Pager 保留窗口在读取期间持续变化: %w", lastErr)
}

func waitJobPagerDone(pager spi.JobPager, id string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		probe, err := pager.OutputPage(id, 0, 1, 0)
		if err != nil {
			return err
		}
		if probe.Done {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		started := time.Now()
		page, err := pager.OutputPage(id, probe.Total, 1, remaining)
		if err != nil {
			return err
		}
		if page.Done {
			return nil
		}
		// 非标准 Pager 若不兑现 wait，也不能让适配器忙等占满 CPU。
		if time.Since(started) < time.Millisecond {
			time.Sleep(time.Millisecond)
		}
	}
}

func readJobPagerSnapshotOnce(pager spi.JobPager, sessionID, id string, wait time.Duration) (transport.JobOutput, error) {
	first, err := pager.OutputPage(id, 0, 1, wait)
	if err != nil {
		return transport.JobOutput{}, err
	}
	base, total := first.Offset, first.Total
	if base < 0 || total < base {
		return transport.JobOutput{}, fmt.Errorf("Pager 返回非法保留范围 offset=%d total=%d", base, total)
	}
	retained := total - base
	if retained > maxJobPagerSnapshotBytes {
		return transport.JobOutput{}, fmt.Errorf("Pager 保留窗口超过 1 MiB 适配上限: retained=%d", retained)
	}
	data := make([]byte, 0, int(retained))
	meta := first
	for cursor := base; cursor < total; {
		limit := spi.JobDefaultPageLimit
		if remaining := total - cursor; remaining < int64(limit) {
			limit = int(remaining)
		}
		page, err := pager.OutputPage(id, cursor, limit, 0)
		if err != nil {
			return transport.JobOutput{}, err
		}
		if page.Total != total || page.Offset != cursor {
			return transport.JobOutput{}, fmt.Errorf("%w: cursor=%d offset=%d total=%d/%d", errJobPagerSnapshotChanged, cursor, page.Offset, page.Total, total)
		}
		n := int64(len(page.Output))
		if n <= 0 || n > total-cursor || page.NextCursor != cursor+n {
			return transport.JobOutput{}, fmt.Errorf("%w: cursor=%d next=%d bytes=%d", errJobPagerSnapshotChanged, cursor, page.NextCursor, n)
		}
		data = append(data, page.Output...)
		cursor = page.NextCursor
		meta = page
	}
	// 再读首尾，确认组装期间没有淘汰已读前缀或追加改变 total；变化就有限重试。
	head, err := pager.OutputPage(id, base, 1, 0)
	if err != nil {
		return transport.JobOutput{}, err
	}
	tail, err := pager.OutputPage(id, total, 1, 0)
	if err != nil {
		return transport.JobOutput{}, err
	}
	if head.Total != total || head.Offset != base || tail.Total != total || tail.Offset != total {
		return transport.JobOutput{}, fmt.Errorf("%w: base=%d/%d total=%d/%d", errJobPagerSnapshotChanged, head.Offset, base, tail.Total, total)
	}
	meta.Done, meta.ExitCode, meta.Cwd = tail.Done, tail.ExitCode, tail.Cwd
	// 与旧 Output 一致，HTTP 投影仍裁掉末尾换行；Pager 原始字节语义不泄漏到 HTTP。
	text := strings.TrimRight(string(data), "\n")
	return transport.JobOutput{
		ID: id, SessionID: sessionID, Output: text, Done: meta.Done, ExitCode: meta.ExitCode, Cwd: meta.Cwd,
		Total: len([]rune(text)), Truncated: first.Truncated || base > 0,
	}, nil
}

func jobActionError(err error, stopping bool) (string, string) {
	code := ""
	switch {
	case errors.Is(err, spi.ErrJobWrongInstance):
		code = "cross_session"
	case errors.Is(err, spi.ErrJobFinished), errors.Is(err, spi.ErrJobProcessExited):
		code = "job_finished"
	case errors.Is(err, spi.ErrJobInvalidCursor):
		code = "bad_cursor"
	case errors.Is(err, spi.ErrJobNotFound):
		code = "not_found"
	case !stopping:
		// 旧 Job.Output 没有类型化错误，W67 一直把未知 id 映射成 not_found。
		code = "not_found"
	}
	return code, err.Error()
}

func scopedJobOutput(sessionID string, out spi.JobOutput, cursor, limit int) *transport.JobOutput {
	page := transport.JobOutput{ID: out.ID, SessionID: sessionID, Output: out.Output, Done: out.Done, ExitCode: out.ExitCode, Cwd: out.Cwd, Total: len([]rune(out.Output))}
	return scopedTransportJobOutput(page, cursor, limit)
}

func scopedTransportJobOutput(out transport.JobOutput, cursor, limit int) *transport.JobOutput {
	if limit <= 0 {
		limit = transport.DefaultJobOutputLimit
	}
	if limit > transport.MaxJobOutputLimit {
		limit = transport.MaxJobOutputLimit
	}
	page := transport.PageJobOutput(out, cursor, limit)
	return &page
}

func writeJobActionResponse(resp jobActionResponse) {
	raw, err := json.Marshal(resp)
	if err == nil {
		fmt.Fprintf(os.Stderr, "%s%s\n", jobsOutRespTag, raw)
	}
}

func persistJobOutputs(job spi.Job, home, sessionID string) error {
	if job == nil {
		return fmt.Errorf("保留任务输出失败: jobs 服务为 nil")
	}
	var errs []error
	for _, info := range job.List() {
		out, err := readJobOutputSnapshot(job, sessionID, info.ID, 2*time.Second)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取任务 %s 最终输出: %w", info.ID, err))
			continue
		}
		if err := persistTransportJobOutput(home, sessionID, out); err != nil {
			errs = append(errs, fmt.Errorf("任务 %s 最终输出写盘: %w", info.ID, err))
		}
	}
	return errors.Join(errs...)
}

func persistJobOutput(home, sessionID string, out spi.JobOutput) error {
	doc := transport.JobOutput{ID: out.ID, SessionID: sessionID, Output: out.Output, Done: out.Done, ExitCode: out.ExitCode, Cwd: out.Cwd, Total: len([]rune(out.Output))}
	return persistTransportJobOutput(home, sessionID, doc)
}

func persistTransportJobOutput(home, sessionID string, out transport.JobOutput) error {
	if !validJobActionPart(sessionID) || !validJobActionPart(out.ID) {
		return fmt.Errorf("任务输出归属 id 非法")
	}
	jobPersistMu.Lock()
	defer jobPersistMu.Unlock()
	dir := filepath.Join(home, "state", "jobs", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	doc := out
	doc.SessionID = sessionID
	doc.Cursor, doc.NextCursor, doc.HasMore = 0, 0, false
	doc.Total = len([]rune(out.Output))
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := filepath.Join(dir, out.ID+".json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	return nil
}
