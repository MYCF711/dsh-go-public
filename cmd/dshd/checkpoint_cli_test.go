package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/agent"
	"dshgo/internal/modes"
	"dshgo/internal/session"
)

// 本文件钉住官方 #78 `session-checkpoint-policy` 的 **CLI 入口侧车**（任务 A）。
//
// 官方原件（0.2.2-rc.1，asar 只读解出，dsh-session-checkpoint-policy/lib/index.js）：
//
//	:61-66  ctx.on("tools/execute", async (exec, next) => {
//	           if (exec.agent === void 0 || exec.parent !== void 0) return next();
//	           await ctx.sessions.flush(exec.agent.session);
//	           if (exec.signal.aborted) return abortedBeforeDispatchResult();
//	           return next();
//	        });
//	:67-70  ctx.on("agent/pre-step", async ({ agent }, next) => {
//	           await ctx.sessions.flush(agent.session);
//	           return next();
//	        });
//
// 我方三条入口（run / serve / acp）里，serve 那条已经由 withDurableCheckpointOptions
// 包住；run 与 acp 之前**没有**侧车，所以「模型请求前必须已经落盘」这件事在
// 这两个入口上根本不成立。判定分两层：
//   - 行为层：真实子进程把请求发出去（假网关收到 POST 才写 in-flight 标记），
//     然后整进程被杀；重启后必须能用既有 checkpoint 读取 API 复原请求前缀。
//   - 失败层：侧车落不下来时，模型请求**一次都不许发出去**（fail-closed）。

const (
	ckptCLIChildEnv  = "DSHD_CKPT_CLI_CHILD"
	ckptCLIModeEnv   = "DSHD_CKPT_CLI_MODE"
	ckptCLIHomeEnv   = "DSHD_CKPT_CLI_HOME"
	ckptCLISIDEnv    = "DSHD_CKPT_CLI_SID"
	ckptCLIBaseEnv   = "DSHD_CKPT_CLI_BASE"
	ckptCLIMarkEnv   = "DSHD_CKPT_CLI_MARKER"
	ckptCLIPrompt    = "杀掉进程之前这一句必须已经落成耐久快照"
	ckptCLIRunMode   = "run"
	ckptCLIACPAMode  = "acp"
	ckptCLIChildName = "^TestCheckpointCLIChildHelper$"
)

// ckptCLIManifest 是最小 manifest：cmdRun/cmdACP 只需要 tools 能力。
const ckptCLIManifest = "# #78 CLI checkpoint 测试用最小 manifest\n[[module]]\nid = \"tools\"\n"

// ckptCLIWorkspace 造一个 CLI 子进程可用的工作区：manifest + 预建会话。
func ckptCLIWorkspace(t *testing.T, sid string) (home, cwd string) {
	t.Helper()
	cwd = t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "dsh.toml"), []byte(ckptCLIManifest), 0o644); err != nil {
		t.Fatalf("写 manifest: %v", err)
	}
	home = t.TempDir()
	lg, err := session.CreateSession(home, cwd, sid, "default")
	if err != nil {
		t.Fatalf("预建会话: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("关预建会话: %v", err)
	}
	return home, cwd
}

// ckptCLIChildArgs 是子进程要跑的 CLI 入口。
func ckptCLIChildArgs(mode, home, sid, base string) []string {
	switch mode {
	case ckptCLIRunMode:
		return []string{
			"--home", home, "--session", sid,
			"--prompt", ckptCLIPrompt, "--policy", "allow",
			"--base", base, "--model", "ckpt-model",
		}
	case ckptCLIACPAMode:
		return []string{"--home", home, "--base", base, "--model", "ckpt-model"}
	}
	return nil
}

