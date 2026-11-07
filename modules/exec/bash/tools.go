package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/sandbox"
	"dshgo/internal/spi"
)

// Options 是执行层的可调参数。
type Options struct {
	// IdleBudget 是「多久没有输出就转后台」。默认 10 分钟（与 DSH 的前台宽限同量级）。
	IdleBudget time.Duration
	// SandboxGate 是命令策略闸门。nil 时从进程环境解析（DSHD_SANDBOX_MODE）；
	// 环境也没有 = 策略层未接线，工具保持旧行为，但任何 sandbox_permissions
	// 请求都会 fail-loud，不会静默当作放行。
	SandboxGate *sandbox.Gate
}

func (o Options) idle() time.Duration {
	if o.IdleBudget > 0 {
		return o.IdleBudget
	}
	return 10 * time.Minute
}

func registerTools(s *kernel.Scope, svc *Service, opts Options) error {
	reg, err := kernel.Inject(s, spi.ToolsKey)
	if err != nil {
		return err
	}
	gate := opts.SandboxGate
	if gate == nil {
		// 装配期解析：未知 DSHD_SANDBOX_MODE 必须在这里炸，不能等到第一条命令。
		gate, err = sandbox.GateFromEnv(svc.Cwd())
		if err != nil {
			return fmt.Errorf("bash 装配沙箱策略失败: %w", err)
		}
	}
	for _, t := range []spi.Tool{
		&shellTool{svc: svc, budget: opts.idle(), gate: gate},
		&jobOutputTool{svc: svc},
		&jobKillTool{svc: svc},
		&jobListTool{svc: svc},
	} {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// ---------- shell ----------

var shellSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的 bash 命令；cwd 跨命令保留"},
    "description": {"type": "string", "description": "这段命令干什么，5-10 个字，给用户看的（例如「列出当前目录」）"},
    "idle_seconds": {"type": "integer", "minimum": 1, "maximum": 900, "description": "多久无输出转后台，默认 600"},
    "run_in_background": {"type": "boolean", "description": "立刻转后台并返回 job_id，不等命令结束（长任务用这个，别去调小 idle_seconds）"},
    "sandbox_permissions": {"type": "string", "enum": ["workspace-write", "danger-full-access"], "description": "可选：仅本次调用的 sandbox 请求。danger-full-access 需要先经审批；不会改变会话档位"}
  },
  "required": ["command", "description"],
  "additionalProperties": false
}`)

type shellTool struct {
	svc    *Service
	budget time.Duration
	// gate 是命令策略闸门；nil = 策略层未接线（旧行为，但拒绝任何一次性升级请求）。
	gate *sandbox.Gate
}

func (t *shellTool) Name() string   { return "bash" }
func (t *shellTool) Caps() spi.Caps { return spi.Caps{Writes: true, Executes: true} }
func (t *shellTool) Description() string {
	return "在持久 bash 会话里执行命令，cwd 跨命令保留；无输出超时就转后台。"
}
func (t *shellTool) Schema() json.RawMessage { return shellSchema }

type shellArgs struct {
	Command            string `json:"command"`
	Description        string `json:"description"`
	IdleSeconds        int    `json:"idle_seconds"`
	RunInBackground    bool   `json:"run_in_background"`
	SandboxPermissions string `json:"sandbox_permissions"`
}

type shellData struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Cwd       string `json:"cwd"`
	JobID     string `json:"job_id,omitempty"`
	MovedToBg bool   `json:"moved_to_bg,omitempty"`
}

func (t *shellTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a shellArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"command": "..."}`}, fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return spi.Result{Hint: "command 不能为空"}, fmt.Errorf("command 为空")
	}
	permission, err := sandbox.ParsePermission(a.SandboxPermissions)
	if err != nil {
		return spi.Result{Hint: `sandbox_permissions 只能是 workspace-write 或 danger-full-access（或省略）`}, err
	}
	// 策略先于一切进程动作：deny / needs-approval 在这里就返回，
	// svc.Run/RunBackground 不会被调用，因此不会启动 shell、不会留下 job。
	decision, err := t.authorize(ctx, a.Command, permission)
	if err != nil {
		out := spi.Result{Data: sandboxResult(decision)}
		if decision != nil && decision.Action == sandbox.ActionNeedsApproval {
			out.Summary = "沙箱升级待审批，命令未执行"
		} else {
			out.Summary = "沙箱策略拒绝，命令未执行"
		}
		out.Hint = sandboxHint(err)
		return out, err
	}

	// 立刻转后台：不走"等 idle 超时"那条路。
	// 语义与官方一致——调用方马上拿到 job_id，用 job_output 去取。
	if a.RunInBackground {
		res, err := t.svc.RunBackground(a.Command)
		if err != nil {
			return spi.Result{
				Hint: "后台启动失败：看 job_list 有没有残留，或去掉 run_in_background 在前台跑一次看真实报错",
			}, err
		}
		if res.JobID == "" {
			// 1ms 内就跑完的命令（echo/true 这类）：这不是错误，如实说它在后台跑完了。
			return spi.Result{
				Summary: "命令瞬间结束（没进后台队列）",
				Text:    res.Output,
				Data:    shellData{Output: res.Output, ExitCode: res.ExitCode, Cwd: res.Cwd},
			}, nil
		}
		return spi.Result{
			Summary: "已在后台启动 " + res.JobID,
			Text:    "已转后台：" + res.JobID + "（用 job_output 取输出，job_kill 终止）",
			Data:    shellData{JobID: res.JobID, MovedToBg: true, Cwd: res.Cwd},
		}, nil
	}

	budget := t.budget
	if a.IdleSeconds > 0 {
		budget = time.Duration(a.IdleSeconds) * time.Second
	}
	if budget > 15*time.Minute {
		budget = 15 * time.Minute
	}

	// 中断语义：ctx 取消（客户端 cancel / 断连）时必须**立刻**返回，
	// 不能让调用方陪着 shell 等到 idle 超时（实测能等 20 秒）。
	// 命令可能还在 shell 里跑——那是诚实的"结果未知"，
	// 由上层把它报成 outcome is unknown，而不是假装失败。
	type runOut struct {
		res spi.ExecResult
		err error
	}
	done := make(chan runOut, 1)
	go func() {
		r, e := t.svc.Run(a.Command, budget)
		done <- runOut{r, e}
	}()

	var res spi.ExecResult
	select {
	case out := <-done:
		res, err = out.res, out.err
	case <-ctx.Done():
		return spi.Result{
			Hint: "命令可能仍在 shell 里跑；先 job_list 看有没有后台任务，或用一条只读命令确认现场的当前状态",
		}, ctx.Err()
	}

	if err != nil {
		if errors.Is(err, ErrSessionEnded) {
			return spi.Result{Hint: "命令自己结束了 shell（例如 exit）；下一条命令会自动开新会话，但 cwd 会回到起点"}, err
		}
		return spi.Result{Hint: "命令没能执行：确认 shell 可用（Git Bash）、命令语法正确"}, err
	}

	d := shellData{
		Output:    res.Output,
		ExitCode:  res.ExitCode,
		Cwd:       res.Cwd,
		JobID:     res.JobID,
		MovedToBg: res.MovedToBg,
	}
	summary := fmt.Sprintf("退出码 %d，输出 %d 字节", res.ExitCode, len(res.Output))
	hint := ""
	switch {
	case res.MovedToBg:
		summary = fmt.Sprintf("已转后台 %s（期间无输出）", res.JobID)
		hint = "任务在后台继续跑；用 job_output <id> 取后续输出，job_kill <id> 终止"
	case res.ExitCode != 0:
		hint = "命令失败：先读输出里的报错原文，再决定改命令还是改参数；不要原样重试"
	}
	// Text 是**模型看到的正文**：直接给原始输出。
	//
	// 不给的话，投影层会退回 Summary + JSON(Data)，于是命令输出会被
	// JSON 转义一遍（换行和引号都要带反斜杠），外面还裹着 exit_code/cwd
	// 这些模型用不上的字段。bash 占工具回显的 64.5%，这一层浪费被放大得最厉害。
	return spi.Result{
		Summary: summary,
		Text:    shellView(res),
		Data:    d,
		Hint:    hint,
		View: &spi.ViewSpec{
			Kind:  spi.ViewPlain,
			Title: "bash",
			Badge: fmt.Sprintf("exit %d", res.ExitCode),
			Body:  res.Output,
		},
	}, nil
}

