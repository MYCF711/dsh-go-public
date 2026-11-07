package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"dshgo/internal/llm"
	"dshgo/internal/transport"
)

// modelOverrideLoopRunner 给默认 loopRunner 补上每轮模型与权限解析。
//
// 每轮构造一个轻量 loopRunner 副本，而不是改写共享 base.model/base.policy：
// 不同会话可以并发跑，后开始的 B 绝不能覆盖 A 即将使用的 --model/--policy。
// jobsMu/jobs 不复制；
// 后台任务仍由原 base 与 broker 的回调维护，journal、审批与推送出口保持原样。
//
// **每加一个"每轮透传给子进程"的字段，都必须在这里补一行**：
// 这个副本是逐字段拷的，漏一个就会让**某一轮**静默退回默认值——
// 比如漏了 toolsMode，就只有"走 modelOverrideLoopRunner 的那条路"不带
// --tools-mode，而 run 那条路带着；两条路的表不一样，且从外面完全看不出来。
// TestServeForwardsToolsModeToTurnChild 钉住的就是这一格。
type modelOverrideLoopRunner struct {
	base *loopRunner
	// permissions 与 session.load/setPermission 共用同一个 store。生产路径
	// 不走第二份内存副本，load 显示的值就是下一轮要透传的值。
	// nil 只保留给直接构造 runner 的测试/旧调用者，此时按持久化文件解析。
	permissions *permissionOverrideStore
	// recordEffort 由生产 cmdServe 显式开启；旧 argv 探针不写额外 v4 夹具。
	recordEffort bool
	// catalog 是生产 serve 的同一目录视图，含物理缺失时的真实 env 投影。
	catalog *llm.ModelCatalogStore
	// titles 是本进程的自动标题写口（官方 #14 首轮标题），懒建一次。
	// 它只持有 home/journal 出口与两个小缓存；实现与证据见 serve_title_auto.go。
	titlesMu sync.Mutex
	titles   *autoTitleService
}

func (r *modelOverrideLoopRunner) Run(ctx context.Context, sessionID, turnID, text string) error {
	model, err := resolveServeModel(r.base.home, sessionID, r.base.model)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	policy, err := r.resolvePermission(sessionID)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	reasoning, effortLevel, _, err := resolveServeEffort(r.base.home, sessionID, model, r.base.reasoning, r.catalog)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	next := &loopRunner{
		journals:     r.base.journals,
		broker:       r.base.broker,
		jobActions:   r.base.jobActions,
		model:        model,
		reasoning:    reasoning,
		reasoningSet: true, // 父进程默认已解析；空值也不能让 child 再继承环境。
		effortLevel:  effortLevel,
		recordEffort: r.recordEffort,
		pub:          r.base.pub,
		home:         r.base.home,
		policy:       policy,
		maxSteps:     r.base.maxSteps,
		toolsMode:    r.base.toolsMode,
	}
	provider, routed, err := resolveServeProvider(r.base.home, model)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	if routed {
		if err := runProviderLoop(ctx, next, provider, sessionID, turnID, text); err != nil {
			return err
		}
	} else if err := next.Run(ctx, sessionID, turnID, text); err != nil {
		return err
	}
	// 这一轮正常收工：唯一一处自动标题触发点（官方 #14）。turn/done 已经在上面
	// 推出去，标题调用在后台跑，绝不让它挡在结局帧或下一轮前面。
	r.kickAutoTitle(sessionID, model, provider, routed)
	return nil
}

// RunContent 与 Run 使用同一模型/权限解析，但把有序块交给 loopRunner 持久化。
func (r *modelOverrideLoopRunner) RunContent(ctx context.Context, sessionID, turnID string, blocks []transport.ContentBlock) error {
	model, err := resolveServeModel(r.base.home, sessionID, r.base.model)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	policy, err := r.resolvePermission(sessionID)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	reasoning, effortLevel, _, err := resolveServeEffort(r.base.home, sessionID, model, r.base.reasoning, r.catalog)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	next := &loopRunner{journals: r.base.journals, broker: r.base.broker, jobActions: r.base.jobActions,
		model: model, reasoning: reasoning, reasoningSet: true, effortLevel: effortLevel, recordEffort: r.recordEffort,
		pub: r.base.pub, home: r.base.home, policy: policy, maxSteps: r.base.maxSteps, toolsMode: r.base.toolsMode}
	provider, routed, err := resolveServeProvider(r.base.home, model)
	if err != nil {
		r.base.emitTurn(sessionID, turnID, err)
		return err
	}
	if routed {
		path, err := writePromptContentFile(next.home, blocks)
		if err != nil {
			next.emitTurn(sessionID, turnID, err)
			return err
		}
		defer os.Remove(path)
		if err := runProviderLoop(ctx, next, provider, sessionID, turnID, "", path); err != nil {
			return err
		}
	} else if err := next.RunContent(ctx, sessionID, turnID, blocks); err != nil {
		return err
	}
	// 同 Run：块输入（附件/引用）这条路也必须在正常收工后触发一次自动标题。
	r.kickAutoTitle(sessionID, model, provider, routed)
	return nil
}

