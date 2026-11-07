package main

import (
	"context"
	"path/filepath"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/approval"
	"dshgo/internal/llm"
	"dshgo/internal/modes"
	"dshgo/internal/session"
)

func newTestRunner(t *testing.T) *acpRunner {
	t.Helper()
	home := t.TempDir()
	reg, err := modes.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &acpRunner{
		home:         home,
		modeReg:      reg,
		modeID:       modes.DefaultID,
		sessionModes: map[string]string{},
		approvals:    approval.New(),
		logs:         map[string]*session.Log{},
		hist:         map[string][]llm.Message{},
		turns:        map[string]int{},
		used:         map[string]int{},
		meter:        map[string]*session.Meter{},
	}
}

// 模式必须写进 header.agentPreset（DSH 白名单键），否则 resume 就丢了。
func TestEnsureSessionRecordsMode(t *testing.T) {
	r := newTestRunner(t)
	cwd := "/tmp/mode-a"
	r.sessionModes["s1"] = "read-only"

	l, err := r.ensureSession("s1", cwd)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	p, err := session.SessionPath(r.home, cwd, "s1")
	if err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Header.AgentPreset != "read-only" {
		t.Fatalf("header.agentPreset = %q，期望 read-only", back.Header.AgentPreset)
	}
	if filepath.ToSlash(p) != filepath.ToSlash(l.Path) {
		t.Fatalf("路径不一致: %s vs %s", p, l.Path)
	}
}

// 新进程 resume 时应当从 header 采纳模式，而不是回到 default。
func TestResumeAdoptsModeFromHeader(t *testing.T) {
	r := newTestRunner(t)
	cwd := "/tmp/mode-b"
	r.sessionModes["s2"] = "read-only"
	l, err := r.ensureSession("s2", cwd)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	// 换一个"新进程"（不给 --mode）
	r2 := newTestRunner(t)
	r2.home = r.home
	if err := r2.Resume(context.Background(), "s2", cwd); err != nil {
		t.Fatal(err)
	}
	defer r2.Close("s2")
	if got := r2.modeOf("s2").ID; got != "read-only" {
		t.Fatalf("resume 后模式 = %q，期望 read-only（权限不能变大）", got)
	}
}
