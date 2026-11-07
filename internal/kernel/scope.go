// Package kernel 是插件运行时：作用域、注册表、事件总线、装配器。
//
// 它不认识任何业务概念（agent / tool / session / llm）。这是 R1：
// 「内核零业务」的物理形态——本包的任何 import 里出现业务包都算违规。
package kernel

import (
	"context"
	"log/slog"
	"sync"
)

// Scope 是插件实例的生命周期作用域，等价于 cordis 的 fiber 的上下文。
//
// 规则（A5「卸载即撤销」）：模块创建的 goroutine / 文件句柄 / 订阅 / 定时器
// 都必须经 Effect 登记，Dispose 后必须归零。
type Scope struct {
	parent *Scope
	name   string
	ctx    context.Context
	cancel context.CancelFunc
	kernel *Kernel

	mu        sync.Mutex
	children  []*Scope
	disposers []func()
	services  map[string]any
	disposed  bool
}

// Kernel 是根作用域 + 事件总线 + 日志。
type Kernel struct {
	root *Scope
	bus  *Bus
	log  *slog.Logger
}

// New 创建根内核。
func New(name string) *Kernel {
	ctx, cancel := context.WithCancel(context.Background())
	k := &Kernel{bus: newBus(), log: slog.Default()}
	k.root = &Scope{
		name:     name,
		ctx:      ctx,
		cancel:   cancel,
		kernel:   k,
		services: make(map[string]any),
	}
	return k
}

func (k *Kernel) Root() *Scope      { return k.root }
func (k *Kernel) Bus() *Bus         { return k.bus }
func (k *Kernel) Log() *slog.Logger { return k.log }

func (s *Scope) Name() string             { return s.name }
func (s *Scope) Kernel() *Kernel          { return s.kernel }
func (s *Scope) Context() context.Context { return s.ctx }
func (s *Scope) Bus() *Bus                { return s.kernel.bus }
func (s *Scope) Disposed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disposed
}

// Child 创建一个子作用域。父 Dispose 会先撤销所有子作用域。
func (s *Scope) Child(name string) *Scope {
	ctx, cancel := context.WithCancel(s.ctx)
	c := &Scope{
		parent:   s,
		name:     name,
		ctx:      ctx,
		cancel:   cancel,
		kernel:   s.kernel,
		services: make(map[string]any),
	}
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		c.cancel()
		return c
	}
	s.children = append(s.children, c)
	s.mu.Unlock()
	return c
}

// Effect 登记一个可撤销副作用。已撤销的作用域上登记会立即执行一次（清理语义）。
func (s *Scope) Effect(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		safeCall(s.name, fn)
		return
	}
	s.disposers = append(s.disposers, fn)
	s.mu.Unlock()
}

// Dispose 逆序撤销：先副作用，再子作用域，最后取消 context。幂等。
func (s *Scope) Dispose() {
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return
	}
	s.disposed = true
	ds := s.disposers
	s.disposers = nil
	kids := s.children
	s.children = nil
	s.mu.Unlock()

	for i := len(ds) - 1; i >= 0; i-- {
		safeCall(s.name, ds[i])
	}
	for i := len(kids) - 1; i >= 0; i-- {
		kids[i].Dispose()
	}
	s.cancel()
}

// safeCall 保证一个模块的清理 panic 不会打穿宿主（dsc 踩过同样的坑）。
func safeCall(scope string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("scope disposer panicked", "scope", scope, "panic", r)
		}
	}()
	fn()
}

// rootLog 是内核的日志出口。模块应通过注入拿到自己的 logger，
// 这里只给装配器与总线兜底用。
func rootLog() *slog.Logger { return slog.Default() }
