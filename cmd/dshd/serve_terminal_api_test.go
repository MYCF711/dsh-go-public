package main

// 官方 terminal.* / job.* 的**真实**验证：真 shell 会话、真进程树回收、真落盘。
//
// 纪律：
//   - 只用 t.TempDir() 里的临时 home 与临时工作目录；
//   - 只回收本测试自己启动的进程（socket 会话由 bash.Service 关，外部子进程自己 Kill）；
//   - 不杀任何既有 DSH/dshd 进程，不写安装目录。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dshgo/internal/transport"
	"dshgo/modules/exec/bash"
)

// newTestTerminalAPI 建一套真实终端层（临时 home + 真实 bash.Service）。
func newTestTerminalAPI(t *testing.T, cwd string, unattended time.Duration) (*officialTerminalAPI, string) {
	t.Helper()
	home := t.TempDir()
	svc := bash.NewService(cwd)
	t.Cleanup(svc.Close)
	api, err := newOfficialTerminalAPI(home, svc,
		func(sessionID string) (string, error) { return cwd, nil }, unattended)
	if err != nil {
		t.Fatalf("起终端层失败：%v", err)
	}
	t.Cleanup(api.Close)
	return api, home
}

// waitScreen 轮询 attach 快照直到出现 marker（真输出，不是等固定时长）。
func waitScreen(t *testing.T, api *officialTerminalAPI, sessionID, id, marker string) transport.OfficialTerminalSnapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last transport.OfficialTerminalSnapshot
	for time.Now().Before(deadline) {
		snap, err := api.TerminalAttach(context.Background(), sessionID, id)
		if err != nil {
			t.Fatalf("attach 失败：%v", err)
		}
		last = snap
		if strings.Contains(snap.Screen, marker) {
			return snap
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到输出 %q（最后 screen=%q）", marker, last.Screen)
	return last
}

// TestOfficialJobTerminal_TerminalCreateRunsRealShell 证明 create 起的是**真 shell**：
// 写入的命令真的被执行，输出能从 attach 快照读回来。
func TestOfficialJobTerminal_TerminalCreateRunsRealShell(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()

	info, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{
		ID: "t1", Cols: 120, Rows: 40,
	})
	if err != nil {
		t.Fatalf("create 失败：%v", err)
	}
	if info.State != "running" {
		t.Fatalf("刚起的终端 state 要 running，拿到 %q", info.State)
	}
	if info.ExitCode != nil {
		t.Fatalf("运行中的终端 exitCode 必须是 null，拿到 %v", *info.ExitCode)
	}
	if info.Cwd != cwd {
		t.Fatalf("cwd 要如实回填（%q），拿到 %q", cwd, info.Cwd)
	}
	if _, err := os.Stat(info.Shell.Path); err != nil {
		t.Fatalf("shell path 必须是真实存在的可执行文件 %q：%v", info.Shell.Path, err)
	}
	if info.Shell.Args == nil {
		t.Fatal("shell.args 必须是数组（哪怕是空数组）")
	}

	marker := fmt.Sprintf("OFFICIAL-REAL-%d", time.Now().UnixNano())
	if err := api.Input("sess-a", "t1", "echo "+marker+"\n"); err != nil {
		t.Fatalf("写终端失败：%v", err)
	}
	snap := waitScreen(t, api, "sess-a", "t1", marker)
	if !snap.Writable {
		t.Fatal("活着的终端必须 writable=true")
	}
	if snap.Sequence <= 0 {
		t.Fatalf("sequence 必须是真实已写字节数（>0），拿到 %d", snap.Sequence)
	}
	if !snap.OneShot {
		t.Fatal("attach 是一次性快照，必须 oneShot=true")
	}
}

// TestOfficialJobTerminal_TerminalListCrossSession 证明 A/B 会话互不串。
func TestOfficialJobTerminal_TerminalListCrossSession(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	listA, err := api.TerminalList("sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].ID != "t1" {
		t.Fatalf("A 会话要看到自己的 t1：%+v", listA)
	}
	listB, err := api.TerminalList("sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 0 {
		t.Fatalf("B 会话不许看到 A 的终端：%+v", listB)
	}

	if _, err := api.TerminalAttach(ctx, "sess-b", "t1"); !errors.Is(err, transport.ErrCrossSession) {
		t.Fatalf("跨会话 attach 必须是 cross_session，拿到 %v", err)
	}
	if err := api.TerminalClose(ctx, "sess-b", "t1"); !errors.Is(err, transport.ErrCrossSession) {
		t.Fatalf("跨会话 close 必须是 cross_session，拿到 %v", err)
	}
	if err := api.TerminalRename(ctx, "sess-b", "t1", "偷改"); !errors.Is(err, transport.ErrCrossSession) {
		t.Fatalf("跨会话 rename 必须是 cross_session，拿到 %v", err)
	}
	// A 的标题没被改。
	listA, _ = api.TerminalList("sess-a")
	if listA[0].Title == "偷改" {
		t.Fatal("跨会话 rename 居然生效了")
	}
}

