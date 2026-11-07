package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loopRunner 会重 exec 当前可执行文件。测试进程在 testing 解析参数前拦截
// `run ...`，记录真实 argv 后自行退出，因此不需要起模型、也不留下子进程。
func init() {
	capture := os.Getenv("DSHD_MODEL_WIRE_ARGV")
	if capture == "" || len(os.Args) < 2 || os.Args[1] != "run" {
		return
	}
	b, err := json.Marshal(os.Args[1:])
	if err != nil {
		os.Exit(91)
	}
	if err := os.WriteFile(capture, b, 0o644); err != nil {
		os.Exit(92)
	}
	if captureEnv := os.Getenv("DSHD_MODEL_WIRE_ENV"); captureEnv != "" {
		b, err = json.Marshal(os.Environ())
		if err != nil {
			os.Exit(93)
		}
		if err := os.WriteFile(captureEnv, b, 0o600); err != nil {
			os.Exit(94)
		}
	}
	os.Exit(0)
}

func readLoopModelArg(t *testing.T, capture string) (model string, args []string) {
	t.Helper()
	b, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("子进程没有记录 argv: %v", err)
	}
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--model" && i+1 < len(args) {
			if model != "" {
				t.Fatalf("argv 有多个 --model: %v", args)
			}
			model = args[i+1]
			i++
		}
	}
	return model, args
}

func runLoopAndReadModel(t *testing.T, r *modelOverrideLoopRunner, capture, sid, wantSID string) (string, []string) {
	t.Helper()
	if err := r.Run(context.Background(), sid, "turn-"+sid, "next-prompt"); err != nil {
		t.Fatalf("Run(%s): %v", sid, err)
	}
	model, args := readLoopModelArg(t, capture)
	if !containsArgPair(args, "--session", wantSID) {
		t.Fatalf("子进程会话参数变了: %v", args)
	}
	if !containsArgPair(args, "--policy", "deny") || !containsArgPair(args, "--max-steps", "6") {
		t.Fatalf("permission/max-steps 参数变了: %v", args)
	}
	return model, args
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

// 默认 loopRunner 路径必须真的把覆盖放进下一次子进程 argv，而不是只修 debug runner。
func TestDefaultLoopRunnerSessionModelOverrideLifecycle(t *testing.T) {
	home := t.TempDir()
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	base := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "default-model"}
	runner := &modelOverrideLoopRunner{base: base}
	overrides := newModelOverrideStore(home)

	if err := overrides.Set("session-a", "model-x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := runLoopAndReadModel(t, runner, capture, "session-a", "session-a"); got != "model-x" {
		t.Fatalf("A 设置后的下一轮子进程 model = %q，要 model-x", got)
	}
	if got, _ := runLoopAndReadModel(t, runner, capture, "session-b", "session-b"); got != "default-model" {
		t.Fatalf("B 不应受 A 影响，子进程 model = %q", got)
	}
	if err := overrides.Set("session-a", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := runLoopAndReadModel(t, runner, capture, "session-a", "session-a"); got != "default-model" {
		t.Fatalf("清空后应回默认，子进程 model = %q", got)
	}

	if err := overrides.Set("session-a", "model-after-restart"); err != nil {
		t.Fatal(err)
	}
	restartedStore := newModelOverrideStore(home)
	if err := restartedStore.Load(); err != nil {
		t.Fatal(err)
	}
	if got := restartedStore.Get("session-a"); got != "model-after-restart" {
		t.Fatalf("重启 Load 后 Get(A) = %q", got)
	}
	restartedRunner := &modelOverrideLoopRunner{base: base}
	if got, _ := runLoopAndReadModel(t, restartedRunner, capture, "session-a", "session-a"); got != "model-after-restart" {
		t.Fatalf("重启 Load 后的下一轮子进程 model = %q", got)
	}
}

// 权限覆盖必须进入下一次真实子进程的 --policy，不能只出现在 session.load。
// 同一个 store 连续切 A/B、清空并模拟重启，顺便钉住 base 不被污染。
func TestDefaultLoopRunnerSessionPermissionOverrideLifecycle(t *testing.T) {
	home := t.TempDir()
	capture := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	base := &loopRunner{home: home, policy: "deny", maxSteps: 6, model: "default-model"}
	overrides := newPermissionOverrideStore(home)
	runner := &modelOverrideLoopRunner{base: base, permissions: overrides}
	src := newSessionSource(home, "")
	src.perms = overrides

	assertPolicy := func(sid, want string) {
		t.Helper()
		if err := runner.Run(context.Background(), sid, "turn-"+sid, "next-prompt"); err != nil {
			t.Fatalf("Run(%s): %v", sid, err)
		}
		_, args := readLoopModelArg(t, capture)
		if !containsArgPair(args, "--policy", want) {
			t.Fatalf("%s 下一轮子进程 --policy 要是 %q，实际 argv=%v", sid, want, args)
		}
	}

	if err := overrides.Set("session-a", "allow"); err != nil {
		t.Fatal(err)
	}
	if got, err := src.Permission("session-a"); err != nil || got != "allow" {
		t.Fatalf("session.load 权限投影 = %q, %v；要 allow", got, err)
	}
	assertPolicy("session-a", "allow")
	assertPolicy("session-b", "deny")
	if base.policy != "deny" {
		t.Fatalf("A 的覆盖污染了 base.policy：%q", base.policy)
	}

	if err := overrides.Set("session-a", "ask"); err != nil {
		t.Fatal(err)
	}
	assertPolicy("session-a", "ask")
	if err := overrides.Set("session-a", ""); err != nil {
		t.Fatal(err)
	}
	assertPolicy("session-a", "deny")

	if err := overrides.Set("session-a", "auto"); err != nil {
		t.Fatal(err)
	}
	restartedStore := newPermissionOverrideStore(home)
	if err := restartedStore.Load(); err != nil {
		t.Fatal(err)
	}
	if got := restartedStore.Get("session-a"); got != "auto" {
		t.Fatalf("重启 Load 后权限覆盖 = %q，要 auto", got)
	}
	runner = &modelOverrideLoopRunner{base: base, permissions: restartedStore}
	src.perms = restartedStore
	if got, err := src.Permission("session-a"); err != nil || got != "auto" {
		t.Fatalf("重启后的 session.load 权限投影 = %q, %v；要 auto", got, err)
	}
	assertPolicy("session-a", "auto")
}

func TestDefaultLoopRunnerRejectsCorruptModelOverrideStore(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state", "session-model.json"), []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "must-not-exist.json")
	t.Setenv("DSHD_MODEL_WIRE_ARGV", capture)
	t.Setenv("DSHD_SERVE_NO_FOLLOW", "1")
	runner := &modelOverrideLoopRunner{base: &loopRunner{
		home: home, policy: "deny", maxSteps: 6, model: "default-model",
	}}
	err := runner.Run(context.Background(), "session-a", "turn-corrupt", "must-not-spawn")
	if err == nil || !strings.Contains(err.Error(), "session-model.json") {
		t.Fatalf("损坏 store 要返回含路径的错误，拿到 %v", err)
	}
	if _, statErr := os.Stat(capture); !os.IsNotExist(statErr) {
		t.Fatalf("损坏 store 后仍起了子进程，stat err = %v", statErr)
	}
}