// TestCheckpointCLIChildHelper 是子进程入口：它执行**真实**的 cmdRun / cmdACP。
func TestCheckpointCLIChildHelper(t *testing.T) {
	if os.Getenv(ckptCLIChildEnv) != "1" {
		t.Skip("仅作为子进程入口")
	}
	mode := os.Getenv(ckptCLIModeEnv)
	argv := ckptCLIChildArgs(mode, os.Getenv(ckptCLIHomeEnv), os.Getenv(ckptCLISIDEnv), os.Getenv(ckptCLIBaseEnv))
	var err error
	switch mode {
	case ckptCLIRunMode:
		err = cmdRun(argv)
	case ckptCLIACPAMode:
		err = cmdACP(argv)
	default:
		t.Fatalf("未知子进程模式 %q", mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "子进程 CLI 入口失败: %v\n", err)
		os.Exit(1)
	}
}

// ckptCLIStartChild 起一个真实子进程；返回 kill 函数（幂等）。
func ckptCLIStartChild(t *testing.T, mode, home, cwd, sid, base, marker string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+ckptCLIChildName)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		ckptCLIChildEnv+"=1",
		ckptCLIModeEnv+"="+mode,
		ckptCLIHomeEnv+"="+home,
		ckptCLISIDEnv+"="+sid,
		ckptCLIBaseEnv+"="+base,
		ckptCLIMarkEnv+"="+marker,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("起子进程: %v", err)
	}
	var once atomic.Bool
	kill := func() {
		if once.CompareAndSwap(false, true) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(kill)
	return cmd, kill
}