// providerLoopEnvironment 只修改这一轮 exec.Cmd 的环境。先删除继承的同名
// 键（Windows 环境名不区分大小写），避免重复键让子进程读到 decoy/别人的值。
func providerLoopEnvironment(provider llm.ModelProviderConfig) []string {
	env := os.Environ()
	managed := make([]string, 0, len(env)+2)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && (strings.EqualFold(name, "DSHD_LLM_BASE") || strings.EqualFold(name, "DSHD_LLM_KEY")) {
			continue
		}
		managed = append(managed, entry)
	}
	return append(managed, "DSHD_LLM_BASE="+provider.BaseURL, "DSHD_LLM_KEY="+provider.Credential())
}

// runProviderLoop 与 loopRunner.Run 使用同一组 follow/journal/approval/jobs
// 组件，但 argv 显式带 --base，环境只作用于本子进程。credential 不进 argv，
// 也不 os.Setenv；父进程可同时运行使用不同 provider 的其它会话。
func runProviderLoop(ctx context.Context, r *loopRunner, provider llm.ModelProviderConfig, sessionID, turnID, text string, promptContentFile ...string) error {
	exe, err := os.Executable()
	if err != nil {
		r.emitTurn(sessionID, turnID, err)
		return err
	}
	args := append(r.childArgs(sessionID, text, promptContentFile...), "--turn-id", turnID, "--base", provider.BaseURL)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = providerLoopEnvironment(provider)
	var stderr bytes.Buffer

	var watchWG sync.WaitGroup
	var jobStdin io.WriteCloser
	if r.broker != nil {
		stdin, serr := cmd.StdinPipe()
		if serr == nil {
			jobStdin = stdin
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
			fmt.Fprintf(os.Stderr, "stderr 管道没建起来（%v），这一轮看不到后台任务\n", eerr)
			cmd.Stderr = &stderr
		}
	} else {
		cmd.Stderr = &stderr
	}

	prog := r.followBaseline(sessionID)
	if err := cmd.Start(); err != nil {
		err = redactServeProviderError(err, provider.Credential())
		r.emitTurn(sessionID, turnID, err)
		return err
	}
	var actionRun *jobActionRun
	if r.jobActions != nil {
		run, err := r.jobActions.registerTurnRun(sessionID, turnID, cmd.Process.Pid, exe, jobStdin)
		actionRun = run
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			watchWG.Wait()
			r.emitTurn(sessionID, turnID, err)
			return err
		}
		defer r.jobActions.unregisterRun(run)
	}
	// baseURL 已经过 catalog 校验且禁止 userinfo；argv 不含 credential。
	fmt.Fprintf(os.Stderr, "spawn pid=%d argv=%v\n", cmd.Process.Pid, args)

	var fw sync.WaitGroup
	done := make(chan struct{})
	if os.Getenv("DSHD_SERVE_NO_FOLLOW") != "1" {
		fw.Add(1)
		go func() {
			defer fw.Done()
			r.follow(ctx, sessionID, done, &prog)
		}()
	}

	werr := cmd.Wait()
	watchWG.Wait()
	r.jobActions.markExited(actionRun)
	if s := strings.TrimSpace(stderr.String()); s != "" {
		fmt.Fprintf(os.Stderr, "child(stderr): %s\n", llm.RedactCredential(tail(s, 800), provider.Credential()))
	}
	if werr != nil || ctx.Err() != nil {
		r.dropChildLock(sessionID, cmd.Process.Pid)
		if turn, serr := r.settleTurn(ctx, sessionID, cmd.Process.Pid, werr); serr != nil {
			fmt.Fprintf(os.Stderr, "补 turn/end 失败（日志里这一轮仍是悬挂）：%s\n", llm.RedactCredential(serr.Error(), provider.Credential()))
		} else if turn > 0 {
			fmt.Fprintf(os.Stderr, "子进程没写 turn/end，宿主补了一条：turn=%d\n", turn)
		}
	}
	close(done)
	fw.Wait()

	if werr != nil {
		e := fmt.Errorf("run 失败：%v；stderr: %s", werr, strings.TrimSpace(tail(stderr.String(), 400)))
		e = redactServeProviderError(e, provider.Credential())
		r.emitTurn(sessionID, turnID, e)
		return e
	}
	r.emitTurn(sessionID, turnID, nil)
	return nil
}

// resolvePermission 在每轮构造独立 loopRunner 时解析权限。覆盖只进入 next，
// 不改 base.policy，因此并发/后续的其它会话仍各自解析，不会互相污染。
func (r *modelOverrideLoopRunner) resolvePermission(sessionID string) (string, error) {
	if r.permissions != nil {
		if permission := r.permissions.Get(sessionID); permission != "" {
			return permission, nil
		}
		return r.base.policy, nil
	}
	overrides := newPermissionOverrideStore(r.base.home)
	if err := overrides.Load(); err != nil {
		return "", fmt.Errorf("读会话权限覆盖: %w", err)
	}
	if permission := overrides.Get(sessionID); permission != "" {
		return permission, nil
	}
	return r.base.policy, nil
}