// authorize 在启动任何进程之前做策略判定。
//
// 返回 nil decision 表示"策略层未接线且本次没有请求升级"——旧行为；
// 只要 decision 非 nil 且不是 allow，调用方必须直接返回错误。
func (t *shellTool) authorize(ctx context.Context, command string, permission sandbox.Permission) (*sandbox.Decision, error) {
	if t.gate == nil {
		if permission != "" {
			return nil, fmt.Errorf("%w: 本进程没有 %s；带 sandbox_permissions 的调用一律拒绝",
				sandbox.ErrPolicyUnwired, sandbox.EnvMode)
		}
		return nil, nil
	}
	d, err := t.gate.EvaluateCommand(ctx, command, t.svc.Cwd(), permission)
	if err != nil {
		return nil, err
	}
	if d.Action == sandbox.ActionAllow {
		return &d, nil
	}
	return &d, d.Error()
}

func sandboxResult(d *sandbox.Decision) any {
	if d == nil {
		return nil
	}
	return map[string]any{"sandbox": d}
}

func sandboxHint(err error) string {
	switch {
	case errors.Is(err, sandbox.ErrNeedsApproval):
		return "这次调用请求了 danger-full-access，但审批还没完成；让用户批准这次调用后重试，或改成工作区内的操作"
	case errors.Is(err, sandbox.ErrDenied):
		return "当前会话 sandbox 档位拒绝这条命令；把命令限制在工作区 cwd 内，或让用户显式切到 danger-full-access 档"
	case errors.Is(err, sandbox.ErrPolicyUnwired):
		return "宿主没有把 sandbox 策略接进本进程；去掉 sandbox_permissions 参数，或让宿主设置 DSHD_SANDBOX_MODE"
	case errors.Is(err, sandbox.ErrMissingContext):
		return "这次工具调用没有会话上下文，策略无法判定；不要重试同一次调用，报告宿主"
	default:
		return "命令没有启动；按错误里的原因调整后再试"
	}
}

