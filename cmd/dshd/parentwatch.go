package main

// 父进程存活看门狗：启动它的那个进程没了，宿主自己就该停。
//
// 为什么需要：`dshd serve` 是**子进程**。启动它的东西（测试脚本、GUI 拉起器、
// 终端里的 python）被 Ctrl-C / 崩溃 / 关窗口之后，它成了孤儿——继续监听端口、
// 占着 home 目录、继续跑，而且**永远不会自己退出**。
//
// 真发现场（2026-11-04）：任务管理器里 34 个 dshd 进程，33 个来自
// `dsh-ui\tmp\`，都是 `serve --home tmp/xxx-home --policy allow`，每个占一个
// 端口、4~5MB，最早 08:04 最新 16:26 —— 一直在泄漏。
//
// 口径（三条都很重要）：
//   - 启动时记下父进程 PID：`--parent-pid` > 环境变量 `DSHD_PARENT_PID` >
//     `os.Getppid()`。两种传法都要能工作：脚本/壳知道自己看着谁时显式传，
//     普通父子关系下用 os.Getppid() 就够。
//   - 定期探一次，**连续** 3 次确认消失才动手。父进程被 Ctrl+Z 挂起、调度卡顿、
//     或探针自己偶发失败，都不该换来一次误杀。
//   - 动手之前在 stderr 留**一行**可查的原因，然后由调用方决定怎么收尾：
//     serve 关端口、acp 取消在飞的轮次、run 中断当前 turn。
//
// **启动时父进程就已经不在了，不退出**：systemd / 服务 / 双击启动这类场景里
// 父进程本来就可能是 1 或早已被回收，那不是泄漏。这种情况只记一行说明，
// 看门狗直接不启动（见 startParentWatchWith 的第二个分支）。
//
// 判活本身是平台相关的：Windows 上 `os.FindProcess` 对任何 PID 都成功，
// 不能拿它判活。见 parentwatch_windows.go / parentwatch_other.go。

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultParentWatchInterval 是默认探测间隔。1 秒：连续 3 次也就是
	// 3~4 秒内发现——够快（人不会在孤儿进程上等），又不至于把一个刚被
	// 挂起的父进程当成死掉。
	defaultParentWatchInterval = time.Second
	// defaultParentWatchMisses 是"连续几次探不到才算数"。
	defaultParentWatchMisses = 3
	// minParentWatchInterval 是间隔下限：防止有人传 `--parent-watch-interval=1ns`
	// 把核烧了（1000 次/秒的进程探测不是免费动作）。
	minParentWatchInterval = 20 * time.Millisecond
)

// parentProbe 是"这个 PID 还活着吗"的唯一入口（平台相关实现见另外两个文件）。
//
// Alive 的语义刻意不对称：**拿不准就返回 true**。宁可漏停一个孤儿，
// 也不要因为一次权限不足 / 一次 syscall 失败把正在干活的宿主杀掉。
type parentProbe interface {
	Alive() bool
	Close() error
	// Note 是"这次判活是怎么判的"的一句话说明（比如权限不足只能看退出码）。
	// 空 = 没什么要说的。**看不见的探测方式与没探测一样。**
	Note() string
}

// parentProbeGone 表示"这个 PID 一定不在"：非正数 PID、OpenProcess 说没有。
type parentProbeGone struct{ note string }

func (p *parentProbeGone) Alive() bool  { return false }
func (p *parentProbeGone) Close() error { return nil }
func (p *parentProbeGone) Note() string { return p.note }

// parentWatchConfig 是一次看门狗的全部输入。
type parentWatchConfig struct {
	// Enabled 对应 --parent-watch（默认 true）。
	Enabled bool
	// PID 是要看着的父进程。负数 = 显式不监护（--parent-pid < 0）。
	PID int
	// Interval / Misses 留 0 就用默认值。
	Interval time.Duration
	Misses   int
	// Log 收那一行原因。nil = os.Stderr（serve 的 stdout 是协议，不能用）。
	Log io.Writer
}

// parentWatcher 是看门狗本体。
type parentWatcher struct {
	pid      int
	interval time.Duration
	misses   int
	probe    parentProbe
	logw     io.Writer

	done chan struct{} // 父进程确认消失时关闭；没在监护时**永不关闭**
	stop chan struct{} // 关看门狗
	once sync.Once

	mu       sync.Mutex
	reason   string
	inactive bool
}

// Done 在父进程被确认消失之后关闭。关掉看门狗时这个 channel 永不关闭——
// 所以调用方可以放心 `select { case <-w.Done(): }`，不用判空、不用分支。
func (w *parentWatcher) Done() <-chan struct{} { return w.done }

// Reason 返回触发原因（还没触发 / 没在监护时为空串）。
func (w *parentWatcher) Reason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// Watching 报告看门狗是否真的在探（关掉、启动时父进程已不在，都是 false）。
func (w *parentWatcher) Watching() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.inactive
}

// Stop 关掉看门狗并释放探针句柄。可重复调用。
func (w *parentWatcher) Stop() {
	w.once.Do(func() {
		close(w.stop)
		if w.probe != nil {
			_ = w.probe.Close()
		}
	})
}

// parentGoneReason 是"父进程没了"那一行的**唯一**措辞。
//
// 三个入口共用一句：事后按句子找原因（grep stderr / 搜日志）的人，
// 不该因为入口不同而要找三种说法。
func parentGoneReason(pid int) string {
	return fmt.Sprintf("父进程 %d 已退出，宿主自动停止", pid)
}

// parentWatchLine 往 logw 写一行。所有提示都带前缀，免得和 serve 的
// policy/工具表播报混在一起看不出是谁说的。
func parentWatchLine(w io.Writer, format string, args ...any) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "父进程看门狗："+format+"\n", args...)
}

