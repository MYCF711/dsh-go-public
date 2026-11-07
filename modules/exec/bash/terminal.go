package bash

import (
	"fmt"
	"sync"
)

// Terminal 是一条**长连接**的 shell 会话：用户敲什么它执行什么，输出一直流。
//
// # 和 job 的区别
//
//   - job：跑一条命令，有 `Done`/`ExitCode`，跑完就结束
//   - terminal：一直开着，没有"一条命令"的概念，靠 `input` 喂、靠 `output` 读
//
// 所以它不复用 `job` 的任何结构——硬合并只会让两边的状态都说不清。
//
// # 输出怎么攒
//
// `session.readLoop` 每读到一块就 append 到 `buf`。用 buffer 而不是 channel：
// 终端输出是**累积的**，客户端要的是"从第 N 字节开始到现在"，
// 而不是"给我下一条"——channel 的语义对不上。
type Terminal struct {
	mu   sync.Mutex
	id   string
	sess *session
	buf  []byte
	dead bool
}

// OpenTerminal 起一条终端会话（`cwd` 空则用 Service 当前目录）。
//
// **失败就是失败**：起不来要返回错误，不能给一个"看起来能用的空终端"——
// 那会让用户对着一个不响应的黑框敲字。
func (s *Service) OpenTerminal(cwd string) (*Terminal, error) {
	s.mu.Lock()
	base := s.cwd
	if cwd == "" {
		cwd = base
	}
	s.mu.Unlock()
	if cwd == "" {
		cwd = base
	}

	sess, err := startSession(cwd)
	if err != nil {
		return nil, err
	}
	t := &Terminal{id: fmt.Sprintf("term-%d", s.nextTermID()), sess: sess}
	// 输出接到这个终端的 buffer 上。**在 startSession 之后挂**：
	// startSession 里已经起了 readLoop，但第一块输出要等 bash 真的吐出东西
	// （提示符、命令结果），那时 sink 早就挂好了。
	sess.mu.Lock()
	sess.sink = t.append
	sess.mu.Unlock()

	s.mu.Lock()
	if s.terms == nil {
		s.terms = map[string]*Terminal{}
	}
	s.terms[t.id] = t
	s.mu.Unlock()
	return t, nil
}

// nextTermID 分配一个终端 id（term-1、term-2…）。
func (s *Service) nextTermID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tseq++
	return s.tseq
}

// ID 是这个终端的标识（Transport 那边用它做 open 的返回）。
func (t *Terminal) ID() string { return t.id }

// append 是 session 的 sink：把输出攒进 buffer。
func (t *Terminal) append(p []byte) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	t.mu.Unlock()
}

// Input 往终端的 stdin 写一段（**原样**，调用方负责换行）。
func (t *Terminal) Input(s string) error {
	t.mu.Lock()
	sess := t.sess
	dead := t.dead
	t.mu.Unlock()
	if dead || sess == nil {
		return fmt.Errorf("终端已经关了")
	}
	_, err := sess.stdin.Write([]byte(s))
	return err
}

// Output 取"从第 since 字节开始到目前为止"的输出。
//
// **返回 next**：调用方下次拿它当 since。这个设计让轮询是增量的——
// 每帧都把整段历史传一遍的话，一个跑了半小时的终端会把界面拖死。
func (t *Terminal) Output(since int) (out []byte, next int, dead bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since < 0 {
		since = 0
	}
	if since > len(t.buf) {
		since = len(t.buf)
	}
	return t.buf[since:], len(t.buf), t.dead
}

// Close 关掉终端并把它从 Service 里摘掉。
func (s *Service) CloseTerminal(id string) error {
	s.mu.Lock()
	t := s.terms[id]
	delete(s.terms, id)
	s.mu.Unlock()
	if t == nil {
		return fmt.Errorf("没有这个终端: %s", id)
	}
	t.mu.Lock()
	t.dead = true
	sess := t.sess
	t.mu.Unlock()
	if sess != nil {
		sess.close()
	}
	return nil
}

// Terminal 按 id 取一条终端（给 Transport 用）。
func (s *Service) Terminal(id string) *Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terms[id]
}