// ---------- job_output ----------

var jobOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "job_id": {"type": "string", "description": "任务 id，如 job-1"},
    "wait": {"type": "boolean", "description": "是否阻塞等待任务结束，默认 false"},
    "timeout_ms": {"type": "integer", "minimum": 0, "maximum": 120000, "description": "wait=true 时最多等多少毫秒，默认 30000"}
  },

  "additionalProperties": false
}`)

type jobOutputTool struct{ svc *Service }

func (t *jobOutputTool) Name() string   { return "job_output" }
func (t *jobOutputTool) Caps() spi.Caps { return spi.Caps{} }
func (t *jobOutputTool) Description() string {
	return "取后台任务输出（可短等）。任务没结束也能反复取。"
}
func (t *jobOutputTool) Schema() json.RawMessage { return jobOutputSchema }

func (t *jobOutputTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a struct {
		JobID     string `json:"job_id"`
		Wait      bool   `json:"wait"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"job_id": "job-1", "wait"?: false, "timeout_ms"?: 30000}`}, err
	}
	// 空 id 是**参数没给**，不是"任务不存在"。两者的下一步完全不同：
	// 前者是补参数，后者是去 job_list 里找 id。别把前者报成后者。
	id, perr := t.svc.pickJobID(a.JobID, "job_output")
	if perr != nil {
		return spi.Result{Hint: perr.Error()}, perr
	}
	a.JobID = id
	wait := time.Duration(0)
	if a.Wait {
		ms := a.TimeoutMS
		if ms <= 0 {
			ms = 30000
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	out, err := t.svc.Output(a.JobID, wait)
	if err != nil {
		return spi.Result{Hint: "用 job_list 看有哪些任务 id"}, err
	}
	summary := fmt.Sprintf("%s 输出 %d 字节（%s）", out.ID, len(out.Output), doneText(out.Done))
	hint := ""
	if !out.Done && out.Output == "" {
		hint = "任务还没吐字：可能是子进程把 stdout 缓冲在管道里；要实时看就别把输出重定向到文件"
	}
	if out.Done && out.ExitCode != 0 {
		hint = fmt.Sprintf("任务已结束，退出码 %d；读输出里的报错再决定下一步", out.ExitCode)
	}
	return spi.Result{Summary: summary, Data: out, Hint: hint}, nil
}

func doneText(done bool) string {
	if done {
		return "已结束"
	}
	return "运行中"
}

// ---------- job_kill ----------

var jobKillSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "job_id": {"type": "string", "description": "任务 id"},
    "reason": {"type": "string", "description": "可选：记录在日志里并转发给任务"}
  },

  "additionalProperties": false
}`)

