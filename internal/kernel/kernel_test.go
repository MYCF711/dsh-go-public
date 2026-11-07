package kernel

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type svc struct{ Name string }

var svcKey = NewKey[*svc]("svc")

func TestEffectRunsInReverseOrder(t *testing.T) {
	k := New("t")
	var got []int
	k.Root().Effect(func() { got = append(got, 1) })
	k.Root().Effect(func() { got = append(got, 2) })
	k.Root().Dispose()
	if want := []int{2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("撤销顺序 = %v，期望 %v", got, want)
	}
}

func TestDisposeIsIdempotent(t *testing.T) {
	k := New("t")
	n := 0
	k.Root().Effect(func() { n++ })
	k.Root().Dispose()
	k.Root().Dispose()
	if n != 1 {
		t.Fatalf("disposer 执行了 %d 次，期望 1", n)
	}
	if !k.Root().Disposed() {
		t.Fatal("Dispose 后 Disposed() 应为 true")
	}
}

func TestEffectAfterDisposeRunsImmediately(t *testing.T) {
	k := New("t")
	k.Root().Dispose()
	ran := false
	k.Root().Effect(func() { ran = true })
	if !ran {
		t.Fatal("已撤销作用域上的 Effect 应立即执行（清理语义）")
	}
}

func TestChildDisposedWithParent(t *testing.T) {
	k := New("t")
	child := k.Root().Child("child")
	ran := false
	child.Effect(func() { ran = true })
	k.Root().Dispose()
	if !ran {
		t.Fatal("父 Dispose 应级联撤销子作用域")
	}
}

func TestDisposerPanicIsIsolated(t *testing.T) {
	k := New("t")
	ran := false
	k.Root().Effect(func() { ran = true })
	k.Root().Effect(func() { panic("boom") })
	k.Root().Dispose() // 逆序：先 panic（被 recover），再跑正常 disposer
	if !ran {
		t.Fatal("一个 disposer panic 不应阻止其它 disposer")
	}
}

func TestProvideAndInject(t *testing.T) {
	k := New("t")
	s := k.Root().Child("m")
	if err := Provide(s, svcKey, &svc{Name: "x"}); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	got, err := Inject(s, svcKey)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got.Name != "x" {
		t.Fatalf("注入到了 %+v", got)
	}
}

