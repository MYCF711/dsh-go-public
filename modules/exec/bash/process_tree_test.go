package bash

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/internal/spi"
)

func startTestJob(t *testing.T, svc *Service, command string) string {
	t.Helper()
	res, err := svc.Run(command, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("启动后台任务失败: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("命令没有转入后台: %+v", res)
	}
	return res.JobID
}

func TestOutputPageAfterDoneAndBoundaries(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `printf 0123456789abcde; sleep 0.15`)
	legacy, err := svc.Output(id, 3*time.Second)
	if err != nil || !legacy.Done {
		t.Fatalf("等待结束失败: %+v, %v", legacy, err)
	}

	var want = []struct {
		cursor int64
		text   string
		next   int64
	}{{0, "0123", 4}, {4, "4567", 8}, {8, "89ab", 12}, {12, "cde", 15}, {15, "", 15}}
	for _, tc := range want {
		page, err := svc.OutputPage(id, tc.cursor, 4, 0)
		if err != nil {
			t.Fatalf("cursor=%d: %v", tc.cursor, err)
		}
		if page.Output != tc.text || page.Offset != tc.cursor || page.NextCursor != tc.next || page.Total != 15 || !page.Done || page.ExitCode != 0 || page.Truncated {
			t.Fatalf("cursor=%d 页错误: %+v", tc.cursor, page)
		}
		again, err := svc.OutputPage(id, tc.cursor, 4, 0)
		if err != nil || again != page {
			t.Fatalf("结束后同游标必须幂等: first=%+v again=%+v err=%v", page, again, err)
		}
	}
	if _, err := svc.OutputPage(id, -1, 4, 0); !errors.Is(err, spi.ErrJobInvalidCursor) {
		t.Fatalf("负游标必须拒绝: %v", err)
	}
	if _, err := svc.OutputPage(id, 16, 4, 0); !errors.Is(err, spi.ErrJobInvalidCursor) {
		t.Fatalf("越过 total 的游标必须拒绝: %v", err)
	}
	if _, err := svc.OutputPage(id, 0, -1, 0); err == nil {
		t.Fatal("负 limit 必须拒绝")
	}
	page, err := svc.OutputPage(id, 0, spi.JobMaxPageLimit+1, 0)
	if err != nil || len(page.Output) > spi.JobMaxPageLimit {
		t.Fatalf("limit 必须有硬上限: %+v, %v", page, err)
	}
}

func TestOutputPageWaitsForOutputAndDone(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `sleep 0.1; printf ok`)
	start := time.Now()
	page, err := svc.OutputPage(id, 0, 4, 2*time.Second)
	if err != nil || page.Output != "ok" || page.NextCursor != 2 {
		t.Fatalf("等待输出失败: %+v, %v", page, err)
	}
	// 新输出和结束哨兵可以分两次到；第二页必须一直等到真实 done。
	ended, err := svc.OutputPage(id, page.NextCursor, 4, 2*time.Second)
	if err != nil || !ended.Done || ended.ExitCode != 0 || ended.Total != 2 || ended.NextCursor != 2 {
		t.Fatalf("等待 done 失败: %+v, %v", ended, err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("结束后不应继续等满 wait: %s", time.Since(start))
	}
}

func TestStopUnknownCrossInstanceAndDuplicate(t *testing.T) {
	requireBash(t)
	a := newSvc(t)
	b := newSvc(t)
	idA := startTestJob(t, a, `sleep 30`)
	idB := startTestJob(t, b, `sleep 30`)
	if idA == idB {
		t.Fatalf("不同 Service 不能分配相同任务 id: %q", idA)
	}
	if err := b.Stop(idA); !errors.Is(err, spi.ErrJobWrongInstance) {
		t.Fatalf("跨实例停止必须拒绝: %v", err)
	}
	if page, err := b.OutputPage(idB, 0, 1, 0); err != nil || page.Done {
		t.Fatalf("跨实例拒绝不得误杀本实例任务: %+v, %v", page, err)
	}
	if err := a.Stop("missing"); !errors.Is(err, spi.ErrJobNotFound) {
		t.Fatalf("未知 id 必须返回 not-found: %v", err)
	}
	if err := a.Stop(idA); err != nil {
		t.Fatalf("第一次停止失败: %v", err)
	}
	if err := a.Stop(idA); !errors.Is(err, spi.ErrJobFinished) {
		t.Fatalf("重复停止必须明确拒绝: %v", err)
	}
	page, err := a.OutputPage(idA, 0, 1, 2*time.Second)
	if err != nil || !page.Done {
		t.Fatalf("停止后应可读 ended 状态: %+v, %v", page, err)
	}

	doneID := startTestJob(t, a, `printf done; sleep 0.1`)
	if out, err := a.Output(doneID, 2*time.Second); err != nil || !out.Done {
		t.Fatalf("等待自然结束: %+v, %v", out, err)
	}
	if err := a.Stop(doneID); !errors.Is(err, spi.ErrJobFinished) {
		t.Fatalf("自然结束后停止必须拒绝: %v", err)
	}
}

