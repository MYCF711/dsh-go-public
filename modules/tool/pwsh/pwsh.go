// Package pwsh 提供 `pwsh` 工具：在 Windows 上（或任何装了 PowerShell 7 的机器上）
// 执行一条 PowerShell 命令，把合并后的输出交回模型。
//
// 与 bash 工具的关系：两者是同一件事的两个壳，选哪个由壳的可用性决定，不由模型猜。
// 本工具优先用 pwsh（PowerShell 7，默认 UTF-8），退回到 powershell.exe
// （Windows PowerShell 5.1），后者要额外把控制台输出编码掰成 UTF-8，否则中文全变乱码。
//
// 三条实现上的注意点（都是踩过的坑）：
//  1. -NoProfile / -NonInteractive：不加载用户 profile、不弹交互提示——否则可能挂死；
//  2. 输出统一限长：命令一跑就是几百 KB，不拦的话截断层之外还多一层浪费；
//  3. 非零退出码是**命令失败**而不是**工具失败**：输出照回，错误照报，Hint 给下一步。
package pwsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/sandbox"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的 PowerShell 命令（一条或多条，用分号分隔）"},
    "workdir": {"type": "string", "description": "可选：工作目录（相对当前工作目录），默认当前目录"},
    "timeout_seconds": {"type": "integer", "description": "可选：超时秒数，默认 120，上限 600"},
    "sandbox_permissions": {"type": "string", "enum": ["workspace-write", "danger-full-access"], "description": "可选：仅本次调用的 sandbox 请求。danger-full-access 需要先经审批；不会改变会话档位"}
  },
  "required": ["command"],
  "additionalProperties": false
}`)

const (
	// 单次调用回给模型的输出上限。命令输出通常远超这个量，
	// 与其让它把上下文撑爆，不如这里就截断并明说。
	maxOutput = 64 << 10
	// 默认/最长超时。超过这个量级的任务该用后台任务工具，不是这里。
	defaultTimeout = 120 * time.Second
	maxTimeout     = 600 * time.Second
)

// legacyPrologue 只在退回 Windows PowerShell 5.1 时加：它重定向时按控制台代码页
// 写字节，中文会变成乱码。pwsh 7 默认就是 UTF-8，不需要动。
const legacyPrologue = `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; `

// execCommandContext 是包内唯一的进程构造点。
// 测试把它换成一个计数桩，直接证明"策略拒绝时零进程"，而不是只看副作用。
var execCommandContext = exec.CommandContext

type Tool struct {
	// gate 是命令策略闸门；nil = 策略层未接线（旧行为，但拒绝任何一次性升级请求）。
	gate *sandbox.Gate
	// lastDecision 只保留给错误结果的结构化 Data；授权判断本身不读它。
	lastDecision *sandbox.Decision
}

// Option 是 Tool 的可选项（测试注入与将来宿主接线用）。
type Option func(*Tool)

// WithSandboxGate 注入命令策略闸门。nil 被忽略。
func WithSandboxGate(g *sandbox.Gate) Option {
	return func(t *Tool) {
		if g != nil {
			t.gate = g
		}
	}
}

func newTool(opts ...Option) (*Tool, error) {
	t := &Tool{}
	for _, opt := range opts {
		opt(t)
	}
	if t.gate == nil {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("取当前目录失败: %w", err)
		}
		gate, err := sandbox.GateFromEnv(wd)
		if err != nil {
			return nil, fmt.Errorf("pwsh 装配沙箱策略失败: %w", err)
		}
		t.gate = gate
	}
	return t, nil
}

func (t *Tool) Name() string { return "pwsh" }
func (t *Tool) Caps() spi.Caps {
	// 会起子进程，命令里想写文件也拦不住，所以两样都如实声明。
	return spi.Caps{Executes: true, Writes: true}
}
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "执行 PowerShell 命令（优先 pwsh，退回 powershell）。查系统、跑脚本时用。"
}

type args struct {
	Command            string `json:"command"`
	Workdir            string `json:"workdir"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
	SandboxPermissions string `json:"sandbox_permissions"`
}

