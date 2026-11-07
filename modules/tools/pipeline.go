package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dshgo/internal/agent"
	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// Options 是流水线的可调参数。默认值来自实测：dsc 的 spill 阈值 4000 字符。
type Options struct {
	SpillDir string // 外置目录；空 = <temp>/dshd-spill
	MaxChars int    // 超过就外置；<=0 = 4000
	// ToolTimeout 是**每一次工具调用**的统一截止时间（timeout-policy）。
	//
	// 三值约定（本仓统一）：0 = 未配置，按 DSHD_TOOL_TIMEOUT_SECS → 内置默认；
	// 负值 = 明确关闭（不装 deadline，行为退回"只看各工具自己"）；正值 = 这个预算。
	//
	// 为什么要有这一层：bash/web/MCP 各自有超时，但**流水线这一层没有**。任何一个
	// 不自带超时的工具（新工具、第三方工具、卡在无 deadline 的网络读上、卡在第三方
	// 库里的调用）挂住，整个 turn 就跟着挂住——模型在等一个永远不会来的结果，人看到
	// 的是"卡死了"，而且没有任何一层会说话。这一层是最后一道网。
	ToolTimeout time.Duration
	// BeforeDispatch 可选：工具体执行**之前**的最后一道闸（官方 0.2.2 #78
	// `session-checkpoint-policy` 的 `tools/execute` 钩子）。
	//
	// 官方原件（asar 只读解出，dsh-session-checkpoint-policy/lib/index.js:61-66）：
	//
	//	ctx.on("tools/execute", async (exec, next) => {
	//	    if (exec.agent === void 0 || exec.parent !== void 0) return next();
	//	    await ctx.sessions.flush(exec.agent.session);
	//	    if (exec.signal.aborted) return abortedBeforeDispatchResult();
	//	    return next();
	//	});
	//
	// 三条语义，缺一条这个钩子就等于没接：
	//   - 顶层调用先落盘；返回错误 ⇒ 工具体**零执行**（fail-closed 在副作用边界上）；
	//   - 落盘成功之后若这一轮已 abort ⇒ 返回 TOOL_ABORTED_BEFORE_DISPATCH，
	//     工具体同样零执行；
	//   - 无会话（agent === void 0）与嵌套子调用（parent !== void 0）原样放行，
	//     嵌套复用外层那次已经耐久的调用，不重复落盘。
	BeforeDispatch func(ctx context.Context, call *spi.Call) error
}

// ToolAbortedBeforeDispatch 是官方 #78 在「flush 成功、但这一轮已 abort」时
// 返回给模型/客户端的错误码（原词来自 @deepseek-ai/dsh-tools）。
const ToolAbortedBeforeDispatch = "TOOL_ABORTED_BEFORE_DISPATCH"

// ErrToolAbortedBeforeDispatch 是上面那个 code 的错误值形态；
// 调用方用 errors.Is 认它，用 Result.Data["code"] 拿官方原词。
var ErrToolAbortedBeforeDispatch = errors.New("tool call aborted before dispatch")

// abortedBeforeDispatchResult 是官方 abortedBeforeDispatchResult 的 Go 投影
// （lib/index.js:75-84）：模型可见正文、isError、AbortError + 官方 code 三样都要有。
func abortedBeforeDispatchResult() spi.Result {
	return spi.Result{
		Summary: "工具调用在派发前被取消（未执行）",
		Text:    "Error: tool call aborted before dispatch",
		Data: map[string]any{
			"name": "AbortError",
			"code": ToolAbortedBeforeDispatch,
		},
		// R3：失败必须给下一步。取消是"不确定它做没做"里最确定的一种——
		// 它就是没做，所以下一步是"要看结果就重新发起"。
		Hint: "这一轮在工具体执行前被取消，它**没有执行过**、没有任何副作用；" +
			"需要结果就重新发起调用（不是重试一次已完成的调用）",
	}
}