// pickJobID 处理"只有一个后台任务时不填 job_id"的情形。
//
// 与 schedule/watch 同一套规则：唯一时兜底，多个时必须选（猜错就杀错了任务）。
// 拒绝时把候选 id 列出来——只说"必填"等于让模型再跑一次 job_list。
func (t *Service) pickJobID(id, who string) (string, error) {
	if strings.TrimSpace(id) != "" {
		return id, nil
	}
	list := t.List()
	switch len(list) {
	case 0:
		return "", fmt.Errorf("还没有后台任务：用 bash 起一个（返回里会带 job id）")
	case 1:
		return list[0].ID, nil
	default:
		ids := make([]string, 0, len(list))
		for _, j := range list {
			ids = append(ids, j.ID)
		}
		return "", fmt.Errorf("%s：现在有 %d 个后台任务（%s），job_id 必须填一个", who, len(list), strings.Join(ids, "、"))
	}
}

type jobKillTool struct{ svc *Service }

func (t *jobKillTool) Name() string            { return "job_kill" }
func (t *jobKillTool) Caps() spi.Caps          { return spi.Caps{Destructive: true} }
func (t *jobKillTool) Description() string     { return "终止一个后台任务。" }
func (t *jobKillTool) Schema() json.RawMessage { return jobKillSchema }

func (t *jobKillTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"job_id": "job-1", "reason"?: "..."}`}, err
	}
	id, perr := t.svc.pickJobID(a.JobID, "job_kill")
	if perr != nil {
		return spi.Result{Hint: perr.Error()}, perr
	}
	a.JobID = id
	if err := t.svc.Kill(a.JobID); err != nil {
		return spi.Result{Hint: "用 job_list 确认 id；已结束的任务不需要终止"}, err
	}
	return spi.Result{
		Summary: "已终止 " + a.JobID,
		Data:    map[string]any{"job_id": a.JobID, "killed": true},
		Hint:    "已停止该任务所属的进程树；Windows 使用 Job Object，Unix 使用独立进程组，停止前已核对根进程身份",
	}, nil
}

// ---------- job_list ----------

var jobListSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

type jobListTool struct{ svc *Service }

func (t *jobListTool) Name() string            { return "job_list" }
func (t *jobListTool) Caps() spi.Caps          { return spi.Caps{} }
func (t *jobListTool) Description() string     { return "列出后台任务及其状态。" }
func (t *jobListTool) Schema() json.RawMessage { return jobListSchema }

func (t *jobListTool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	jobs := t.svc.List()
	if len(jobs) == 0 {
		return spi.Result{
			Summary: "没有后台任务",
			Data:    map[string]any{"jobs": []spi.JobInfo{}},
		}, nil
	}
	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, []string{j.ID, doneText(j.Done), fmt.Sprintf("%d", j.ExitCode), fmt.Sprintf("%d", j.Bytes), j.Command})
	}
	return spi.Result{
		Summary: fmt.Sprintf("%d 个后台任务", len(jobs)),
		Data:    map[string]any{"jobs": jobs},
		View: &spi.ViewSpec{
			Kind:    spi.ViewTable,
			Title:   "后台任务",
			Columns: []string{"id", "状态", "退出码", "字节", "命令"},
			Rows:    rows,
		},
	}, nil
}

// shellView 是命令结果给模型看的样子：一行元信息 + 原始输出（不做 JSON 转义）。
func shellView(res spi.ExecResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[exit %d]", res.ExitCode)
	if res.Cwd != "" {
		fmt.Fprintf(&b, " [cwd %s]", res.Cwd)
	}
	if res.Output != "" {
		b.WriteString("\n")
		b.WriteString(res.Output)
	}
	return b.String()
}
