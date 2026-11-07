// Package tools 是工具目录的默认实现（static 层）。
//
// 它只做三件事：注册、查找、枚举。中间件链、可见性、schema 快照都不在这里——
// 那些属于装配期与请求期，由上层负责。
package tools

import (
	"fmt"
	"sort"
	"sync"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

// Key / RunKey 的唯一定义在 internal/spi/keys.go，这里只是本包内的别名。
var (
	Key    = spi.ToolsKey
	RunKey = spi.ToolRunnerKey
)

// Registry 是 spi.ToolRegistry 的默认实现。
type Registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]spi.Tool
}

// New 造一个空目录。
func New() *Registry {
	return &Registry{byName: make(map[string]spi.Tool)}
}

// Register 注册工具。重名是错误——工具的 schema 是缓存前缀的一部分，
// 静默覆盖会让模型前后看到两个不同的同名工具。
func (r *Registry) Register(t spi.Tool) error {
	if t == nil {
		return fmt.Errorf("注册了 nil 工具")
	}
	name := t.Name()
	if name == "" {
		return fmt.Errorf("工具没有名字")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("工具 %q 已注册，不允许重名", name)
	}
	r.byName[name] = t
	r.order = append(r.order, name)
	return nil
}

// Get 按名查找。
func (r *Registry) Get(name string) (spi.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Tools 按注册顺序返回（顺序稳定是模型侧缓存友好的前提）。
func (r *Registry) Tools() []spi.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]spi.Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Names 按注册顺序返回名字。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Sorted 返回按名字排序的副本（诊断用）。
func (r *Registry) Sorted() []string {
	out := r.Names()
	sort.Strings(out)
	return out
}

// Module 返回装配用的模块描述。
//
// 它提供两个服务：工具目录（"tools"）与唯一执行入口（"toolrunner"）。
// 两者必须同时存在——只给目录不给 Runner，等于允许调用方绕过全部中间件。
func Module(opts Options) kernel.Module {
	return kernel.Module{
		ID:       "tools",
		Backend:  kernel.BackendStatic,
		Provides: []string{"tools", "toolrunner"},
		Register: func(s *kernel.Scope) error {
			reg := New()
			if err := kernel.Provide(s, Key, spi.ToolRegistry(reg)); err != nil {
				return err
			}
			return kernel.Provide(s, RunKey, spi.Runner(NewPipeline(reg, s.Bus(), opts)))
		},
	}
}