// defaultToolTimeout 是统一截止时间的默认预算。
//
// 取值不是拍脑袋：本仓里自带超时的工具，最长的一条是 bash 的 idle 预算上限
// 15 分钟（modules/exec/bash/tools.go:124，默认 10 分钟），execlit/pwsh 的命令
// 上限也是 10 分钟。统一层必须**明显宽于**这些，否则它就变成了"悄悄改掉各工具
// 的超时语义"——一条按 15 分钟预算跑的命令会在 10 分钟时被上层掐掉，而工具参数里
// 仍写着 timeout_seconds=900，模型和人都不明白为什么提前死了。30 分钟 = 最长
// 自带预算的 2 倍，只对**真正挂死**的调用生效。
const defaultToolTimeout = 30 * time.Minute

// toolTimeoutEnv 让运维不改代码就能调这一个值（单位：秒）。
//
// 空/非法 = 用内置默认；0 = 同上（本仓三值约定：0 是"未定义"）；负值 = 关闭。
const toolTimeoutEnv = "DSHD_TOOL_TIMEOUT_SECS"

// toolTimeoutFromEnv 解析环境变量；解析不了就当没配（宁可留默认，也不静默关掉
// 唯一一层兜底——把笔误变成"超时永远不生效"是最坏的失败方式）。
func toolTimeoutFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(toolTimeoutEnv))
	if v == "" {
		return defaultToolTimeout
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultToolTimeout
	}
	switch {
	case n < 0:
		return -1 // 明确关闭
	case n == 0:
		return defaultToolTimeout
	}
	return time.Duration(n) * time.Second
}

func (o Options) withDefaults() Options {
	if o.SpillDir == "" {
		o.SpillDir = filepath.Join(os.TempDir(), "dshd-spill")
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 4000
	}
	if o.ToolTimeout == 0 {
		o.ToolTimeout = toolTimeoutFromEnv()
	}
	return o
}

// Pipeline 实现 spi.Runner：三条硬约束都在这里，工具作者一行都不用写。
//
//	pre-execute（审批/沙箱，可 veto）→ execute → post-execute（外置/裁剪/提醒）→ result（广播）
type Pipeline struct {
	reg  spi.ToolRegistry
	bus  *kernel.Bus
	opts Options
	log  *slog.Logger

	mu         sync.Mutex
	violations []string // R3 违规记录：失败却没给 Hint
	seq        int

	// repeat 是 repeat-tool-reminder 的计数器（官方 0.2.2 base #83）。
	//
	// 为什么挂在 Pipeline 上而不是包级变量：一个进程里可能同时有多个内核、
	// 多个会话在跑工具。包级计数会让 A 会话的重复把 B 会话的链冲掉。
	repeat *repeatGuard
}

// NewPipeline 造一条流水线。opts 会被补默认值。
func NewPipeline(reg spi.ToolRegistry, bus *kernel.Bus, opts Options) *Pipeline {
	return &Pipeline{
		reg:    reg,
		bus:    bus,
		opts:   opts.withDefaults(),
		log:    slog.Default(),
		repeat: newRepeatGuard(),
	}
}

// Violations 返回 R3 违规（失败路径没有 Hint）的快照，供测试与诊断使用。
func (p *Pipeline) Violations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.violations...)
}

// recordR3Violation 记一笔"工具失败却没给 Hint"。
//
// 记账点必须跟着**兜底的那个地方**走：统一截止时间那一层（executeWithDeadline）
// 会给漏 Hint 的失败补一句通用话术，补完之后 Run 里那段 `Hint == ""` 就再也不会
// 成立——如果只在 Run 里记账，R3 的"哪个工具漏了 Hint"从此永远查不出来。
func (p *Pipeline) recordR3Violation(name string, cause error) {
	p.mu.Lock()
	p.violations = append(p.violations, name)
	p.mu.Unlock()
	p.log.Warn("工具失败但没有给 Hint（R3）", "tool", name, "error", cause)
}

