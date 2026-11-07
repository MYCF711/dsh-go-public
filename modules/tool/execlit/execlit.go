// Package execlit 提供 `exec_literal` 工具：在 Windows 上直接启动一个 .exe，
// 参数**逐个原样**传给程序，不经过任何 shell。
//
// 为什么需要它：Git-Bash / MSYS 会对命令行做路径转换——
// `ipconfig /all` 里的 `/all` 会被改写成 `C:/Program Files/Git/all`，
// `tasklist //V` 要写双斜杠才勉强不炸。这不是程序的问题，是中间那层 shell 的问题。
// 绕过 shell 就没有转换：exec.CommandContext 直接把 argv 交给 CreateProcess。
//
// 边界感：
//   - program 必须是**绝对路径**：相对路径要靠 PATH/CWD 解析，那一步各壳不一样，
//     正是本工具要避开的不确定性；也让"它到底跑了哪个 exe"是可复核的。
//   - 不给 shell 留口子：args 不做拆分、不做转义、不做去重，原样 append。
//   - 输出有上限：一个 exe 可以吐无穷多行，模型上下文不行。
//   - 非零退出码是**程序失败**而不是**工具失败**：输出照回，Hint 给下一步。
package execlit

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
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "program": {"type": "string", "description": ".exe 的绝对路径，例如 C:/Windows/System32/ipconfig.exe"},
    "args": {"type": "array", "items": {"type": "string"}, "description": "可选：逐个原样传给程序的参数，不拆分、不转义"},
    "timeout_seconds": {"type": "integer", "description": "可选：超时秒数，默认 60，上限 600"}
  },
  "required": ["program"],
  "additionalProperties": false
}`)

const (
	// 单次调用回给模型的输出上限（stdout + stderr 合计）。
	maxOutput = 200000
	// 默认/最长超时。更长的任务该丢给后台任务工具。
	defaultTimeout = 60 * time.Second
	maxTimeout     = 600 * time.Second
)

type Tool struct{}

func (t *Tool) Name() string { return "exec_literal" }
func (t *Tool) Caps() spi.Caps {
	// 起子进程。程序自己会不会写盘由程序决定，工具层拦不住，所以 Writes 也如实声明。
	return spi.Caps{Executes: true, Writes: true}
}
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "直接运行 .exe，参数逐个原样传入、不被 Git-Bash 转换。适合 ipconfig /all。"
}

type args struct {
	Program        string   `json:"program"`
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type data struct {
	Program   string `json:"program"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"program": "C:/…/x.exe", "args": ["/all"]}`},
			fmt.Errorf("参数不合法: %w", err)
	}

	program := strings.TrimSpace(a.Program)
	if program == "" {
		return spi.Result{Hint: `program 不能为空：给 .exe 的绝对路径，例如 C:/Windows/System32/ipconfig.exe`},
			fmt.Errorf("program 为空")
	}
	// 正斜杠照收（模型和会话里都写正斜杠），但判定用本机路径规则。
	program = filepath.FromSlash(program)
	if !filepath.IsAbs(program) {
		return spi.Result{Hint: "program 必须是绝对路径：相对路径要靠 PATH 与当前目录解析，" +
				"而这正是各壳不一致的地方（也说不清到底跑了哪个 exe）。先 working_directory 或 glob 拿到完整路径再传"},
			fmt.Errorf("program 不是绝对路径: %s", a.Program)
	}

	st, err := os.Stat(program)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return spi.Result{Hint: "program 不存在：先用 glob 确认这个 .exe 的真实路径（Windows 自带程序多在 C:/Windows/System32/ 下）"},
				fmt.Errorf("program 不存在: %s", program)
		}
		return spi.Result{Hint: "program 打不开（权限或不合法路径）：确认路径拼写，或换一个可执行的备份路径"},
			fmt.Errorf("program 无法访问: %s: %w", program, err)
	}
	if st.IsDir() {
		return spi.Result{Hint: "program 是目录不是可执行文件：给到具体的 .exe，例如 …/System32/ipconfig.exe"},
			fmt.Errorf("program 是目录: %s", program)
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

	// 关键一行：args 逐个 append，**不做任何转换**。
	// 没有 shell、没有引号重写、没有路径改写——这正是本工具存在的理由。
	cmd := exec.CommandContext(runCtx, program, a.Args...)
	out := &limitWriter{max: maxOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	body := out.String()
	trunc := out.Truncated()
	if trunc {
		body += fmt.Sprintf("\n…（输出超过 %d 字节已截断：缩小参数范围（例如换成单条查询）或把结果重定向到文件再用 read 分段看）", maxOutput)
	}
	if strings.TrimSpace(body) == "" {
		body = "（无输出）"
	}

	code := exitCodeOf(runErr)
	res := spi.Result{
		Summary: fmt.Sprintf("%s 退出码 %d（%d 毫秒，%d 字节）", filepath.Base(program), code, elapsed.Milliseconds(), out.Len()),
		Text:    body,
		Data:    data{Program: program, ExitCode: code, Truncated: trunc},
	}
	if trunc {
		res.Hint = fmt.Sprintf("输出超过 %d 字节已截断：缩小参数范围，或重定向到文件后用 read 的 offset/limit 分段看", maxOutput)
	}

	if runCtx.Err() == context.DeadlineExceeded || runCtx.Err() == context.Canceled {
		res.Hint = fmt.Sprintf("程序超过 %s 未结束已终止：加大 timeout_seconds（上限 %s），或换用后台任务工具跑长任务",
			timeout, maxTimeout)
		return res, fmt.Errorf("程序超时（%s）: %s", timeout, firstLine(body))
	}
	if runErr != nil {
		res.Hint = fmt.Sprintf("程序以退出码 %d 结束（不是工具出错）：读上面的报错，先修掉第一条再重试；"+
			"注意参数是原样传入的，别按 shell 习惯加引号或双斜杠", code)
		return res, fmt.Errorf("%s 退出码 %d: %s", filepath.Base(program), code, firstLine(body))
	}
	return res, nil
}

// limitWriter 只留前 max 字节，多出来的丢弃但继续消费——
// 不消费的话子进程会卡在写管道上，看起来像"程序不响应"。
type limitWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	max    int
	trunc  bool
	seenIn int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seenIn += len(p)
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			w.trunc = true
		}
	} else {
		w.trunc = true
	}
	// 返回 len(p)：告诉调用方"全部收下了"，否则子进程会看到短写错误。
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
	return w.seenIn
}

func (w *limitWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trunc
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
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-execlit",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{})
		},
	}
}
