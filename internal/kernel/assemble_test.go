package kernel

import (
	"strings"
	"testing"
)

func mod(id string, provides, requires []string, register func(*Scope) error) Module {
	return Module{ID: id, Backend: BackendStatic, Provides: provides, Requires: requires, Register: register}
}

func TestTopoSortActivatesProvidersFirst(t *testing.T) {
	// 故意把消费者写在提供者前面：激活顺序必须由能力依赖决定（A4）。
	mods := []Module{
		{ID: "tool-read", Requires: []string{"tools"}},
		{ID: "tools", Provides: []string{"tools"}},
	}
	order, err := TopoSort(mods)
	if err != nil {
		t.Fatal(err)
	}
	if order[0].ID != "tools" {
		t.Fatalf("激活顺序 = %v，期望 tools 在前", ids(order))
	}
}

func TestTopoSortMissingCapabilityIsActionable(t *testing.T) {
	_, err := TopoSort([]Module{{ID: "tool-read", Requires: []string{"tools"}}})
	if err == nil {
		t.Fatal("缺能力必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tools") || !strings.Contains(msg, "没有模块提供它") {
		t.Fatalf("错误信息应说清缺什么: %s", msg)
	}
}

func TestTopoSortDuplicateProviderFails(t *testing.T) {
	_, err := TopoSort([]Module{
		{ID: "a", Provides: []string{"x"}},
		{ID: "b", Provides: []string{"x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "两个模块同时提供") {
		t.Fatalf("同一能力两个提供者必须 fail-loud，得到 %v", err)
	}
}

func TestTopoSortDetectsCycle(t *testing.T) {
	_, err := TopoSort([]Module{
		{ID: "a", Provides: []string{"a"}, Requires: []string{"b"}},
		{ID: "b", Provides: []string{"b"}, Requires: []string{"a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "环") {
		t.Fatalf("环必须被检出，得到 %v", err)
	}
}

func TestAssembleRollsBackOnRegisterError(t *testing.T) {
	k := New("t")
	disposed := false
	mods := []Module{
		mod("a", []string{"cap-a"}, nil, func(s *Scope) error {
			s.Effect(func() { disposed = true })
			return nil
		}),
		mod("b", nil, []string{"cap-a"}, func(*Scope) error { return errFake }),
	}
	_, err := Assemble(k.Root(), mods)
	if err == nil || !strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("装配失败必须回滚并说明，得到 %v", err)
	}
	if !disposed {
		t.Fatal("已实例化的模块必须被撤销")
	}
}

func TestAssembleProducesWorkingServices(t *testing.T) {
	k := New("t")
	mods := []Module{
		mod("provider", []string{"svc"}, nil, func(s *Scope) error {
			return Provide(s, svcKey, &svc{Name: "wired"})
		}),
		mod("consumer", nil, []string{"svc"}, func(s *Scope) error {
			got, err := Inject(s, svcKey)
			if err != nil {
				return err
			}
			if got.Name != "wired" {
				t.Fatalf("注入值 = %q", got.Name)
			}
			return nil
		}),
	}
	plan, err := Assemble(k.Root(), mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Order) != 2 {
		t.Fatalf("plan 顺序长度 = %d", len(plan.Order))
	}
	k.Root().Dispose()
}

func TestAssembleFailsWithoutProvider(t *testing.T) {
	k := New("t")
	_, err := Assemble(k.Root(), []Module{{ID: "consumer", Requires: []string{"nope"}}})
	if err == nil {
		t.Fatal("缺能力时装配必须失败")
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "假装失败" }

func ids(mods []Module) []string {
	out := make([]string, len(mods))
	for i, m := range mods {
		out[i] = m.ID
	}
	return out
}