// Run 是唯一执行入口：绕过它直接调 Tool.Exec 会跳过审批、沙箱、截断与提示。
//
// 返回值用具名形式，是为了让收尾那层（repeat-tool-reminder）能覆盖**每一条**
// 返回路径（包括被 veto、工具名不存在、参数不是 JSON 的那些），同时又不必在每个
// 分支里各插一行——那样既容易漏，也会把 R3 那套"每个 error 分支紧邻赋 Hint"
// 的扫描形状改坏。
func (p *Pipeline) Run(ctx context.Context, name string, args json.RawMessage) (res spi.Result, err error) {
	call := &spi.Call{Name: name, Args: args, Session: spi.SessionFrom(ctx)}
	// 子调用（workflow 里发的）标出来：宿主据此决定"这条要不要额外落 v4"。
	call.Nested = spi.SubDispatchFrom(ctx).Depth > 0
	started := time.Now()
	// 每次工具调用都在日志里留一条：名字、耗时、成败、错误原文。
	// 出事之后人要能回答"它到底跑了什么、跑了多久、成没成"。
	defer func() {
		attrs := []any{
			"tool", name,
			"ms", time.Since(started).Milliseconds(),
			"ok", call.Err == nil,
			"args_bytes", len(args),
		}
		if call.Err != nil {
			attrs = append(attrs, "error", call.Err.Error())
			p.log.Warn("工具调用失败", attrs...)
			return
		}
		p.log.Info("工具调用", attrs...)
	}()

	// 6. post-execute 收尾：repeat-tool-reminder（官方 0.2.2 base #83）。
	//
	// 为什么用 defer：计数口径是"每一次**尝试**"，不只是成功执行完的那些。官方把
	// observe 放在 tools/post-execute waterfall 上，被 pre-execute 拒绝的调用同样
	// 会经过它（lib/index.js:2220-2227）。本仓的 veto / 未知工具 / 参数非法都是
	// 提前 return，只有 defer 能一次覆盖全部出口。
	//
	// 它只往 Hint 上追加提醒，不改 err、不动原始输出——成败语义与 R3 都不受影响。
	defer func() {
		p.applyRepeatReminder(ctx, call, &res)
	}()

	// 1. pre-execute：waterfall，可 veto。
	if _, err := p.bus.Waterfall(spi.EventToolPreExecute, call); err != nil {
		call.Err = err
		call.Result.Hint = "被策略拒绝：" + err.Error() + "；改用其它工具或先问人"
		return call.Result, call.Err
	}

	// 0. 参数必须是 JSON 对象。
	//
	// 模型偶尔会吐 `{"arguments": "{not json"` 这种。以前直接透传给工具——
	// 大多数工具自己 Unmarshal 会失败，但**不校验参数的工具会拿着垃圾照跑**。
	// 在执行前拦一次，比让每个工具各写一遍可靠。
	//
	// 判据只有一份，实现在 agent.ValidateToolCallArguments：agent 循环用它做
	// 整条响应的预检，这里用它做执行前的深防御。两层各写一遍时已经漂过一次
	// （循环拒绝 JSON null、这里放行），同一个参数在两处得到两种结论。
	if err := agent.ValidateToolCallArguments(call.Args); err != nil {
		call.Err = fmt.Errorf("参数不是 JSON 对象: %w", err)
		call.Result.Hint = "tool_call.arguments 必须是 JSON 对象（键值对）；常见原因是引号没转义或写成了字符串"
		return call.Result, call.Err
	}

	tool, ok := p.reg.Get(call.Name)
	if !ok {
		// 措辞对齐 DSH：这是最常见的一类失败（真实语料里 367 个失败样本中 84 个是它，
		// 因为工具集会随模式/配置变）。模型看到的名字和它调用的名字必须一致。
		call.Err = fmt.Errorf(`unknown tool %q`, call.Name)
		call.Result.Hint = "用工具表里真实存在的名字；不确定就先看当前模式给了哪些工具"
		return call.Result, call.Err
	}

	// 2. execute。统一截止时间（timeout-policy）装在这一层：工具自己的超时是
	// "它知道自己该跑多久"，这一层是"它挂死了也得有人拉闸"。
	p.bus.Emit(spi.EventToolExecute, call)

	// 2a. tools/execute：顶层调用的工具体是**副作用边界**。官方在这里先让
	// 请求前缀耐久，再判 abort；两条失败路径都绝不进入工具体（零执行）。
	//
	// 为什么 abort 判定放在落盘之后：官方顺序就是 flush → check aborted。
	// 反过来会让"已取消"的调用连落盘都不做，崩溃时这一轮在飞什么就无从判断。
	if p.opts.BeforeDispatch != nil && call.Session != "" && !call.Nested {
		if err := p.opts.BeforeDispatch(ctx, call); err != nil {
			call.Err = fmt.Errorf("工具调用前落盘失败，工具体未执行: %w", err)
			call.Result.Hint = "这次调用没有派发：请求前缀没能耐久落盘。先修好 home/磁盘再重试；" +
				"不要假设工具体做过任何副作用"
			return call.Result, call.Err
		}
		if err := ctx.Err(); err != nil {
			// 官方 abortedBeforeDispatchResult()：不是"结果未知"，是"确定没做"。
			call.Result = abortedBeforeDispatchResult()
			call.Err = fmt.Errorf("%w: %w", ErrToolAbortedBeforeDispatch, err)
			return call.Result, call.Err
		}
	}
	call.Result, call.Err = p.executeWithDeadline(ctx, tool, call.Args)

	// 2b. 成功、但什么都没返回 —— 静默成功的工具版。
	//
	// 模型会看到「（无输出）」：它不知道该不该相信这次调用真的做了什么，
	// 下一步只能瞎试。而流水线这边一切正常（ok=true、无 error），没人会去查。
	// 这里补一句人话 + 一条下一步，不改失败语义（err 仍然是 nil）。
	//
	// 判据是"模型看得见的字段全空"，不是"看起来短"：Data = [] 是**有效载荷**
	// （"集合是空的"本身就是答案），Summary 只有一行也算有产出。
	if call.Err == nil && isEmptySuccess(call.Result) {
		call.Result.Summary = fmt.Sprintf("工具 %q 执行成功，但没有返回任何模型可见的输出", call.Name)
		call.Result.Hint = "不要假设它做了什么：需要确认就用只读手段核实（读文件/看状态/查日志）；" +
			"这通常是该工具实现的问题，不是你的参数写错了"
		p.log.Warn("工具成功但没有产出（静默成功）", "tool", call.Name)
	}

	// 3. 提示层：失败必须带下一步。缺失就兜底并记账（CI 应该让这种代码进不来）。
	if call.Err != nil && call.Result.Hint == "" {
		call.Result.Hint = fmt.Sprintf("工具 %q 失败了；先确认参数与环境再决定是否重试", call.Name)
		p.recordR3Violation(call.Name, call.Err)
	}

	// 4. 序列化层 + 截断层。
	if err := enforceSerialization(&call.Result); err != nil {
		call.Err = err
		call.Result.Hint = "结果无法序列化成 JSON；这是该工具实现的 bug，改用其它工具或修复它"
		return call.Result, call.Err
	}
	p.spill(&call.Result, call.Name)

	// 5. post-execute：可改写结果（裁剪、脱敏、补提示）。
	if _, err := p.bus.Waterfall(spi.EventToolPostExecute, call); err != nil {
		call.Err = err
		if call.Result.Hint == "" {
			call.Result.Hint = "后置策略拒绝了这次结果：" + err.Error()
		}
		return call.Result, call.Err
	}

	// 6. result：广播，不等待。
	p.bus.Emit(spi.EventToolResult, call)
	return call.Result, call.Err
}