func TestStopRejectsPIDReuse(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `read _`)
	j, err := svc.job(id)
	if err != nil {
		t.Fatal(err)
	}
	control := j.sess.control
	control.mu.Lock()
	original := control.identity
	control.identity.CreationID = original.CreationID + "-stale"
	control.mu.Unlock()
	t.Cleanup(func() {
		control.mu.Lock()
		control.identity = original
		control.mu.Unlock()
	})

	if err := svc.Stop(id); !errors.Is(err, spi.ErrJobProcessReused) {
		t.Fatalf("创建代次不匹配必须按 PID 复用拒绝: %v", err)
	}
	if page, err := svc.OutputPage(id, 0, 1, 0); err != nil || page.Done {
		t.Fatalf("身份不匹配不得杀掉真实进程: %+v, %v", page, err)
	}
	control.mu.Lock()
	control.identity = original
	control.mu.Unlock()
	if err := svc.Stop(id); err != nil {
		t.Fatalf("恢复正确代次后应能安全停止: %v", err)
	}
}

func TestProcessControlRejectsExitedRoot(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `read _`)
	j, err := svc.job(id)
	if err != nil {
		t.Fatal(err)
	}
	identity := j.sess.control.metadata()
	current, err := queryProcessIdentity(identity.PID)
	if err != nil || validateProcessIdentity(identity, current) != nil {
		t.Fatalf("杀测试 root 前必须核对归属: %+v, %v", current, err)
	}
	// 仅测试进程直接杀自己刚核对的 bash root，制造真实已退出分支。
	if err := j.sess.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-j.sess.waitDone
	if err := j.sess.control.stop(); !errors.Is(err, spi.ErrJobProcessExited) {
		t.Fatalf("已退出 root 必须拒绝: %v", err)
	}
}

func TestJobMetadataIsReal(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `sleep 30`)
	meta, err := svc.Metadata(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != id || meta.OwnerID == "" || !strings.Contains(id, meta.OwnerID) || meta.RootPID <= 0 || meta.RootImage == "" || meta.CreationID == "" {
		t.Fatalf("元数据缺真实归属: %+v", meta)
	}
	if !strings.Contains(strings.ToLower(meta.RootImage), "bash") {
		t.Fatalf("root 映像不是本次启动的 bash: %+v", meta)
	}
	if runtime.GOOS == "windows" && meta.ProcessTree != "windows-job-object" {
		t.Fatalf("Windows 必须记录 Job Object: %+v", meta)
	}
	if runtime.GOOS != "windows" && (meta.ProcessTree != "unix-process-group" || meta.ProcessGroupID != meta.RootPID) {
		t.Fatalf("Unix 必须记录独立 process group: %+v", meta)
	}
	list := svc.List()
	if len(list) != 1 || list[0].RootPID != meta.RootPID || list[0].CreationID != meta.CreationID || list[0].OwnerID != meta.OwnerID {
		t.Fatalf("List 与 Metadata 不能分叉: list=%+v meta=%+v", list, meta)
	}
}