// ckptCLIWaitMarker 等假网关写下 in-flight 标记（= 模型请求真的发出去了）。
func ckptCLIWaitMarker(t *testing.T, marker string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.ReadFile(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s（等 in-flight 标记超时）", why)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ckptCLIRecoverAndVerify 是「重启后能恢复」的唯一判据：复用既有
// session.RecoverDurableCheckpoint，逐字节复核前缀，并确认请求身份留在侧车里。
func ckptCLIRecoverAndVerify(t *testing.T, home, sid, wantPrompt, why string) session.RecoveredPrefix {
	t.Helper()
	rec, err := session.RecoverDurableCheckpoint(home, sid)
	if err != nil {
		t.Fatalf("%s，但重启后恢复不出请求前缀: %v", why, err)
	}
	if rec.Checkpoint.SessionID != sid {
		t.Fatalf("快照会话 id = %q，期望 %q", rec.Checkpoint.SessionID, sid)
	}
	if rec.Checkpoint.SizeBytes <= 0 || int64(len(rec.Events)) == 0 {
		t.Fatalf("恢复出的前缀是空的：%+v", rec.Checkpoint)
	}
	raw, err := os.ReadFile(rec.Checkpoint.SessionPath)
	if err != nil {
		t.Fatalf("读会话文件: %v", err)
	}
	if int64(len(raw)) < rec.Checkpoint.SizeBytes {
		t.Fatalf("会话文件 %d 字节短于快照记的 %d 字节", len(raw), rec.Checkpoint.SizeBytes)
	}
	sum := sha256.Sum256(raw[:rec.Checkpoint.SizeBytes])
	if got := hex.EncodeToString(sum[:]); got != rec.Checkpoint.SHA256 {
		t.Fatalf("前缀已被改写：磁盘 %s，快照 %s", got, rec.Checkpoint.SHA256)
	}
	if rec.Checkpoint.Request == nil {
		t.Fatalf("%s：侧车没有记录这次请求的身份（崩溃时在飞的是哪一条无从判断）", why)
	}
	if rec.Checkpoint.Request.Text != wantPrompt {
		t.Fatalf("侧车记的请求正文 = %q，期望 %q", rec.Checkpoint.Request.Text, wantPrompt)
	}
	var sawUser bool
	for _, ev := range rec.Events {
		if ev.Type == "user/message" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatalf("恢复出的前缀里没有 user/message：%d 个事件全不对", len(rec.Events))
	}
	return rec
}

// TestCheckpointCLIRunSurvivesKill 是任务 A 的硬证据（run 入口）：
// 子进程把请求真发出去（假网关收到 POST 才写 in-flight 标记），然后整进程被杀；
// 父进程重启后必须能恢复请求前的全部帧。
func TestCheckpointCLIRunSurvivesKill(t *testing.T) {
	home, cwd := ckptCLIWorkspace(t, "ckpt-cli-run-kill")
	sid := "ckpt-cli-run-kill"
	marker := filepath.Join(home, "run-in-flight.marker")
	release := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(marker, []byte(r.Method+" "+r.URL.Path), 0o600); err != nil {
			t.Errorf("写 in-flight 标记: %v", err)
		}
		// 不产生任何响应：请求一直吊着，父进程在这时杀。
		// release 让收尾不依赖「被杀进程的 socket 何时被系统回收」——
		// 否则测试失败路径会在 httptest.Server.Close 上挂死，掩盖真正的断言。
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer gateway.Close()
	defer close(release)

	_, kill := ckptCLIStartChild(t, ckptCLIRunMode, home, cwd, sid, gateway.URL, marker)
	ckptCLIWaitMarker(t, marker, 90*time.Second, "dshd run 没有把模型请求发出去")
	kill()

	rec := ckptCLIRecoverAndVerify(t, home, sid, ckptCLIPrompt,
		fmt.Sprintf("dshd run 的请求已发出（标记=%s），进程已被杀", marker))
	// 前缀必须停在请求之前：最后一帧是 user/message（请求载荷），绝不能是
	// assistant/message（那意味着请求已经产生过回答）。
	last := rec.Events[len(rec.Events)-1].Type
	if last != "user/message" {
		t.Fatalf("请求前快照的最后一帧 = %q，期望 user/message（请求载荷）", last)
	}
	// 快照之后只允许有观察事件（step/start），且必须是**完整帧**：
	// 半帧尾巴意味着杀进程时正在写，前缀结论不能依赖它。
	if rec.ExtraBytes > 0 && rec.TailTruncatedAt != 0 {
		t.Fatalf("快照之后的尾部有半帧（offset=%d），请求前的前缀不完整", rec.TailTruncatedAt)
	}
}

// TestCheckpointCLIACPSurvivesKill 是任务 A 的硬证据（acp 入口）：
// 与 run 同形，但走的是真实 ACP 入口（stdio JSON-RPC：initialize → session/new
// → session/prompt）。kill 之后重启必须能恢复 ACP 那一轮的请求前缀。
func TestCheckpointCLIACPSurvivesKill(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "dsh.toml"), []byte(ckptCLIManifest), 0o644); err != nil {
		t.Fatalf("写 manifest: %v", err)
	}
	marker := filepath.Join(home, "acp-in-flight.marker")
	release := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(marker, []byte(r.Method+" "+r.URL.Path), 0o600); err != nil {
			t.Errorf("写 in-flight 标记: %v", err)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer gateway.Close()
	defer close(release)

	cmd := exec.Command(os.Args[0], "-test.run="+ckptCLIChildName)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		ckptCLIChildEnv+"=1",
		ckptCLIModeEnv+"="+ckptCLIACPAMode,
		ckptCLIHomeEnv+"="+home,
		ckptCLIBaseEnv+"="+gateway.URL,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("建 stdin 管道: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("建 stdout 管道: %v", err)
	}
	var childErr bytes.Buffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatalf("起 ACP 子进程: %v", err)
	}
	var once atomic.Bool
	kill := func() {
		if once.CompareAndSwap(false, true) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	defer kill()

	enc := json.NewEncoder(stdin)
	reader := bufio.NewReader(stdout)
	call := func(id float64, method string, params any) map[string]any {
		t.Helper()
		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("写 %s 请求: %v", method, err)
		}
		return ckptCLIReadRPC(t, reader, id, &childErr)
	}

	call(1, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
	})
	created := call(2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	result, _ := created["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new 没给出 sessionId：%v", created)
	}

	// 这一条发出去之后子进程就卡在模型请求上（假网关不回应）；父进程等标记后杀。
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 3.0, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": ckptCLIPrompt}},
		},
	}); err != nil {
		t.Fatalf("写 session/prompt: %v", err)
	}
	ckptCLIWaitMarker(t, marker, 90*time.Second, "dshd acp 没有把模型请求发出去")
	kill()

	ckptCLIRecoverAndVerify(t, home, sid, ckptCLIPrompt,
		fmt.Sprintf("dshd acp 的请求已发出（标记=%s），进程已被杀；stderr=%s", marker, childErr.String()))
}