// ToolTimeoutError 是一次工具调用超过统一截止时间的结构化错误。
//
// 它**刻意不 wrap context.DeadlineExceeded**：agent 循环把 DeadlineExceeded 归进
// "这次调用被打断了、结果未知"那一类文案，而超时是个确定结论（工具没在预算内返回，
// 已经被取消）。两者的下一步不一样——取消是"有人把它停了，先核实现场"，超时是
// "它太慢，换更小的范围或转后台"。混成一类，模型会照着错的那条走。
//
// 调用方用 errors.As 认它；用 errors.Is(err, context.Canceled) 认用户取消。
type ToolTimeoutError struct {
	Tool   string        // 工具名（模型看到的名字）
	Budget time.Duration // 这次的统一预算
	Waited time.Duration // 实际等到的时长（≈ 预算；诊断用，不进模型文案）
}

func (e *ToolTimeoutError) Error() string {
	return fmt.Sprintf("tool call %q timed out after %s (TOOL_TIMEOUT); it was cancelled and its result is unknown",
		e.Tool, e.Budget)
}

// execOutcome 是执行 goroutine 交回的结果。
//
// 用值传递、不用共享指针：超时之后工具可能还在写自己的字段，任何共享状态都会
// 变成数据竞争（-race 下最先炸的就是这里）。
type execOutcome struct {
	res spi.Result
	err error
}