type data struct {
	Shell      string `json:"shell"`
	Command    string `json:"command"`
	Workdir    string `json:"workdir,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated,omitempty"`
	Bytes      int    `json:"bytes"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"command": "Get-ChildItem"}`}, err
	}
	cmdline := strings.TrimSpace(a.Command)
	if cmdline == "" {
		return spi.Result{Hint: "command 不能为空：给一条要执行的 PowerShell 命令"}, fmt.Errorf("command 为空")
	}

	dir := strings.TrimSpace(a.Workdir)
	if dir != "" {
		dir = filepath.FromSlash(dir)
		st, err := os.Stat(dir)
		if err != nil {
			return spi.Result{Hint: "workdir 不存在：先用 glob 或 read 确认目录，再传相对路径"},
				fmt.Errorf("workdir 不存在: %s", a.Workdir)
		}
		if !st.IsDir() {
			return spi.Result{Hint: "workdir 是文件不是目录：指定到父目录"}, fmt.Errorf("workdir 不是目录: %s", a.Workdir)
		}
	}

	permission, err := sandbox.ParsePermission(a.SandboxPermissions)
	if err != nil {
		return spi.Result{Hint: `sandbox_permissions 只能是 workspace-write 或 danger-full-access（或省略）`}, err
	}
	// 策略判定必须发生在 resolveShell/exec.Command 之前：deny/needs-approval 时零进程。
	if err := t.authorize(ctx, cmdline, dir, permission); err != nil {
		return spi.Result{
			Summary: "沙箱策略拒绝，命令未执行",
			Data:    map[string]any{"sandbox": t.lastDecision},
			Hint:    sandboxHint(err),
		}, err
	}

	shell, isLegacy, err := resolveShell()
	if err != nil {
		return spi.Result{Hint: "找不到 PowerShell：装 PowerShell 7（提供 pwsh）或改用 bash 工具"}, err
	}

	timeout := defaultTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	script := cmdline
	if isLegacy {
		script = legacyPrologue + cmdline
	}
	// 注意：这里一个参数传整条脚本。命令里带双引号时，Go 会按 argv 规则转义，
	// PowerShell 多数情况下能正确还原；极复杂的嵌套引号建议写进 .ps1 再执行。
	cmd := execCommandContext(runCtx, shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	if dir != "" {
		cmd.Dir = dir
	}
	out := &limitWriter{max: maxOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	body := out.String()
	trunc := out.Truncated()
	if trunc {
		body += fmt.Sprintf("\n…（输出超过 %d 字节已截断：缩小范围、加 Select-Object -First N 或写到文件再读）", maxOutput)
	}
	if strings.TrimSpace(body) == "" {
		body = "（无输出）"
	}

	res := spi.Result{
		Summary: fmt.Sprintf("%s 退出码 %d（%d 毫秒，%d 字节）", filepath.Base(shell), exitCodeOf(runErr), elapsed.Milliseconds(), out.Len()),
		Text:    body,
		Data: data{
			Shell:      filepath.Base(shell),
			Command:    cmdline,
			Workdir:    dir,
			ExitCode:   exitCodeOf(runErr),
			DurationMS: elapsed.Milliseconds(),
			Truncated:  trunc,
			Bytes:      out.Len(),
		},
	}

	if runCtx.Err() == context.DeadlineExceeded || runCtx.Err() == context.Canceled {
		res.Hint = fmt.Sprintf("命令超过 %s 未结束已终止：拆成更小的步骤，或把长任务丢给后台任务工具", timeout)
		return res, fmt.Errorf("命令超时（%s）: %s", timeout, firstLine(body))
	}
	if runErr != nil {
		res.Hint = "命令失败：读上面的报错，先修掉第一条错误再重试；确认是不是路径或权限问题"
		return res, fmt.Errorf("pwsh 退出码 %d: %s", exitCodeOf(runErr), firstLine(body))
	}
	return res, nil
}

// authorize 在启动任何进程之前做策略判定。
func (t *Tool) authorize(ctx context.Context, command, workdir string, permission sandbox.Permission) error {
	cwd := workdir
	if strings.TrimSpace(cwd) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("取当前目录失败: %w", err)
		}
		cwd = wd
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("解析工作目录失败: %w", err)
	}
	if t.gate == nil {
		if permission != "" {
			return fmt.Errorf("%w: 本进程没有 %s；带 sandbox_permissions 的调用一律拒绝",
				sandbox.ErrPolicyUnwired, sandbox.EnvMode)
		}
		return nil
	}
	d, err := t.gate.EvaluateCommand(ctx, command, cwd, permission)
	if err != nil {
		return err
	}
	t.lastDecision = &d
	if d.Action == sandbox.ActionAllow {
		return nil
	}
	return d.Error()
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

// resolveShell 首选 pwsh（7+），退回 Windows PowerShell 5.1，两者都没有就报错。
// isLegacy 标记退回的那个是老壳，需要补 UTF-8 序言。
func resolveShell() (path string, isLegacy bool, err error) {
	if p, e := exec.LookPath("pwsh"); e == nil {
		return p, false, nil
	}
	if p, e := exec.LookPath("powershell"); e == nil {
		return p, true, nil
	}
	return "", false, fmt.Errorf("PATH 里既没有 pwsh 也没有 powershell")
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		s = "（无输出）"
	}
	return s
}

// limitWriter 收集输出并在到达上限后静默丢弃其余部分，只记一个标记。
// 并发安全：exec 在 Stdout/Stderr 是同一个可比较的 Writer 时只用一个 goroutine，
// 但为将来拆开流留个锁，代价可以忽略。
type limitWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *limitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.max - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *limitWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *limitWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

func (w *limitWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// Module 返回装配用的模块描述。Options 只服务测试与将来的宿主接线；
// 生产 cmd/dshd 仍按旧签名调用，闸门从进程环境解析。
func Module(opts ...Option) kernel.Module {
	return kernel.Module{
		ID:       "tool-pwsh",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			tool, err := newTool(opts...)
			if err != nil {
				return err
			}
			return reg.Register(tool)
		},
	}
}
