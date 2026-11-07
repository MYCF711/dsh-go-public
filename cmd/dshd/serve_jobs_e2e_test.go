package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestJobsRealDSHDOutputStopAndOwnedProcessTree(t *testing.T) {
	exe := buildPluginGlueDSHD(t)
	helper := buildJobsIntegrationTreeHelper(t)
	home := t.TempDir()
	createModelWireSession(t, home, t.TempDir(), "session-jobs")
	pidDir := filepath.Join(home, "owned-tree")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{"parent", "child", "grandchild"}
	finish := make(chan struct{})
	var finishOnce atomic.Bool
	defer func() {
		if finishOnce.CompareAndSwap(false, true) {
			close(finish)
		}
		// 红测/断言失败也只清理 PID 文件明确登记且映像匹配的测试 helper。
		cleanupOwnedJobHelpers(pidDir, names, helper)
	}()
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			command := fmt.Sprintf("printf 'A甲🙂乙B'; %s parent %s", quoteJobsIntegrationShell(filepath.ToSlash(helper)), quoteJobsIntegrationShell(filepath.ToSlash(pidDir)))
			args, _ := json.Marshal(map[string]any{
				"command": command, "description": "owned multi-level process tree", "run_in_background": true,
			})
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-job\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", args)
		} else {
			select {
			case <-finish:
			case <-time.After(15 * time.Second):
				t.Error("等待 jobs.stop 后放行模型超时")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"started\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer gateway.Close()
	server := startTaskActionDSHD(t, exe, home, "DSHD_LLM_BASE="+gateway.URL+"/v1")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("dshd stderr:\n%s", server.stderr.String())
		}
	})
	client := connectPluginGlueServer(t, server)
	postPluginGlue(t, client, server.base, "session.prompt", map[string]any{"sessionId": "session-jobs", "text": "start one job"}, http.StatusOK)

	var jobID string
	deadline := time.Now().Add(8 * time.Second)
	for jobID == "" && time.Now().Before(deadline) {
		envelope := postPluginGlue(t, client, server.base, "jobs.list", map[string]any{}, http.StatusOK)
		jobs := envelope["result"].(map[string]any)["jobs"].([]any)
		for _, raw := range jobs {
			job := raw.(map[string]any)
			if job["sessionId"] == "session-jobs" && job["done"] == false {
				jobID, _ = job["id"].(string)
			}
		}
		if jobID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if jobID == "" {
		t.Fatal("真实 bash 后台任务未出现在 jobs.list")
	}
	var gotPage map[string]any
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{"sessionId": "session-jobs", "id": jobID, "cursor": 0, "limit": 5}, http.StatusOK)
		gotPage = page["result"].(map[string]any)
		if gotPage["total"] == float64(5) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotPage["output"] != "A甲🙂乙B" || gotPage["total"] != float64(5) || gotPage["nextCursor"] != float64(5) {
		t.Fatalf("真实 Pager rune output 尚未到齐：%+v", gotPage)
	}
	page := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{"sessionId": "session-jobs", "id": jobID, "cursor": 1, "limit": 2}, http.StatusOK)
	gotPage = page["result"].(map[string]any)
	if gotPage["output"] != "甲🙂" || gotPage["total"] != float64(5) || gotPage["nextCursor"] != float64(3) || gotPage["hasMore"] != true {
		t.Fatalf("真实 CJK/emoji rune 分页：%+v", gotPage)
	}
	postPluginGlue(t, client, server.base, "jobs.stop", map[string]any{"sessionId": "other-session", "id": jobID}, http.StatusForbidden)
	postPluginGlue(t, client, server.base, "jobs.stop", map[string]any{"sessionId": "session-jobs", "id": "unknown"}, http.StatusNotFound)
	for _, name := range names {
		pid := waitOwnedJobHelper(t, pidDir, name, helper)
		t.Logf("OWNED_TREE name=%s pid=%d image=%q", name, pid, helper)
	}
	stopped := postPluginGlue(t, client, server.base, "jobs.stop", map[string]any{"sessionId": "session-jobs", "id": jobID}, http.StatusOK)
	if stopped["result"].(map[string]any)["done"] != true {
		t.Fatalf("stop 未确认 done：%+v", stopped)
	}
	postPluginGlue(t, client, server.base, "jobs.stop", map[string]any{"sessionId": "session-jobs", "id": jobID}, http.StatusConflict)
	assertOwnedJobHelpersGone(t, pidDir, names, helper)
	readback := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{"sessionId": "session-jobs", "id": jobID, "cursor": 3, "limit": 2}, http.StatusOK)
	if readback["result"].(map[string]any)["output"] != "乙B" || readback["result"].(map[string]any)["done"] != true {
		t.Fatalf("停止后 readback：%+v", readback)
	}
	if finishOnce.CompareAndSwap(false, true) {
		close(finish)
	}
	// 子进程退出后仍从当前 home 读回同一 rune 页，重复读取必须完全一致。
	retained := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{"sessionId": "session-jobs", "id": jobID, "cursor": 3, "limit": 2}, http.StatusOK)
	again := postPluginGlue(t, client, server.base, "jobs.output", map[string]any{"sessionId": "session-jobs", "id": jobID, "cursor": 3, "limit": 2}, http.StatusOK)
	if !reflect.DeepEqual(retained, again) || retained["result"].(map[string]any)["output"] != "乙B" || retained["result"].(map[string]any)["done"] != true {
		t.Fatalf("停止/退出后保留输出不幂等：first=%+v again=%+v", retained, again)
	}
}

func buildJobsIntegrationTreeHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	exe := filepath.Join(dir, "jobs-tree-helper")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	const source = `package main
import (
 "bytes"
 "fmt"
 "os"
 "os/exec"
 "path/filepath"
 "strconv"
 "time"
)
func main() {
 if len(os.Args) != 3 { panic("mode dir required") }
 mode, dir := os.Args[1], os.Args[2]
 if mode == "output" {
  payload := append(bytes.Repeat([]byte("x"), 1024*1024), []byte("甲🙂乙")...)
  if _, err := os.Stdout.Write(payload); err != nil { panic(err) }
  return
 }
 next := ""
 if mode == "parent" { next = "child" } else if mode == "child" { next = "grandchild" } else if mode != "grandchild" { panic("bad mode") }
 if next != "" {
  cmd := exec.Command(os.Args[0], next, dir)
  if err := cmd.Start(); err != nil { panic(err) }
  _ = cmd.Process.Release()
 }
 if err := os.WriteFile(filepath.Join(dir, mode+".pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil { panic(err) }
 deadline := time.Now().Add(60*time.Second)
 for time.Now().Before(deadline) {
  _ = os.WriteFile(filepath.Join(dir, mode+".beat"), []byte(fmt.Sprintf("%d %d", os.Getpid(), time.Now().UnixNano())), 0600)
  time.Sleep(20*time.Millisecond)
 }
}
`
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", exe, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建临时多级 helper 失败: %v\n%s", err, out)
	}
	canonical, err := canonicalProcessImage(exe)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func quoteJobsIntegrationShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func startTaskActionDSHD(t *testing.T, exe, home string, extraEnv ...string) *pluginGlueServer {
	t.Helper()
	cmd := exec.Command(exe, "serve", "--home", home, "--addr", "127.0.0.1:0")
	cmd.Env = append(pluginGlueEnvironment(filepath.Join(t.TempDir(), "decoy")), extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	server := &pluginGlueServer{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &server.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		server.waitErr = cmd.Wait()
		close(server.done)
	}()
	t.Cleanup(server.stop)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("ready: %v stderr=%s", err, server.stderr.String())
	}
	if !strings.HasPrefix(line, "ready ") {
		t.Fatalf("ready line=%q stderr=%s", line, server.stderr.String())
	}
	var ready readyLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "ready "))), &ready); err != nil {
		t.Fatal(err)
	}
	server.base, server.token = ready.URL, ready.Token
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return server
}

func readOwnedJobHelperPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("非法 helper PID %q: %w", raw, err)
	}
	return pid, nil
}

func waitOwnedJobHelper(t *testing.T, dir, name, image string) int {
	t.Helper()
	path := filepath.Join(dir, name+".pid")
	deadline := time.Now().Add(5 * time.Second)
	for {
		pid, err := readOwnedJobHelperPID(path)
		var imageErr error
		if err == nil {
			imageErr = verifyProcessImage(pid, image)
			if imageErr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待归属 helper %s PID/映像超时: pid=%d readErr=%v imageErr=%v", name, pid, err, imageErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cleanupOwnedJobHelpers(dir string, names []string, image string) {
	for _, name := range names {
		pid, err := readOwnedJobHelperPID(filepath.Join(dir, name+".pid"))
		if err != nil || verifyProcessImage(pid, image) != nil {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill() // 仅测试 PID 文件登记且刚核对映像的归属 helper
		}
	}
}

func assertOwnedJobHelpersGone(t *testing.T, dir string, names []string, image string) {
	t.Helper()
	for _, name := range names {
		pid, err := readOwnedJobHelperPID(filepath.Join(dir, name+".pid"))
		if err != nil {
			t.Fatalf("读取归属 helper %s PID 失败: %v", name, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for verifyProcessImage(pid, image) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if verifyProcessImage(pid, image) == nil {
			t.Fatalf("jobs.stop 后归属 helper %s PID %d 仍存活：没有停完整进程树", name, pid)
		}
		t.Logf("OWNED_TREE_EXIT name=%s pid=%d", name, pid)
	}
}