// executeWithDeadline 在统一预算内执行工具；到点立刻返回，**不等**它。
//
// 三种结局必须分得开：
//
//   - 预算内返回：原样透传。**已完成的调用不受影响**——成功还是成功，失败还是那个
//     失败，既有行为一条都不改。
//   - 预算到点：返回 *ToolTimeoutError + 结构化 Result（Summary/Data/Hint）。工具
//     晚到的结果**绝不复用**：超时就是失败，不能因为它几百毫秒后自己跑完了就改口
//     说这次成功。
//   - 父 ctx 结束（用户 cancel / 上游把这轮掐了）：原样返回父 ctx 的错误，
//     errors.Is(err, context.Canceled) 仍然成立——取消与超时是两件事。
//
// 为什么必须起一个 goroutine：协作式取消只对**看 ctx** 的工具有效。卡在无 deadline
// 的读、卡在 CGO 调用、卡在第三方库里的工具根本不看 ctx。流水线要是同步调 Exec，
// 就还是得陪着它等——那样"超时"只是写给日志看的一句话，turn 照样死。
//
// 代价写在明处：Go 杀不掉 goroutine，挂死的工具那个 goroutine 会一直挂着（它的 ctx
// 已经结束，能不能退出来取决于工具自己）。这一层能保证的是**流水线不再等它**，
// 而不是"它一定停止"。
func (p *Pipeline) executeWithDeadline(ctx context.Context, tool spi.Tool, args json.RawMessage) (spi.Result, error) {
	budget := p.opts.ToolTimeout
	if budget <= 0 {
		// 明确关闭（负值）：行为与加这一层之前逐字节一致，连 panic 也照常上抛。
		return tool.Exec(ctx, args)
	}

	execCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel() // 超时路径上这一句同样必要：让还在跑的工具看到 ctx 结束

	// 有缓冲 1：超时之后 goroutine 仍能把结果写完并退出，既不泄漏也不永久阻塞。
	done := make(chan execOutcome, 1)
	started := time.Now()
	go func() {
		out := execOutcome{}
		defer func() {
			// 这条 goroutine 没有人替它 recover：工具 panic 会带走整个宿主进程。
			// 转成错误返回——和超时一样，只是"这次调用失败，turn 继续"。
			if r := recover(); r != nil {
				out = execOutcome{err: fmt.Errorf("工具 %q 执行时 panic: %v", tool.Name(), r)}
			}
			done <- out
		}()
		out.res, out.err = tool.Exec(execCtx, args)
	}()

	select {
	case out := <-done:
		if !cutByDeadline(out, execCtx, ctx) {
			res, err := out.res, out.err
			if err != nil && strings.TrimSpace(res.Hint) == "" {
				// R3：失败必须带一句给模型看的下一步指引。工具自己没给就补一句通用的。
				res.Hint = "这次调用失败了：先读现场（文件/进程/日志）确认它到底做没做，再决定重试与否"
				p.recordR3Violation(tool.Name(), err)
			}
			return res, err
		}
		// 被本层 deadline 叫醒的"假完成"：按超时继续往下走。
	case <-execCtx.Done():
		// 工具可能正好在这一刻返回：先收一次，已完成的调用不受影响。
		select {
		case out := <-done:
			if !cutByDeadline(out, execCtx, ctx) {
				res, err := out.res, out.err
				if err != nil && strings.TrimSpace(res.Hint) == "" {
					res.Hint = "这次调用失败了：先读现场（文件/进程/日志）确认它到底做没做，再决定重试与否"
					p.recordR3Violation(tool.Name(), err)
				}
				return res, err
			}
		default:
		}
	}
	if err := ctx.Err(); err != nil {
		// 父 ctx 先结束 = 取消（或上游自己设的 deadline），不是本层的超时。
		// 原样返回父 ctx 的错误，让上层还能认出"这是取消"。
		return spi.Result{
			Hint: "结果未知：先读一下现场（文件/进程/日志）确认它到底做没做，再决定重试与否",
		}, err
	}
	waited := time.Since(started)
	p.log.Warn("工具调用超时（统一截止时间）", "tool", tool.Name(),
		"timeout_ms", budget.Milliseconds(), "waited_ms", waited.Milliseconds())
	timedOut := timeoutResult(tool.Name(), budget, waited)
	// R3：把 Hint 显式落在紧邻 return 的位置（timeoutResult 已经填了同一句），
	// 让「每个 error 分支都带 Hint」的守卫能按同一层配对。
	timedOut.Hint = fmt.Sprintf("这次调用没在 %s 内返回、已被取消，结果未知：别当成没生效——"+
		"先用只读手段核实现场（读文件/看进程/查状态）；要重试就缩小范围或分步做，"+
		"预计很久的活改用 run_in_background", budget)
	return timedOut, &ToolTimeoutError{Tool: tool.Name(), Budget: budget, Waited: waited}
}

