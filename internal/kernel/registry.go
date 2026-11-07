package kernel

import "fmt"

// Key 是带类型的服务键。等价于 cordis 的 ctx.provide(key)。
//
// 用泛型而不是字符串：注册表的灵活性保留，类型错误从运行期挪到编译期。
// 唯一允许类型擦除的地方是跨进程插件的字符串键（走 gRPC）。
type Key[T any] struct{ name string }

// NewKey 造一个服务键。约定：小写、单数、与 SPI 同名（如 "tools"、"session"）。
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

func (k Key[T]) String() string { return k.name }

// ErrMissingService 是「缺能力」的 fail-loud 错误。
//
// 措辞对模型和人都必须可行动：说清缺什么、谁来提供、怎么补。
type ErrMissingService struct {
	Key   string
	Scope string
}

func (e *ErrMissingService) Error() string {
	return fmt.Sprintf("缺 %q 能力（作用域 %s）：没有模块 provides 它；在 dsh.toml 里启用提供该能力的模块", e.Key, e.Scope)
}

// serviceRoot 返回最外层作用域。
//
// 服务命名空间是**部署级**的（一个能力全部署只有一个提供者），作用域只管生命周期：
// 提供者在自己的作用域上挂 disposer，Dispose 时服务即刻消失。
// 这与「同一能力两个提供者 → fail-loud」是同一条设计：不允许隐式取第一个。
func (s *Scope) serviceRoot() *Scope {
	for s.parent != nil {
		s = s.parent
	}
	return s
}

// Provide 把实现注册到部署级命名空间，撤销挂在调用方作用域上。
func Provide[T any](s *Scope, k Key[T], impl T) error {
	root := s.serviceRoot()
	root.mu.Lock()
	if root.disposed {
		root.mu.Unlock()
		return fmt.Errorf("内核已撤销，不能再 provide %q", k.name)
	}
	if prev, ok := root.services[k.name]; ok {
		root.mu.Unlock()
		return fmt.Errorf("服务 %q 已被 %T 提供，不允许重复注册（一个能力只允许一个提供者）", k.name, prev)
	}
	root.services[k.name] = impl
	root.mu.Unlock()

	s.Effect(func() {
		root.mu.Lock()
		delete(root.services, k.name)
		root.mu.Unlock()
	})
	return nil
}

// Inject 从部署级命名空间取服务。找不到返回 ErrMissingService，绝不返回零值。
func Inject[T any](s *Scope, k Key[T]) (T, error) {
	var zero T
	root := s.serviceRoot()
	root.mu.Lock()
	v, ok := root.services[k.name]
	root.mu.Unlock()
	if !ok {
		return zero, &ErrMissingService{Key: k.name, Scope: s.name}
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("服务 %q 的实现类型是 %T，不是 %T", k.name, v, zero)
	}
	return t, nil
}

// Has 判断某能力在当前部署里是否可用（装配器做能力解析用）。
func Has(s *Scope, name string) bool {
	root := s.serviceRoot()
	root.mu.Lock()
	_, ok := root.services[name]
	root.mu.Unlock()
	return ok
}
