package main

import (
	"context"
	"testing"
)

// 只给 session id、不给 cwd 也要能恢复。
//
// 真实场景：会话在 A 目录建的，另一个进程/另一台机器上只知道 id
// （收别人的信、接别人没干完的活）。要求 cwd 的话这条路就断了。
func TestResumeWithoutCwdFindsByID(t *testing.T) {
	r := newTestRunner(t)
	cwd := "/tmp/resume-byid"
	l, err := r.ensureSession("s-byid", cwd)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	r2 := newTestRunner(t)
	r2.home = r.home
	if err := r2.Resume(context.Background(), "s-byid", ""); err != nil {
		t.Fatalf("只给 id 应当能恢复: %v", err)
	}
	defer r2.Close("s-byid")
}

// 给错 cwd 也要能靠 id 兜住——projectKey 是按 cwd 字符串算的，
// 换个盘符/挂载点就算不出来，而 id 是全局唯一的。
func TestResumeWithWrongCwdFallsBackToID(t *testing.T) {
	r := newTestRunner(t)
	l, err := r.ensureSession("s-wrongcwd", "/tmp/real-place")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	r2 := newTestRunner(t)
	r2.home = r.home
	if err := r2.Resume(context.Background(), "s-wrongcwd", "/tmp/entirely-different"); err != nil {
		t.Fatalf("cwd 不对时应当按 id 兜底: %v", err)
	}
	defer r2.Close("s-wrongcwd")
}

// 不存在的 id 要明确报错，而不是建个空会话糊弄过去。
func TestResumeUnknownIDFails(t *testing.T) {
	r := newTestRunner(t)
	if err := r.Resume(context.Background(), "s-does-not-exist", ""); err == nil {
		t.Fatal("不存在的会话必须报错")
	}
}