// ckptCLIReadRPC 读一条 JSON-RPC 应答；测试框架自己往 stdout 写的非 JSON 行直接跳过。
func ckptCLIReadRPC(t *testing.T, r *bufio.Reader, wantID float64, childErr *bytes.Buffer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读 ACP 子进程输出失败（id=%v）: %v；stderr=%s", wantID, err, childErr.String())
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue // "=== RUN" / "PASS" 之类的测试框架输出
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		got, ok := msg["id"].(float64)
		if !ok || got != wantID {
			continue // 服务端推的 session/update 通知没有 id
		}
		if e, ok := msg["error"].(map[string]any); ok {
			t.Fatalf("ACP 方法返回错误（id=%v）：%v", wantID, e)
		}
		return msg
	}
	t.Fatalf("等 ACP 应答超时（id=%v）", wantID)
	return nil
}

// TestCheckpointCLIRunFailsClosedOnDiskFailure 钉「落盘失败即不派发」（run 入口）：
// 侧车根目录被一个普通文件占住（MkdirAll 必然失败），这一轮必须报错，
// 且假网关**一次请求都没收到**。
func TestCheckpointCLIRunFailsClosedOnDiskFailure(t *testing.T) {
	home, _ := d1Workspace(t, "ckpt-cli-run-diskfail")
	var requests int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, runPlainText)
	}))
	defer gateway.Close()
	if err := os.WriteFile(filepath.Join(home, "durability-checkpoints"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := d1Run(t, home, "ckpt-cli-run-diskfail", gateway.URL)
	if err == nil {
		t.Fatalf("侧车落盘失败却成功收工（不是 fail-closed）")
	}
	if !strings.Contains(err.Error(), "durability checkpoint") {
		t.Fatalf("错误没有指出是 durability checkpoint 失败：%v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("落盘失败仍派发了 %d 次模型请求（fail-closed 不成立）", got)
	}
}

// TestCheckpointCLIACPFailsClosedOnDiskFailure 钉「落盘失败即不派发」（acp 入口）。
func TestCheckpointCLIACPFailsClosedOnDiskFailure(t *testing.T) {
	var requests int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, runPlainText)
	}))
	defer gateway.Close()

	runner, _, _ := newPromptHarness(t, gateway.URL, modes.DefaultID, nil)
	if err := os.WriteFile(filepath.Join(runner.home, "durability-checkpoints"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	_, _, err := runner.Prompt(context.Background(), "s-ckpt-acp-diskfail", cwd, "做点事",
		func(agent.Event) error { return nil })
	if err == nil {
		t.Fatalf("ACP 侧车落盘失败却成功收尾（不是 fail-closed）")
	}
	if !strings.Contains(err.Error(), "durability checkpoint") {
		t.Fatalf("错误没有指出是 durability checkpoint 失败：%v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("落盘失败仍派发了 %d 次模型请求（fail-closed 不成立）", got)
	}
}

// TestCheckpointCLIPreStepWritesSidecar 钉官方 `agent/pre-step`：
// 一个带工具调用的两模型步轮次，必须在**第二步请求之前**留下快照——
// 否则「每步请求前前缀耐久」只在第一步成立，之后每一步都是裸奔。
func TestCheckpointCLIPreStepWritesSidecar(t *testing.T) {
	home, _ := d1Workspace(t, "ckpt-cli-prestep")
	base := runSSE(t, runReasoningCall, runPlainText)
	if err := d1Run(t, home, "ckpt-cli-prestep", base); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}

	cps, err := session.ListDurableCheckpoints(home, "ckpt-cli-prestep")
	if err != nil {
		t.Fatalf("列快照: %v", err)
	}
	if len(cps) < 2 {
		t.Fatalf("两模型步一轮只落了 %d 份快照，期望 ≥2（第一步请求前 + 第二步请求前）", len(cps))
	}
	anchors := make([]string, 0, len(cps))
	for _, cp := range cps {
		anchors = append(anchors, fmt.Sprint(cp.CoveredEvents))
	}
	if cps[len(cps)-1].CoveredEvents <= cps[0].CoveredEvents {
		t.Fatalf("快照锚点没有随步骤推进：%v", anchors)
	}
	// 每一份都必须能独立恢复（前缀逐字节自洽），不只是最后一份。
	for _, cp := range cps {
		rec, err := session.RecoverDurableCheckpoint(home, cp.SessionID)
		if err != nil {
			t.Fatalf("锚点 %d 恢复失败: %v", cp.CoveredEvents, err)
		}
		if rec.Checkpoint.SessionID != "ckpt-cli-prestep" {
			t.Fatalf("快照归属错了：%+v", rec.Checkpoint)
		}
	}
}

// TestCheckpointCLIACPSingleSidecarPerAnchor 钉需求 3 的入口面：
// ACP 的 prompt 侧车（covered=2）与 pre-step 侧车在**同一锚点**，
// 必须只落一份——同一轮不是「每包一层就多一份快照」。
func TestCheckpointCLIACPSingleSidecarPerAnchor(t *testing.T) {
	runner, _, _ := newPromptHarness(t, ssePlainText(t, "好"), modes.DefaultID, nil)
	cwd := t.TempDir()
	promptOnceAt(t, runner, "s-ckpt-acp-idem", cwd)

	cps, err := session.ListDurableCheckpoints(runner.home, "s-ckpt-acp-idem")
	if err != nil {
		t.Fatalf("列快照: %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("同一轮应只有一份快照（prompt 与 pre-step 同锚点），实际 %d 份: %+v", len(cps), cps)
	}
	// system/message + turn/start + user/message = 3 帧；ACP 的 sink 不落 step/start。
	if cps[0].CoveredEvents != 3 {
		t.Fatalf("快照覆盖事件数 = %d，期望 3（system/message + turn/start + user/message）", cps[0].CoveredEvents)
	}
}

// TestCheckpointCLIIdempotentPerAnchor 钉需求 3 的 API 面：
// 同一 (session, covered) 重复落盘只落一份、字节不被改写，且对 v4 **零写入**。
func TestCheckpointCLIIdempotentPerAnchor(t *testing.T) {
	home, _ := d1Workspace(t, "ckpt-cli-idem")
	base := ssePlainText(t, "好")
	if err := d1Run(t, home, "ckpt-cli-idem", base); err != nil {
		t.Fatalf("这一轮应当正常收工: %v", err)
	}
	path, err := newSessionSource(home, "").resolve("ckpt-cli-idem")
	if err != nil {
		t.Fatalf("解析会话文件: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 v4: %v", err)
	}
	cpsBefore, err := session.ListDurableCheckpoints(home, "ckpt-cli-idem")
	if err != nil {
		t.Fatalf("列快照: %v", err)
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatalf("解析会话事件数: %v", err)
	}
	anchor := int64(len(log.Events))
	req := session.CheckpointRequest{Kind: "text", SHA256: strings.Repeat("0", 64), Bytes: 1}
	if _, err := session.WriteDurableCheckpoint(home, path, &req); err != nil {
		t.Fatalf("新锚点 %d 落盘失败: %v", anchor, err)
	}
	dir, err := session.CheckpointDir(home, "ckpt-cli-idem", anchor)
	if err != nil {
		t.Fatalf("算侧车目录: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatalf("读新锚点侧车: %v", err)
	}
	// 同一轮重复调用：同一份快照，字节不许被改写。
	if _, err := session.WriteDurableCheckpoint(home, path, &req); err != nil {
		t.Fatalf("同一锚点重复落盘失败: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatalf("重复落盘后读侧车: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("同一锚点重复调用改写了快照\n第一次: %s\n第二次: %s", first, second)
	}
	cpsAfter, err := session.ListDurableCheckpoints(home, "ckpt-cli-idem")
	if err != nil {
		t.Fatalf("重复落盘后列快照: %v", err)
	}
	if len(cpsAfter) != len(cpsBefore)+1 {
		t.Fatalf("同一锚点重复调用多落了快照：%d -> %d 份", len(cpsBefore), len(cpsAfter))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("重复落盘后读 v4: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("快照路径改写了 v4：%d -> %d 字节", len(before), len(after))
	}
}