// cutByDeadline 判断一次"刚好卡在截止时间上返回"的结果算不算**被本层掐断**。
//
// 为什么需要它：听 ctx 的工具（`<-ctx.Done(); return Result{}, ctx.Err()`）在我们
// 的 deadline 触发后会被**立刻**唤醒，把 ctx.Err()（DeadlineExceeded）交回来。这
// 和"工具真的跑完了"在同一时刻抵达，select 会随机挑一个——同一次超时有时报成
// "超时"，有时报成"被打断/结果未知"，而 agent 循环对这两类文案完全不同。
//
// 判据只有一条：父 ctx 还活着，而工具交回的错误就是本层 ctx 的结束原因。
// 那时它不可能是"完成"，只可能是被我们叫醒的。
func cutByDeadline(out execOutcome, execCtx, parent context.Context) bool {
	if parent.Err() != nil || execCtx.Err() == nil {
		// 父 ctx 结束（取消/上游截止）由调用方单独分派；ctx 还没结束则是真完成。
		return false
	}
	return out.err != nil && errors.Is(out.err, context.DeadlineExceeded)
}

// timeoutResult 是超时这一刻模型能看到的东西。
//
// 三样都要给：Summary 给 TUI 那一行；Data 给模型结构化的事实（哪个工具、多大预算、
// 能不能原样重试）；Hint 是 R3 要的下一步——没有它，模型唯一会做的就是原样再调一次，
// 然后再等一个预算。**不复述**：长句只在 Hint 里出现一次，Data 只放事实。
func timeoutResult(tool string, budget, waited time.Duration) spi.Result {
	return spi.Result{
		Summary: fmt.Sprintf("工具 %q 超过 %s 没有返回，已取消", tool, budget),
		Data: map[string]any{
			"code":       "tool_timeout",
			"tool":       tool,
			"timeout_ms": budget.Milliseconds(),
			"waited_ms":  waited.Milliseconds(),
			"retryable":  false,
		},
		Hint: fmt.Sprintf("这次调用没在 %s 内返回、已被取消，结果未知：别当成没生效——"+
			"先用只读手段核实现场（读文件/看进程/查状态）；要重试就缩小范围或分步做，"+
			"预计很久的活改用 run_in_background", budget),
	}
}