// resolveParentPID 定出要看着的 PID：显式 > 环境变量 > os.Getppid()。
//
// 三种来源都要能工作：
//   - `--parent-pid 1234`：壳/脚本知道自己看着谁时显式传（最可靠）；
//   - `DSHD_PARENT_PID=1234`：同上，但不必改命令行（壳已经拼好参数时用）；
//   - 都没有：`os.Getppid()`，普通父子关系下就是它。
func resolveParentPID(explicit int) int {
	if explicit < 0 {
		return explicit // 显式"不监护"
	}
	if explicit > 0 {
		return explicit
	}
	if v := strings.TrimSpace(os.Getenv("DSHD_PARENT_PID")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			return n
		}
	}
	return os.Getppid()
}

// startParentWatch 是生产入口：用平台的探针。
func startParentWatch(cfg parentWatchConfig) *parentWatcher {
	return startParentWatchWith(cfg, nil)
}

// startParentWatchWith 起一个看门狗。open 为 nil 时用平台实现
// （测试可以塞一个脚本化的探针，把"连续 3 次"这条规则钉死）。
//
// 返回值永远非 nil：没在监护时 Done() 永不关闭。
func startParentWatchWith(cfg parentWatchConfig, open func(pid int) parentProbe) *parentWatcher {
	if open == nil {
		open = openParentProbe
	}
	logw := cfg.Log
	if logw == nil {
		logw = os.Stderr
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultParentWatchInterval
	}
	if interval < minParentWatchInterval {
		interval = minParentWatchInterval
	}
	misses := cfg.Misses
	if misses <= 0 {
		misses = defaultParentWatchMisses
	}
	w := &parentWatcher{
		pid: cfg.PID, interval: interval, misses: misses,
		logw: logw, done: make(chan struct{}), stop: make(chan struct{}),
	}
	if !cfg.Enabled {
		w.inactive = true
		// 关掉也要说一声：**关掉的看门狗和没有看门狗**，在进程留下来之后
		// 长得一模一样，不说就没人能判断这是不是故意的。
		parentWatchLine(logw, "已关闭（--parent-watch=false）：父进程退出后本进程不会自己停")
		return w
	}
	if cfg.PID < 0 {
		w.inactive = true
		parentWatchLine(logw, "没有要看住的父进程（PID %d，显式不监护），看门狗不启动", cfg.PID)
		return w
	}
	w.probe = open(cfg.PID)
	if note := w.probe.Note(); note != "" {
		parentWatchLine(logw, "%s", note)
	}
	// 起点先探一次：**已经不在**只记一行，不据此退出。
	if !w.probe.Alive() {
		w.inactive = true
		_ = w.probe.Close()
		parentWatchLine(logw,
			"父进程 %d 启动时已不存在（服务/守护场景？），不据此退出；此后也不再据此判死", cfg.PID)
		return w
	}
	go w.loop()
	return w
}

// loop 每 interval 探一次；连续 misses 次探不到才触发。
func (w *parentWatcher) loop() {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	miss := 0
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			if w.probe.Alive() {
				miss = 0 // 看一眼就回来一次 = 父进程只是被挂起/卡了一下，清零
				continue
			}
			miss++
			if miss < w.misses {
				continue
			}
			w.fire()
			return
		}
	}
}

// fire 是唯一的触发点：先留原因，再放行调用方的收尾。
//
// 顺序不能反：Done() 一关，调用方可能立刻关端口/退进程；原因必须在那之前落笔，
// 否则最需要那一行的时候恰好写不进去。
func (w *parentWatcher) fire() {
	reason := parentGoneReason(w.pid)
	w.mu.Lock()
	w.reason = reason
	w.mu.Unlock()
	parentWatchLine(w.logw, "%s（--parent-watch=false 可关）", reason)
	close(w.done)
}

// parentWatchFlags 是三个入口（run/serve/acp）共用的一组开关。
//
// 为什么抽成结构：同一个开关换个入口就消失，是最难解释的差异
// （scripts/help-guard.py 钉的就是这条）。三个入口都调同一个
// addParentWatchFlags，措辞与默认值就只有一份。
type parentWatchFlags struct {
	enabled  *bool
	interval *time.Duration
	pid      *int
}

// addParentWatchFlags 把三个开关挂到 fs 上。
func addParentWatchFlags(fs *flag.FlagSet) *parentWatchFlags {
	enabled := fs.Bool("parent-watch", true,
		"父进程没了就自己退出（看门狗）：每 --parent-watch-interval 探一次，连续 3 次探不到才退。后台常驻/服务场景写 false")
	interval := fs.Duration("parent-watch-interval", defaultParentWatchInterval,
		"看门狗探测间隔（默认 1s；连续 3 次探不到父进程才退，所以 3~4 秒内发现）")
	pid := fs.Int("parent-pid", 0,
		"显式指定要看住的父进程 PID（默认 0=自动：环境变量 DSHD_PARENT_PID → os.Getppid()；负数=不监护）")
	return &parentWatchFlags{enabled: enabled, interval: interval, pid: pid}
}

// start 按解析后的开关起看门狗。日志一律写 stderr：serve 的 stdout 是协议。
func (f *parentWatchFlags) start(logw io.Writer) *parentWatcher {
	if f == nil || f.enabled == nil {
		// 没接线也要有看门狗？没有开关就是不知道用户的意图，按默认（开）走。
		return startParentWatch(parentWatchConfig{
			Enabled: true, PID: resolveParentPID(0), Log: logw,
		})
	}
	return startParentWatch(parentWatchConfig{
		Enabled: *f.enabled, PID: resolveParentPID(*f.pid),
		Interval: *f.interval, Misses: defaultParentWatchMisses, Log: logw,
	})
}
