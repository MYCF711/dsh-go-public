package modes

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/internal/spi"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string            { return f.name }
func (f fakeTool) Description() string     { return "d" }
func (f fakeTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fakeTool) Caps() spi.Caps          { return spi.Caps{} }
func (f fakeTool) Exec(ctx context.Context, a json.RawMessage) (spi.Result, error) {
	return spi.Result{}, nil
}

type fakeReg struct{ tools []spi.Tool }

func (f *fakeReg) Register(t spi.Tool) error { f.tools = append(f.tools, t); return nil }
func (f *fakeReg) Tools() []spi.Tool         { return f.tools }
func (f *fakeReg) Get(n string) (spi.Tool, bool) {
	for _, t := range f.tools {
		if t.Name() == n {
			return t, true
		}
	}
	return nil, false
}

func TestBuiltinsAreReadOnly(t *testing.T) {
	if len(Builtins()) == 0 {
		t.Fatal("至少要有 default")
	}
	for _, m := range Builtins() {
		if !m.Builtin {
			t.Fatalf("%s 应该是内置的", m.ID)
		}
	}
}

func TestLookupAndResolve(t *testing.T) {
	m, ok := Lookup("")
	if !ok || m.ID != DefaultID {
		t.Fatal("空 id 应该回落到 default")
	}
	if _, err := Resolve("turbo"); err == nil {
		t.Fatal("未知模式必须报错，不能静默回落")
	}
	m, err := Resolve("read-only")
	if err != nil {
		t.Fatal(err)
	}
	if m.Allows("write") {
		t.Fatal("read-only 不该允许 write")
	}
	if !m.Allows("read") {
		t.Fatal("read-only 应该允许 read")
	}
	// history_search 只读日志，属于"看"的能力，read-only 下应该能用。
	if !m.Allows("history_search") {
		t.Fatal("read-only 应该允许 history_search")
	}
}

func TestOverrideBuiltin(t *testing.T) {
	if err := OverrideBuiltin("default"); err == nil {
		t.Fatal("改写内置模式必须报错")
	}
	if err := OverrideBuiltin("my-mode"); err != nil {
		t.Fatalf("新 id 不该被拦: %v", err)
	}
}

func TestViewFilters(t *testing.T) {
	reg := &fakeReg{tools: []spi.Tool{
		fakeTool{"read"}, fakeTool{"write"}, fakeTool{"bash"},
	}}
	ro, _ := Resolve("read-only")
	v := Filter(reg, ro)

	names := []string{}
	for _, tl := range v.Tools() {
		names = append(names, tl.Name())
	}
	if len(names) != 1 || names[0] != "read" {
		t.Fatalf("裁剪后 = %v，期望只有 read", names)
	}
	if _, ok := v.Get("write"); ok {
		t.Fatal("被裁掉的工具不该能 Get 到")
	}
	if err := v.Register(fakeTool{"x"}); err == nil {
		t.Fatal("视图不能注册")
	}
}

func TestViewDefaultPassesThrough(t *testing.T) {
	reg := &fakeReg{tools: []spi.Tool{fakeTool{"read"}, fakeTool{"write"}}}
	d, _ := Resolve(DefaultID)
	v := Filter(reg, d)
	if len(v.Tools()) != 2 {
		t.Fatalf("default 应该全放行，得到 %d 个", len(v.Tools()))
	}
	if _, ok := v.Get("write"); !ok {
		t.Fatal("default 下 write 应该可见")
	}
}