// isEmptySuccess 判断一次**成功**的工具调用是不是什么载荷都没留下。
//
// 判据是"**模型**能不能看到东西"：Summary/Text/Data 是给模型的正文，
// Artifacts/Truncated 里有取回路径，Hint 是下一步，Images 会另起一条 user 消息发过去。
// 任何一样存在，这次调用就有东西可看。
//
// **View 不算**：那是给客户端画卡片的，模型看不到它。一个只返回 View 的工具
// 在界面上有内容、在模型眼里却是一行「（无输出）」——正是要抓的形状。
//
// 特别注意 Data 的 nil / 空数组之别：nil = 没有数据；[] = "集合是空的"，
// 后者是答案，不能当空结果处理（否则 glob 搜不到文件时会被说成工具坏了）。
func isEmptySuccess(res spi.Result) bool {
	return res.Summary == "" && res.Text == "" && res.Data == nil &&
		len(res.Artifacts) == 0 && res.Truncated == nil && res.Hint == "" &&
		len(res.Images) == 0
}

// enforceSerialization 保证 Data 是合法 JSON（键名约定靠 review + 快照测试）。
func enforceSerialization(res *spi.Result) error {
	if res.Data == nil {
		return nil
	}
	if _, err := json.Marshal(res.Data); err != nil {
		return fmt.Errorf("结果 Data 不能序列化为 JSON: %w", err)
	}
	return nil
}

// spill 是截断层：超阈值外置成 artifact + 指针。
//
// 尽力而为：存不下去就保留内联，绝不把一次成功的调用变成失败；
// 替换体本身永远不超阈值，防止「取回 → 又被外置」的死循环。
//
// **Text 也必须一起截**。只换 Data 的话，ProjectResult 会优先用 Text，
// 模型照样看到全文——那样 spill 等于没做（实测 9000 字符的输出，
// 阈值 4000，模型仍然看到 9361 字符）。
func (p *Pipeline) spill(res *spi.Result, toolName string) {
	// 先把 Text 截掉：无论走不走 Data 外置，模型看到的那段文本都要有界。
	textSpilled := false
	if len(res.Text) > p.opts.MaxChars {
		textSpilled = true
	}

	if res.Data == nil {
		// 没有结构化 Data 也不能放过：Text 本身超了就直接截。
		if textSpilled {
			p.spillText(res, textSpilled, "")
		}
		return
	}
	raw, err := json.Marshal(res.Data)
	if err != nil || len(raw) <= p.opts.MaxChars {
		if textSpilled {
			p.spillText(res, textSpilled, "")
		}
		return
	}

	path, err := p.writeSpill(raw)
	if err != nil {
		p.log.Warn("结果外置失败，保留内联", "error", err)
		if textSpilled {
			p.spillText(res, textSpilled, "")
		}
		return
	}

	// Data 里**不再放 preview**：模型看到的正文由 Text 承担（见 spillText），
	// 再塞一份预览等于同一段内容出现两次，而且预览是 JSON 串（多一层转义）。
	res.Artifacts = append(res.Artifacts, spi.Artifact{Name: toolName, Path: path, Bytes: int64(len(raw))})
	res.Truncated = &spi.Truncation{
		Kept:    len([]rune(res.Text)),
		Total:   len(raw),
		Pointer: path,
	}
	res.Data = map[string]any{
		"spilled":  true,
		"bytes":    len(raw),
		"artifact": path,
	}
	if textSpilled {
		p.spillText(res, textSpilled, path)
	}
	// 正文已经被截过（里面带着指针），就别再补一条同样意思的 Hint：
	// 投影层还会再加一条"已截断"说明，三条消息说同一件事纯属浪费。
	if res.Hint == "" && !textSpilled {
		res.Hint = "结果已外置到 " + path + "；用 read 工具（offset/limit 分段）读全文"
	}
}

