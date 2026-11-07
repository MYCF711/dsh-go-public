package kernel

import (
	"errors"
	"fmt"
	"sync"
)

// Event 穿过总线的一次通知。
type Event struct {
	Name    string
	Payload any
	Source  *Scope
}

// Handler 是监听器。返回 error 时：
//   - Waterfall / Bail 中断
//   - Serial 记下来继续
//   - Emit 忽略（只记日志）
//   - Parallel 收集后合并返回
type Handler func(*Event) error

type subscription struct {
	h    Handler
	who  string
	dead bool
}

// Bus 是五种分发模式的事件总线（对齐 cordis events.ts）。
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]*subscription
}

func newBus() *Bus { return &Bus{subs: make(map[string][]*subscription)} }

// On 注册监听器，随作用域撤销。
func (b *Bus) On(s *Scope, event string, h Handler) {
	if s == nil || h == nil {
		return
	}
	sub := &subscription{h: h, who: s.name}
	b.mu.Lock()
	b.subs[event] = append(b.subs[event], sub)
	b.mu.Unlock()

	s.Effect(func() {
		b.mu.Lock()
		sub.dead = true
		b.mu.Unlock()
	})
}

// Count 是观测用：当前活着的监听器数。
func (b *Bus) Count(event string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, s := range b.subs[event] {
		if !s.dead {
			n++
		}
	}
	return n
}

func (b *Bus) snapshot(event string) []*subscription {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src := b.subs[event]
	out := make([]*subscription, 0, len(src))
	for _, s := range src {
		if !s.dead {
			out = append(out, s)
		}
	}
	return out
}

// invoke 保证单个监听器 panic 不会打穿宿主。
func (b *Bus) invoke(e *Event, sub *subscription) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("事件 %q 的监听器 %q panic: %v", e.Name, sub.who, r)
		}
	}()
	return sub.h(e)
}

// Emit 广播，不等待：错误只记录，不影响其它监听器，也不返回。
func (b *Bus) Emit(name string, payload any) {
	e := &Event{Name: name, Payload: payload}
	for _, sub := range b.snapshot(name) {
		if err := b.invoke(e, sub); err != nil {
			rootLog().Warn("事件监听器失败（emit 不中断）", "event", name, "error", err)
		}
	}
}

// Serial 顺序跑完全部监听器，把所有错误合并返回。
func (b *Bus) Serial(name string, payload any) error {
	e := &Event{Name: name, Payload: payload}
	var errs []error
	for _, sub := range b.snapshot(name) {
		if err := b.invoke(e, sub); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Bail 顺序跑，第一个非 nil 错误即停。
func (b *Bus) Bail(name string, payload any) error {
	e := &Event{Name: name, Payload: payload}
	for _, sub := range b.snapshot(name) {
		if err := b.invoke(e, sub); err != nil {
			return err
		}
	}
	return nil
}

// Waterfall 是拦截/改写链：监听器可以就地改 e.Payload，任何错误即断开。
//
// Go 没有 next() 闭包洋葱，等价物是「可变载荷 + 顺序短路」：
// 审批、沙箱、spill、重复提醒、超时全部挂在这条链上。
func (b *Bus) Waterfall(name string, payload any) (any, error) {
	e := &Event{Name: name, Payload: payload}
	for _, sub := range b.snapshot(name) {
		if err := b.invoke(e, sub); err != nil {
			return e.Payload, err
		}
	}
	return e.Payload, nil
}

// Parallel 并发跑，等全部完成，合并错误。任一监听器 panic 只影响它自己。
func (b *Bus) Parallel(name string, payload any) error {
	subs := b.snapshot(name)
	if len(subs) == 0 {
		return nil
	}
	errs := make([]error, len(subs))
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub *subscription) {
			defer wg.Done()
			e := &Event{Name: name, Payload: payload}
			errs[i] = b.invoke(e, sub)
		}(i, sub)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// OnTyped 注册一个把载荷断言成 T 的监听器。
//
// 总线本身不认得任何业务类型（载荷是 any）；这个泛型包装让调用方少写断言，
// 同时把「载荷类型不对」变成明确的错误，而不是静默跳过。
func OnTyped[T any](b *Bus, s *Scope, event string, h func(T) error) {
	b.On(s, event, func(e *Event) error {
		v, ok := e.Payload.(T)
		if !ok {
			var zero T
			return fmt.Errorf("事件 %q 的载荷类型是 %T，不是 %T", event, e.Payload, zero)
		}
		return h(v)
	})
}
