package main

// 父进程看门狗的两层证据：
//  1. **规则层**（脚本化探针）：连续 3 次探不到才触发；中间活着一次就清零；
//     启动时父进程已不在 / 开关关掉 → 只记一行，永不触发。
//  2. **真机形态**（真进程）：父进程是真的进程，**它自己退出**（不 kill 子进程、
//     不 wait），被监护的进程必须自己停。这一条是红过的：没有看门狗时，
//     子进程在父进程死掉之后仍然活着——正是 2026-11-04 那 33 个孤儿。

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeParentProbe 按剧本回答"还活着吗"，并数被问了几次。
// 用剧本而不是真进程：这条测试要钉的是**计数规则**，不能靠睡时间来赌调度。
type fakeParentProbe struct {
	mu       sync.Mutex
	plan     []bool // 依次回答；用完之后一律返回 fallback
	fallback bool
	calls    int
	closed   bool
}

func (p *fakeParentProbe) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.plan) > 0 {
		v := p.plan[0]
		p.plan = p.plan[1:]
		return v
	}
	return p.fallback
}

func (p *fakeParentProbe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakeParentProbe) Note() string { return "" }

func (p *fakeParentProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakeParentProbe) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// waitFor 每 20ms 试一次，直到 cond 成立或超时。失败时 caller 自己报。
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestParentWatchTripsAfterMisses 同时钉两件事：**必须**在连续 3 次探不到之后
// 触发，而且**恰好**是 3 次——2 次就触发等于误杀，4 次等于慢半拍。
//
// 判据用探测次数而不是墙钟：循环是串行的（一次 tick 一次探测），
// 所以触发时的调用次数是确定的：起点 1 次 + 2 次活着 + 3 次探不到 = 6。
func TestParentWatchTripsAfterMisses(t *testing.T) {
	probe := &fakeParentProbe{plan: []bool{true, true, true, false, false, false}}
	var log bytes.Buffer
	w := startParentWatchWith(parentWatchConfig{
		Enabled: true, PID: 4321, Interval: 20 * time.Millisecond,
		Misses: defaultParentWatchMisses, Log: &log,
	}, func(int) parentProbe { return probe })
	defer w.Stop()

	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("父进程连续 %d 次探不到，宿主却没停（探测 %d 次）",
			defaultParentWatchMisses, probe.callCount())
	}

	if got := probe.callCount(); got != 6 {
		t.Fatalf("应该在**第 6 次**探测时触发（起点 + 2 次活着 + 3 次探不到），实际 %d 次", got)
	}
	wantReason := "父进程 4321 已退出，宿主自动停止"
	if got := w.Reason(); got != wantReason {
		t.Fatalf("触发原因应是 %q，实际 %q", wantReason, got)
	}
	// stderr 上必须**只有一行**可查的原因：多行会被当成多条日志，
	// 一行里塞了换行则会让壳/日志切分把它当成两件事。
	lines := strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("应该只有一行原因，实际 %d 行：%q", len(lines), log.String())
	}
	if !strings.Contains(lines[0], strconv.Itoa(4321)) || !strings.Contains(lines[0], "已退出") {
		t.Fatalf("那一行要带父进程 PID 与「已退出」：%q", lines[0])
	}
	if w.Watching() == false {
		t.Fatalf("触发之后仍然应该报告「在监护过」（只是已经结束）")
	}
}

// TestParentWatchSingleMissDoesNotTrip 钉住"连续"两个字：
// 中间只要探到一次活着就清零，两次孤立的探不到**不许**触发。
func TestParentWatchSingleMissDoesNotTrip(t *testing.T) {
	probe := &fakeParentProbe{
		plan:     []bool{true, false, false, true, false, false, true},
		fallback: true, // 剧本用完之后一直活着
	}
	var log bytes.Buffer
	w := startParentWatchWith(parentWatchConfig{
		Enabled: true, PID: 999, Interval: 20 * time.Millisecond,
		Misses: defaultParentWatchMisses, Log: &log,
	}, func(int) parentProbe { return probe })
	defer w.Stop()

	if !waitFor(2*time.Second, func() bool { return probe.callCount() >= 8 }) {
		t.Fatalf("探针没被问够次数：%d", probe.callCount())
	}
	select {
	case <-w.Done():
		t.Fatalf("两次孤立的探不到就触发了（应该是连续 %d 次才算）：%q",
			defaultParentWatchMisses, w.Reason())
	default:
	}
	if log.Len() != 0 {
		t.Fatalf("没触发就不该往 stderr 写东西：%q", log.String())
	}
}

