package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

func createServeLifecycleSession(t *testing.T, home, sid string) string {
	t.Helper()
	lg, err := session.CreateSession(home, t.TempDir(), sid, "default")
	if err != nil {
		t.Fatal(err)
	}
	path := lg.Path
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// delete 的取消请求已经发出，也不等于工具收工。目录必须活到工具最后一笔
// 写入结束之后；否则 RemoveAll 会和 v4/工具写入并发。
func TestServeDeleteWaitsForLiveToolBeforeRemoveAll(t *testing.T) {
	home := t.TempDir()
	const sid = "session-delete-live"
	path := createServeLifecycleSession(t, home, sid)
	dir := filepath.Dir(path)
	started := make(chan struct{})
	cancelSeen := make(chan struct{})
	releaseTool := make(chan struct{})
	finalWrite := make(chan error, 1)
	run := turnRunnerFunc(func(ctx context.Context, _, _, _ string) error {
		close(started)
		<-ctx.Done()
		close(cancelSeen)
		<-releaseTool
		finalWrite <- os.WriteFile(filepath.Join(dir, "tool-final.txt"), []byte("final"), 0o644)
		return ctx.Err()
	})
	p := newPromptService(run)
	p.cancelWait = 2 * time.Second
	if _, err := p.Submit(context.Background(), sid, "run"); err != nil {
		t.Fatal(err)
	}
	<-started

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- serveDelete(home, sid, nil, p) }()
	<-cancelSeen
	select {
	case err := <-deleteDone:
		t.Fatalf("工具仍在写结果时 delete 已返回：%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("工具仍执行时会话目录已被删：%v", err)
	}
	if _, err := p.Submit(context.Background(), sid, "delete 期间不许起新轮"); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete 收工窗口必须挡住新轮，实际 %v", err)
	}

	close(releaseTool)
	if err := <-finalWrite; err != nil {
		t.Fatalf("delete 与工具写入并发，最终结果没落盘：%v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("工具收工后的 delete 失败：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("工具已收工，delete 仍未返回")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("delete 成功后会话目录仍存在，stat err=%v", err)
	}
}

// 等待超时不能删除目录，也不能把 live turn 抹掉。工具最终结果落盘、runner
// 返回之后，同一个 delete 请求可以重试并成功。
func TestServeDeleteTimeoutKeepsSessionAndLiveTurn(t *testing.T) {
	home := t.TempDir()
	const sid = "session-delete-timeout"
	path := createServeLifecycleSession(t, home, sid)
	started := make(chan struct{})
	releaseTool := make(chan struct{})
	finalResult := make(chan string, 1)
	run := turnRunnerFunc(func(ctx context.Context, _, _, _ string) error {
		close(started)
		<-ctx.Done()
		<-releaseTool
		finalResult <- "delete-timeout-tool-final"
		return ctx.Err()
	})
	p := newPromptService(run)
	p.cancelWait = 20 * time.Millisecond
	if _, err := p.Submit(context.Background(), sid, "run"); err != nil {
		t.Fatal(err)
	}
	<-started

	err := serveDelete(home, sid, nil, p)
	if !errors.Is(err, ErrCancelTimeout) {
		t.Fatalf("delete 等待超时必须返回 ErrCancelTimeout，实际 %v", err)
	}
	if !p.Busy(sid) {
		t.Fatal("delete 超时把 live turn 清掉了")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("delete 超时却删了会话：%v", statErr)
	}

	close(releaseTool)
	select {
	case got := <-finalResult:
		if got != "delete-timeout-tool-final" {
			t.Fatalf("工具结果 = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete 超时后工具最终结果丢失")
	}
	waitBusy(t, p, sid, false)
	if err := serveDelete(home, sid, nil, p); err != nil {
		t.Fatalf("真实收工后重试 delete 失败：%v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("重试成功后目录仍存在，stat err=%v", err)
	}
}

// 已完成（无 live turn）的会话可正常删除；不存在与取消已完成都必须报明确
// 错误，不能由 RemoveAll 的 nil 或 Cancel 的成功响应冒充。
func TestServeDeleteCompletedAndMissingSession(t *testing.T) {
	home := t.TempDir()
	p := newPromptService(turnRunnerFunc(func(context.Context, string, string, string) error { return nil }))
	createServeLifecycleSession(t, home, "session-delete-complete")
	if err := serveDelete(home, "session-delete-complete", nil, p); err != nil {
		t.Fatalf("删除已完成会话失败：%v", err)
	}
	if err := p.Cancel("session-delete-complete"); !errors.Is(err, ErrNoSuchTurn) {
		t.Fatalf("取消已完成会话要 ErrNoSuchTurn，实际 %v", err)
	}
	if err := serveDelete(home, "session-does-not-exist", nil, p); !errors.Is(err, transport.ErrNoSuchSession) {
		t.Fatalf("删除不存在会话要 ErrNoSuchSession，实际 %v", err)
	}
}
