package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

type fakeTool struct{ name string }

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake" }

func (f *fakeTool) Caps() spi.Caps          { return spi.Caps{} }
func (f *fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeTool) Exec(context.Context, json.RawMessage) (spi.Result, error) {
	return spi.Result{Summary: "ok"}, nil
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "read"}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&fakeTool{name: "read"})
	if err == nil || !strings.Contains(err.Error(), "不允许重名") {
		t.Fatalf("重名必须报错，得到 %v", err)
	}
}

func TestRegistryKeepsRegistrationOrder(t *testing.T) {
	r := New()
	for _, n := range []string{"c", "a", "b"} {
		if err := r.Register(&fakeTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Names()
	if strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("顺序 = %v，期望注册顺序（schema 稳定性依赖它）", got)
	}
	if got := r.Sorted(); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("Sorted = %v", got)
	}
}

func TestModuleProvidesRegistry(t *testing.T) {
	k := kernel.New("t")
	defer k.Root().Dispose()
	if err := Module(Options{}).Register(k.Root()); err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.Inject(k.Root(), Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Tools()) != 0 {
		t.Fatalf("新目录应为空，得到 %d 个工具", len(reg.Tools()))
	}
	if err := reg.Register(&fakeTool{name: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("x"); !ok {
		t.Fatal("注册后应能查到")
	}
}