// spillText 把模型可见正文压到预算内：留头留尾，中间换一行指针。
//
// 留尾巴是因为很多输出（日志、报错、命令结果）的关键信息在最后。
func (p *Pipeline) spillText(res *spi.Result, over bool, pointer string) {
	budget := p.opts.MaxChars * 3 / 4 // 留出指针/提示的位置，正文本身不超阈值
	head := budget * 3 / 4
	tail := budget - head

	orig := res.Text
	total := len([]rune(orig))
	r := []rune(orig)
	keepHead := r[:minInt(head, len(r))]
	keepTail := []rune{}
	if len(r) > head+tail {
		keepTail = r[len(r)-tail:]
	}

	var b strings.Builder
	b.WriteString(string(keepHead))
	b.WriteString("\n" + omittedNotice(string(keepHead), string(keepTail), total-head-tail, pointer) + "\n")
	b.WriteString(string(keepTail))
	res.Text = b.String()
	if res.Truncated == nil {
		res.Truncated = &spi.Truncation{Kept: head + tail, Total: total, Pointer: pointer}
	}
}

// omittedNotice 写省略说明。
//
// 带行号的输出（read/grep 渲染成 "N: 内容"）要说清**省略了哪几行、怎么一次取回**。
// 不然模型只知道"中间少了东西"，要么再试探一次，要么干脆放弃。
// 一次精确调用 vs 两次试探，差 5–6k token（见 docs/token-budget.md §6）。
func omittedNotice(head, tail string, omitted int, pointer string) string {
	from, to, ok := omittedLineRange(head, tail)
	if ok && to >= from && from > 0 {
		return fmt.Sprintf("…（第 %d–%d 行已省略；取回：read offset=%d limit=%d%s）…",
			from, to, from, to-from+1, pointerSuffix(pointer))
	}
	if pointer != "" {
		return fmt.Sprintf("…（中间省略 %d 字符，全文在 %s）…", omitted, pointer)
	}
	return fmt.Sprintf("…（中间省略 %d 字符）…", omitted)
}

// omittedLineRange 从头部最后一行号与尾部第一行号，推出被省略的行区间。
func omittedLineRange(head, tail string) (int, int, bool) {
	lastHead := lastLineNumber(head)
	firstTail := firstLineNumber(tail)
	if lastHead == 0 || firstTail == 0 {
		return 0, 0, false
	}
	return lastHead + 1, firstTail - 1, true
}

func lastLineNumber(s string) int {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, ok := lineNumber(lines[i]); ok {
			return n
		}
	}
	return 0
}

func firstLineNumber(s string) int {
	for _, ln := range strings.Split(s, "\n") {
		if n, ok := lineNumber(ln); ok {
			return n
		}
	}
	return 0
}

// lineNumber 认 "123: 内容" 这种渲染格式（行号最多 9 位）。
func lineNumber(line string) (int, bool) {
	i := strings.Index(line, ": ")
	if i <= 0 || i > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(line[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func pointerSuffix(pointer string) string {
	if pointer == "" {
		return ""
	}
	return "；或从 " + pointer + " 取全文"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (p *Pipeline) writeSpill(raw []byte) (string, error) {
	if err := os.MkdirAll(p.opts.SpillDir, 0o755); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()

	f, err := os.CreateTemp(p.opts.SpillDir, fmt.Sprintf("spill-%04d-*.json", seq))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return "", err
	}
	return filepath.ToSlash(f.Name()), nil
}

// truncateUTF8 按字节预算截断，但不会切断一个多字节字符。
func truncateUTF8(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
