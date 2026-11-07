// Package bash 是执行层的默认实现：真实 bash 进程（R6），进程内解释器一律不用。
//
// 设计取舍：
//   - **持久会话**：一个 bash 进程常驻，cwd 跨命令保留；
//   - **不持久环境变量**：`export` 不会留到下一次调用（每条命令在子 shell 里展开）；
//   - **自动转后台**：一条命令空闲超过预算，就把整个会话交给后台任务，下一次调用换新会话。
//   - **进程树归属**：每个 bash 会话有独立的 Windows Job Object / Unix process group。
package bash

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dshgo/internal/spi"
)

// markerRe 匹配命令结束哨兵：\x01DSHD:<seq>:<rc>\x01<cwd>\x01\n
var markerRe = regexp.MustCompile("\x01DSHD:(\\d+):(-?\\d+)\x01([^\x01]*)\x01\n")

// ErrSessionEnded 表示命令结束了 shell 自己（如裸 exit）。
var ErrSessionEnded = errors.New("bash 会话被命令结束")

const maxRetainedJobOutput = 1024 * 1024

// findBash 找 bash：环境变量优先，其次 PATH，最后 Git for Windows 的常见位置。
func findBash() (string, error) {
	if p := os.Getenv("DSHD_BASH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 bash：装 Git for Windows，或把 bash 加进 PATH，或设 DSHD_BASH")
}

// session 是一个常驻 bash 进程。同一时刻只跑一条命令。
type session struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     io.ReadCloser
	control *processControl

	mu   sync.Mutex
	task *task
	cwd  string
	dead bool
	seq  int
	// sink 是**额外的输出出口**（终端用）。
	sink func([]byte)

	waitDone  chan struct{}
	processRC int
	closeOnce sync.Once
}

func startSession(cwd string) (*session, error) {
	bash, err := findBash()
	if err != nil {
		return nil, err
	}
	c := exec.Command(bash, "--noprofile", "--norc", "-s")
	c.Dir = cwd
	if err := prepareProcessTree(c); err != nil {
		return nil, err
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	c.Stdout = pw
	c.Stderr = pw
	if err := c.Start(); err != nil {
		pr.Close()
		pw.Close()
		_ = stdin.Close()
		return nil, fmt.Errorf("启动 bash 失败: %w", err)
	}
	pw.Close() // 父进程不留写端，否则读不到 EOF
	control, err := attachProcessTree(c)
	if err != nil {
		// 这里只碰刚刚由本函数启动且仍由 cmd 持有的 root；attach 自己回收 Job。
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
		_ = stdin.Close()
		_ = pr.Close()
		return nil, fmt.Errorf("建立 bash 进程树归属失败: %w", err)
	}
	s := &session{cmd: c, stdin: stdin, out: pr, cwd: cwd, control: control,
		waitDone: make(chan struct{}), processRC: -1}
	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

func (s *session) waitLoop() {
	err := s.cmd.Wait()
	rc := -1
	if err == nil {
		rc = 0
	} else if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
		rc = exitErr.ExitCode()
	}
	s.mu.Lock()
	s.processRC = rc
	s.dead = true
	close(s.waitDone)
	s.mu.Unlock()
}

func (s *session) readLoop() {
	br := bufio.NewReaderSize(s.out, 32*1024)
	buf := make([]byte, 8192)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			s.mu.Lock()
			t := s.task
			sink := s.sink
			s.mu.Unlock()
			if t != nil {
				t.append(buf[:n])
			}
			if sink != nil {
				sink(buf[:n])
			}
		}
		if err != nil {
			// EOF 之后 Wait 必然能结束；用真实 root 退出码替代无条件 -1。
			<-s.waitDone
			s.mu.Lock()
			t := s.task
			s.dead = true
			rc := s.processRC
			s.mu.Unlock()
			if t != nil {
				t.finish(rc, "")
			}
			return
		}
	}
}

func (s *session) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *session) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.task != nil && !s.task.finished()
}

// stop 只终止经过 processControl 身份复核的树；不关闭 out，让 EOF 前排空输出。
func (s *session) stop(t *task) error {
	if t == nil || t.finished() {
		return spi.ErrJobFinished
	}
	if err := s.control.stop(); err != nil {
		return err
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	return nil
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		if s.control != nil {
			s.control.close()
		}
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.out != nil {
			_ = s.out.Close()
		}
	})
}

