package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := findBash(); err != nil {
		t.Skip("本机没有 bash：", err)
	}
}

func newSvc(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc := NewService(dir)
	t.Cleanup(svc.Close)
	return svc
}

func TestEcho(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	res, err := svc.Run("echo hi", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "hi" {
		t.Fatalf("输出 = %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码 = %d", res.ExitCode)
	}
}

func TestExitEndsSessionAndRecovers(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	// 裸 exit 会结束 bash 自己：这是真实终端的行为，宿主必须能自愈。
	if _, err := svc.Run("exit 3", 5*time.Second); err == nil {
		t.Fatal("exit 结束会话时应报错，而不是假装成功")
	}
	res, err := svc.Run("echo back", 5*time.Second)
	if err != nil {
		t.Fatalf("会话应能自动重开: %v", err)
	}
	if strings.TrimSpace(res.Output) != "back" {
		t.Fatalf("重开后的输出 = %q", res.Output)
	}
}

func TestStderrIsMerged(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	res, err := svc.Run("echo oops 1>&2", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "oops") {
		t.Fatalf("stderr 没被收集：%q", res.Output)
	}
}

func TestCwdPersistsAcrossCommands(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	if _, err := svc.Run("cd /tmp", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Run("pwd", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "tmp") {
		t.Fatalf("cwd 没保留：%q", res.Output)
	}
	if res.Cwd == "" {
		t.Fatal("结果里应回报 cwd")
	}
}

func TestAutoBackground(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	res, err := svc.Run("sleep 2; echo done", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.MovedToBg || res.JobID == "" {
		t.Fatalf("静默超时应转后台，得到 %+v", res)
	}

	out, err := svc.Output(res.JobID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("等了 10s 还没结束：%+v", out)
	}
	if !strings.Contains(out.Output, "done") {
		t.Fatalf("后台任务输出丢了：%q", out.Output)
	}
	if out.ExitCode != 0 {
		t.Fatalf("退出码 = %d", out.ExitCode)
	}
}

func TestJobListAndKill(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	res, err := svc.Run("sleep 30", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	jobs := svc.List()
	if len(jobs) != 1 || jobs[0].ID != res.JobID {
		t.Fatalf("任务表 = %+v", jobs)
	}
	if err := svc.Kill(res.JobID); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Output(res.JobID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatal("终止后任务应标记结束")
	}
}

func TestEmptyCommandRejected(t *testing.T) {
	requireBash(t)
	svc := newSvc(t)
	if _, err := svc.Run("   ", time.Second); err == nil {
		t.Fatal("空命令必须报错")
	}
}

func TestOutputIsBoundedByPipeline(t *testing.T) {
	// 这条不是在测 bash，是在测「大输出不会直接进上下文」这条约定的入口。
	requireBash(t)
	svc := newSvc(t)
	res, err := svc.Run(strings.Repeat("echo line\n", 200), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) == 0 {
		t.Fatal("应该有输出")
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码 = %d", res.ExitCode)
	}
}

// ctx 取消时必须立刻返回，不能陪着 shell 等到 idle 超时。
// 实测过：不修之前，取消一个 sleep 20 的调用要等 20 秒才返回。
func TestExecReturnsOnContextCancel(t *testing.T) {
	svc := NewService(t.TempDir())
	defer svc.Close()
	tool := &shellTool{svc: svc, budget: 30 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := tool.Exec(ctx, json.RawMessage(`{"command":"sleep 20"}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("取消之后应该返回错误")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("取消后 %v 才返回——ctx 没被尊重", elapsed)
	}
}