// TestParentWatchDisabledByFlag：--parent-watch=false 之后**一次都不探**，
// 也不能触发；但必须在 stderr 说清"已经关了"（关掉的看门狗与没有看门狗，
// 在进程留下来之后长得一模一样）。
func TestParentWatchDisabledByFlag(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	pw := addParentWatchFlags(fs)
	if err := fs.Parse([]string{"--parent-watch=false"}); err != nil {
		t.Fatal(err)
	}
	asked := 0
	var log bytes.Buffer
	w := startParentWatchWith(parentWatchConfig{
		Enabled: *pw.enabled, PID: 1, Interval: 20 * time.Millisecond, Log: &log,
	}, func(int) parentProbe { asked++; return &fakeParentProbe{fallback: false} })
	defer w.Stop()

	select {
	case <-w.Done():
		t.Fatal("关掉看门狗之后还在触发")
	case <-time.After(200 * time.Millisecond):
	}
	if asked != 0 {
		t.Fatalf("关掉之后不该再去探父进程，实际探了 %d 次", asked)
	}
	if w.Watching() {
		t.Fatal("关掉之后 Watching() 必须是 false")
	}
	if !strings.Contains(log.String(), "已关闭") {
		t.Fatalf("关掉也要说一声：%q", log.String())
	}
}

// TestParentWatchFlagsDefaults：默认必须是**开**。
//
// 这条是防漂移：把默认改成 false 能让所有测试一下子全绿，
// 而现象（孤儿 dshd 继续堆着）要等到下次翻任务管理器才看得见。
func TestParentWatchFlagsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	pw := addParentWatchFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if !*pw.enabled {
		t.Fatal("--parent-watch 默认必须是 true（泄漏就是这么来的）")
	}
	if *pw.interval != defaultParentWatchInterval {
		t.Fatalf("默认间隔应是 %v，实际 %v", defaultParentWatchInterval, *pw.interval)
	}
	for _, name := range []string{"parent-watch", "parent-watch-interval", "parent-pid"} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("缺 --%s", name)
		}
		// 只有名字没有说明 = 等于没写（scripts/help-guard.py 也钉这条）。
		if strings.TrimSpace(f.Usage) == "" {
			t.Fatalf("--%s 缺说明文字", name)
		}
	}
}

// TestParentWatchParentAlreadyGoneAtStartup：启动时父进程就不在了（systemd /
// 服务 / 双击启动），**不许**立刻退出，只记一行说明。
func TestParentWatchParentAlreadyGoneAtStartup(t *testing.T) {
	probe := &fakeParentProbe{plan: []bool{false}, fallback: false}
	var log bytes.Buffer
	w := startParentWatchWith(parentWatchConfig{
		Enabled: true, PID: 4242, Interval: 20 * time.Millisecond, Log: &log,
	}, func(int) parentProbe { return probe })
	defer w.Stop()

	select {
	case <-w.Done():
		t.Fatalf("启动时父进程已不在，不该据此退出：%q", w.Reason())
	case <-time.After(200 * time.Millisecond):
	}
	if w.Watching() {
		t.Fatal("启动时父进程已不在 → 看门狗不启动（Watching() 应为 false）")
	}
	if got := probe.callCount(); got != 1 {
		t.Fatalf("只该做那一次起点探测，实际 %d 次", got)
	}
	if !probe.isClosed() {
		t.Fatal("探针要收掉（句柄不还给系统就是另一种泄漏）")
	}
	if !strings.Contains(log.String(), "启动时已不存在") {
		t.Fatalf("要留一行说明：%q", log.String())
	}
	if n := strings.Count(log.String(), "\n"); n != 1 {
		t.Fatalf("说明只该一行，实际 %d 行：%q", n, log.String())
	}
}