// run 写一条命令并等它结束（或空闲超时）。
func (s *session) run(cmd string, idle time.Duration) (spi.ExecResult, *task, error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return spi.ExecResult{}, nil, fmt.Errorf("bash 会话已退出，需要新会话")
	}
	if s.task != nil && !s.task.finished() {
		s.mu.Unlock()
		return spi.ExecResult{}, nil, fmt.Errorf("bash 会话正忙")
	}
	s.seq++
	t := newTask(s.seq)
	s.task = t
	s.mu.Unlock()

	script := fmt.Sprintf(
		"{ %s\n} 2>&1; __dshd_rc=$?\nprintf '\\001DSHD:%d:%%d\\001%%s\\001\\n' \"$__dshd_rc\" \"$PWD\"\n",
		cmd, t.seq)
	if _, err := io.WriteString(s.stdin, script); err != nil {
		return spi.ExecResult{}, nil, fmt.Errorf("写命令到 bash 失败: %w", err)
	}

	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		if t.finished() {
			s.mu.Lock()
			if s.task == t {
				s.task = nil
			}
			if cwd := t.resultCwd(); cwd != "" {
				s.cwd = cwd
			}
			s.mu.Unlock()
			rc, cwd, _ := t.result()
			if t.endedByEOF() && s.isDead() {
				return spi.ExecResult{Output: t.text(), ExitCode: rc, Cwd: cwd}, t,
					fmt.Errorf("%w（比如 exit）：已丢弃该会话，下次调用会自动重开", ErrSessionEnded)
			}
			return spi.ExecResult{Output: t.text(), ExitCode: rc, Cwd: cwd}, t, nil
		}
		notify := t.notification()
		select {
		case <-notify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return spi.ExecResult{Output: t.text(), MovedToBg: true}, t, nil
		}
	}
}

// task 是一条命令的有界累积输出与结束状态。base/total 是绝对字节偏移。
type task struct {
	seq int

	mu    sync.Mutex
	buf   []byte
	base  int64
	total int64
	done  bool
	eof   bool
	rc    int
	cwd   string
	sig   chan struct{}
}

func newTask(seq int) *task {
	return &task{seq: seq, sig: make(chan struct{})}
}

func (t *task) append(b []byte) {
	t.mu.Lock()
	if !t.done {
		t.buf = append(t.buf, b...)
		t.total += int64(len(b))
		if len(t.buf) > maxRetainedJobOutput {
			drop := len(t.buf) - maxRetainedJobOutput
			copy(t.buf, t.buf[drop:])
			t.buf = t.buf[:maxRetainedJobOutput]
			t.base += int64(drop)
		}
		t.scanLocked()
		t.notifyLocked()
	}
	t.mu.Unlock()
}

// scanLocked 在保留缓冲里找哨兵；marker 和其后字节不计入用户输出 total。
func (t *task) scanLocked() {
	s := string(t.buf)
	if !strings.Contains(s, "\x01DSHD:") {
		return
	}
	loc := markerRe.FindStringSubmatchIndex(s)
	if loc == nil {
		return
	}
	rc, _ := strconv.Atoi(s[loc[4]:loc[5]])
	cwd := s[loc[6]:loc[7]]
	before := s[:loc[0]]
	t.total = t.base + int64(loc[0])
	t.buf = append(t.buf[:0], before...)
	if !t.done {
		t.done = true
		t.rc = rc
		t.cwd = cwd
	}
}

func (t *task) finish(rc int, cwd string) {
	t.mu.Lock()
	if !t.done {
		t.done = true
		t.eof = true
		t.rc = rc
		t.cwd = cwd
	}
	t.notifyLocked()
	t.mu.Unlock()
}

// notifyLocked 是 close-and-replace 广播：多个 OutputPage 读者不会互相抢信号。
func (t *task) notifyLocked() {
	close(t.sig)
	t.sig = make(chan struct{})
}

func (t *task) notification() chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sig
}

func (t *task) finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

func (t *task) endedByEOF() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eof
}

func (t *task) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimRight(string(t.buf), "\n")
}

func (t *task) result() (int, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rc, t.cwd, t.done
}

func (t *task) resultCwd() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

func (t *task) resultCode() int {
	rc, _, _ := t.result()
	return rc
}

func (t *task) retention() (base, total int64, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base, t.total, t.base > 0
}

func (t *task) bufferedBytes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}

func (t *task) page(id string, cursor int64, limit int) (spi.JobOutputPage, chan struct{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cursor < 0 || cursor > t.total {
		return spi.JobOutputPage{}, t.sig, spi.ErrJobInvalidCursor
	}
	offset := cursor
	if offset < t.base {
		offset = t.base
	}
	start := int(offset - t.base)
	end := start + limit
	if end > len(t.buf) {
		end = len(t.buf)
	}
	out := string(append([]byte(nil), t.buf[start:end]...))
	return spi.JobOutputPage{
		ID: id, Output: out, Offset: offset, NextCursor: offset + int64(len(out)),
		Total: t.total, Truncated: t.base > 0, Done: t.done, ExitCode: t.rc, Cwd: t.cwd,
	}, t.sig, nil
}