func TestProvideDuplicateFails(t *testing.T) {
	k := New("t")
	s := k.Root().Child("m")
	if err := Provide(s, svcKey, &svc{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	err := Provide(s, svcKey, &svc{Name: "b"})
	if err == nil {
		t.Fatal("同作用域重复 provide 必须报错，不能静默覆盖")
	}
	if !strings.Contains(err.Error(), "不允许重复注册") {
		t.Fatalf("错误信息不具行动性: %v", err)
	}
}

func TestInjectMissingIsActionable(t *testing.T) {
	k := New("t")
	_, err := Inject(k.Root(), svcKey)
	if err == nil {
		t.Fatal("缺服务必须报错，不能返回零值")
	}
	var miss *ErrMissingService
	if !errors.As(err, &miss) {
		t.Fatalf("期望 ErrMissingService，得到 %T", err)
	}
	if !strings.Contains(err.Error(), "dsh.toml") {
		t.Fatalf("错误信息应告诉人怎么补: %v", err)
	}
}

func TestProvideIsVisibleToSiblings(t *testing.T) {
	k := New("t")
	root := k.Root()
	provider := root.Child("provider")
	consumer := root.Child("consumer")

	if err := Provide(provider, svcKey, &svc{Name: "global"}); err != nil {
		t.Fatal(err)
	}
	got, err := Inject(consumer, svcKey)
	if err != nil {
		t.Fatalf("兄弟作用域应能看到同一个能力: %v", err)
	}
	if got.Name != "global" {
		t.Fatalf("注入到了 %q", got.Name)
	}

	// 提供者撤销后能力消失（作用域只管生命周期）。
	provider.Dispose()
	if _, err := Inject(consumer, svcKey); err == nil {
		t.Fatal("提供者撤销后能力必须消失")
	}
}

func TestDuplicateAcrossScopesFails(t *testing.T) {
	k := New("t")
	root := k.Root()
	if err := Provide(root.Child("a"), svcKey, &svc{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	err := Provide(root.Child("b"), svcKey, &svc{Name: "b"})
	if err == nil || !strings.Contains(err.Error(), "一个能力只允许一个提供者") {
		t.Fatalf("跨作用域重复提供必须 fail-loud，得到 %v", err)
	}
}

func TestBusEmitIgnoresError(t *testing.T) {
	k := New("t")
	b := k.Bus()
	ran := false
	b.On(k.Root(), "e", func(*Event) error { return errors.New("x") })
	b.On(k.Root(), "e", func(*Event) error { ran = true; return nil })
	b.Emit("e", nil)
	if !ran {
		t.Fatal("Emit 不应因一个监听器失败而中断")
	}
}

func TestBusSerialCollectsAll(t *testing.T) {
	k := New("t")
	b := k.Bus()
	b.On(k.Root(), "e", func(*Event) error { return errors.New("a") })
	b.On(k.Root(), "e", func(*Event) error { return errors.New("b") })
	err := b.Serial("e", nil)
	if err == nil || !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("Serial 应合并全部错误，得到 %v", err)
	}
}

func TestBusBailStopsAtFirst(t *testing.T) {
	k := New("t")
	b := k.Bus()
	second := false
	b.On(k.Root(), "e", func(*Event) error { return errors.New("stop") })
	b.On(k.Root(), "e", func(*Event) error { second = true; return nil })
	if err := b.Bail("e", nil); err == nil {
		t.Fatal("Bail 应返回第一个错误")
	}
	if second {
		t.Fatal("Bail 应在第一个错误处停止")
	}
}

func TestBusWaterfallCanRewritePayload(t *testing.T) {
	k := New("t")
	b := k.Bus()
	b.On(k.Root(), "e", func(ev *Event) error { ev.Payload = "rewritten"; return nil })
	got, err := b.Waterfall("e", "orig")
	if err != nil {
		t.Fatal(err)
	}
	if got != "rewritten" {
		t.Fatalf("Waterfall 应传递改写后的载荷，得到 %v", got)
	}
}

func TestBusWaterfallAbortsOnError(t *testing.T) {
	k := New("t")
	b := k.Bus()
	b.On(k.Root(), "e", func(*Event) error { return errors.New("veto") })
	if _, err := b.Waterfall("e", nil); err == nil || !strings.Contains(err.Error(), "veto") {
		t.Fatalf("Waterfall 应中断并返回错误，得到 %v", err)
	}
}

func TestBusParallelCollectsErrors(t *testing.T) {
	k := New("t")
	b := k.Bus()
	for i := 0; i < 5; i++ {
		b.On(k.Root(), "e", func(*Event) error { return errors.New("boom") })
	}
	err := b.Parallel("e", nil)
	if err == nil {
		t.Fatal("Parallel 应返回合并错误")
	}
}

func TestBusHandlerPanicIsolated(t *testing.T) {
	k := New("t")
	b := k.Bus()
	b.On(k.Root(), "e", func(*Event) error { panic("kaboom") })
	// 不 panic 即通过；Emit 里已 recover。
	b.Emit("e", nil)
	if err := b.Bail("e", nil); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Bail 应把 panic 变成错误，得到 %v", err)
	}
}

func TestBusUnsubscribesOnDispose(t *testing.T) {
	k := New("t")
	b := k.Bus()
	child := k.Root().Child("c")
	b.On(child, "e", func(*Event) error { return nil })
	if got := b.Count("e"); got != 1 {
		t.Fatalf("Count = %d，期望 1", got)
	}
	child.Dispose()
	if got := b.Count("e"); got != 0 {
		t.Fatalf("撤销后 Count = %d，期望 0", got)
	}
}

// TestMissingManifestEnablesAll 锁死一条语义：没有 manifest 文件时二进制必须能用。
// （v0.2.1-go.5 发布后下载到空目录跑 `dshd tools`，报了「缺 tools 能力」——就是这条没测。）
func TestMissingManifestEnablesAll(t *testing.T) {
	m, err := LoadManifest("no-such-dsh.toml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Present {
		t.Fatal("文件不存在时 Present 必须是 false")
	}
	mods := []Module{{ID: "a", Provides: []string{"x"}}, {ID: "b", Provides: []string{"y"}}}
	got, err := m.Apply(mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("没有 manifest 时应全开，得到 %d 个", len(got))
	}
}

// TestEmptyManifestDisablesAll 反过来：文件存在但空，就是"一个都不开"。
func TestEmptyManifestDisablesAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsh.toml")
	if err := os.WriteFile(path, []byte("[profile]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Present {
		t.Fatal("文件存在时 Present 必须是 true")
	}
	got, err := m.Apply([]Module{{ID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("空 manifest 应该什么都不开，得到 %d 个", len(got))
	}
}

// TestUnimplementedBackendFailsLoud：manifest 声明了还没实现的后端，必须**当场报错**。
//
// 为什么值得一条测试：静默降级的后果是"我改了配置，却什么都没发生"——
// 这类故障没有任何错误信息可查，而它的成本全部落在下一个人身上。
// 用的后端是 process：wasm 已经在 2026-11-04 落地（装载与校验在 cmd/dshd/wasmload.go），
// 不能再拿它当"未实现"的例子。
func TestUnimplementedBackendFailsLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsh.toml")
	body := "[profile]\nname = \"x\"\n\n[[module]]\nid = \"a\"\nbackend = \"process\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply([]Module{{ID: "a"}}); err == nil {
		t.Fatal("声明的 process 后端还没实现，Apply 必须报错，而不是静默按 static 跑")
	} else if !strings.Contains(err.Error(), "process") {
		t.Fatalf("错误信息要指出是哪个后端：%v", err)
	}
}

// TestWasmBackendAccepted：wasm 已经是**真的能装载**的后端，Apply 不能再用它报错。
//
// 这里只钉 kernel 这一侧：Apply 放行、backend 落到模块上、能力仍为空。
// "真的装起来"由 cmd/dshd 的 wasmload 测试证明（kernel 不许 import wasmhost）。
func TestWasmBackendAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsh.toml")
	body := "[profile]\nname = \"x\"\n\n[[module]]\nid = \"a\"\nbackend = \"wasm\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Apply([]Module{{ID: "a"}})
	if err != nil {
		t.Fatalf("wasm 已经落地，Apply 不该再报错：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当装配 1 个模块，得到 %d", len(got))
	}
	if got[0].Backend != BackendWasm {
		t.Fatalf("backend 应落到模块上，得到 %q", got[0].Backend)
	}
	if len(got[0].Provides) != 0 {
		t.Fatalf("wasm 模块不该凭空多出能力：%v", got[0].Provides)
	}
}

// 反过来：static（以及不写 backend）必须照常通过。
func TestStaticBackendStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsh.toml")
	body := "[profile]\nname = \"x\"\n\n[[module]]\nid = \"a\"\nbackend = \"static\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadManifest(path)
	got, err := m.Apply([]Module{{ID: "a"}})
	if err != nil {
		t.Fatalf("static 是实现了的，不该报错：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当装配 1 个模块，得到 %d", len(got))
	}
}

// enabled = false 必须真的把模块关掉。
//
// 这条是真机验出来的：字段以前只解析、不使用，于是 manifest 里写着 enabled=false，
// 模块照样装配、工具照样在表里——"改了配置却什么都没发生"，而且没有任何提示。
func TestDisabledModuleIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsh.toml")
	body := "[profile]\nname = \"x\"\n\n[[module]]\nid = \"a\"\n\n[[module]]\nid = \"b\"\nenabled = false\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Apply([]Module{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("只该剩 a，实际 %v", got)
	}
}
