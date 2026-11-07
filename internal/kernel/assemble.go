package kernel

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Plan 是装配结果：按依赖排好的模块序列。
type Plan struct {
	Order []Module
}

// Assemble 做三件事：能力解析 → 拓扑排序 → 逐模块实例化（失败回滚）。
//
// 关键语义（A4「装配先于执行」）：
//   - **激活由能力可用性驱动，不按 manifest 行顺序**——所以要先排序再实例化；
//   - 同一能力被两个模块提供 → 报错，不静默取第一个；
//   - 依赖的能力没人提供 → fail-loud，写清缺什么、谁提供；
//   - 任一模块 Register 失败 → 已实例化的模块全部回滚。
func Assemble(root *Scope, mods []Module) (*Plan, error) {
	if root == nil {
		return nil, fmt.Errorf("Assemble: root 为 nil")
	}
	order, err := TopoSort(mods)
	if err != nil {
		return nil, err
	}

	created := make([]*Scope, 0, len(order))
	for _, mod := range order {
		scope := root.Child(mod.ID)
		if mod.Register == nil {
			created = append(created, scope)
			continue
		}
		if err := mod.Register(scope); err != nil {
			scope.Dispose()
			for i := len(created) - 1; i >= 0; i-- {
				created[i].Dispose()
			}
			return nil, fmt.Errorf("模块 %q 装配失败，已回滚: %w", mod.ID, err)
		}
		created = append(created, scope)
	}
	return &Plan{Order: order}, nil
}

// TopoSort 只做排序与校验，不实例化（供 `dshd modules graph` 与测试使用）。
func TopoSort(mods []Module) ([]Module, error) {
	if len(mods) == 0 {
		return nil, nil
	}

	// 1. 能力 → 提供者。重复提供是硬错误。
	provider := make(map[string]int, len(mods))
	for i, mod := range mods {
		for _, cap := range mod.Provides {
			if cap == "" {
				return nil, fmt.Errorf("模块 %q 提供了空能力名", mod.ID)
			}
			if j, dup := provider[cap]; dup {
				return nil, fmt.Errorf("能力 %q 被两个模块同时提供：%q 与 %q", cap, mods[j].ID, mod.ID)
			}
			provider[cap] = i
		}
	}

	// 2. 依赖边：requires → provider。缺能力即报错。
	deps := make([]map[int]bool, len(mods))
	for i, mod := range mods {
		deps[i] = make(map[int]bool, len(mod.Requires))
		for _, cap := range mod.Requires {
			j, ok := provider[cap]
			if !ok {
				avail := make([]string, 0, len(provider))
				for c := range provider {
					avail = append(avail, c)
				}
				sort.Strings(avail)
				return nil, fmt.Errorf("模块 %q 需要能力 %q，但没有模块提供它；当前可用能力: %v", mod.ID, cap, avail)
			}
			if j != i {
				deps[i][j] = true
			}
		}
	}

	// 3. Kahn。同层按声明顺序，保证可复现——但顺序本身不承载语义。
	indeg := make([]int, len(mods))
	for i := range mods {
		indeg[i] = len(deps[i])
	}
	ready := make([]int, 0, len(mods))
	for i := range mods {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Ints(ready)

	out := make([]Module, 0, len(mods))
	done := make([]bool, len(mods))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		if done[i] {
			continue
		}
		done[i] = true
		out = append(out, mods[i])
		for j := range mods {
			if deps[j][i] {
				delete(deps[j], i)
				indeg[j]--
				if indeg[j] == 0 {
					ready = append(ready, j)
					sort.Ints(ready)
				}
			}
		}
	}

	if len(out) != len(mods) {
		var stuck []string
		for i, mod := range mods {
			if !done[i] {
				stuck = append(stuck, mod.ID)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("装配依赖存在环，无法确定顺序: %v", stuck)
	}
	return out, nil
}

// Write 打印模块图：谁先激活、提供什么、依赖什么。
func (p *Plan) Write(w io.Writer) {
	fmt.Fprintf(w, "%-22s %-9s %-26s %s\n", "MODULE", "BACKEND", "PROVIDES", "REQUIRES")
	for _, mod := range p.Order {
		be := mod.Backend
		if be == "" {
			be = BackendStatic
		}
		fmt.Fprintf(w, "%-22s %-9s %-26s %s\n",
			mod.ID, be,
			strings.Join(mod.Provides, ","),
			strings.Join(mod.Requires, ","))
	}
}