// TestOfficialJobTerminal_TerminalCloseKillsProcessTree 证明 close 真的回收进程树：
// 会话里起的后台循环在 close 之后必须停止写心跳文件。
func TestOfficialJobTerminal_TerminalCloseKillsProcessTree(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	beat := filepath.Join(cwd, "heartbeat.txt")
	// 后台循环，每 100ms 覆盖一次时间戳；它属于这条 shell 的进程树。
	cmd := fmt.Sprintf("nohup bash -c 'while true; do date +%%s%%N > %q; sleep 0.1; done' >/dev/null 2>&1 &\n", beat)
	if err := api.Input("sess-a", "t1", cmd); err != nil {
		t.Fatalf("起心跳失败：%v", err)
	}

	readBeat := func() string {
		raw, err := os.ReadFile(beat)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	deadline := time.Now().Add(10 * time.Second)
	for readBeat() == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if readBeat() == "" {
		t.Fatal("心跳文件没出现：后台循环没跑起来，测试无效")
	}
	// 确认它真的在跳。
	first := readBeat()
	changed := false
	for i := 0; i < 40 && !changed; i++ {
		time.Sleep(50 * time.Millisecond)
		changed = readBeat() != first
	}
	if !changed {
		t.Fatal("心跳没在动：测试无效")
	}

	if err := api.TerminalClose(ctx, "sess-a", "t1"); err != nil {
		t.Fatalf("close 失败：%v", err)
	}
	// 官方语义：重复 close 成功。
	if err := api.TerminalClose(ctx, "sess-a", "t1"); err != nil {
		t.Fatalf("重复 close 必须成功（官方语义），拿到 %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	after := readBeat()
	time.Sleep(400 * time.Millisecond)
	if now := readBeat(); now != after {
		t.Fatalf("close 之后进程树还在写心跳：%q → %q", after, now)
	}

	if err := api.Input("sess-a", "t1", "echo x\n"); !errors.Is(err, transport.ErrJTOfficialTerminalGone) {
		t.Fatalf("关掉的终端不许还能写，拿到 %v", err)
	}
	// 官方：关掉的身份在 list 里消失，attach 报 unavailable（本层 = 404 gone）。
	if _, err := api.TerminalAttach(ctx, "sess-a", "t1"); !errors.Is(err, transport.ErrJTOfficialTerminalGone) {
		t.Fatalf("close 之后 attach 必须报 gone，拿到 %v", err)
	}
	if list, err := api.TerminalList("sess-a"); err != nil || len(list) != 0 {
		t.Fatalf("close 之后 list 不许再有它：%v %+v", err, list)
	}
}

// TestOfficialJobTerminal_TerminalRetainIsRealAndPersists 证明 retain 有两个真实效果：
// 落盘（重启后仍能看到身份 + 屏幕尾部）与持有（不被无人值守回收）。
func TestOfficialJobTerminal_TerminalRetainIsRealAndPersists(t *testing.T) {
	cwd := t.TempDir()
	api, home := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("RETAIN-%d", time.Now().UnixNano())
	if err := api.Input("sess-a", "t1", "echo "+marker+"\n"); err != nil {
		t.Fatal(err)
	}
	waitScreen(t, api, "sess-a", "t1", marker)

	if err := api.TerminalRename(ctx, "sess-a", "t1", "构建日志"); err != nil {
		t.Fatalf("rename 失败：%v", err)
	}
	if err := api.TerminalRetain(ctx, "sess-a", "t1"); err != nil {
		t.Fatalf("retain 失败：%v", err)
	}

	// 落盘是真实文件，不是内存里的旗标。
	layout := filepath.Join(home, "state", "terminals", "sess-a.json")
	raw, err := os.ReadFile(layout)
	if err != nil {
		t.Fatalf("retain 之后必须有落盘布局 %s：%v", layout, err)
	}
	var persisted persistedTerminalLayout
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("布局不是合法 JSON：%v", err)
	}
	if len(persisted.Terminals) != 1 || persisted.Terminals[0].Title != "构建日志" || !persisted.Terminals[0].Retained {
		t.Fatalf("落盘内容不对：%+v", persisted.Terminals)
	}
	if !strings.Contains(persisted.Terminals[0].Screen, marker) {
		t.Fatalf("retain 要留下屏幕尾部（含 %q），实际 %q", marker, persisted.Terminals[0].Screen)
	}

	// 模拟宿主重启：同一 home 上的新实例应当恢复"被保留的身份"。
	svc2 := bash.NewService(cwd)
	defer svc2.Close()
	api2, err := newOfficialTerminalAPI(home, svc2,
		func(string) (string, error) { return cwd, nil }, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer api2.Close()
	list, err := api2.TerminalList("sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("重启后要恢复 1 条被保留的终端，拿到 %d：%+v", len(list), list)
	}
	got := list[0]
	if got.ID != "t1" || got.Title != "构建日志" {
		t.Fatalf("恢复的身份不对：%+v", got)
	}
	if got.State != "exited" {
		t.Fatalf("重启后进程已不存在，state 必须是 exited（不许假装 running），拿到 %q", got.State)
	}
	if got.ExitCode != nil {
		t.Fatalf("拿不到退出码时必须报 null，拿到 %v", *got.ExitCode)
	}
	snap, err := api2.TerminalAttach(ctx, "sess-a", "t1")
	if err != nil {
		t.Fatalf("恢复的身份要能 attach：%v", err)
	}
	if snap.Writable {
		t.Fatal("恢复的已退出终端必须 writable=false")
	}
	if !strings.Contains(snap.Screen, marker) {
		t.Fatalf("恢复的屏幕尾部要含 %q，实际 %q", marker, snap.Screen)
	}
}

// TestOfficialJobTerminal_TerminalRetainBlocksReclaim 证明"持有"是真的：
// 同样的空闲阈值下，被 retain 的不被回收，没被 retain 的被回收。
func TestOfficialJobTerminal_TerminalRetainBlocksReclaim(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 200*time.Millisecond)
	ctx := context.Background()
	for _, id := range []string{"kept", "dropped"} {
		if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: id, Cols: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.TerminalRetain(ctx, "sess-a", "kept"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := api.TerminalList("sess-a")
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(list))
		for _, info := range list {
			ids = append(ids, info.ID)
		}
		if len(ids) == 1 && ids[0] == "kept" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("无人值守回收没生效：列表还是 %v", ids)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 被回收的那条真的没了。
	if _, err := api.TerminalAttach(ctx, "sess-a", "dropped"); !errors.Is(err, transport.ErrJTOfficialTerminalGone) {
		t.Fatalf("被回收的终端应当已消失，拿到 %v", err)
	}
	if snap, err := api.TerminalAttach(ctx, "sess-a", "kept"); err != nil || !snap.Writable {
		t.Fatalf("被 retain 的终端必须还活着：%v %+v", err, snap)
	}
}

// TestOfficialJobTerminal_TerminalCreateIdempotent 证明官方幂等语义与"关闭即退役"。
func TestOfficialJobTerminal_TerminalCreateIdempotent(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	req := transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 80, Rows: 24}
	first, err := api.TerminalCreate(ctx, "sess-a", req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := api.TerminalCreate(ctx, "sess-a", req)
	if err != nil {
		t.Fatalf("同参数重复 create 必须成功（幂等）：%v", err)
	}
	if first.Shell.Path != second.Shell.Path || first.ID != second.ID {
		t.Fatalf("幂等返回的身份不一致：%+v vs %+v", first, second)
	}
	if list, _ := api.TerminalList("sess-a"); len(list) != 1 {
		t.Fatalf("幂等 create 不许起第二条会话，列表 %d 条", len(list))
	}
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 100, Rows: 40}); !errors.Is(err, transport.ErrJTOfficialBadParam) {
		t.Fatalf("同 id 换参数必须 fail-loud，拿到 %v", err)
	}
	if err := api.TerminalClose(ctx, "sess-a", "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.TerminalCreate(ctx, "sess-a", req); !errors.Is(err, transport.ErrJTOfficialTerminalGone) {
		t.Fatalf("已关闭的身份不许复活，拿到 %v", err)
	}
	// 换一条会话可以用同一个 id（身份作用域是会话）。
	if _, err := api.TerminalCreate(ctx, "sess-b", req); err != nil {
		t.Fatalf("另一个会话用同一 id 必须能建：%v", err)
	}
}

// TestOfficialJobTerminal_TerminalEnvironmentReal 证明 environment 的每个数都有强制点。
func TestOfficialJobTerminal_TerminalEnvironmentReal(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	env, err := api.TerminalEnvironment(context.Background(), "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if env.Cwd != cwd {
		t.Fatalf("cwd 要来自会话真值（%q），拿到 %q", cwd, env.Cwd)
	}
	if env.MaxInputBytes != officialTerminalMaxInputBytes {
		t.Fatalf("maxInputBytes 要等于真实强制值 %d，拿到 %d", officialTerminalMaxInputBytes, env.MaxInputBytes)
	}
	if env.MaxBufferedBytes != officialTerminalMaxScreenBytes {
		t.Fatalf("maxBufferedBytes 要等于真实快照上限 %d，拿到 %d", officialTerminalMaxScreenBytes, env.MaxBufferedBytes)
	}
	if env.MaxCols != officialTerminalMaxCols || env.MaxRows != officialTerminalMaxRows {
		t.Fatalf("maxCols/maxRows 不对：%d×%d", env.MaxCols, env.MaxRows)
	}
	if env.Scrollback != 0 {
		t.Fatalf("没有 PTY 就没有行滚动缓冲，必须如实报 0，拿到 %d", env.Scrollback)
	}
}

// TestOfficialJobTerminal_TerminalShellsAreProbed 证明 shells 是真实探测，不是硬编码清单。
func TestOfficialJobTerminal_TerminalShellsAreProbed(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	shells, err := api.TerminalShells(context.Background(), "sess-a")
	if err != nil {
		t.Fatalf("shells 失败：%v", err)
	}
	if len(shells) == 0 {
		t.Fatal("至少要报出默认 shell")
	}
	def := shells[0]
	if def.Supported == nil || !*def.Supported {
		t.Fatalf("第一条必须是可用的默认 shell：%+v", def)
	}
	// supported 的判据必须和 create 同源：默认恒真；非默认只在 Windows 上真。
	for i, shell := range shells {
		if shell.Supported == nil {
			t.Fatalf("第 %d 条缺 supported：%+v", i, shell)
		}
		want := i == 0 || runtime.GOOS == "windows"
		if *shell.Supported != want {
			t.Fatalf("%s 的 supported=%v，按 create 判据应当是 %v", shell.Path, *shell.Supported, want)
		}
	}
	for _, shell := range shells {
		if _, err := os.Stat(shell.Path); err != nil {
			t.Fatalf("报出来的 shell 必须真实存在：%s（%v）", shell.Path, err)
		}
		if shell.Args == nil {
			t.Fatalf("args 必须是数组：%+v", shell)
		}
		kind := strings.ToLower(strings.TrimSuffix(filepath.Base(shell.Path), filepath.Ext(shell.Path)))
		switch kind {
		case "cmd":
			if len(shell.Args) != 0 {
				t.Fatalf("cmd 的参数按官方要为空：%+v", shell)
			}
		case "pwsh", "powershell":
			if len(shell.Args) != 1 || shell.Args[0] != "-NoLogo" {
				t.Fatalf("pwsh/powershell 的参数按官方要是 -NoLogo：%+v", shell)
			}
		}
	}
	// 默认 shell 必须能真跑（否则"可用"就是假的）。
	if _, err := api.TerminalCreate(context.Background(), "sess-a",
		transport.OfficialTerminalCreateRequest{ID: "probe", Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("默认 shell 起不来：%v", err)
	}
}

// TestOfficialJobTerminal_TerminalCreateOtherShell 证明选了别的 shell 就真的切过去。
// 机器上没有 cmd.exe 就跳过（不是失败）。
func TestOfficialJobTerminal_TerminalCreateOtherShell(t *testing.T) {
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skip("这台机器上没有 cmd.exe")
	}
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	info, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{
		ID: "c1", Cols: 80, Rows: 24, ShellPath: cmdPath,
	})
	if err != nil {
		t.Fatalf("用 cmd.exe 建终端失败：%v", err)
	}
	if !strings.EqualFold(filepath.Base(info.Shell.Path), "cmd.exe") {
		t.Fatalf("回填的 shell 应当是被选中的 cmd.exe，拿到 %q", info.Shell.Path)
	}
	// `ver` 只有 cmd 认；bash 认不出这个命令（会报 command not found）。
	if err := api.Input("sess-a", "c1", "ver\r\n"); err != nil {
		t.Fatalf("写 cmd 失败：%v", err)
	}
	snap := waitScreen(t, api, "sess-a", "c1", "Microsoft")
	if !strings.Contains(strings.ToLower(snap.Screen), "microsoft") {
		t.Fatalf("cmd 没真的跑起来（screen=%q）", snap.Screen)
	}

	// 同一个引导路径换 pwsh：装了就验，没装就跳过（不假装支持）。
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Log("没有 pwsh，跳过第二语言 shell 验证")
		return
	}
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{
		ID: "p1", Cols: 80, Rows: 24, ShellPath: pwsh,
	}); err != nil {
		t.Fatalf("用 pwsh 建终端失败：%v", err)
	}
	if err := api.Input("sess-a", "p1", "Write-Output ('PSEDITION=' + $PSVersionTable.PSEdition)\n"); err != nil {
		t.Fatalf("写 pwsh 失败：%v", err)
	}
	// PSEDITION=Core 只有 PowerShell 认得；bash 只会报 command not found。
	waitScreen(t, api, "sess-a", "p1", "PSEDITION=Core")
}

