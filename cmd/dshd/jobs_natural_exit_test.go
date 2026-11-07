package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/internal/spi"
)

// TestJobsRealDSHDNaturalExitReadback 不经过 jobs.stop：先等真实 bash job
// 自然结束，再让 cmdRun 退出，最后由一个没有 live run 的新 serve 经 HTTP 读回。
func TestJobsRealDSHDNaturalExitReadback(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	tests := []struct {
		name    string
		command string
		output  string
		exit    int
	}{
		{name: "success", command: "printf 'NATURAL_SUCCESS_A甲🙂乙B'; sleep 0.2", output: "NATURAL_SUCCESS_A甲🙂乙B", exit: 0},
		{name: "failure", command: "printf 'NATURAL_FAILURE_失敗🙂乙B'; sleep 0.2; exit 7", output: "NATURAL_FAILURE_失敗🙂乙B", exit: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			sessionID := "session-natural-" + tt.name
			createModelWireSession(t, home, t.TempDir(), sessionID)
			finish := make(chan struct{})
			var finishOnce atomic.Bool
			release := func() {
				if finishOnce.CompareAndSwap(false, true) {
					close(finish)
				}
			}
			defer release()
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					args, _ := json.Marshal(map[string]any{
						"command": tt.command, "description": "natural exit readback", "run_in_background": true,
					})
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-natural-job\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", args)
				} else {
					select {
					case <-finish:
					case <-time.After(15 * time.Second):
						t.Error("等待自然 job 结束后放行模型超时")
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"natural job finished\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer gateway.Close()

			cmd := exec.Command(exe, "run", "--home", home, "--session", sessionID,
				"--prompt", "start one natural background job", "--policy", "allow", "--max-steps", "4",
				"--base", gateway.URL+"/v1")
			cmd.Env = append(os.Environ(), "DSHD_JOBS_STDIO=1")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stdout, childStderr syncBuffer
			cmd.Stdout = &stdout
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			broker := newJobActionBroker(home)
			run, err := broker.registerRun(sessionID, cmd.Process.Pid, exe, stdin)
			if err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal(err)
			}
			jobIDs := make(chan string, 8)
			scanDone := make(chan struct{})
			go func() {
				defer close(scanDone)
				scanner := bufio.NewScanner(stderr)
				for scanner.Scan() {
					line := scanner.Text()
					fmt.Fprintf(&childStderr, "%s\n", line)
					if jobs, ok := parseJobsLine(line); ok {
						for _, job := range jobs {
							select {
							case jobIDs <- job.ID:
							default:
							}
						}
					}
					broker.deliver(line)
				}
			}()
			waitCh := make(chan error, 1)
			go func() { waitCh <- cmd.Wait() }()
			waited, scanned := false, false
			defer func() {
				release()
				if !waited {
					_ = cmd.Process.Kill() // 只回收本测试启动且仍持有 exec.Cmd 的 cmdRun PID
					<-waitCh
				}
				broker.unregisterRun(run)
				_ = stdin.Close()
				if !scanned {
					<-scanDone
				}
				if t.Failed() {
					t.Logf("cmdRun stdout:\n%s\ncmdRun stderr:\n%s", stdout.String(), childStderr.String())
				}
			}()

			var jobID string
			select {
			case jobID = <-jobIDs:
			case err := <-waitCh:
				waited = true
				t.Fatalf("cmdRun 在登记 job 前退出: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("等待真实自然 job id 超时")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var liveOutput string
			for {
				out, outputErr := broker.Output(ctx, sessionID, jobID, 0, 256, 25*time.Millisecond)
				if outputErr == nil && out.Done {
					if out.Output != tt.output || out.ExitCode != tt.exit {
						t.Fatalf("自然结束真值不符合命令事实: out=%+v wantOutput=%q wantExit=%d", out, tt.output, tt.exit)
					}
					liveOutput = out.Output
					t.Logf("NATURAL_JOB session=%s id=%s done=%v exit=%d output=%q", sessionID, jobID, out.Done, out.ExitCode, out.Output)
					break
				}
				if ctx.Err() != nil {
					t.Fatalf("等待自然 job 结束: out=%+v err=%v ctx=%v", out, outputErr, ctx.Err())
				}
			}
			if liveOutput != tt.output {
				t.Fatalf("自然输出被改写: got=%q want=%q", liveOutput, tt.output)
			}
			release()
			if err := <-waitCh; err != nil {
				waited = true
				t.Fatalf("cmdRun 自然收尾失败: %v", err)
			}
			waited = true
			broker.unregisterRun(run)
			_ = stdin.Close()
			<-scanDone
			scanned = true

			// 新 serve 没有这轮 cmdRun 的 live run；成功只能来自已落盘的最终快照。
			server := startTaskActionDSHD(t, exe, home)
			client := connectPluginGlueServer(t, server)
			retained := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{
				"sessionId": sessionID, "id": jobID, "cursor": 0, "limit": 256,
			}, http.StatusOK)
			again := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{
				"sessionId": sessionID, "id": jobID, "cursor": 0, "limit": 256,
			}, http.StatusOK)
			if !reflect.DeepEqual(retained, again) {
				t.Fatalf("自然结束后重复读取不幂等: first=%+v again=%+v", retained, again)
			}
			result := retained["result"].(map[string]any)
			runes := []rune(tt.output)
			if result["output"] != tt.output || result["done"] != true || result["exit_code"] != float64(tt.exit) ||
				result["total"] != float64(len(runes)) || result["hasMore"] != false {
				t.Fatalf("自然结束后 HTTP 保留快照: %+v", result)
			}
			page := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{
				"sessionId": sessionID, "id": jobID, "cursor": 1, "limit": 2,
			}, http.StatusOK)["result"].(map[string]any)
			if page["output"] != string(runes[1:3]) || page["done"] != true || page["exit_code"] != float64(tt.exit) {
				t.Fatalf("自然结束后保留 rune 页: %+v", page)
			}
			retainedPath := filepath.Join(home, "state", "jobs", sessionID, jobID+".json")
			if _, err := os.Stat(retainedPath); err != nil {
				t.Fatalf("最终快照文件不可读: %v", err)
			}
		})
	}
}

func TestPersistJobOutputsFailLoud(t *testing.T) {
	t.Run("nil-service", func(t *testing.T) {
		if err := persistJobOutputs(nil, t.TempDir(), "session-persist"); err == nil || !strings.Contains(err.Error(), "nil") {
			t.Fatalf("nil jobs 服务必须返回错误: %v", err)
		}
	})
	t.Run("read", func(t *testing.T) {
		job := &integrationJob{out: spi.JobOutput{ID: "job-read-error", Done: true}, outputErr: fmt.Errorf("read exploded")}
		if err := persistJobOutputs(job, t.TempDir(), "session-persist"); err == nil ||
			!strings.Contains(err.Error(), "job-read-error") || !strings.Contains(err.Error(), "read exploded") {
			t.Fatalf("最终输出读取错误必须带任务 id 返回: %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "home-is-a-file")
		if err := os.WriteFile(home, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		job := &integrationJob{out: spi.JobOutput{ID: "job-write-error", Output: "raw failure", Done: true, ExitCode: 9}}
		if err := persistJobOutputs(job, home, "session-persist"); err == nil || !strings.Contains(err.Error(), "job-write-error") {
			t.Fatalf("最终输出写盘错误必须带任务 id 返回: %v", err)
		}
	})
}
