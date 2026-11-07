package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

func TestJobActionBrokerRunningEndedUnknownCrossSessionAndSecondStop(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	homeA := t.TempDir()
	homeB := t.TempDir()
	cmd := exec.Command(exe, "-test.run=^TestJobActionHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_JOB_ACTION_HELPER=1", "JOB_ACTION_HOME="+homeA)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	var waitErr error
	defer func() {
		if !waited && cmd.Process != nil {
			_ = cmd.Process.Kill() // 仅此测试启动并持有 exec.Cmd 的 helper PID
			_ = cmd.Wait()
		}
		if waitErr != nil {
			t.Fatalf("helper: %v stderr=%s", waitErr, stderr.String())
		}
	}()
	broker := newJobActionBroker(homeA)
	run, err := broker.registerRun("session-a", cmd.Process.Pid, exe, stdin)
	if err != nil {
		t.Fatal(err)
	}
	linesDone := make(chan struct{})
	go func() {
		defer close(linesDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if !broker.deliver(scanner.Text()) {
				t.Errorf("未识别的 helper 应答行：%s", scanner.Text())
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := broker.Output(ctx, "session-a", "job-running", 2, 2, 0)
	if err != nil || page.Output != "cd" || page.Cursor != 2 || page.NextCursor != 4 || !page.HasMore || page.Total != 6 || page.Done {
		t.Fatalf("第一页：out=%+v err=%v", page, err)
	}
	page, err = broker.Output(ctx, "session-a", "job-running", 4, 2, 0)
	if err != nil || page.Output != "ef" || page.HasMore {
		t.Fatalf("第二页：out=%+v err=%v", page, err)
	}
	if _, err := broker.Output(ctx, "session-a", "unknown", 0, 2, 0); !errors.Is(err, transport.ErrNoSuchJob) {
		t.Fatalf("未知 id：%v", err)
	}
	if _, err := broker.Stop(ctx, "session-b", "job-running"); !errors.Is(err, transport.ErrCrossSession) {
		t.Fatalf("跨会话 stop 必须拒绝：%v", err)
	}
	page, err = broker.Output(ctx, "session-a", "job-running", 0, 1, 0)
	if err != nil || page.Done {
		t.Fatalf("跨会话拒绝后任务被误停：out=%+v err=%v", page, err)
	}
	if _, err := broker.Stop(ctx, "session-a", "job-done"); !errors.Is(err, transport.ErrJobFinished) {
		t.Fatalf("已结束任务 stop：%v", err)
	}
	stopped, err := broker.Stop(ctx, "session-a", "job-running")
	if err != nil || !stopped.Stopped || !stopped.Done {
		t.Fatalf("首次 stop：result=%+v err=%v", stopped, err)
	}
	if _, err := broker.Stop(ctx, "session-a", "job-running"); !errors.Is(err, transport.ErrJobFinished) {
		t.Fatalf("第二次 stop 必须 job_finished：%v", err)
	}
	page, err = broker.Output(ctx, "session-a", "job-running", 5, 2, 0)
	if err != nil || page.Output != "f" || !page.Done {
		t.Fatalf("停止后输出：out=%+v err=%v", page, err)
	}

	broker.unregisterRun(run)
	_ = stdin.Close()
	waitErr = cmd.Wait()
	waited = true
	<-linesDone
	page, err = broker.Output(ctx, "session-a", "job-running", 4, 2, 0)
	if err != nil || page.Output != "ef" || !page.Done {
		t.Fatalf("进程退出后保留输出：out=%+v err=%v", page, err)
	}
	if _, err := broker.Stop(ctx, "session-a", "job-running"); !errors.Is(err, transport.ErrJobFinished) {
		t.Fatalf("进程退出后的第二次 stop：%v", err)
	}
	otherHome := newJobActionBroker(homeB)
	if _, err := otherHome.Output(ctx, "session-a", "job-running", 0, 2, 0); !errors.Is(err, transport.ErrNoSuchJob) {
		t.Fatalf("B home 不应读到 A 的输出：%v", err)
	}
	if err := verifyProcessImage(os.Getpid(), filepath.Join(t.TempDir(), "wrong-image.exe")); err == nil {
		t.Fatal("错误映像归属必须验证失败")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestJobActionHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_JOB_ACTION_HELPER") != "1" {
		return
	}
	home := os.Getenv("JOB_ACTION_HOME")
	jobs := map[string]spi.JobOutput{
		"job-running": {ID: "job-running", Output: "abcdef", Done: false, ExitCode: 0, Cwd: home},
		"job-done":    {ID: "job-done", Output: "done", Done: true, ExitCode: 0, Cwd: home},
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, jobsOutReqTag) {
			continue
		}
		var req jobActionRequest
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, jobsOutReqTag)), &req); err != nil {
			os.Exit(91)
		}
		resp := jobActionResponse{RequestID: req.RequestID, SessionID: req.SessionID, ID: req.ID}
		if req.SessionID != "session-a" {
			resp.Code, resp.Error = "cross_session", "wrong session"
			writeHelperJobResponse(resp)
			continue
		}
		job, ok := jobs[req.ID]
		if !ok {
			resp.Code, resp.Error = "not_found", "unknown job"
			writeHelperJobResponse(resp)
			continue
		}
		if req.Stop {
			if job.Done {
				resp.Code, resp.Error = "job_finished", "already done"
				writeHelperJobResponse(resp)
				continue
			}
			job.Done, job.ExitCode = true, -1
			jobs[req.ID] = job
			resp.Stopped = true
			if err := persistJobOutput(home, req.SessionID, job); err != nil {
				resp.Error = err.Error()
				resp.Stopped = false
			}
		}
		resp.Output = scopedJobOutput(req.SessionID, job, req.Cursor, req.Limit)
		writeHelperJobResponse(resp)
		if req.Stop && resp.Stopped {
			// 模拟 cmdRun 在 stop 后正常退出；文件已在退出前持久化。
			// os.Exit 避免 testing 框架再向协议 stdout 打一行 PASS。
			os.Exit(0)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
}

func writeHelperJobResponse(resp jobActionResponse) {
	raw, _ := json.Marshal(resp)
	fmt.Printf("%s%s\n", jobsOutRespTag, raw)
}
