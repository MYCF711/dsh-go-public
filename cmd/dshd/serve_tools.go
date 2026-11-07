package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"dshgo/codecs/v4"
	"dshgo/internal/modes"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// loopRunner 是 serve 的真 runner：起子进程 `dshd run`，把新消息推给界面。
//
// 为什么用子进程：跑工具要 kernel.Assemble 那一整套，它埋在 cmdRun 里；
// 搬出来会同时影响 run / schedule run / subagent 三条路。子进程是零风险复用——
// run 已经被 400 多个测试、真机、e2e 验过，不用重验一遍。
//
// 写锁：子进程按既有规矩拿会话写锁；这里**只读**日志，不抢锁。
type loopRunner struct {
	// jobsMu 保护 jobs：后台任务清单由子进程推上来（stderr 的 @@JOBS@@ 行），
	// jobs.list 从这里读。**子进程推、父进程存**——因为 spi.Job 在子进程的
	// kernel Scope 里，宿主这边没有那个实例。
	jobsMu sync.Mutex
	jobs   []transport.JobInfo
	// journals 是**每会话一条**的序号域（H14）。帧的 seq 由它分配，
	// WS 推送与 session.history 读同一份——否则客户端判断断号时看到的是假象。
	// nil 时 emit 退化成"只推不记账"（老测试不必改）。
	journals *transport.Journals
	// broker 是审批的父进程半边（H13）。只在 --policy ask 时用；
	// 它把子进程 stderr 上的 @@APPROVAL@@ 转成 approval/request 帧，
	// 并把界面 session.approve 的决定写回子进程 stdin。
	broker     *approvalBroker
	jobActions *jobActionBroker
	// sandboxPresets / sandboxStore 是沙盒三档的真值来源：子进程的 bash/pwsh
	// 靠注入的环境变量判定档位，**不读父进程 env**（A/B 会互相污染）。
	sandboxPresets *modes.Registry
	sandboxStore   *sandboxModeStore
	// model 是透传给子进程的模型 id（空 = 用 run 的默认值）。
	model string
	// reasoning 是透传给子进程的推理强度 wire（空 = 不要）。
	// reasoningSet 区分“无覆盖，沿用子进程默认”与 off:null 的显式空 wire。
	reasoning    string
	reasoningSet bool
	// effortLevel 是本轮真实 catalog key；recordEffort 只在生产 wrapper 开启。
	effortLevel  string
	recordEffort bool
	pub          func(topic string, f transport.ServerFrame)
	home         string
	policy       string
	maxSteps     int
	// toolsMode 是透传给子进程的工具表形态（compact/full/merged/lazy）。
	//
	// 为什么是"透传"而不是"serve 自己投影一张表"：serve 的每一轮**就是**
	// 子进程 `dshd run`，也就是 cmdRun 本身——lazy 的整条接线（常驻集投影 →
	// 从**这个会话**的 v4 日志回放装载集 → 拼系统提示目录 → 挡回未装载调用）
	// 全都在那一侧。父进程再实现一遍就是同一件事的第二个实现，两边一定会漂
	// （历史上 serve/run/acp 三条路各投影一次，表就不一样过）。
	//
	// 于是 lazy 的会话隔离也是免费的：每轮一个新进程、一份新表，回放的输入
	// 只有 `--session` 指的那份日志。父进程**不留**任何装载集状态，
	// 也就没有"A 会话的装载集漏到 B"的载体。
	toolsMode string
}