// TestResolveParentPIDOrder：显式 > 环境变量 > os.Getppid()。
// 两种传法都要能工作——脚本/壳知道自己看着谁时显式传，普通父子关系下靠 getppid。
func TestResolveParentPIDOrder(t *testing.T) {
	t.Setenv("DSHD_PARENT_PID", "777")
	if got := resolveParentPID(0); got != 777 {
		t.Fatalf("环境变量没被用上：%d", got)
	}
	if got := resolveParentPID(888); got != 888 {
		t.Fatalf("显式 --parent-pid 必须赢过环境变量：%d", got)
	}
	if got := resolveParentPID(-1); got != -1 {
		t.Fatalf("负数 = 显式不监护，要原样保留：%d", got)
	}
	t.Setenv("DSHD_PARENT_PID", "")
	if got := resolveParentPID(0); got != os.Getppid() {
		t.Fatalf("都没有时应该用 os.Getppid()=%d，实际 %d", os.Getppid(), got)
	}
	t.Setenv("DSHD_PARENT_PID", "not-a-number")
	if got := resolveParentPID(0); got != os.Getppid() {
		t.Fatalf("环境变量不是数字时应该退回 os.Getppid()=%d，实际 %d", os.Getppid(), got)
	}
}

// TestParentWatchWiredInAllThreeEntrypoints 是接线守卫。
//
// 为什么要有它：定义开关只要三行，接到循环里要改三处。漏一处**不报错、不冒烟**，
// 表现就是"那个入口还是会在父进程死后变成孤儿"——而这正是本次要修的 bug。
func TestParentWatchWiredInAllThreeEntrypoints(t *testing.T) {
	// go test 的工作目录就是包目录，所以这里用裸文件名。
	for _, path := range []string{
		"serve.go", "acp.go", "main.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s: %v", path, err)
		}
		text := string(src)
		if !strings.Contains(text, "addParentWatchFlags(fs)") {
			t.Errorf("%s 没挂 --parent-watch 开关组（addParentWatchFlags）", path)
		}
		if !strings.Contains(text, "<-watch.Done()") {
			t.Errorf("%s 没把 watch.Done() 接进收尾分支：开关接上了但没人听，等于没接", path)
		}
		if !strings.Contains(text, "watch.Stop()") {
			t.Errorf("%s 没有 Stop()：正常退出时探针句柄不会还", path)
		}
	}
}

// TestWindowsProbeDoesNotUseFindProcess 钉住那条 Windows 特有的坑：
// os.FindProcess 在 Windows 上对任何 PID 都成功，不能拿来判活。
func TestWindowsProbeDoesNotUseFindProcess(t *testing.T) {
	src, err := os.ReadFile("parentwatch_windows.go")
	if err != nil {
		t.Fatalf("读 parentwatch_windows.go: %v", err)
	}
	text := string(src)
	if strings.Contains(text, "os.FindProcess(") {
		t.Fatal("Windows 上 os.FindProcess 永远成功，不能用来判活")
	}
	for _, must := range []string{"OpenProcess", "GetExitCodeProcess", "STILL_ACTIVE"} {
		if !strings.Contains(text, must) {
			t.Errorf("Windows 探针缺 %q", must)
		}
	}
}

// ---- 真进程形态 ----

// TestParentWatchHelper 不是测试：它是**子进程里的角色**，由下面两条测试启动。
// 三种角色：
//
//	parent  —— 中间进程：起一个被监护的子进程，读到它的 ready 之后**自己退出**
//	           （不 kill 子进程、不 wait）——真机上 python 被打掉就是这个样子。
//	guarded —— 被监护的进程：只看父进程，父进程没了就写证据再退出。
//	idle    —— 一块"活着的靶子"，用来测探针本身。
func TestParentWatchHelper(t *testing.T) {
	role := os.Getenv("PW_HELPER_ROLE")
	if role == "" {
		t.Skip("只在子进程里跑（PW_HELPER_ROLE 未设）")
	}
	switch role {
	case "parent":
		helperParent()
	case "guarded":
		helperGuarded()
	case "idle":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知角色 %q\n", role)
		os.Exit(2)
	}
}

