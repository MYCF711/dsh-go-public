package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/kernel"
	"dshgo/internal/session"
	"dshgo/internal/spi"
)

// 本文件钉住「**真实装配**里的 tools/execute 前钩子」：
// 单元测试证明 Pipeline 有这个能力不够，还要证明 builtinModules 真的把它接上了——
// 否则能力存在、生产没接，等于没做（这是本仓最容易交付假绿的那种形状）。
//
// 判据：
//   - 顶层 + 有会话：工具体执行前必须已经落盘；
//   - 落盘失败：工具体**零执行**（用真的会写盘的 write 工具，文件不存在）；
//   - 无会话：不落盘（`dshd exec/tools` 这类单工具入口不该产生侧车）。

// ckptDispatchSession 建一个带一帧 turn/start 的会话（侧车要求 Frames ≥ 1）。
func ckptDispatchSession(t *testing.T, home, wd, sid string) string {
	t.Helper()
	lg, err := session.CreateSession(home, wd, sid, "default")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	if err := lg.Append("turn/start", session.TurnStartData(1)); err != nil {
		t.Fatalf("写 turn/start: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关会话: %v", err)
	}
	path, err := newSessionSource(home, "").resolve(sid)
	if err != nil {
		t.Fatalf("解析会话文件: %v", err)
	}
	return path
}

// ckptDispatchRunner 按生产同一条装配路径拿到工具执行器。
func ckptDispatchRunner(t *testing.T, wd, home string) spi.Runner {
	t.Helper()
	k := kernel.New("ckpt-dispatch")
	root := k.Root()
	t.Cleanup(root.Dispose)
	if _, err := kernel.Assemble(root, builtinModules(wd, home, 0, nil)); err != nil {
		t.Fatalf("装配 builtinModules: %v", err)
	}
	runner, err := kernel.Inject(root, spi.ToolRunnerKey)
	if err != nil {
		t.Fatalf("注入 ToolRunnerKey: %v", err)
	}
	return runner
}

// TestCheckpointToolDispatchWritesSidecar 钉正向面：真实装配下，
// 工具体执行前已经有一份可恢复的前缀快照。
func TestCheckpointToolDispatchWritesSidecar(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	sid := "ckpt-tool-dispatch"
	note := filepath.Join(wd, "note.txt")
	if err := os.WriteFile(note, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := ckptDispatchSession(t, home, wd, sid)
	runner := ckptDispatchRunner(t, wd, home)

	ctx := spi.WithSession(context.Background(), sid)
	args, _ := json.Marshal(map[string]any{"file_path": note})
	if _, err := runner.Run(ctx, "read", args); err != nil {
		t.Fatalf("read 应成功: %v", err)
	}

	cps, err := session.ListDurableCheckpoints(home, sid)
	if err != nil {
		t.Fatalf("列快照: %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("工具派发前应落 1 份快照，实际 %d 份: %+v", len(cps), cps)
	}
	if cps[0].CoveredEvents != 1 {
		t.Fatalf("快照覆盖事件数 = %d，期望 1（turn/start）", cps[0].CoveredEvents)
	}
	dir, err := session.CheckpointDir(home, sid, cps[0].CoveredEvents)
	if err != nil {
		t.Fatalf("算侧车目录: %v", err)
	}
	cp := readCheckpointOnDisk(t, dir, "工具派发前")
	sessionBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 v4: %v", err)
	}
	verifyCheckpointPrefix(t, cp, sessionBytes)
}

// TestCheckpointToolDispatchZeroExecOnFlushFailure 钉零执行：
// 侧车根目录被普通文件占住（MkdirAll 必然失败）时，**真的会写盘的工具**
// 一次都没跑——目标文件必须不存在。
func TestCheckpointToolDispatchZeroExecOnFlushFailure(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	sid := "ckpt-tool-diskfail"
	ckptDispatchSession(t, home, wd, sid)
	if err := os.WriteFile(filepath.Join(home, "durability-checkpoints"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := ckptDispatchRunner(t, wd, home)

	target := filepath.Join(wd, "should-not-exist.txt")
	args, _ := json.Marshal(map[string]any{"file_path": target, "content": "x"})
	ctx := spi.WithSession(context.Background(), sid)
	res, err := runner.Run(ctx, "write", args)
	if err == nil {
		t.Fatalf("派发前落盘失败却成功派发工具体（res=%+v）", res)
	}
	if !strings.Contains(err.Error(), "durability checkpoint") && !strings.Contains(err.Error(), "落盘") {
		t.Fatalf("错误没有指出是 durability checkpoint 失败：%v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("工具体在落盘失败后仍执行了（%s 存在，stat=%v）", target, statErr)
	}
}

// TestCheckpointToolDispatchSkipsSessionless 钉放行条件：
// 没有会话的调用（`dshd exec/tools`）不许产生侧车。
func TestCheckpointToolDispatchSkipsSessionless(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	note := filepath.Join(wd, "note.txt")
	if err := os.WriteFile(note, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := ckptDispatchRunner(t, wd, home)
	args, _ := json.Marshal(map[string]any{"file_path": note})
	if _, err := runner.Run(context.Background(), "read", args); err != nil {
		t.Fatalf("read 应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "durability-checkpoints")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("无会话的调用产生了侧车（%v）", err)
	}
}