// TestOfficialJobTerminal_TerminalInputLimit 证明 maxInputBytes 是被强制的，不是只报出来的数。
func TestOfficialJobTerminal_TerminalInputLimit(t *testing.T) {
	cwd := t.TempDir()
	api, _ := newTestTerminalAPI(t, cwd, 0)
	ctx := context.Background()
	if _, err := api.TerminalCreate(ctx, "sess-a", transport.OfficialTerminalCreateRequest{ID: "t1", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", officialTerminalMaxInputBytes+1)
	if err := api.Input("sess-a", "t1", big); !errors.Is(err, transport.ErrJTOfficialBadParam) {
		t.Fatalf("超过 maxInputBytes 必须拒绝，拿到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// job.*：包在**真实** jobActionBroker 之上
// ---------------------------------------------------------------------------

// writeRetainedJob 写既有 jobs 服务认的保留输出文件（`readRetained` 的路径）。
func writeRetainedJob(t *testing.T, home, sessionID, id, text string, done bool, exitCode int) {
	t.Helper()
	dir := filepath.Join(home, "state", "jobs", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := transport.JobOutput{ID: id, SessionID: sessionID, Output: text, Done: done, ExitCode: exitCode, Total: len([]rune(text))}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOfficialJobTerminal_JobListFencesSessions 证明清单按会话围栏，且字段换算如实。
func TestOfficialJobTerminal_JobListFencesSessions(t *testing.T) {
	roster := func(sessionID string) []transport.JobInfo {
		return []transport.JobInfo{
			{ID: "job-a1", SessionID: "sess-a", Command: "npm test\n--watch", Done: false, Bytes: 42},
			{ID: "job-b1", SessionID: "sess-b", Command: "sleep 1", Done: true, ExitCode: 0, Bytes: 7},
			{ID: "job-free", SessionID: "", Command: "unowned", Done: true, ExitCode: 3, Bytes: 0},
		}
	}
	svc := &officialJobService{roster: roster}
	frame, err := svc.JobList("sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != "rows" {
		t.Fatalf("type 必须是 rows，拿到 %q", frame.Type)
	}
	ids := []string{}
	for _, job := range frame.Jobs {
		ids = append(ids, job.ID)
	}
	if len(ids) != 2 || ids[0] != "job-a1" || ids[1] != "job-free" {
		t.Fatalf("A 会话只该看到自己的 + 未归属的：%v", ids)
	}
	running := frame.Jobs[0]
	if running.Status != "running" || running.Owner != "sess-a" || running.Kind != "shell" {
		t.Fatalf("运行中任务的换算不对：%+v", running)
	}
	if running.Label != "npm test" {
		t.Fatalf("label 要压成一行（命令首行），拿到 %q", running.Label)
	}
	if running.Output.Total != 42 {
		t.Fatalf("output.total 要等于写出字节数 42，拿到 %d", running.Output.Total)
	}
	if running.StartedAt != 0 {
		t.Fatalf("既有服务没有登记时间，startedAt 必须如实报 0，拿到 %d", running.StartedAt)
	}
	failed := frame.Jobs[1]
	if failed.Owner != "" {
		t.Fatalf("未归属任务不许乱填 owner：%+v", failed)
	}
	if failed.Status != "failed" || failed.Detail != "exit code: 3" {
		t.Fatalf("非零退出要如实成 failed + detail：%+v", failed)
	}

	frameB, err := svc.JobList("sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(frameB.Jobs) != 2 || frameB.Jobs[0].ID != "job-b1" {
		t.Fatalf("B 会话的清单不对：%+v", frameB.Jobs)
	}
	if frameB.Jobs[0].Status != "completed" {
		t.Fatalf("退出码 0 要成 completed，拿到 %q", frameB.Jobs[0].Status)
	}
}

// TestOfficialJobTerminal_JobKillAndFollowReuseRealBroker 用**真实** jobActionBroker 验证
// 409（已结束）与字节窗口/终态帧，不重写任何围栏判断。
func TestOfficialJobTerminal_JobKillAndFollowReuseRealBroker(t *testing.T) {
	home := t.TempDir()
	broker := newJobActionBroker(home)
	svc := &officialJobService{
		output: broker.Output,
		stop:   broker.Stop,
		roster: func(string) []transport.JobInfo { return nil },
	}
	ctx := context.Background()
	writeRetainedJob(t, home, "sess-a", "job-1", "hello\nworld\n", true, 0)

	// 已结束 → 既有服务回 ErrJobFinished（传输层据此 409）。
	if _, err := svc.JobKill(ctx, "sess-a", "job-1"); !errors.Is(err, transport.ErrJobFinished) {
		t.Fatalf("已结束的任务要 ErrJobFinished，拿到 %v", err)
	}
	// 不存在 → ErrNoSuchJob。
	if _, err := svc.JobKill(ctx, "sess-a", "job-missing"); !errors.Is(err, transport.ErrNoSuchJob) {
		t.Fatalf("不存在的任务要 ErrNoSuchJob，拿到 %v", err)
	}

	frame, err := svc.JobFollow(ctx, "sess-a", "job-1", 0)
	if err != nil {
		t.Fatalf("follow 失败：%v", err)
	}
	if frame.Type != "output" || len(frame.Chunks) != 1 {
		t.Fatalf("要回一帧 output：%+v", frame)
	}
	if frame.Chunks[0].Text != "hello\nworld\n" || frame.Chunks[0].At != 0 {
		t.Fatalf("字节窗口不对：%+v", frame.Chunks[0])
	}
	if frame.Next == nil || *frame.Next != int64(len("hello\nworld\n")) {
		t.Fatalf("next 要等于读到的字节数，拿到 %v", frame.Next)
	}

	// 从末尾再读：已结算且排干 → status 帧。
	tail, err := svc.JobFollow(ctx, "sess-a", "job-1", int64(len("hello\nworld\n")))
	if err != nil {
		t.Fatalf("follow 末尾失败：%v", err)
	}
	if tail.Type != "status" || tail.Job == nil {
		t.Fatalf("排干后要回 status 帧：%+v", tail)
	}
	if tail.Job.Status != "completed" {
		t.Fatalf("终态应当是 completed，拿到 %q", tail.Job.Status)
	}

	// 请求超过末尾 → 停在末尾，不编造数据。
	over, err := svc.JobFollow(ctx, "sess-a", "job-1", 999)
	if err != nil {
		t.Fatalf("越界 from 不该报错（应当回终态）：%v", err)
	}
	if over.Type != "status" {
		t.Fatalf("越界 from 应当回 status 帧，拿到 %q", over.Type)
	}
}

// TestOfficialJobTerminal_JobKillCrossSession 用真实 broker 的"别的会话有活进程"分支证 403。
func TestOfficialJobTerminal_JobKillCrossSession(t *testing.T) {
	if _, err := exec.LookPath("cmd.exe"); err != nil {
		t.Skip("这台机器上没有 cmd.exe")
	}
	home := t.TempDir()
	broker := newJobActionBroker(home)
	svc := &officialJobService{stop: broker.Stop, output: broker.Output}

	// 起一个**本测试自己的**子进程，并把它登记成 sess-b 的宿主进程。
	cmdPath, _ := exec.LookPath("cmd.exe")
	child := exec.Command(cmdPath, "/c", "timeout", "/t", "30")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("起测试子进程失败：%v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	run, err := broker.registerRun("sess-b", child.Process.Pid, cmdPath, stdin)
	if err != nil {
		t.Fatalf("登记测试进程失败：%v", err)
	}
	defer broker.unregisterRun(run)

	// A 会话没有任何活进程，却想杀 job-x：既有服务判成 cross_session。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := svc.JobKill(ctx, "sess-a", "job-x"); !errors.Is(err, transport.ErrCrossSession) {
		t.Fatalf("跨会话杀任务必须 ErrCrossSession，拿到 %v", err)
	}
}