// childArgs 拼"起一轮 run 子进程"的全部参数。
//
// 抽成方法是为了可测，也为了把 lazy 的接线钉在一处：--tools-mode 必须和
// --session 出现在**同一条** argv 里。少传一个，serve 这条链就根本没接上
// 懒加载（表永远停在 compact，装载集无从回放），而界面上看不出任何异常。
func (r *loopRunner) childArgs(sessionID, text string, promptContentFile ...string) []string {
	args := []string{"run",
		"--home", r.home,
		"--session", sessionID,
		"--prompt", text,
		"--policy", r.policy,
		"--max-steps", fmt.Sprint(r.maxSteps),
	}
	// 空 = 用子进程自己的默认值（compact）。cmdServe 一定会给一个解析过的值；
	// 直接构造 loopRunner 的老调用者（测试）不受影响。
	if r.toolsMode != "" {
		args = append(args, "--tools-mode", r.toolsMode)
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	if r.reasoning != "" || r.reasoningSet {
		// off:null 也必须显式带空值，否则 cmdRun 会重新继承 DSHD_REASONING_EFFORT。
		args = append(args, "--reasoning", r.reasoning)
	}
	if r.recordEffort {
		// level 与 wire 必须一起到达真正发请求的 cmdRun；provider 分支也复用
		// 这份 argv，因此不再依赖父进程回合后的 override store 补记。
		args = append(args, "--record-effort", "--effort-level", r.effortLevel)
	}
	if len(promptContentFile) > 0 && promptContentFile[0] != "" {
		args = append(args, "--prompt-content-file", promptContentFile[0])
		// 与 --prompt 互斥；模型文本由子进程从真实块和附件字节派生。
		for i := 0; i < len(args); i++ {
			if args[i] == "--prompt" {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
	}
	return args
}

func (r *loopRunner) Run(ctx context.Context, sessionID, turnID, text string) error {
	return r.run(ctx, sessionID, turnID, text, "")
}

// RunContent 保留完整块到 run 子进程，结束后只清理本轮自己的交接文件。
func (r *loopRunner) RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	path, err := writePromptContentFile(r.home, blocks)
	if err != nil {
		r.emitTurn(sessionID, turnID, err)
		return err
	}
	defer os.Remove(path)
	return r.run(ctx, sessionID, turnID, "", path)
}

func (r *loopRunner) run(ctx context.Context, sessionID, turnID, text, promptContentFile string) error {
	exe, err := os.Executable()
	if err != nil {
		r.emitTurn(sessionID, turnID, err)
		return err
	}
	args := append(r.childArgs(sessionID, text, promptContentFile), "--turn-id", turnID)
	cmd := exec.CommandContext(ctx, exe, args...)
	// 沙盒档位：子进程的 bash/pwsh 靠这几个变量判定 allow/deny/needs-approval。
	// 只写子进程 Env（os.Environ + 增量），绝不 os.Setenv——A/B 会话会互相污染。
	if r.sandboxStore != nil && r.sandboxPresets != nil {
		env, envErr := sandboxWriteChildEnv(r.home, sessionID, r.sandboxPresets, r.sandboxStore)
		if envErr != nil {
			r.emitTurn(sessionID, turnID, envErr)
			return envErr
		}
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer

	// 审批要"跑到一半停下来问"（H13），所以 ask 策略下不能等子进程跑完再读 stderr。
	// **顺序很要紧**：先 StderrPipe 再决定 cmd.Stderr——StderrPipe 在 cmd.Stderr 已设时
	// 会直接返回错误，而那个错误如果不看，就变成"子进程卡在等审批、父进程一无所知"。
	var watchWG sync.WaitGroup
	// **不论什么策略都要挂 Watch**：这条 stderr 通道不只跑审批，还跑子进程推上来的
	// `@@JOBS@@`（后台任务清单）。只在 ask 时挂的话，`--policy allow` 下 jobs.list
	// 永远是空的——实测过一次：模型真起了 job-1、子进程真写了 @@JOBS@@ 行，
	// 而父进程因为没挂 Watch 什么都没收到，界面上看到一个空列表。
	var jobStdin io.WriteCloser
	if r.broker != nil {
		stdin, serr := cmd.StdinPipe()
		if serr == nil {
			jobStdin = stdin
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			cmd.Env = append(cmd.Env, "DSHD_JOBS_STDIO=1", "DSHD_STEER_STDIO=1")
			if r.policy == "ask" {
				cmd.Env = append(cmd.Env, "DSHD_APPROVAL_STDIO=1")
				r.broker.useStdin(stdin)
			}
		} else {
			fmt.Fprintf(os.Stderr, "jobs/审批 stdin 没建起来（%v），本轮任务动作不可用\n", serr)
		}
		epipe, eerr := cmd.StderrPipe()
		if eerr == nil {
			watchWG.Add(1)
			go func() {
				defer watchWG.Done()
				r.broker.WatchSession(sessionID, epipe, &stderr)
			}()
		} else {
			// 拿不到管道就退回缓冲：至少错误播报还在（jobs 就看不到了）。
			fmt.Fprintf(os.Stderr, "stderr 管道没建起来（%v），这一轮看不到后台任务\n", eerr)
			cmd.Stderr = &stderr
		}
	} else {
		cmd.Stderr = &stderr
	}

	// 起跑水位必须在**起子进程之前**取：子进程一起来就会写 turn/start 与
	// user/message，等 follow 的 goroutine 被调度起来再问"日志现在多长"，
	// 那两帧就被当成历史跳过去了（见 followBaseline）。
	prog := r.followBaseline(sessionID)

	if err := cmd.Start(); err != nil {
		r.emitTurn(sessionID, turnID, err)
		return err
	}
	var actionRun *jobActionRun
	if r.jobActions != nil {
		run, err := r.jobActions.registerTurnRun(sessionID, turnID, cmd.Process.Pid, exe, jobStdin)
		actionRun = run
		if err != nil {
			_ = cmd.Process.Kill() // 只回收本函数刚启动、已核对 executable 的 PID
			_ = cmd.Wait()
			watchWG.Wait()
			r.emitTurn(sessionID, turnID, err)
			return err
		}
		defer r.jobActions.unregisterRun(run)
	}
	// 把子进程的身份写在 stderr 上：出问题时第一句该问的是"它到底起没起来"。
	// 上一轮就是因为没有这一行，分不清"被杀"和"压根没起"。
	fmt.Fprintf(os.Stderr, "spawn pid=%d argv=%v\n", cmd.Process.Pid, args)

	// 边跑边推：日志是 append 的，按条数差量推就够。
	// 必须**等 follow 真的退出**再发终局帧：否则它可能在我们发出 turn/done 之后
	// 又推一帧（子进程刚写完、我们刚读完的那个窗口），界面就会看到"结束后还有消息"。
	var fw sync.WaitGroup
	done := make(chan struct{})
	// DSHD_SERVE_NO_FOLLOW=1 时不起 follow goroutine。
	// 只为二分：增长到底来自"spawn 子进程"还是"follow 读日志"。
	if os.Getenv("DSHD_SERVE_NO_FOLLOW") != "1" {
		fw.Add(1)
		go func() {
			defer fw.Done()
			r.follow(ctx, sessionID, done, &prog)
		}()
	}

	werr := cmd.Wait()
	// 等 stderr 那条扫描 goroutine 收尾：它在写 stderr 缓冲区，
	// 而下面读那个缓冲区——不等就是一个数据竞争（也读不全最后几行）。
	watchWG.Wait()
	// 所有已写出的 steer ACK 都已分发后再宣布退出；之后的提交立刻明确过晚。
	r.jobActions.markExited(actionRun)
	// 子进程说了什么，运维侧必须看得到。
	// 没有这一行时，serve 起的子进程失败只能靠 WS 上的 turn/error 观察，
	// 而 e2e/curl 场景根本没人读 WS——于是"它没写盘"就变成一个无解释的现象。
	if s := strings.TrimSpace(stderr.String()); s != "" {
		fmt.Fprintf(os.Stderr, "child(stderr): %s\n", tail(s, 800))
	}

	// 子进程已经退出、写锁该释放了：它没能自己闭合的那一轮，这里补上。
	//
	// 只读日志的人没有退出码、也没有 WS 上的 turn/error 可看。日志里留一个
	// 开了没结局的 turn，他们只能猜"这一轮到底跑完没有"（D1）。
	//
	// 成功路径不读日志：`dshd run` 的退出码 0 只有一种可能——循环正常收工，
	// 而收工前它一定写过 turn/end（cmdRun 的 Emit）。失败/取消才需要回看。
	//
	// 位置在 close(done) **之前**：follow 收尾时会再 drain 一次，补写的这一帧
	// 于是也进了 journal——session.history 与 session.load 看到的是同一个结局。
	if werr != nil || ctx.Err() != nil {
		// 先清写锁，再补 turn/end：被**杀**掉的子进程不会删自己的锁
		// （TerminateProcess 连 defer 都不跑）。不先清，settleTurn 自己也会被
		// 那把锁挡在门外——于是既没有 turn/end，也没有下一轮。
		r.dropChildLock(sessionID, cmd.Process.Pid)
		if turn, serr := r.settleTurn(ctx, sessionID, cmd.Process.Pid, werr); serr != nil {
			fmt.Fprintf(os.Stderr, "补 turn/end 失败（日志里这一轮仍是悬挂）：%v\n", serr)
		} else if turn > 0 {
			fmt.Fprintf(os.Stderr, "子进程没写 turn/end，宿主补了一条：turn=%d\n", turn)
		}
	}
	close(done)
	fw.Wait()

	topic := "session:" + sessionID
	if werr != nil {
		e := fmt.Errorf("run 失败：%v；stderr: %s", werr, strings.TrimSpace(tail(stderr.String(), 400)))
		r.emitTurn(sessionID, turnID, e)
		return e
	}
	// effort selection 已由 cmdRun 在每个真实模型 step 的入口处写入；
	// 父进程不再于回合结束后按 override store 补一条无法证明请求生效的记录。
	// 结局帧的序号由 journal 统一分配：它一定排在已经推出去的那些事件之后，
	// 不再需要"把计数器推到日志末尾"这种影子同步——那样两个来源迟早对不上。
	_ = topic
	r.emitTurn(sessionID, turnID, nil)
	return nil
}

// followBaseline 取 follow 的起跑水位：**这一轮开始之前**日志已经写到哪。
//
// 为什么要有它：follow 原来从 offset 0 / lastSeq 0 起跑，于是**每一轮**都把
// 整份会话日志从头再推一遍——第 k 轮界面上就多出前 k-1 轮的消息。四轮实测
// （scripts/tui-session-e2e.py）：四次 prompt、日志里 4 条 user/message，
// 而 journal 里 10 条 user/message、10 个 turn/start（seq 1-66）。
// **日志是干净的，脏的是推给界面的那一路。**
//
// 为什么用文件长度而不是"最后一条事件的 seq"：日志是 O_APPEND 追加的，
// 长度就是下一帧的起点；按 seq 过滤则要先把整份日志读一遍（2.2 万事件
// 300ms 级），每轮付一次没有意义。尾部有半帧也不影响：新帧从 EOF 开始，
// splitFrames 从那里解是干净的。
//
// 返回值同时带着已解析的 path：follow 拿到它就不用再 glob 一次。
//
// lastSeq 从 **-1** 起，不是 0：v4 的 seq 从 0 开始（`session.Log` 写下的第一条
// 事件就是 seq=0），而 follow 的判据是 `ev.Seq <= lastSeq` ——初值取 0 会把
// **每个会话的第一条事件**永久吞掉。实测证据：tui-session-e2e 的日志第一条是
// seq=0 的 system/message，四轮里界面上从来没有它。
func (r *loopRunner) followBaseline(sessionID string) followProgress {
	base := followProgress{lastSeq: -1}
	src := newSessionSource(r.home, "")
	path, err := src.resolve(sessionID)
	if err != nil {
		// 还没有这份会话（多半是子进程会建）：本来就是空的，从 0 起跑没有历史可重复。
		return base
	}
	base.path = path
	st, serr := os.Stat(path)
	if serr != nil {
		// 拿不到"现在多长"就没法划水位。退回老行为（可能重推历史）——
		// 文件刚被删/换的时候，**看得见比干净重要**：宁可重复，也不吞掉这一轮。
		return base
	}
	base.offset = st.Size()
	return base
}

// follow 每 200ms 看一次日志，把起跑水位之后的**全部事件**推出去。
//
// 为什么不是只推消息：界面要显示"它调了什么工具"。
// 只推 assistant/message 的话，用户看到答案却看不到过程——
// 而"看不到过程"正是这类 agent 界面最不能接受的事。
//
// **起跑水位由 followBaseline 给**（Run 在起子进程之前取）：offset 之前的
// 事件属于已经推过的轮次，再推一遍就是界面上"一次 prompt 变两三turn"的重复。
func (r *loopRunner) follow(ctx context.Context, sessionID string, done <-chan struct{}, prog *followProgress) {
	src := newSessionSource(r.home, "")
	// 全程复用一个解码器：这个循环每 200ms 跑一次，建一次解码器就够。
	//
	// DSHD_SERVE_NO_REUSE=1 时退回"每次新建"，只为做 A/B——
	// 改动的收益必须能被同一台机器上的一次对照证明，不能靠"改完看着好了"。
	noReuse := os.Getenv("DSHD_SERVE_NO_REUSE") == "1"
	var rd *v4.Reader
	if !noReuse {
		var rerr error
		rd, rerr = v4.NewReader()
		if rerr != nil {
			return
		}
		defer rd.Close()
	}
	var (
		// 起跑水位：这一轮之前写下的都不再推（见 followBaseline）。
		offset = prog.offset
		// -1 = 本轮还没推过任何一条。**不能是 0**：v4 的 seq 从 0 起，
		// `ev.Seq <= lastSeq` 会把 seq=0 那条（每个会话的第一条事件）吞掉。
		lastSeq = prog.lastSeq
		path    = prog.path
	)
	defer func() {
		prog.offset, prog.lastSeq, prog.path = offset, lastSeq, path
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			// 收尾再读一次。子进程刚写完的 assistant/message、turn/end
			// 常常在 done 之后才落盘；不读这一次，界面永远看不到最后一条回答。
			// 2026-11-03 实测：会话文件里有 seq=3 assistant/message，流里没有。
			r.drain(rd, noReuse, path, &offset, &lastSeq, sessionID)
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			if path == "" {
				p, err := src.resolve(sessionID)
				if err != nil {
					continue
				}
				path = p
			}
			// 二分用：DSHD_FOLLOW_MODE=resolve 只解析路径就睡；=read 再读但不推。
			mode := os.Getenv("DSHD_FOLLOW_MODE")
			if mode == "resolve" {
				continue
			}
			// 增量读：只解上次之后的新帧。全量重读在 2.2 万事件的会话上是 301ms，
			// 而这里每 200ms 读一次——读得比跑得还慢，界面会越落越远。
			var evs []v4.Event
			var next int64
			var err error
			if noReuse {
				evs, next, err = v4.ReadSince(path, offset)
			} else {
				evs, next, err = rd.ReadSince(path, offset)
			}
			if err != nil {
				continue
			}
			offset = next
			if mode == "read" {
				continue
			}
			dbgFollow := os.Getenv("DSHD_DEBUG_FOLLOW") != ""
			for _, ev := range evs {
				if dbgFollow {
					fmt.Fprintf(os.Stderr, "follow: got seq=%d type=%s lastSeq=%d\n", ev.Seq, ev.Type, lastSeq)
				}
				if ev.Seq <= lastSeq {
					if dbgFollow {
						fmt.Fprintf(os.Stderr, "follow: SKIP seq=%d (<= lastSeq=%d)\n", ev.Seq, lastSeq)
					}
					continue
				}
				lastSeq = ev.Seq
				// W144 的 selection 是 v4 内部真实配置记录，不是 WS 白名单事件；
				// 水位必须前进，但不能因为它移到 step/start 就意外新增一类推送。
				if ev.Type == "model/selection" {
					continue
				}
				// 走 emit：由 journal 发号。**不要再拿 ev.Seq 当送达序号**——
				// 那是 v4 日志的序号，跟这条连接上推了几帧没关系。
				emitFrameAt(r.journals, r.pub, sessionID, ev.Type, projectV4MessageIdentity(ev), ev.Time)
			}
		}
	}
}

// drain 把 offset 之后的所有事件读完并推出去。
//
// 存在的理由只有一个：follow 的 select 会在 done 时立刻 return，
// 而子进程最后写的那几帧（assistant/message、turn/end）往往晚一步落盘。
func (r *loopRunner) drain(rd *v4.Reader, noReuse bool, path string, offset *int64, lastSeq *int64, sessionID string) {
	for {
		var evs []v4.Event
		var next int64
		var err error
		if noReuse {
			evs, next, err = v4.ReadSince(path, *offset)
		} else {
			evs, next, err = rd.ReadSince(path, *offset)
		}
		if err != nil || len(evs) == 0 {
			return
		}
		*offset = next
		for _, ev := range evs {
			if ev.Seq <= *lastSeq {
				continue
			}
			*lastSeq = ev.Seq
			if ev.Type == "model/selection" {
				continue
			}
			emitFrameAt(r.journals, r.pub, sessionID, ev.Type, projectV4MessageIdentity(ev), ev.Time)
		}
	}
}

// openTurn 找出日志里**最后一个开了但没闭合**的 turn（0 = 全部闭合）。
//
// 用顺序状态机而不是"数 turn/start 与 turn/end 的条数"：`dshd run` 每轮都写
// turn=1（同一会话续跑时编号会重复），按编号配对会把新轮配到旧轮的结局上。
func openTurn(back *v4.Log) int {
	open := 0
	for _, ev := range back.Events {
		switch ev.Type {
		case "turn/start":
			var d struct {
				Turn int `json:"turn"`
			}
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				open = d.Turn
			}
		case "turn/end":
			open = 0
		}
	}
	return open
}

// settleTurn 给"子进程没能自己收口"的那一轮补一条 turn/end。
//
// 子进程就是 `dshd run`，它自己有一条统一出口（cmdRun 的 turnCloser）。
// 这里兜的是它**根本没机会跑那条出口**的情形：ctx 取消时 exec.CommandContext
// 直接 TerminateProcess、进程崩溃、被外部工具掐掉。这些情况下日志里会留下
// 悬挂 turn，而只读日志的消费者无从知道这一轮结束没有。
//
// 幂等：只补"最后一个 turn/start 之后没有 turn/end"的那一轮；已经闭合时
// 这里一个字节都不写。
func (r *loopRunner) settleTurn(ctx context.Context, sessionID string, childPID int, cause error) (int, error) {
	src := newSessionSource(r.home, "")
	path, err := src.resolve(sessionID)
	if err != nil {
		// 没有这份会话 = 子进程连会话都没建起来，没有"开了的 turn"可补。
		return 0, nil
	}
	back, err := v4.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("读不出会话日志，无法判断要不要补 turn/end：%w", err)
	}
	turn := openTurn(back)
	if turn <= 0 {
		return 0, nil
	}
	logw, err := openForSettle(path, childPID)
	if err != nil {
		return 0, err
	}
	defer logw.Close()
	kind, detail := turnEndForSettle(ctx, cause)
	if err := logw.Append("turn/end", session.TurnEndData(turn, kind, detail)); err != nil {
		return 0, fmt.Errorf("写 turn/end 失败：%w", err)
	}
	return turn, nil
}