// helperParent 起被监护的子进程，读到 ready 后自己退出。
//
// 两个细节：
//   - 等 ready 再退：否则子进程可能"启动时父进程就已经不在了"，
//     那是另一个分支（不退出），这条测试就测不到想测的东西。
//   - 先睡 500ms：让子进程的起点探测落到"父进程还在"上。
func helperParent() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "取自身路径失败: %v\n", err)
		os.Exit(2)
	}
	child := exec.Command(exe, "-test.run=TestParentWatchHelper", "-test.timeout=120s")
	child.Env = append(os.Environ(),
		"PW_HELPER_ROLE=guarded",
		"PW_LOG="+os.Getenv("PW_LOG"),
		"PW_EXIT="+os.Getenv("PW_EXIT"),
		"PW_PID_MODE="+os.Getenv("PW_PID_MODE"),
	)
	out, err := child.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "拿不到子进程 stdout: %v\n", err)
		os.Exit(2)
	}
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "起不来子进程: %v\n", err)
		os.Exit(2)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "guarded-ready") {
		fmt.Fprintf(os.Stderr, "没等到子进程 ready（%q, %v）\n", line, err)
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("PW_PIDFILE"),
		[]byte(fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写 pid 文件失败: %v\n", err)
		os.Exit(2)
	}
	time.Sleep(500 * time.Millisecond)
	// 父进程"崩了"：不 kill、不 wait，子进程原地变孤儿。
	os.Exit(0)
}

// helperGuarded 是那个"本来会变成孤儿"的进程。
func helperGuarded() {
	mode := os.Getenv("PW_PID_MODE")
	if mode == "env" {
		// 走环境变量那条路：显式告诉它看谁（不要用 os.Getppid()）。
		os.Setenv("DSHD_PARENT_PID", strconv.Itoa(os.Getppid()))
	}
	logf, err := os.OpenFile(os.Getenv("PW_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "开日志失败: %v\n", err)
		os.Exit(2)
	}
	w := startParentWatch(parentWatchConfig{
		Enabled: true,
		PID:     resolveParentPID(0),
		// 单测里用 100ms 而不是默认 1s：规则一样，只是不陪它睡 4 秒。
		Interval: 100 * time.Millisecond,
		Misses:   defaultParentWatchMisses,
		Log:      logf,
	})
	defer w.Stop()

	fmt.Printf("guarded-ready %d\n", os.Getpid())
	os.Stdout.Sync()

	select {
	case <-w.Done():
		appendExit(os.Getenv("PW_EXIT"), "fired: "+w.Reason())
		logf.Close()
		os.Exit(0)
	case <-time.After(30 * time.Second):
		// 兜底：这条测试失败时也不留下真孤儿（自己回收自己）。
		appendExit(os.Getenv("PW_EXIT"), "timeout")
		logf.Close()
		os.Exit(1)
	}
}

func appendExit(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintln(f, line)
	f.Close()
}

func readPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 pid 文件 %s: %v", path, err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		t.Fatalf("pid 文件格式不对：%q", string(b))
	}
	a, err1 := strconv.Atoi(fields[0])
	b2, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("pid 文件不是两个数字：%q", string(b))
	}
	return a, b2
}