func TestConcurrentOutputMetadataAndStop(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	id := startTestJob(t, svc, `i=0; while [ "$i" -lt 1000 ]; do printf x; i=$((i+1)); sleep 0.005; done`)
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var cursor int64
			for {
				page, err := svc.OutputPage(id, cursor, 32, 0)
				if err != nil {
					errCh <- err
					return
				}
				if page.NextCursor < cursor {
					errCh <- fmt.Errorf("游标倒退: %+v", page)
					return
				}
				cursor = page.NextCursor
				if _, err := svc.Metadata(id); err != nil {
					errCh <- err
					return
				}
				if page.Done {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	if err := svc.Stop(id); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestTaskOutputRetentionIsBounded(t *testing.T) {
	task := newTask(1)
	payload := []byte(strings.Repeat("x", maxRetainedJobOutput+123))
	task.append(payload)
	task.finish(0, "")
	base, total, truncated := task.retention()
	if total != int64(len(payload)) || base != 123 || !truncated || task.bufferedBytes() != maxRetainedJobOutput {
		t.Fatalf("保留窗口错误: base=%d total=%d truncated=%v buffered=%d", base, total, truncated, task.bufferedBytes())
	}
	svc := NewService(t.TempDir())
	id, err := svc.register("bounded", nil, task)
	if err != nil {
		t.Fatal(err)
	}
	page, err := svc.OutputPage(id, 0, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Offset != 123 || page.NextCursor != 128 || page.Total != total || !page.Truncated || !page.Done || page.Output != "xxxxx" {
		t.Fatalf("过期游标必须明确跳到保留起点: %+v", page)
	}
}

func buildTreeHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	exe := filepath.Join(dir, "tree-helper")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	const source = `package main
import (
 "fmt"
 "os"
 "os/exec"
 "path/filepath"
 "strconv"
 "time"
)
func record(dir, mode string) {
 p := filepath.Join(dir, mode+".pid")
 if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil { panic(err) }
}
func main() {
 if len(os.Args) != 3 { panic("mode dir required") }
 mode, dir := os.Args[1], os.Args[2]
 if mode == "parent" {
  c := exec.Command(os.Args[0], "child", dir)
  if err := c.Start(); err != nil { panic(err) }
  record(dir, "parent")
  record(dir, "parent-child")
  _ = c.Process.Release()
 } else if mode == "child" { record(dir, "child") } else { panic("bad mode") }
 deadline := time.Now().Add(30*time.Second)
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
		t.Fatalf("构建临时 helper 失败: %v\n%s", err, out)
	}
	return exe
}

func bashQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type recordedPID struct {
	pid      int
	identity processIdentity
	name     string
}

func recordHelperPID(t *testing.T, dir, name string) recordedPID {
	t.Helper()
	path := filepath.Join(dir, name+".pid")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && pid > 0 {
				identity, err := queryProcessIdentity(pid)
				if err == nil && identity.Active {
					r := recordedPID{pid: pid, identity: identity, name: name}
					t.Logf("OWNED_HELPER name=%s pid=%d image=%q creation=%q", name, pid, identity.Image, identity.CreationID)
					return r
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 helper %s PID/身份超时", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitOwnedProcessExit(t *testing.T, r recordedPID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := queryProcessIdentity(r.pid)
		if err != nil || !current.Active || current.CreationID != r.identity.CreationID {
			t.Logf("OWNED_HELPER_EXIT name=%s pid=%d creation=%q", r.name, r.pid, r.identity.CreationID)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("归属进程仍存活: name=%s pid=%d current=%+v", r.name, r.pid, current)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRealHelperProcessTreeStopAndDispose(t *testing.T) {
	requireBash(t)
	helper := buildTreeHelper(t)
	for _, mode := range []string{"stop", "dispose"} {
		t.Run(mode, func(t *testing.T) {
			svc := newSvc(t)
			dir := t.TempDir()
			command := bashQuote(filepath.ToSlash(helper)) + " parent " + bashQuote(filepath.ToSlash(dir))
			id := startTestJob(t, svc, command)
			parent := recordHelperPID(t, dir, "parent")
			child := recordHelperPID(t, dir, "child")
			meta, err := svc.Metadata(id)
			if err != nil {
				t.Fatal(err)
			}
			root := recordedPID{pid: meta.RootPID, name: "bash-root"}
			root.identity, err = queryProcessIdentity(root.pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				svc.Close()
				for _, r := range []recordedPID{child, parent} {
					cleanupOwnedTestProcess(r)
				}
			})

			if mode == "stop" {
				if err := svc.Stop(id); err != nil {
					t.Fatalf("停止进程树失败: %v", err)
				}
			} else {
				svc.Close()
			}
			waitOwnedProcessExit(t, root)
			waitOwnedProcessExit(t, parent)
			waitOwnedProcessExit(t, child)
			page, err := svc.OutputPage(id, 0, 1, 2*time.Second)
			if err != nil || !page.Done {
				t.Fatalf("收工后输出仍应可读: %+v, %v", page, err)
			}
		})
	}
}