// turnEndForSettle 给"子进程没写结局"挑 kind。
//
// 被取消 → aborted（宿主主动中止，v4 的 aborted 带原因）；
// 崩溃/非零退出 → error，把子进程的失败原因原样带上。
func turnEndForSettle(ctx context.Context, cause error) (kind, detail string) {
	switch {
	case ctx != nil && ctx.Err() != nil:
		return "aborted", "这一轮被宿主中止：子进程被取消，没来得及写 turn/end"
	case cause != nil:
		return "error", "子进程退出但这一轮没有 turn/end：" + cause.Error()
	default:
		return "error", "子进程正常退出，但这一轮没有 turn/end（宿主缺陷，不是这一轮跑完了）"
	}
}

// openForSettle 打开会话写入端；被**刚退出的那个子进程**留下的写锁挡道时，
// 只清理那一把锁，然后重试一次。
func openForSettle(path string, childPID int) (*session.Log, error) {
	logw, err := session.OpenSessionAt(path)
	if err == nil {
		return logw, nil
	}
	// 挡道的如果是刚退出那个子进程的锁，清掉再试；别人的锁就让错误原样报上去。
	if derr := dropStaleLock(path, childPID); derr != nil {
		return nil, err
	}
	return session.OpenSessionAt(path)
}

// dropStaleLock 删掉**刚刚退出的那个子进程**留下的会话写锁。
//
// 为什么必须有人做：`exec.CommandContext` 取消时是 TerminateProcess，
// 子进程的 defer 一行都跑不到——它 claimWriter 建的那把锁就留在了磁盘上。
// 下一轮的子进程一上来就抢这把锁，抢到的是死进程的，于是第二轮直接
// "会话正被另一个进程写入"。现象是"取消一次，这个会话就废了"。
//
// 只认 pid **精确匹配**：锁里记着 pid，而 cmd.Wait 已经证明这个 pid 退出了。
// 别的 pid 的锁一概不碰——删错锁等于把"同一份会话只有一个写入者"的约束拆了。
func dropStaleLock(sessionPath string, childPID int) error {
	if childPID <= 0 {
		return nil
	}
	lock := sessionPath + ".lock"
	if !lockBelongsTo(lock, childPID) {
		return nil
	}
	if err := os.Remove(lock); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// dropChildLock 是 dropStaleLock 的"按会话找锁"版本。
func (r *loopRunner) dropChildLock(sessionID string, pid int) {
	src := newSessionSource(r.home, "")
	path, err := src.resolve(sessionID)
	if err != nil {
		return // 没有这份会话 = 没有锁可清
	}
	if err := dropStaleLock(path, pid); err != nil {
		fmt.Fprintf(os.Stderr, "清理子进程留下的会话写锁失败（下一轮可能撞锁）：%v\n", err)
	}
}

// lockBelongsTo 判断锁文件是不是 childPID 留下的（只认 pid 精确匹配）。
func lockBelongsTo(lockPath string, pid int) bool {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	want := fmt.Sprintf("pid=%d", pid)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// tail 取字符串末尾 n 个字节（错误正文可能很长，界面只要看得懂最后一句）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