// TestParentWatchStopsWhenParentExits 是这条修复的**真机形态**单测：
// 父进程（真进程）自己退出，被监护的进程必须自己停。
//
// 跑两遍：PID 从 os.Getppid() 拿、以及从 DSHD_PARENT_PID 拿——
// 两种传法都要能工作，不能只有一种"看起来接了"。
func TestParentWatchStopsWhenParentExits(t *testing.T) {
	for _, mode := range []string{"auto", "env"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatalf("取测试二进制路径: %v", err)
			}
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pids.txt")
			watchLog := filepath.Join(dir, "watch.log")
			exitFile := filepath.Join(dir, "exit.txt")

			// 父进程的 stdout 直接落文件（不是管道）：管道会让 Wait 等孙子进程
			// 把写端也关掉，于是"父进程已退出"这件事看不出来。
			parentOut, err := os.Create(filepath.Join(dir, "parent.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer parentOut.Close()

			parent := exec.Command(exe, "-test.run=TestParentWatchHelper", "-test.timeout=120s")
			parent.Env = append(os.Environ(),
				"PW_HELPER_ROLE=parent",
				"PW_PIDFILE="+pidFile,
				"PW_LOG="+watchLog,
				"PW_EXIT="+exitFile,
				"PW_PID_MODE="+mode,
			)
			parent.Stdout = parentOut
			parent.Stderr = parentOut
			if err := parent.Start(); err != nil {
				t.Fatalf("起父进程: %v", err)
			}
			if err := parent.Wait(); err != nil {
				b, _ := os.ReadFile(filepath.Join(dir, "parent.log"))
				t.Fatalf("父进程应当正常退出（它只是 os.Exit(0)）: %v\n%s", err, b)
			}

			// 到这里父进程已经没了 —— 真机上 python 被杀之后就是这个状态。
			parentPID, guardedPID := readPIDs(t, pidFile)
			if parentPID == os.Getpid() {
				t.Fatal("父进程 PID 不该等于测试进程自己的 PID")
			}

			deadline := 10 * time.Second
			if !waitFor(deadline, func() bool {
				b, err := os.ReadFile(exitFile)
				return err == nil && strings.Contains(string(b), "fired")
			}) {
				b, _ := os.ReadFile(exitFile)
				lg, _ := os.ReadFile(watchLog)
				t.Fatalf("父进程 %d 退出 %v 之后，被监护进程 %d 还没自己停（exit=%q log=%q）",
					parentPID, deadline, guardedPID, b, lg)
			}

			// 证据一：那一行原因，且指向**真的父进程**。
			exit, _ := os.ReadFile(exitFile)
			want := "fired: " + parentGoneReason(parentPID)
			if !strings.Contains(string(exit), want) {
				t.Fatalf("退出原因应是 %q，实际 %q", want, string(exit))
			}
			// 证据二：stderr 上恰好一行（可查），内容一致。
			lg, err := os.ReadFile(watchLog)
			if err != nil {
				t.Fatalf("读看门狗日志: %v", err)
			}
			lines := strings.Split(strings.TrimSuffix(string(lg), "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], parentGoneReason(parentPID)) {
				t.Fatalf("stderr 上应恰好一行原因，实际 %q", string(lg))
			}
			// 证据三：进程真的没了（不只是"写了文件"）。
			//
			// 这里必须**等**它消失，不能立刻断言：被监护进程先写 exit 文件、再关日志、
			// 再 os.Exit，中间有几毫秒。直接 Alive() 会把"正在退出的进程"判成"还在"，
			// 于是这条测试会随机红——而它红的理由和看门狗无关。（实测踩过：auto 那遍红、
			// env 那遍绿，差别只是谁先跑完最后那几行。）
			probe := openParentProbe(guardedPID)
			defer probe.Close()
			if !waitFor(3*time.Second, func() bool { return !probe.Alive() }) {
				t.Fatalf("被监护进程 %d 在 3 秒内没有退出（应该已经退出）", guardedPID)
			}
		})
	}
}

// TestParentProbeLiveThenDead 单独验探针本身：**活着的进程判活、杀掉判死**。
// 这是"Windows 上不能靠 os.FindProcess"那条的正面证据。
func TestParentProbeLiveThenDead(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestParentWatchHelper", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "PW_HELPER_ROLE=idle")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起 idle 子进程: %v", err)
	}
	// 自己起的才回收：只按这个 PID 收（先杀再 wait，绝不按镜像名杀）。
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	target := cmd.Process.Pid
	probe := openParentProbe(target)
	defer probe.Close()

	if !waitFor(5*time.Second, probe.Alive) {
		t.Fatalf("刚起的子进程 %d 应该判活", target)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("回收自己起的子进程 %d: %v", target, err)
	}
	if !waitFor(5*time.Second, func() bool { return !probe.Alive() }) {
		t.Fatalf("子进程 %d 已经杀掉，探针还说它活着", target)
	}
}

// TestParentProbeGoneForDeadPID：起点探测遇到"根本不存在的 PID"必须报死
// （这就是"启动时父进程已不在"在真机上的样子）。
func TestParentProbeGoneForDeadPID(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径: %v", err)
	}
	// 起一个马上退出的子进程，回收掉 —— 然后拿它已经死掉的 PID 当探针输入。
	cmd := exec.Command(exe, "-test.run=TestParentWatchHelper", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "PW_HELPER_ROLE=none")
	if err := cmd.Run(); err == nil {
		t.Fatal("PW_HELPER_ROLE=none 应该以非 0 退出")
	}
	gone := cmd.Process.Pid
	probe := openParentProbe(gone)
	defer probe.Close()
	if probe.Alive() {
		t.Fatalf("PID %d 已经退出并被回收，探针却报活着", gone)
	}
}
